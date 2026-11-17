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
			t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", "")
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
	t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", "")
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

const syntheticPersonalLocator = "project-key-location:v1:%5B%22https%3A%2F%2Fbigmodel.cn%22%2C%22bigmodel%22%2C%22current%22%2Cnull%2C1%2C%22non-team%22%5D"

func TestCaptureOAuthPersonalProjectOnly(t *testing.T) {
	for _, provider := range []string{"bigmodel", "zai"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", "")
			t.Setenv("ZCODE_CREDENTIAL_SECRET", "synthetic-oauth-desktop-secret")
			key := sha256.Sum256([]byte("synthetic-oauth-desktop-secret"))
			home := t.TempDir()
			locatorKey := syntheticPersonalLocator
			if provider == "zai" {
				locatorKey = "project-key-location:v1:%5B%22https%3A%2F%2Fapi.z.ai%22%2C%22zai%22%2C%22current%22%2Cnull%2C1%2C%22non-team%22%5D"
			}
			store := map[string]string{
				"oauth:active_provider":                                        provider,
				"oauth:" + provider + ":user_info":                             `{"id":"current","email":"current@example.test"}`,
				"oauth:" + provider + ":access_token":                          "synthetic-login-token",
				"oauth:" + provider + ":refresh_token":                         "must-not-retain-refresh-token",
				locatorKey:                                                     `{"organizationId":"personal-org","projectId":"personal-project","apiKeyId":"personal-key-id"}`,
				strings.Replace(locatorKey, "non-team", "default", 1):          `{"organizationId":"wrong-default","projectId":"wrong-default","apiKeyId":"wrong-default"}`,
				strings.Replace(locatorKey, "%22current%22", "%22other%22", 1): `{"organizationId":"wrong-other-account","projectId":"wrong-other-account","apiKeyId":"wrong-other-account"}`,
				strings.Replace(locatorKey, "non-team", "team", 1):             `{"organizationId":"wrong-team","projectId":"wrong-team","apiKeyId":"wrong-team"}`,
				planKey(provider, "other"):                                     "wrong-old-key",
			}
			for field, value := range store {
				store[field] = encryptV2Fixture(t, "env", key[:], value)
			}
			before := writeFixture(t, home, store)
			blobs, _, err := (Adapter{}).Capture(home)
			if err != nil || len(blobs) != 1 {
				t.Fatalf("OAuth capture count=%d err=%v", len(blobs), err)
			}
			creds, err := CredsFromBlob(blobs[0])
			wantLocation := ProjectKeyLocation{OrganizationID: "personal-org", ProjectID: "personal-project", APIKeyID: "personal-key-id"}
			if err != nil || blobs[0].Identity.StableID != provider+":current" || creds.Provider != provider || creds.APIKey != "" || creds.OAuthAccessToken != "synthetic-login-token" || creds.ProjectKeyLocation == nil || *creds.ProjectKeyLocation != wantLocation {
				t.Fatal("OAuth capture did not bind the current account and personal project")
			}
			for _, excluded := range []string{"wrong-", "must-not-retain-refresh-token"} {
				if strings.Contains(string(blobs[0].Payload), excluded) {
					t.Fatal("capture imported unrelated credentials or project scope")
				}
			}
			after, err := os.ReadFile(credentialsPath(home))
			if err != nil || string(after) != string(before) {
				t.Fatal("OAuth capture changed native credentials")
			}
			files, err := os.ReadDir(filepath.Dir(credentialsPath(home)))
			if err != nil || len(files) != 1 {
				t.Fatal("OAuth capture created files instead of reading an existing locator")
			}
		})
	}
}

func TestCaptureOAuthRejectsMissingOrMismatchedPersonalLocator(t *testing.T) {
	validLocation := `{"organizationId":"personal-org","projectId":"personal-project","apiKeyId":"personal-key-id"}`
	for _, tc := range []struct{ name, key, value, token string }{
		{"default", strings.Replace(syntheticPersonalLocator, "non-team", "default", 1), validLocation, "login-token"},
		{"other account", strings.Replace(syntheticPersonalLocator, "%22current%22", "%22other%22", 1), validLocation, "login-token"},
		{"other provider", strings.Replace(syntheticPersonalLocator, "%22bigmodel%22", "%22zai%22", 1), validLocation, "login-token"},
		{"other origin", strings.Replace(syntheticPersonalLocator, "bigmodel.cn", "other.example.test", 1), validLocation, "login-token"},
		{"team", strings.Replace(syntheticPersonalLocator, "non-team", "team", 1), validLocation, "login-token"},
		{"organization scope", strings.Replace(syntheticPersonalLocator, "null", "%22personal-org%22", 1), validLocation, "login-token"},
		{"other key slot", strings.Replace(syntheticPersonalLocator, "%2C1%2C", "%2C2%2C", 1), validLocation, "login-token"},
		{"missing token", syntheticPersonalLocator, validLocation, ""},
		{"malformed locator", syntheticPersonalLocator, `{"organizationId":"org","projectId":"project"}`, "login-token"},
		{"blank location ID", syntheticPersonalLocator, `{"organizationId":"org","projectId":"project","apiKeyId":" "}`, "login-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			before := writeFixture(t, home, map[string]string{"oauth:active_provider": "bigmodel", "oauth:bigmodel:user_info": `{"id":"current"}`, "oauth:bigmodel:access_token": tc.token, tc.key: tc.value})
			if blobs, _, err := (Adapter{}).Capture(home); err == nil || len(blobs) != 0 {
				t.Fatal("capture selected a missing or mismatched OAuth project")
			}
			after, err := os.ReadFile(credentialsPath(home))
			if err != nil || string(after) != string(before) {
				t.Fatal("failed capture changed the native locator")
			}
		})
	}
}

func TestProjectLocatorMatchesDesktopEncoding(t *testing.T) {
	if got := projectLocationKey("bigmodel", "current"); got != syntheticPersonalLocator {
		t.Fatalf("locator encoding=%s", got)
	}
	const id = "a b+c/:\"&<雪>"
	const expected = "project-key-location:v1:%5B%22https%3A%2F%2Fbigmodel.cn%22%2C%22bigmodel%22%2C%22a%20b%2Bc%2F%3A%5C%22%26%3C%E9%9B%AA%3E%22%2Cnull%2C1%2C%22non-team%22%5D"
	if got := projectLocationKey("bigmodel", id); got != expected {
		t.Fatalf("special account encoding=%s", got)
	}
	if got := planKey("bigmodel", id); got != "account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:a%20b%2Bc%2F%3A%22%26%3C%E9%9B%AA%3E:api-key" {
		t.Fatalf("legacy account encoding=%s", got)
	}
}

func TestSubscriptionBlobRejectsMixedOrIncompleteOAuthAuthorization(t *testing.T) {
	location := ProjectKeyLocation{OrganizationID: "org", ProjectID: "project", APIKeyID: "locator-key-id"}
	for _, creds := range []Creds{
		{Provider: "bigmodel", APIKey: "old-key", OAuthAccessToken: "oauth", ProjectKeyLocation: &location},
		{Provider: "bigmodel", OAuthAccessToken: "oauth"},
		{Provider: "bigmodel", ProjectKeyLocation: &location},
		{Provider: "zai", OAuthAccessToken: "oauth", ProjectKeyLocation: &location},
	} {
		raw, err := json.Marshal(payload{Kind: payloadKind, Identity: adapter.Identity{StableID: "bigmodel:current"}, Creds: creds})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CredsFromBlob(adapter.Blob{Payload: raw}); err == nil {
			t.Fatal("accepted ambiguous or mismatched stored authorization")
		}
	}
}
