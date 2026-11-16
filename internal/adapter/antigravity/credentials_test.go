package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/livefile"
)

type memoryCredentialBackend struct {
	raw           []byte
	readErr       error
	writes        int
	writeThenFail bool
}

func (m *memoryCredentialBackend) Read() ([]byte, error) {
	if m.readErr != nil {
		return nil, m.readErr
	}
	if m.raw == nil {
		return nil, ErrCredentialNotFound
	}
	return append([]byte(nil), m.raw...), nil
}
func (m *memoryCredentialBackend) Write(raw []byte) error {
	m.raw = append([]byte(nil), raw...)
	m.writes++
	if m.writeThenFail {
		m.writeThenFail = false
		return errors.New("synthetic readback failure")
	}
	return nil
}
func (m *memoryCredentialBackend) Delete() error { m.raw = nil; return nil }

func syntheticCredential(label string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"token":       map[string]any{"access_token": label + "-access", "refresh_token": label + "-refresh", "token_type": "Bearer", "expiry": "2030-01-01T00:00:00Z", "future_token_field": "keep"},
		"auth_method": "consumer", "id_token_email": label + "@example.invalid", "writer_id": "native-" + label, "future_metadata": map[string]any{"preserve": true},
	})
	return raw
}

func syntheticCredentialMethod(label, method string) []byte {
	var fields map[string]any
	_ = json.Unmarshal(syntheticCredential(label), &fields)
	if method == "" {
		delete(fields, "auth_method")
	} else {
		fields["auth_method"] = method
	}
	raw, _ := json.Marshal(fields)
	return raw
}

