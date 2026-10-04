package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/kimi"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

func liveKimiFixture(t *testing.T) (*App, adapter.Blob, time.Time) {
	t.Helper()
	a, b, now := parkedKimiAccount(t)
	writeKimiLive(t, a, "old", now.Add(-time.Minute).Unix())
	if err := a.State.SetPointer(state.Pointer{Tool: "kimi", StableID: b.Identity.StableID}); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpdateQuotaSnapshot("kimi", b.Identity.StableID, "ok", 21, now.Add(time.Hour).Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	return a, b, now
}

func TestLiveKimiRenewalProtectsOfficialCLIAndLock(t *testing.T) {
	for _, kind := range []string{"absent CLI", "running CLI", "unknown processes", "peer lock", "credentials changed before lock", "credentials changed before exchange", "credentials changed during exchange", "lock replaced during exchange"} {
		t.Run(kind, func(t *testing.T) {
			a, b, now := liveKimiFixture(t)
			path := a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0]
			lockpath := filepath.Join(a.UserHome, ".kimi-code/oauth/kimi-code.lock")
			old, _ := os.ReadFile(path)
			if kind == "peer lock" {
				if err := os.MkdirAll(lockpath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			processReads := 0
			a.Adapters[adapter.Kimi] = kimi.Adapter{List: func() ([]adapter.Proc, error) {
				processReads++
				switch kind {
				case "running CLI":
					return []adapter.Proc{{PID: 42, Command: "/bin/kimi acp"}}, nil
				case "unknown processes":
					return nil, errors.New("ps unavailable")
				case "credentials changed before lock":
					writeKimiLive(t, a, "new-login", now.Add(time.Hour).Unix())
				case "credentials changed before exchange":
					if processReads == 2 {
						writeKimiLive(t, a, "new-login", now.Add(time.Hour).Unix())
					}
				}
				return nil, nil
			}}
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Path != "/api/oauth/token" {
					t.Fatal("unexpected request before renewing known credentials")
				}
				_ = req.ParseForm()
				if req.Form.Get("refresh_token") != "refresh-old" || req.Form.Get("client_id") != quota.KimiOAuthClientID {
					t.Fatal("refresh used wrong credentials")
				}
				if st, err := os.Stat(lockpath); err != nil || !st.IsDir() {
					t.Fatal("exchange did not hold official directory lock")
				}
				if kind == "credentials changed during exchange" {
					writeKimiLive(t, a, "new-login", now.Add(time.Hour).Unix())
				}
				if kind == "lock replaced during exchange" {
					_ = os.Remove(lockpath)
					_ = os.Mkdir(lockpath, 0700)
				}
				return kimiResponse(req, 200, `{"access_token":"new","refresh_token":"new-refresh","expires_in":900}`), nil
			})}
			_, _, err := a.keepAliveKimiLocked(context.Background(), b, false, false)
			want := 0
			if kind == "absent CLI" || strings.Contains(kind, "during exchange") {
				want = 1
			}
			if calls != want {
				t.Fatalf("refresh requests=%d want=%d err=%v", calls, want, err)
			}
			if strings.Contains(kind, "changed") || strings.Contains(kind, "replaced") {
				if err == nil {
					t.Fatal("race must refuse writeback")
				}
			}
			raw, _ := os.ReadFile(path)
			saved, _ := a.loadBlob(adapter.Kimi, b.Identity.StableID)
			stored, _ := kimi.CredsFromBlob(saved)
			if calls == 1 && (stored.AccessToken != "new" || stored.RefreshToken != "new-refresh" || saved.Identity != b.Identity) {
				t.Fatal("rotated token or account identity not retained in vault")
			}
			if kind == "absent CLI" {
				if err != nil {
					t.Fatal(err)
				}
				live, _ := kimi.ParseCreds(raw)
				if live != stored {
					t.Fatal("vault and live credentials differ")
				}
				st, _ := os.Stat(path)
				if st.Mode().Perm() != 0600 {
					t.Fatal("credential permissions")
				}
			} else if !strings.Contains(kind, "credentials changed") && !bytes.Equal(raw, old) {
				t.Fatal("protected live credentials were overwritten")
			} else if strings.Contains(kind, "credentials changed") {
				live, _ := kimi.ParseCreds(raw)
				if live.AccessToken != "new-login" {
					t.Fatal("concurrent login was overwritten")
				}
			}
			_, lockErr := os.Stat(lockpath)
			peer := kind == "peer lock" || kind == "lock replaced during exchange"
			if peer && lockErr != nil || !peer && !os.IsNotExist(lockErr) {
				t.Fatal("lock cleanup removed a peer lock or leaked its own lock")
			}
			pointer, _ := a.State.GetPointer("kimi")
			if pointer.StableID != b.Identity.StableID {
				t.Fatal("renewal switched account")
			}
		})
	}
}

