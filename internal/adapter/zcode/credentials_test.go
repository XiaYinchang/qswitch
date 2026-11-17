package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func encryptV2Fixture(t *testing.T, source string, key []byte, value string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := []byte("fixture-nonc")
	sealed := gcm.Seal(nil, nonce, []byte(value), nil)
	tag, data := sealed[len(sealed)-16:], sealed[:len(sealed)-16]
	fingerprint := sha256.Sum256(key)
	b64 := base64.RawURLEncoding.EncodeToString
	return "enc:v2:" + source + ":" + hex.EncodeToString(fingerprint[:])[:24] + ":" + b64(nonce) + "." + b64(tag) + "." + b64(data)
}

func clearManagedSecret(t *testing.T) {
	t.Helper()
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "")
	t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", "")
}

func v2Fixture(t *testing.T, provider, source string, key []byte) map[string]string {
	t.Helper()
	profile := `{"id":"current","email":"current@example.test"}`
	if provider == "zai" {
		profile = `{"user_id":"current","email":"current@example.test"}`
	}
	values := map[string]string{
		"oauth:active_provider":               provider,
		"oauth:" + provider + ":user_info":    profile,
		planKey(provider, "current"):          "current-plan-key",
		"oauth:" + provider + ":access_token": "unrelated-oauth-token",
		planKey(provider, "other"):            "unrelated-other-plan-key",
	}
	for field, value := range values {
		values[field] = encryptV2Fixture(t, source, key, value)
	}
	return values
}

func TestCaptureV2UsesExactSourceOnceAndDoesNotMigrate(t *testing.T) {
	for _, source := range []string{"os", "env"} {
		for _, provider := range []string{"bigmodel", "zai"} {
			t.Run(source+"/"+provider, func(t *testing.T) {
				clearManagedSecret(t)
				home := t.TempDir()
				key := sha256.Sum256([]byte("synthetic raw OS key material"))
				if source == "env" {
					t.Setenv("ZCODE_CREDENTIAL_SECRET", "  synthetic secret with whitespace\n")
					key = sha256.Sum256([]byte(os.Getenv("ZCODE_CREDENTIAL_SECRET")))
				}
				before := writeFixture(t, home, v2Fixture(t, provider, source, key[:]))
				calls := 0
				a := Adapter{keyringGet: func(service, account string) (string, error) {
					calls++
					path, err := filepath.Abs(credentialsPath(home))
					if err != nil {
						t.Fatal(err)
					}
					sum := sha256.Sum256([]byte(path))
					if service != "ZCode Credential Encryption" || account != hex.EncodeToString(sum[:]) {
						t.Fatal("wrong native credential identity")
					}
					return base64.RawURLEncoding.EncodeToString(key[:]), nil
				}}
				blobs, warnings, err := a.Capture(home)
				if err != nil || len(blobs) != 1 || len(warnings) != 0 {
					t.Fatalf("capture: count=%d warnings=%d err=%v", len(blobs), len(warnings), err)
				}
				if blobs[0].Identity.StableID != provider+":current" || blobs[0].Identity.Email != "current@example.test" {
					t.Fatal("v2 changed account identity")
				}
				creds, err := CredsFromBlob(blobs[0])
				if err != nil || creds.Provider != provider || creds.APIKey != "current-plan-key" {
					t.Fatal("v2 selected the wrong account quota key")
				}
				if strings.Contains(string(blobs[0].Payload), "unrelated-") {
					t.Fatal("v2 imported unrelated credentials")
				}
				wantCalls := 0
				if source == "os" {
					wantCalls = 1
				}
				if calls != wantCalls {
					t.Fatalf("keyring calls=%d, want %d", calls, wantCalls)
				}
				after, err := os.ReadFile(credentialsPath(home))
				if err != nil || string(after) != string(before) {
					t.Fatal("capture rewrote desktop credentials")
				}
				files, err := os.ReadDir(filepath.Dir(credentialsPath(home)))
				if err != nil || len(files) != 1 || files[0].Name() != "credentials.json" {
					t.Fatal("capture created a key, marker, or migration file")
				}
			})
		}
	}
}

func TestCaptureV2RetriesKeyringAfterRecovery(t *testing.T) {
	clearManagedSecret(t)
	home := t.TempDir()
	key := sha256.Sum256([]byte("synthetic retry key"))
	before := writeFixture(t, home, v2Fixture(t, "bigmodel", "os", key[:]))
	calls := 0
	a := Adapter{keyringGet: func(string, string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("backend failure containing secret-that-must-not-escape")
		}
		return base64.RawURLEncoding.EncodeToString(key[:]), nil
	}}
	if blobs, _, err := a.Capture(home); !errors.Is(err, ErrOSCredential) || len(blobs) != 0 || strings.Contains(err.Error(), "secret-that-must-not-escape") {
		t.Fatal("keyring failure was not safely reported")
	}
	if blobs, _, err := a.Capture(home); err != nil || len(blobs) != 1 || calls != 2 {
		t.Fatalf("keyring recovery was cached or repeated: calls=%d err=%v", calls, err)
	}
	after, _ := os.ReadFile(credentialsPath(home))
	if string(before) != string(after) {
		t.Fatal("recovery changed the desktop store")
	}
}