func putSyntheticCredential(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := livefile.AtomicWrite(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadCredentialsUsesOnlyKnownLocationsAndFailsOnKeyringDenial(t *testing.T) {
	home := t.TempDir()
	paths := CredentialPaths(home)
	putSyntheticCredential(t, paths[0], syntheticCredential("first"))
	putSyntheticCredential(t, paths[1], syntheticCredential("second"))
	putSyntheticCredential(t, paths[0]+".bak", syntheticCredential("backup"))
	bk := &memoryCredentialBackend{raw: []byte(`{"token":null}`)}
	candidates, err := readCredentials(home, bk)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("got %d candidates, err=%v", len(candidates), err)
	}
	if candidates[0].Credential.IDTokenEmail != "first@example.invalid" || candidates[1].Credential.IDTokenEmail != "second@example.invalid" {
		t.Fatal("wrong native candidates")
	}
	bk.readErr = os.ErrPermission
	if _, err := readCredentials(home, bk); !errors.Is(err, os.ErrPermission) {
		t.Fatal("keyring permission denial was ignored")
	}
}

func TestWriteCredentialTransactionRestoresDistinctSlotsAfterClientFailure(t *testing.T) {
	home := t.TempDir()
	paths := CredentialPaths(home)
	first, second, key := syntheticCredential("first"), syntheticCredential("second"), []byte(`{"token":null}`)
	putSyntheticCredential(t, paths[0], first)
	putSyntheticCredential(t, paths[1], second)
	bk := &memoryCredentialBackend{raw: append([]byte(nil), key...)}
	var undo func() error
	if err := writeCredentialTransaction(home, syntheticCredential("target"), bk, livefile.AtomicWrite, &undo); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCredential(bk.raw)
	if err != nil || !strings.HasPrefix(parsed.WriterID, "qswitch-") {
		t.Fatal("new writer identity missing")
	}
	if !bytes.Contains(bk.raw, []byte(`"future_metadata"`)) {
		t.Fatal("native metadata dropped")
	}
	for _, p := range paths[:2] {
		raw, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(raw, bk.raw) {
			t.Fatal("live slots differ")
		}
	}
	if _, err := os.Stat(paths[2]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created an unused fallback")
	}
	if undo == nil {
		t.Fatal("missing rollback")
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	for i, want := range [][]byte{first, second} {
		got, _ := os.ReadFile(paths[i])
		if !bytes.Equal(got, want) {
			t.Fatal("rollback did not restore exact slot")
		}
	}
	if !bytes.Equal(bk.raw, key) {
		t.Fatal("rollback lost empty native item")
	}
}

func TestWriteCredentialTransactionRejectsUnsupportedNativeSlotsBeforeAnyWrite(t *testing.T) {
	unsupported := map[string][]byte{
		"gcp":        syntheticCredentialMethod("old", "gcp"),
		"unknown":    syntheticCredentialMethod("old", ""),
		"api_key":    syntheticCredentialMethod("old", "api_key"),
		"malformed":  []byte(`{"token":`),
		"wrong_type": []byte(`{"token":"invalid"}`),
	}
	for name, previous := range unsupported {
		for _, slot := range []string{"keyring", "fallback", "headless fallback"} {
			t.Run(name+"/"+slot, func(t *testing.T) {
				home := t.TempDir()
				paths := CredentialPaths(home)
				key := syntheticCredential("old")
				fallbacks := [][]byte{syntheticCredential("first"), syntheticCredential("second")}
				bk := &memoryCredentialBackend{raw: key}
				if slot == "keyring" {
					bk.raw = previous
				} else {
					fallbacks[1] = previous
				}
				if slot == "headless fallback" {
					bk.readErr = ErrKeyringUnavailable
				}
				key = append([]byte(nil), bk.raw...)
				for i, raw := range fallbacks {
					putSyntheticCredential(t, paths[i], raw)
				}
				writes := 0
				write := func(path string, raw []byte, mode os.FileMode) error {
					writes++
					return livefile.AtomicWrite(path, raw, mode)
				}
				var undo func() error
				if err := writeCredentialTransaction(home, syntheticCredential("target"), bk, write, &undo); err == nil {
					t.Fatal("unsupported existing account was replaced")
				}
				if bk.writes != 0 || writes != 0 || undo != nil || !bytes.Equal(bk.raw, key) {
					t.Fatal("rejected transaction changed native credentials")
				}
				for i, expected := range fallbacks {
					got, err := os.ReadFile(paths[i])
					if err != nil || !bytes.Equal(got, expected) {
						t.Fatal("rejected transaction changed fallback credentials")
					}
				}
			})
		}
	}
}

func TestWriteCredentialTransactionRejectsUnsupportedTargetAndAllowsEmptySlots(t *testing.T) {
	for _, method := range []string{"gcp", "", "api_key"} {
		bk := &memoryCredentialBackend{raw: syntheticCredential("old")}
		if err := writeCredential(t.TempDir(), syntheticCredentialMethod("target", method), bk, livefile.AtomicWrite); err == nil || bk.writes != 0 {
			t.Fatal("unsupported target reached native storage")
		}
	}
	for _, empty := range [][]byte{nil, []byte(" \n"), []byte("null"), []byte(`{"token":null}`), []byte(`{"token":{}}`)} {
		home := t.TempDir()
		path := CredentialPaths(home)[0]
		putSyntheticCredential(t, path, empty)
		bk := &memoryCredentialBackend{raw: empty}
		if err := writeCredential(home, syntheticCredential("target"), bk, livefile.AtomicWrite); err != nil {
			t.Fatalf("empty native slot rejected: %v", err)
		}
	}
}

func TestWriteCredentialRollsBackOnPartialFailure(t *testing.T) {
	home := t.TempDir()
	paths := CredentialPaths(home)
	first, second := syntheticCredential("first"), syntheticCredential("second")
	putSyntheticCredential(t, paths[0], first)
	putSyntheticCredential(t, paths[1], second)
	bk := &memoryCredentialBackend{}
	write := func(path string, b []byte, mode os.FileMode) error {
		if path == paths[1] {
			return errors.New("synthetic disk failure")
		}
		return livefile.AtomicWrite(path, b, mode)
	}
	if err := writeCredential(home, syntheticCredential("target"), bk, write); err == nil {
		t.Fatal("write failure ignored")
	}
	if bk.raw != nil {
		t.Fatal("created keyring item survived rollback")
	}
	for i, want := range [][]byte{first, second} {
		got, _ := os.ReadFile(paths[i])
		if !bytes.Equal(got, want) {
			t.Fatal("partial write survived rollback")
		}
	}
}

func TestWriteCredentialRestoresNativeMutationReportedAsFailure(t *testing.T) {
	old := syntheticCredential("old")
	bk := &memoryCredentialBackend{raw: old, writeThenFail: true}
	if err := writeCredential(t.TempDir(), syntheticCredential("target"), bk, livefile.AtomicWrite); err == nil {
		t.Fatal("readback failure ignored")
	}
	if !bytes.Equal(old, bk.raw) {
		t.Fatal("native write was not undone")
	}
}

func TestHeadlessCredentialsUseExistingNativeFileWithoutCreatingLogin(t *testing.T) {
	home := t.TempDir()
	bk := &memoryCredentialBackend{readErr: ErrKeyringUnavailable}
	if _, err := readCredentials(home, bk); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatal("empty installation was not recognized")
	}
	if err := writeCredential(home, syntheticCredential("target"), bk, livefile.AtomicWrite); err == nil {
		t.Fatal("manufactured a login before the native client logged in")
	}
	for _, p := range CredentialPaths(home) {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("created a native credential path")
		}
	}
	path := CredentialPaths(home)[0]
	old := syntheticCredential("old")
	putSyntheticCredential(t, path, old)
	var undo func() error
	if err := writeCredentialTransaction(home, syntheticCredential("target"), bk, livefile.AtomicWrite, &undo); err != nil {
		t.Fatal(err)
	}
	if bk.writes != 0 {
		t.Fatal("wrote an unavailable keyring")
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, old) {
		t.Fatal("headless rollback changed the original credential")
	}
}