func TestLiveKimiRepeatedAndConcurrentKeepAlive(t *testing.T) {
	a, b, now := liveKimiFixture(t)
	var refreshes atomic.Int32
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/coding/v1/usages" {
			return kimiResponse(req, 200, `{"usages":{"limit_5h":{"used_ratio":0.02},"limit_7d":{"used_ratio":0}}}`), nil
		}
		if req.URL.Path != "/api/oauth/token" {
			t.Fatal("unexpected upstream")
		}
		n := refreshes.Add(1)
		live, _ := kimi.ReadLiveCreds(a.UserHome)
		_ = req.ParseForm()
		if req.Form.Get("refresh_token") != live.RefreshToken {
			t.Fatal("spent obsolete refresh token")
		}
		return kimiResponse(req, 200, fmt.Sprintf(`{"access_token":"new-%d","refresh_token":"refresh-%d","expires_in":900}`, n, n)), nil
	})}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { a.KeepAlive(context.Background(), adapter.Kimi) })
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatal("concurrent callers spent one token twice")
	}
	// Exercise many short-lived token generations at the real two-minute tick.
	for tick := 1; tick <= 30; tick++ {
		a.Clock = clock.Fixed{T: now.Add(time.Duration(tick) * 2 * time.Minute)}
		a.KeepAlive(context.Background(), adapter.Kimi)
		live, _ := kimi.ReadLiveCreds(a.UserHome)
		if live.ExpiresAt <= a.now().Unix() {
			t.Fatal("proactive keepalive allowed token to expire")
		}
	}
	if refreshes.Load() < 5 {
		t.Fatal("future token generations were not exercised")
	}
	result := a.ProbeAccount(context.Background(), adapter.Kimi, b.Identity.StableID)
	if result.Class != quota.OK || result.UsedPct != 2 || len(result.Buckets) != 2 {
		t.Fatalf("renewed quota did not recover: %+v", result)
	}
}

func TestLiveKimiForcedRecoveryAndRotatedIdentity(t *testing.T) {
	for _, kind := range []string{"HTTP 401", "verified rotated live", "unverified rotated live", "different account"} {
		t.Run(kind, func(t *testing.T) {
			a, b, now := liveKimiFixture(t)
			if kind == "HTTP 401" {
				writeKimiLive(t, a, "old", now.Add(time.Hour).Unix())
			} else {
				writeKimiLive(t, a, "rotated", now.Add(-time.Minute).Unix())
			}
			refreshes := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/coding/v1/me":
					if kind == "unverified rotated live" {
						return kimiResponse(req, 401, `{}`), nil
					}
					id := b.Identity.StableID
					if kind == "different account" {
						id = "other-account"
					}
					return kimiResponse(req, 200, fmt.Sprintf(`{"user_id":%q}`, id)), nil
				case "/coding/v1/usages":
					if req.Header.Get("Authorization") != "Bearer renewed" {
						return kimiResponse(req, 401, `{}`), nil
					}
					return kimiResponse(req, 200, `{"usages":{"limit_5h":{"used_ratio":0.02}}}`), nil
				case "/api/oauth/token":
					refreshes++
					_ = req.ParseForm()
					want := "refresh-old"
					if kind == "verified rotated live" {
						want = "refresh-rotated"
					}
					if req.Form.Get("refresh_token") != want {
						t.Fatal("wrong identity refresh token")
					}
					return kimiResponse(req, 200, `{"access_token":"renewed","refresh_token":"renewed-refresh","expires_in":900}`), nil
				}
				t.Fatal("unexpected endpoint")
				return nil, nil
			})}
			result := a.ProbeAccount(context.Background(), adapter.Kimi, b.Identity.StableID)
			if kind == "unverified rotated live" {
				if refreshes != 0 || result.Class == quota.OK {
					t.Fatal("unverified credentials were refreshed")
				}
			} else if refreshes != 1 || result.Class != quota.OK {
				t.Fatalf("recovery requests=%d result=%+v", refreshes, result)
			}
			live, _ := kimi.ReadLiveCreds(a.UserHome)
			if kind == "different account" && live.AccessToken != "rotated" {
				t.Fatal("parked renewal changed another live account")
			}
		})
	}
}

func TestLiveKimiRefreshFailureKeepsCredentialsAndBacksOff(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprint(permanent), func(t *testing.T) {
			a, b, now := liveKimiFixture(t)
			path := a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0]
			old, _ := os.ReadFile(path)
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if permanent {
					return kimiResponse(req, 400, `{"error":"invalid_grant"}`), nil
				}
				return kimiResponse(req, 503, `{}`), nil
			})}
			a.KeepAlive(context.Background(), adapter.Kimi)
			a.KeepAlive(context.Background(), adapter.Kimi)
			ac, _ := a.State.GetAccount("kimi", b.Identity.StableID)
			want := "ok"
			if permanent {
				want = "expired"
			}
			if calls != 1 || ac.LastQuotaClass != want || ac.LastUsedPct != 21 || ac.RefreshBackoffUntil != now.Add(a.refreshRetryDelay(permanent)).Unix() {
				t.Fatal("refresh failure lost history or ignored backoff")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(old, after) {
				t.Fatal("failed refresh changed credentials")
			}
		})
	}
}