func TestV2KeyringAuthorizationWaitIsNotReportedAsMissingAccount(t *testing.T) {
	clearManagedSecret(t)
	home := t.TempDir()
	key := sha256.Sum256([]byte("synthetic authorization key"))
	writeFixture(t, home, v2Fixture(t, "bigmodel", "os", key[:]))
	a := Adapter{keyringGet: func(string, string) (string, error) { return "", ErrOSCredentialPending }}
	if blobs, _, err := a.Capture(home); !errors.Is(err, ErrOSCredentialPending) || len(blobs) != 0 {
		t.Fatalf("authorization wait error=%v", err)
	}
}

func TestV2SourceSelectionAndFileRules(t *testing.T) {
	secret := "  synthetic file secret with trailing whitespace\n"
	key := sha256.Sum256([]byte(secret))
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{"explicit secret", false}, {"explicit file", false}, {"default private file", false},
		{"both explicit settings", true}, {"missing env key", true}, {"missing explicit file", true},
		{"short file", true}, {"oversize file", true}, {"non UTF8 file", true}, {"directory file", true},
		{"public default file", runtime.GOOS != "windows"}, {"marker pins OS", true}, {"other ciphertext pins OS", true},
		{"plaintext mentioning encryption does not pin", false},
		{"invalid marker", true}, {"empty marker", true}, {"unreadable marker", true},
		{"explicit secret ignores invalid marker", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearManagedSecret(t)
			home := t.TempDir()
			store := v2Fixture(t, "bigmodel", "env", key[:])
			writeFixture(t, home, store)
			dir := filepath.Dir(credentialsPath(home))
			file := filepath.Join(dir, "credential-secret")
			writeSecret := func(data []byte, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(file, data, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(file, mode); err != nil {
					t.Fatal(err)
				}
			}
			writeMarker := func(data string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "credential-source"), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "explicit secret":
				t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
			case "explicit file":
				writeSecret([]byte(secret), 0o644)
				t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", file)
			case "default private file":
				writeSecret([]byte(secret), 0o600)
			case "both explicit settings":
				t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
				t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", file)
			case "missing explicit file":
				t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", file)
			case "short file":
				writeSecret([]byte("short"), 0o600)
			case "oversize file":
				writeSecret([]byte(strings.Repeat("x", 4097)), 0o600)
			case "non UTF8 file":
				writeSecret(append([]byte(strings.Repeat("x", 32)), 0xff), 0o600)
			case "directory file":
				if err := os.Mkdir(file, 0o700); err != nil {
					t.Fatal(err)
				}
			case "public default file":
				writeSecret([]byte(secret), 0o644)
			case "marker pins OS":
				writeSecret([]byte(secret), 0o600)
				writeMarker("os\n")
			case "other ciphertext pins OS":
				writeSecret([]byte(secret), 0o600)
				store["unrelated"] = encryptV2Fixture(t, "os", key[:], "other")
				writeFixture(t, home, store)
			case "plaintext mentioning encryption does not pin":
				writeSecret([]byte(secret), 0o600)
				store["unrelated"] = "plaintext mentioning enc:v2:os: is not ciphertext"
				writeFixture(t, home, store)
			case "invalid marker":
				writeMarker("env\n")
			case "empty marker":
				writeMarker("")
			case "unreadable marker":
				if err := os.Mkdir(filepath.Join(dir, "credential-source"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "explicit secret ignores invalid marker":
				t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
				writeMarker("invalid")
			}
			a := Adapter{keyringGet: func(string, string) (string, error) {
				t.Fatal("source selection fell back to OS keyring")
				return "", nil
			}}
			blobs, _, err := a.Capture(home)
			if (err != nil) != tc.wantErr || !tc.wantErr && len(blobs) != 1 || tc.wantErr && len(blobs) != 0 {
				t.Fatalf("capture result count=%d err=%v; want error=%v", len(blobs), err, tc.wantErr)
			}
		})
	}
}

