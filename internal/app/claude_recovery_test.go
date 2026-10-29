package app

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

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/clock"
	"qswitch/internal/livefile"
)

func setupClaudeRecovery(t *testing.T, metadata string, expired bool) *App {
	t.Helper()
	a, _ := setup(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	expires := now.Add(time.Hour)
	if expired {
		expires = now.Add(-time.Hour)
	}
	writeClaudeRecoveryLive(t, a, "old-access", "old-refresh", expires)
	if metadata != "" {
		if err := os.WriteFile(filepath.Join(a.UserHome, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"`+metadata+`","emailAddress":"stale@example.test"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func writeClaudeRecoveryLive(t *testing.T, a *App, access, refresh string, expires time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"claudeAiOauth": claude.Creds{AccessToken: access, RefreshToken: refresh, ExpiresAt: expires.UnixMilli()},
		"unrelated":     "preserve-me",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := livefile.AtomicWrite(a.Adapters[adapter.Claude].LivePaths(a.UserHome)[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeRecoveryResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func restartClaudeRecovery(t *testing.T, a *App) *App {
	t.Helper()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(a.UserHome, a.DataDir, a.Vault.Keys, nil, a.Notify, func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	next.Clock = a.Clock
	next.HTTP = a.HTTP
	t.Cleanup(func() { next.Close() })
	return next
}

func assertClaudeRecoveryTokens(t *testing.T, a *App, id, access, refresh string) {
	t.Helper()
	b, err := a.loadBlob(adapter.Claude, id)
	if err != nil {
		t.Fatal(err)
	}
	c, err := claude.CredsFromBlob(b)
	if err != nil || c.AccessToken != access || c.RefreshToken != refresh {
		t.Fatal("vault did not retain the expected token pair")
	}
}

func TestClaudeExpiredCaptureQuarantinesUnverifiedIdentity(t *testing.T) {
	for _, metadata := range []string{"", "wrong-account"} {
		t.Run("metadata="+metadata, func(t *testing.T) {
			a := setupClaudeRecovery(t, metadata, true)
			if metadata != "" {
				blobs, _, err := a.Adapters[adapter.Claude].Capture(a.UserHome)
				if err != nil {
					t.Fatal(err)
				}
				wrong, err := claude.WriteIdentity(blobs[0], adapter.Identity{Tool: adapter.Claude, StableID: metadata})
				if err != nil {
					t.Fatal(err)
				}
				if err := a.saveBlob(wrong); err != nil {
					t.Fatal(err)
				}
			}
			refreshes := 0
			profileAvailable := false
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1/oauth/token":
					refreshes++
					return claudeRecoveryResponse(req, 200, `{"access_token":"renewed-access","refresh_token":"renewed-refresh","expires_in":3600}`), nil
				case "/api/oauth/profile":
					if req.Header.Get("Authorization") == "Bearer old-access" {
						return claudeRecoveryResponse(req, 401, `{}`), nil
					}
					if !profileAvailable {
						return claudeRecoveryResponse(req, 503, `{}`), nil
					}
					return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"verified-account"}}`), nil
				default:
					t.Fatalf("unexpected request: %s", req.URL.Path)
					return nil, errors.New("unexpected request")
				}
			})}
			if _, _, err := a.Capture(adapter.Claude); err == nil {
				t.Fatal("unavailable profile reported enrollment success")
			}
			if metadata != "" {
				assertClaudeRecoveryTokens(t, a, metadata, "old-access", "old-refresh")
			}
			records, err := a.claudeRecoveries()
			if err != nil || len(records) != 1 || records[0].AccountID != "" || records[0].Updated == nil {
				t.Fatal("renewal was not quarantined independently of metadata")
			}
			ciphertext, err := os.ReadFile(filepath.Join(a.DataDir, "vault", claudeRecoveryVault, records[0].key+".enc"))
			if err != nil || bytes.Contains(ciphertext, []byte("renewed-refresh")) {
				t.Fatal("recovery is not encrypted")
			}
			a = restartClaudeRecovery(t, a)
			profileAvailable = true
			blobs, _, err := a.Capture(adapter.Claude)
			if err != nil || len(blobs) != 1 || blobs[0].Identity.StableID != "verified-account" || blobs[0].Identity.Email != "" {
				t.Fatalf("verified capture retained untrusted metadata: err=%v", err)
			}
			assertClaudeRecoveryTokens(t, a, "verified-account", "renewed-access", "renewed-refresh")
			if refreshes != 1 {
				t.Fatalf("renewed %d times; expected one exchange across restart", refreshes)
			}
		})
	}
}

// A manual refresh may retry an ordinary failure immediately, but it must not
// keep hitting a server that answered 429: on novix repeated page refreshes
// sent 12 usage requests in 40 s and extended the throttle.
func TestManualClaudeProbeWaitsForServerThrottle(t *testing.T) {
	a := setupClaudeRecovery(t, "", false)
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"live-account"}}`), nil
	})}
	if _, _, err := a.Capture(adapter.Claude); err != nil {
		t.Fatal(err)
	}
	status, usage := http.StatusTooManyRequests, 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"live-account"}}`), nil
		}
		usage++
		return claudeRecoveryResponse(req, status, `{"five_hour":{"utilization":40},"seven_day":{"utilization":10}}`), nil
	})}
	if res := a.ProbeAccount(context.Background(), adapter.Claude, "live-account"); res.Source != "http_throttled" || usage != 1 {
		t.Fatalf("first probe: source=%s requests=%d", res.Source, usage)
	}
	if res := a.ProbeAccount(context.Background(), adapter.Claude, "live-account"); res.Source != "backoff" || usage != 1 {
		t.Fatalf("manual probe ignored the server throttle: source=%s requests=%d", res.Source, usage)
	}
	status = http.StatusServiceUnavailable + 1 // an ordinary failure, not throttling
	ac, _ := a.State.GetAccount("claude", "live-account")
	a.Clock = clock.Fixed{T: time.Unix(ac.HTTPBackoffUntil, 0)}
	a.ProbeAccount(context.Background(), adapter.Claude, "live-account")
	if res := a.ProbeAccount(context.Background(), adapter.Claude, "live-account"); usage != 3 || res.Source == "backoff" {
		t.Fatalf("manual probe should retry an ordinary failure at once: source=%s requests=%d", res.Source, usage)
	}
}

func TestClaudeTransientProfileFailureNeverRefreshes(t *testing.T) {
	for _, status := range []int{0, 403, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := setupClaudeRecovery(t, "wrong-account", true)
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/oauth/profile" {
					t.Fatal("profile failure spent a refresh token")
				}
				if status == 0 {
					return nil, errors.New("temporary network failure")
				}
				return claudeRecoveryResponse(req, status, `{}`), nil
			})}
			if _, _, err := a.Capture(adapter.Claude); err == nil {
				t.Fatal("profile failure accepted credentials")
			}
			records, err := a.claudeRecoveries()
			if err != nil || len(records) != 0 {
				t.Fatal("transient profile failure created a refresh transaction")
			}
		})
	}
}

func TestClaudePendingWritebackSurvivesRestartAndCapture(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged live", true: "external login"}[external], func(t *testing.T) {
			a := setupClaudeRecovery(t, "", false)
			refreshes := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/v1/oauth/token" {
					refreshes++
					return claudeRecoveryResponse(req, 200, `{"access_token":"renewed-access","refresh_token":"renewed-refresh","expires_in":3600}`), nil
				}
				return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"verified-account"}}`), nil
			})}
			blobs, _, err := a.Capture(adapter.Claude)
			if err != nil {
				t.Fatal(err)
			}
			a.claudeWriteLive = func(string, []byte, os.FileMode) error { return errors.New("injected atomic write failure") }
			if _, _, err := a.keepAliveClaudeLocked(context.Background(), blobs[0], true, true); err == nil {
				t.Fatal("failed live write reported successful refresh")
			}
			assertClaudeRecoveryTokens(t, a, "verified-account", "renewed-access", "renewed-refresh")
			if err := a.saveBlob(blobs[0]); err == nil {
				t.Fatal("old still-valid access token overwrote the rotated vault")
			}
			a = restartClaudeRecovery(t, a)
			if external {
				writeClaudeRecoveryLive(t, a, "external-access", "external-refresh", a.now().Add(30*time.Minute))
			} else {
				a.Adapters[adapter.Claude] = claude.Adapter{List: func() ([]adapter.Proc, error) {
					return []adapter.Proc{{PID: 123456, Command: "claude"}}, nil
				}}
				if _, _, err := a.Capture(adapter.Claude); err == nil || !strings.Contains(err.Error(), "running Claude CLI") {
					t.Fatalf("recovery ignored a running CLI: %v", err)
				}
				a.Adapters[adapter.Claude] = claude.Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
			}
			if _, _, err := a.Capture(adapter.Claude); err != nil {
				t.Fatal(err)
			}
			wantAccess, wantRefresh := "renewed-access", "renewed-refresh"
			if external {
				wantAccess, wantRefresh = "external-access", "external-refresh"
			}
			assertClaudeRecoveryTokens(t, a, "verified-account", wantAccess, wantRefresh)
			live, err := claude.ReadLiveCreds(a.UserHome)
			if err != nil || live.AccessToken != wantAccess || live.RefreshToken != wantRefresh {
				t.Fatal("recovery selected the wrong live token pair")
			}
			if _, _, err := a.Capture(adapter.Claude); err != nil {
				t.Fatal(err)
			}
			if refreshes != 1 {
				t.Fatalf("recovery spent refresh token %d times", refreshes)
			}
		})
	}
}

