package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/kimi"
	"qswitch/internal/clock"
)

func writeKimiLive(t *testing.T, a *App, token string, expires int64) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"access_token": token, "refresh_token": "refresh-" + token, "expires_at": expires, "expires_in": 900, "scope": "code", "token_type": "Bearer"})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(a.UserHome, ".kimi-code", "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func kimiResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}
}

func kimiApp(t *testing.T) (*App, time.Time) {
	t.Helper()
	t.Setenv("KIMI_CODE_HOME", "")
	a, _ := setup(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	return a, now
}

func parkedKimiAccount(t *testing.T) (*App, adapter.Blob, time.Time) {
	t.Helper()
	a, now := kimiApp(t)
	writeKimiLive(t, a, "old", now.Add(-time.Minute).Unix())
	bs, _, err := a.Adapters[adapter.Kimi].Capture(a.UserHome)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := kimi.WriteIdentity(bs[0], adapter.Identity{Tool: adapter.Kimi, StableID: "user-old", Email: "old@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(blob); err != nil {
		t.Fatal(err)
	}
	writeKimiLive(t, a, "live", now.Add(time.Hour).Unix())
	return a, blob, now
}

func TestKimiCaptureRequiresStableProfileAndUnchangedToken(t *testing.T) {
	for _, kind := range []string{"ok", "missing_id", "http_failure", "token_changed"} {
		t.Run(kind, func(t *testing.T) {
			a, now := kimiApp(t)
			writeKimiLive(t, a, "start", now.Add(time.Hour).Unix())
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/coding/v1/me" {
					t.Fatalf("unexpected endpoint")
				}
				if kind == "token_changed" {
					writeKimiLive(t, a, "changed", now.Add(time.Hour).Unix())
				}
				if kind == "http_failure" {
					return kimiResponse(req, 503, `{}`), nil
				}
				if kind == "missing_id" {
					return kimiResponse(req, 200, `{"email":"x@example.invalid"}`), nil
				}
				return kimiResponse(req, 200, `{"user_id":"verified-user","email":"x@example.invalid"}`), nil
			})}
			_, _, err := a.Capture(adapter.Kimi)
			if (err == nil) != (kind == "ok") {
				t.Fatalf("capture %s err=%v", kind, err)
			}
			accounts, err := a.State.ListAccounts("kimi")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "ok" {
				if len(accounts) != 1 || accounts[0].StableID != "verified-user" {
					t.Fatalf("identity not saved")
				}
			} else if len(accounts) != 0 {
				t.Fatal("unverified identity saved")
			}
		})
	}
}

func TestKimiLiveAccountNeverRefreshes(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		t.Run(fmt.Sprint(rotated), func(t *testing.T) {
			a, blob, now := parkedKimiAccount(t)
			if !rotated {
				writeKimiLive(t, a, "old", now.Add(-time.Minute).Unix())
			}
			profileHits := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/oauth/token" {
					t.Fatal("live account was refreshed")
				}
				profileHits++
				return kimiResponse(req, 200, `{"user_id":"user-old"}`), nil
			})}
			_, token, err := a.keepAliveKimiLocked(context.Background(), blob, true, false)
			if err != nil || token == "" {
				t.Fatalf("%v", err)
			}
			if (!rotated && profileHits != 0) || (rotated && profileHits != 1) {
				t.Fatal("live detection")
			}
		})
	}
}

func TestKimiParkedRefreshKeepsIdentityAndLiveFile(t *testing.T) {
	a, _, now := parkedKimiAccount(t)
	liveBefore, _ := os.ReadFile(a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0])
	refreshHits := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/coding/v1/me" {
			return kimiResponse(req, 200, `{"user_id":"user-live"}`), nil
		}
		if req.URL.Path != "/api/oauth/token" {
			t.Fatalf("unexpected endpoint")
		}
		refreshHits++
		return kimiResponse(req, 200, `{"access_token":"renewed","refresh_token":"rotated","expires_in":900}`), nil
	})}
	a.keepAliveKimiAccounts(context.Background())
	a.keepAliveKimiAccounts(context.Background())
	if refreshHits != 1 {
		t.Fatalf("refresh count=%d", refreshHits)
	}
	b, err := a.loadBlob(adapter.Kimi, "user-old")
	if err != nil {
		t.Fatal(err)
	}
	c, err := kimi.CredsFromBlob(b)
	if err != nil || c.AccessToken != "renewed" || c.RefreshToken != "rotated" || c.ExpiresAt != now.Unix()+900 || b.Identity.StableID != "user-old" {
		t.Fatal("vault refresh invariant")
	}
	after, _ := os.ReadFile(a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0])
	if string(after) != string(liveBefore) {
		t.Fatal("live file changed")
	}
	accounts, _ := a.State.ListAccounts("kimi")
	if len(accounts) != 1 {
		t.Fatal("refresh created a second identity")
	}
}