func TestCredentialRollbackRefusesExternalChange(t *testing.T) {
	bk := &memoryCredentialBackend{raw: syntheticCredential("old")}
	var undo func() error
	if err := writeCredentialTransaction(t.TempDir(), syntheticCredential("target"), bk, livefile.AtomicWrite, &undo); err != nil {
		t.Fatal(err)
	}
	external := syntheticCredential("external")
	bk.raw = external
	if err := undo(); err == nil {
		t.Fatal("external identity was not detected")
	}
	if !bytes.Equal(bk.raw, external) {
		t.Fatal("external identity overwritten")
	}
}

func TestCredentialFilesRejectSymlink(t *testing.T) {
	home := t.TempDir()
	path := CredentialPaths(home)[0]
	target := filepath.Join(home, "elsewhere")
	putSyntheticCredential(t, target, syntheticCredential("old"))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := readCredentials(home, &memoryCredentialBackend{}); err == nil {
		t.Fatal("credential symlink accepted")
	}
}

type credentialRoundTrip func(*http.Request) (*http.Response, error)

func (f credentialRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func credentialResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestRefreshPreservesMetadataAndRefreshTokenAndAcceptsRotation(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "rotated"}[rotate], func(t *testing.T) {
			client := CredentialsClient{OAuth: &OAuthConfig{ClientID: "synthetic-client", ClientSecret: "synthetic-app-secret"}, Client: &http.Client{Transport: credentialRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://oauth2.googleapis.com/token" || r.Method != "POST" {
					t.Fatal("wrong OAuth endpoint")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != "synthetic-client" || r.Form.Get("client_secret") != "synthetic-app-secret" {
					t.Fatal("native OAuth configuration or stored refresh token missing")
				}
				body := `{"access_token":"new-access","token_type":"Bearer","expires_in":3600}`
				if rotate {
					body = `{"access_token":"new-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`
				}
				return credentialResponse(200, body), nil
			})}}
			raw, err := client.Refresh(context.Background(), syntheticCredential("old"))
			if err != nil {
				t.Fatal(err)
			}
			c, err := ParseCredential(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := "old-refresh"
			if rotate {
				want = "rotated-refresh"
			}
			if c.Token.AccessToken != "new-access" || c.Token.RefreshToken != want || time.Until(c.Token.Expiry) < 59*time.Minute {
				t.Fatal("refresh result incorrect")
			}
			if !bytes.Contains(raw, []byte(`"future_token_field":"keep"`)) || !bytes.Contains(raw, []byte(`"future_metadata"`)) {
				t.Fatal("refresh discarded native metadata")
			}
		})
	}
}

func TestVerifyRequiresMatchingVerifiedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", `{"id":"synthetic-id","email":"old@example.invalid","verified_email":true}`, true},
		{"unverified", `{"id":"synthetic-id","email":"old@example.invalid","verified_email":false}`, false},
		{"mismatch", `{"id":"synthetic-id","email":"other@example.invalid","verified_email":true}`, false},
		{"no_id", `{"email":"old@example.invalid","verified_email":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := CredentialsClient{OAuth: &OAuthConfig{ClientID: "synthetic-client", ClientSecret: "synthetic-app-secret"}, Client: &http.Client{Transport: credentialRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://www.googleapis.com/oauth2/v2/userinfo" || r.Header.Get("Authorization") != "Bearer old-access" {
					t.Fatal("verification did not use the candidate token")
				}
				return credentialResponse(200, tc.body), nil
			})}}
			id, err := client.Verify(context.Background(), syntheticCredential("old"))
			if (err == nil) != tc.ok {
				t.Fatalf("verification success=%v wanted %v", err == nil, tc.ok)
			}
			if tc.ok && id.StableID != "synthetic-id" {
				t.Fatal("wrong stable identity")
			}
		})
	}
}

func TestOAuthErrorsDoNotExposeResponseOrTransportSecrets(t *testing.T) {
	secret := "synthetic-secret-never-log"
	client := CredentialsClient{OAuth: &OAuthConfig{ClientID: "synthetic-client", ClientSecret: "synthetic-app-secret"}, Client: &http.Client{Transport: credentialRoundTrip(func(*http.Request) (*http.Response, error) { return credentialResponse(400, secret), nil })}}
	_, err := client.Refresh(context.Background(), syntheticCredential("old"))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("OAuth response included in error")
	}
	client.Client.Transport = credentialRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New(secret) })
	_, err = client.Verify(context.Background(), syntheticCredential("old"))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("transport details included in error")
	}
}

func TestOAuthFailuresClassifyReauthenticationWithoutLeakingDetails(t *testing.T) {
	const detail = "synthetic-error-detail-must-not-escape"
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		userinfo bool
		wantAuth bool
	}{
		{"revoked refresh token", 400, `{"error":"invalid_grant","error_description":"` + detail + `"}`, false, true},
		{"invalid OAuth token", 401, `{"error":"invalid_token","access_token":"` + detail + `"}`, false, true},
		{"userinfo unauthorized without JSON", 401, detail, true, true},
		{"userinfo explicit token error", 403, `{"error":"invalid_token","error_description":"` + detail + `"}`, true, true},
		{"wrong application configuration", 400, `{"error":"invalid_client","error_description":"` + detail + `"}`, false, false},
		{"refresh unauthorized without code", 401, detail, false, false},
		{"userinfo permission failure", 403, `{"error":{"code":403,"message":"` + detail + `"}}`, true, false},
		{"unknown OAuth code", 400, `{"error":"` + detail + `"}`, false, false},
		{"malformed OAuth body", 400, `{"error":"invalid_grant"} trailing ` + detail, false, false},
		{"throttled token refresh", 429, `{"error":"invalid_grant","error_description":"` + detail + `"}`, false, false},
		{"failed token service", 503, `{"error":"invalid_token","error_description":"` + detail + `"}`, false, false},
		{"throttled userinfo", 429, `{"error":"invalid_token","error_description":"` + detail + `"}`, true, false},
		{"failed userinfo service", 500, `{"error":"invalid_grant","error_description":"` + detail + `"}`, true, false},
		{"oversized OAuth body", 400, `{"error":"invalid_grant","error_description":"` + strings.Repeat("x", maxCredentialBytes) + `"}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := CredentialsClient{
				OAuth: &OAuthConfig{ClientID: "synthetic-client", ClientSecret: "synthetic-app-secret"},
				Client: &http.Client{Transport: credentialRoundTrip(func(*http.Request) (*http.Response, error) {
					return credentialResponse(tc.status, tc.body), nil
				})},
			}
			var err error
			if tc.userinfo {
				_, err = client.Verify(context.Background(), syntheticCredential("old"))
			} else {
				_, err = client.Refresh(context.Background(), syntheticCredential("old"))
			}
			if err == nil || errors.Is(err, ErrReauthenticationRequired) != tc.wantAuth {
				t.Fatalf("reauthentication=%v; want %v (error=%v)", errors.Is(err, ErrReauthenticationRequired), tc.wantAuth, err)
			}
			if strings.Contains(err.Error(), detail) || strings.Contains(err.Error(), "old-refresh") || strings.Contains(err.Error(), "old-access") {
				t.Fatal("OAuth error exposed response details or a credential")
			}
		})
	}
}

func TestMissingRefreshTokenRequiresReauthenticationBeforeHTTP(t *testing.T) {
	client := CredentialsClient{Client: &http.Client{Transport: credentialRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing refresh token must stop before any HTTP request")
		return nil, nil
	})}}
	raw := []byte(`{"auth_method":"consumer","token":{"access_token":"synthetic-access-without-refresh"}}`)
	_, err := client.Refresh(context.Background(), raw)
	if !errors.Is(err, ErrReauthenticationRequired) {
		t.Fatalf("missing refresh token did not request a new login: %v", err)
	}
}

func TestOAuthTransportFailureRemainsRetryable(t *testing.T) {
	const detail = "synthetic-transport-detail-must-not-escape"
	client := CredentialsClient{
		OAuth: &OAuthConfig{ClientID: "synthetic-client", ClientSecret: "synthetic-app-secret"},
		Client: &http.Client{Transport: credentialRoundTrip(func(*http.Request) (*http.Response, error) {
			return nil, errors.New(detail)
		})},
	}
	_, refreshErr := client.Refresh(context.Background(), syntheticCredential("old"))
	_, verifyErr := client.Verify(context.Background(), syntheticCredential("old"))
	for _, err := range []error{refreshErr, verifyErr} {
		if err == nil || errors.Is(err, ErrReauthenticationRequired) || strings.Contains(err.Error(), detail) {
			t.Fatal("transport failure must remain retryable and redact transport details")
		}
	}
}

func TestOAuthRefreshDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	client := CredentialsClient{OAuth: &OAuthConfig{ClientID: "synthetic-client", ClientSecret: "synthetic-app-secret"}, Client: &http.Client{Transport: credentialRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		response := credentialResponse(307, "")
		response.Header.Set("Location", "https://untrusted.example.invalid/token")
		return response, nil
	})}}
	if _, err := client.Refresh(context.Background(), syntheticCredential("old")); err == nil {
		t.Fatal("redirect treated as refresh success")
	}
	if calls != 1 {
		t.Fatal("refresh token forwarded to a redirect target")
	}
}
