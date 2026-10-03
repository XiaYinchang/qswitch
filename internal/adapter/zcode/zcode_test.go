package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
)

func encryptFixture(t *testing.T, secret, value string) string {
	t.Helper()
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := []byte("0123456789ab")
	sealed := gcm.Seal(nil, iv, []byte(value), nil)
	tag := sealed[len(sealed)-gcm.Overhead():]
	data := sealed[:len(sealed)-gcm.Overhead()]
	b64 := base64.RawURLEncoding.EncodeToString
	return "enc:v1:" + b64(iv) + "." + b64(tag) + "." + b64(data)
}

func writeFixture(t *testing.T, home string, values map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := credentialsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCaptureCurrentDesktopSubscriptionOnly(t *testing.T) {
	for _, provider := range []string{"bigmodel", "zai"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("ZCODE_CREDENTIAL_SECRET", "fixture-secret")
			home := t.TempDir()
			profile := `{"id":"current","username":"current@example.test","displayName":"Current","rawProfile":{"email":"current@example.test"}}`
			if provider == "zai" {
				profile = `{"user_id":"current","email":"current@example.test","name":"Current"}`
			}
			values := map[string]string{
				"oauth:active_provider":               provider,
				"oauth:" + provider + ":user_info":    profile,
				"oauth:" + provider + ":access_token": "unneeded-oauth-token",
				"zcodejwttoken":                       "unneeded-zcode-jwt",
				planKey(provider, "current"):          "current-plan-key",
				planKey(provider, "other"):            "other-account-key",
				"unrelated-server-credential":         "unrelated-secret",
			}
			for k, v := range values {
				values[k] = encryptFixture(t, "fixture-secret", v)
			}
			before := writeFixture(t, home, values)
			cli := filepath.Join(home, ".zcode", "cli", "config.json")
			if err := os.MkdirAll(filepath.Dir(cli), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cli, []byte(`{"provider":{"custom":{"apiKey":"independent-cli-key"}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			a := Adapter{}
			blobs, warnings, err := a.Capture(home)
			if err != nil || len(blobs) != 1 || len(warnings) != 0 {
				t.Fatalf("capture count=%d warnings=%v err=%v", len(blobs), warnings, err)
			}
			blob := blobs[0]
			id, err := a.IdentityOf(blob)
			if err != nil || id.Tool != adapter.ZCode || id.StableID != provider+":current" || id.Email != "current@example.test" || id.DisplayName != "Current" {
				t.Fatalf("identity=%+v err=%v", id, err)
			}
			creds, err := CredsFromBlob(blob)
			if err != nil || creds.Provider != provider || creds.APIKey != "current-plan-key" {
				t.Fatalf("wrong quota credential selection: %v", err)
			}
			for _, unrelated := range []string{"unneeded-oauth-token", "unneeded-zcode-jwt", "other-account-key", "unrelated-secret", "independent-cli-key"} {
				if strings.Contains(string(blob.Payload), unrelated) {
					t.Fatal("capture retained an unrelated credential")
				}
			}
			if err := a.Restore(home, blob, adapter.RestoreOpts{}); !errors.Is(err, ErrReadOnly) {
				t.Fatalf("restore error=%v", err)
			}
			if err := a.KillCLI(home); !errors.Is(err, ErrReadOnly) || a.Idle(home, time.Minute) {
				t.Fatal("monitor-only adapter must not authorize termination or switching")
			}
			after, _ := os.ReadFile(credentialsPath(home))
			cliAfter, _ := os.ReadFile(cli)
			if string(after) != string(before) || string(cliAfter) != `{"provider":{"custom":{"apiKey":"independent-cli-key"}}}` {
				t.Fatal("monitoring changed live credentials or CLI config")
			}
			fingerprint, err := a.Fingerprint(home)
			if err != nil || fingerprint != sha256.Sum256(before) || len(a.LivePaths(home)) != 1 || a.LivePaths(home)[0] != credentialsPath(home) {
				t.Fatalf("incorrect desktop fingerprint/path: %v", err)
			}
		})
	}
}

func TestCredentialEncryptionAndMissingSecret(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "")
	secret, err := credentialSecret(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value string
		wantErr     error
	}{
		{"official local fallback", encryptFixture(t, secret, "plain-test-value"), nil},
		{"legacy plaintext", "plain-test-value", nil},
		{"missing custom secret", encryptFixture(t, "desktop-custom-secret", "plain-test-value"), ErrCredentialSecret},
		{"bad envelope", "enc:v1:broken", ErrCredentialFormat},
		{"invalid base64", "enc:v1:?.?.?", ErrCredentialFormat},
		{"wrong IV and tag sizes", "enc:v1:AA.AA.AA", ErrCredentialFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decryptCredential(home, tc.value)
			if !errors.Is(err, tc.wantErr) || (err == nil && got != "plain-test-value") {
				t.Fatalf("decryption result incorrect: err=%v", err)
			}
		})
	}
}

func TestCaptureNeverFallsBackToAnotherAccountKey(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, home, map[string]string{
		"oauth:active_provider":      "bigmodel",
		"oauth:bigmodel:user_info":   `{"id":"current"}`,
		planKey("bigmodel", "other"): "other-key",
	})
	if blobs, _, err := (Adapter{}).Capture(home); err == nil || len(blobs) != 0 {
		t.Fatal("missing current account key must fail, not select another saved account")
	}
}