func TestV2RejectsWrongSourceKeyAndEnvelope(t *testing.T) {
	clearManagedSecret(t)
	secret := "synthetic configured secret"
	t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
	key := sha256.Sum256([]byte(secret))
	valid := encryptV2Fixture(t, "env", key[:], "bigmodel")
	otherKey := sha256.Sum256([]byte("other synthetic secret"))
	for _, tc := range []struct {
		name, value string
		want        error
	}{
		{"unsupported version", "enc:v3:anything", ErrCredentialVersion},
		{"unknown encryption", "enc:unknown", ErrCredentialVersion},
		{"unsupported source", strings.Replace(valid, ":env:", ":other:", 1), ErrCredentialFormat},
		{"OS source with configured env", encryptV2Fixture(t, "os", key[:], "bigmodel"), ErrCredentialSource},
		{"different fingerprint", encryptV2Fixture(t, "env", otherKey[:], "bigmodel"), ErrCredentialKeyMismatch},
		{"malformed fingerprint", "enc:v2:env:zzz:broken", ErrCredentialFormat},
		{"malformed ciphertext", valid[:strings.LastIndex(valid, ":")+1] + "AA.AA.AA", ErrCredentialFormat},
		{"tampered ciphertext", valid[:len(valid)-1] + "!", ErrCredentialFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeFixture(t, home, map[string]string{"oauth:active_provider": tc.value})
			a := Adapter{keyringGet: func(string, string) (string, error) {
				t.Fatal("invalid envelope accessed a different source")
				return "", nil
			}}
			if blobs, _, err := a.Capture(home); !errors.Is(err, tc.want) || len(blobs) != 0 {
				t.Fatalf("capture error=%v, want %v", err, tc.want)
			}
		})
	}
	// Matching fingerprint alone cannot authenticate a changed ciphertext.
	parts := strings.SplitN(valid, ":", 5)
	bits := strings.Split(parts[4], ".")
	data, _ := base64.RawURLEncoding.DecodeString(bits[2])
	data[0] ^= 1
	bits[2] = base64.RawURLEncoding.EncodeToString(data)
	parts[4] = strings.Join(bits, ".")
	if _, err := decryptCredential(t.TempDir(), strings.Join(parts, ":")); !errors.Is(err, ErrCredentialSecret) {
		t.Fatalf("GCM accepted altered ciphertext: %v", err)
	}
}

func TestV2RejectsMalformedOSKeys(t *testing.T) {
	clearManagedSecret(t)
	key := sha256.Sum256([]byte("synthetic OS key"))
	for _, value := range []string{"", "invalid", base64.RawURLEncoding.EncodeToString(key[:31]), base64.RawURLEncoding.EncodeToString(key[:]) + "\n", base64.StdEncoding.EncodeToString(key[:])} {
		t.Run("invalid key", func(t *testing.T) {
			home := t.TempDir()
			writeFixture(t, home, v2Fixture(t, "bigmodel", "os", key[:]))
			a := Adapter{keyringGet: func(string, string) (string, error) { return value, nil }}
			if blobs, _, err := a.Capture(home); !errors.Is(err, ErrOSCredentialFormat) || len(blobs) != 0 {
				t.Fatalf("malformed OS key error=%v", err)
			}
		})
	}
}

func TestV1ManagedFilesAndLegacyFallbackRemainReadOnly(t *testing.T) {
	for _, mode := range []string{"explicit file", "default file", "trimmed secret", "fallback despite env", "OS pinned fallback"} {
		t.Run(mode, func(t *testing.T) {
			clearManagedSecret(t)
			home := t.TempDir()
			secret := "  synthetic legacy managed secret\n"
			fallback, err := fallbackCredentialSecret(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(credentialsPath(home)), 0o700); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "explicit file", "default file":
				path := filepath.Join(filepath.Dir(credentialsPath(home)), "credential-secret")
				if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
					t.Fatal(err)
				}
				if mode == "explicit file" {
					t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", path)
				}
			case "trimmed secret":
				t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
				secret = strings.TrimSpace(secret)
			case "fallback despite env":
				t.Setenv("ZCODE_CREDENTIAL_SECRET", "other configured key")
				secret = fallback
			case "OS pinned fallback":
				if err := os.WriteFile(filepath.Join(filepath.Dir(credentialsPath(home)), "credential-source"), []byte("os\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				secret = fallback
			}
			r := credentialReader{home: home, keyringGet: func(string, string) (string, error) {
				t.Fatal("legacy decrypt initialized or read an unnecessary OS key")
				return "", nil
			}}
			got, err := r.decrypt(encryptFixture(t, secret, "legacy-value"))
			if err != nil || got != "legacy-value" {
				t.Fatalf("legacy value=%q err=%v", got, err)
			}
		})
	}
}

func TestNativeWindowsCredentialIdentityAndEncoding(t *testing.T) {
	if got := windowsCredentialTarget("ZCode Credential Encryption", "synthetic-path-hash"); got != "synthetic-path-hash.ZCode Credential Encryption" {
		t.Fatalf("wrong Rust keyring target: %s", got)
	}
	value := strings.Repeat("A", 43)
	raw := make([]byte, 86)
	for i := range value {
		raw[2*i] = value[i]
	}
	if got, err := decodeWindowsCredential(raw); err != nil || got != value {
		t.Fatalf("valid UTF-16LE key was not decoded: %v", err)
	}
	for _, kind := range []string{"UTF8", "odd bytes", "NUL terminated", "non ASCII", "high byte"} {
		t.Run(kind, func(t *testing.T) {
			bad := append([]byte(nil), raw...)
			switch kind {
			case "UTF8":
				bad = []byte(value)
			case "odd bytes":
				bad = bad[:len(bad)-1]
			case "NUL terminated":
				bad = append(bad, 0, 0)
			case "non ASCII":
				bad[0] = 0x80
			case "high byte":
				bad[1] = 1
			}
			if _, err := decodeWindowsCredential(bad); !errors.Is(err, ErrOSCredentialFormat) {
				t.Fatalf("invalid native key format accepted: %v", err)
			}
		})
	}
}