func TestClaudeUnknownRefreshRequiresLoginWithoutBlockingOtherAccounts(t *testing.T) {
	a := setupClaudeRecovery(t, "", false)
	refreshes := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/oauth/token" {
			refreshes++
			return nil, errors.New("response lost after exchange")
		}
		return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"verified-account"}}`), nil
	})}
	blobs, _, err := a.Capture(adapter.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.keepAliveClaudeLocked(context.Background(), blobs[0], true, true); err == nil {
		t.Fatal("lost response accepted")
	}
	a = restartClaudeRecovery(t, a)
	if _, _, err := a.Capture(adapter.Claude); err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("uncertain exchange was retried: %v", err)
	}
	fresh := blobs[0]
	var env map[string]json.RawMessage
	if err := json.Unmarshal(fresh.Payload, &env); err != nil {
		t.Fatal(err)
	}
	env["credentials_json"], _ = json.Marshal(`{"claudeAiOauth":{"accessToken":"login-access","refreshToken":"login-refresh","expiresAt":4102444800000}}`)
	fresh.Payload, _ = json.Marshal(env)
	other, err := claude.WriteIdentity(fresh, adapter.Identity{Tool: adapter.Claude, StableID: "other-account"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(other); err != nil {
		t.Fatalf("uncertain exchange blocked another account: %v", err)
	}
	// The isolated-login path already verified this identity. Saving it must
	// unblock future recovery without changing the current live login itself.
	if err := a.saveBlob(fresh); err != nil {
		t.Fatalf("a verified replacement login could not recover the account: %v", err)
	}
	live, err := claude.ReadLiveCreds(a.UserHome)
	if err != nil || live.AccessToken != "old-access" {
		t.Fatal("saving isolated login modified live credentials")
	}
	if _, _, err := a.Capture(adapter.Claude); err != nil {
		t.Fatal(err)
	}
	assertClaudeRecoveryTokens(t, a, "verified-account", "login-access", "login-refresh")
	if refreshes != 1 {
		t.Fatal("unknown refresh was reused")
	}
}

func TestClaudeUnreadableRecoveryFailsClosed(t *testing.T) {
	a := setupClaudeRecovery(t, "", false)
	if err := a.Vault.Put(claudeRecoveryVault, "broken", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("unreadable recovery reached the network")
		return nil, errors.New("unexpected request")
	})}
	if _, _, err := a.Capture(adapter.Claude); err == nil {
		t.Fatal("capture ignored a malformed recovery record")
	}
}