func TestKimiParkedRefreshUncertainLiveBacksOff(t *testing.T) {
	for _, kind := range []string{"profile_failed", "token_changed", "refresh_failed", "vault_failed"} {
		t.Run(kind, func(t *testing.T) {
			a, blob, now := parkedKimiAccount(t)
			refreshHits := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/coding/v1/me" {
					if kind == "profile_failed" {
						return kimiResponse(req, 503, `{}`), nil
					}
					if kind == "token_changed" {
						writeKimiLive(t, a, "old", now.Add(time.Hour).Unix())
					}
					return kimiResponse(req, 200, `{"user_id":"user-live"}`), nil
				}
				refreshHits++
				if kind == "refresh_failed" {
					return kimiResponse(req, 503, `{}`), nil
				}
				if kind == "vault_failed" {
					a.Vault.Dir = filepath.Join(t.TempDir(), "not-a-directory")
					if err := os.WriteFile(a.Vault.Dir, []byte("x"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return kimiResponse(req, 200, `{"access_token":"renewed","refresh_token":"rotated","expires_in":900}`), nil
			})}
			_, _, err := a.keepAliveKimiLocked(context.Background(), blob, true, false)
			if err == nil {
				t.Fatal("expected refresh refusal/failure")
			}
			if (kind == "profile_failed" || kind == "token_changed") && refreshHits != 0 {
				t.Fatal("refreshed without establishing live identity")
			}
			ac, err := a.State.GetAccount("kimi", "user-old")
			if err != nil || ac.HTTPBackoffUntil != now.Add(a.Cfg.Backoff()).Unix() {
				t.Fatalf("backoff missing: %v", err)
			}
		})
	}
}

func TestKimiConcurrentKeepAliveReloadsVault(t *testing.T) {
	a, _, _ := parkedKimiAccount(t)
	hits := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/coding/v1/me" {
			return kimiResponse(req, 200, `{"user_id":"user-live"}`), nil
		}
		hits++
		return kimiResponse(req, 200, `{"access_token":"renewed","refresh_token":"rotated","expires_in":900}`), nil
	})}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { <-start; a.keepAliveKimiAccounts(context.Background()) })
	}
	close(start)
	wg.Wait()
	if hits != 1 {
		t.Fatalf("one-time refresh token reused: %d requests", hits)
	}
}

func TestKimiUnverifiedCaptureCannotEnterVault(t *testing.T) {
	a, now := kimiApp(t)
	writeKimiLive(t, a, "unverified", now.Add(time.Hour).Unix())
	blobs, _, err := a.Adapters[adapter.Kimi].Capture(a.UserHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(blobs[0]); err == nil {
		t.Fatal("unverified identity was saved")
	}
	accounts, err := a.State.ListAccounts("kimi")
	if err != nil || len(accounts) != 0 {
		t.Fatal("unverified account persisted")
	}
}

func TestKimiIngestProfileFailureBacksOff(t *testing.T) {
	a, now := kimiApp(t)
	writeKimiLive(t, a, "unverified", now.Add(time.Hour).Unix())
	calls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return kimiResponse(req, 503, `{}`), nil
	})}
	if err := a.Ingest(adapter.Kimi); err == nil {
		t.Fatal("expected profile failure")
	}
	a.Clock = clock.Fixed{T: now.Add(2 * time.Second)}
	if err := a.Ingest(adapter.Kimi); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("ingest retried profile every tick")
	}
	a.Clock = clock.Fixed{T: now.Add(time.Minute)}
	_ = a.Ingest(adapter.Kimi)
	if calls != 2 {
		t.Fatal("profile never retried after backoff")
	}
}
