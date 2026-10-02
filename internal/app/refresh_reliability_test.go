package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/codex"
	"qswitch/internal/adapter/grok"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
)

func parkedRefreshAccount(t *testing.T, tool adapter.Tool) (*App, adapter.Blob, time.Time) {
	t.Helper()
	a, _ := setup(t)
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	id := "acc-a"
	if tool == adapter.Grok {
		writeGrokLive(t, a.UserHome, id, "a@x.com")
	}
	if _, _, err := a.Capture(tool); err != nil {
		t.Fatal(err)
	}
	blob, err := a.loadBlob(tool, id)
	if err != nil {
		t.Fatal(err)
	}
	expiredPayload, _ := json.Marshal(map[string]any{"exp": now.Add(-time.Hour).Unix()})
	expiredAccess := "e30." + b64u(expiredPayload) + ".x"
	if tool == adapter.Codex {
		blob, err = codex.ApplyRefresh(blob, quota.CodexTokens{AccessToken: expiredAccess, RefreshToken: "refresh-old", IDToken: jwtWS("a@x.com", "user-a", id, "plus")}, now.Add(-9*24*time.Hour))
		writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	} else {
		blob, err = grok.ApplyRefresh(blob, quota.GrokTokens{AccessToken: expiredAccess, RefreshToken: "refresh-old", ExpiresIn: 1}, now.Add(-time.Hour))
		writeGrokLive(t, a.UserHome, "acc-b", "b@x.com")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(blob); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Capture(tool); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpdateQuotaSnapshot(string(tool), id, "ok", 21, now.Add(time.Hour).Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	return a, blob, now
}

func refreshForTest(a *App, tool adapter.Tool, blob adapter.Blob, force bool) (string, error) {
	if tool == adapter.Codex {
		_, token, _, err := a.keepAliveCodex(context.Background(), blob, force)
		return token, err
	}
	_, token, err := a.keepAliveGrok(context.Background(), blob, force)
	return token, err
}

func TestRefreshSerializesAndReloadsCredentials(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		t.Run(string(tool), func(t *testing.T) {
			a, staleBlob, _ := parkedRefreshAccount(t, tool)
			var requests atomic.Int32
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"access-new","refresh_token":"refresh-new","expires_in":21600}`)), Header: make(http.Header), Request: req}, nil
			})}
			start := make(chan struct{})
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for range 2 {
				wg.Go(func() {
					<-start
					token, err := refreshForTest(a, tool, staleBlob, true)
					if err == nil && token != "access-new" {
						err = fmt.Errorf("caller did not receive refreshed access token")
					}
					errs <- err
				})
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests.Load() != 1 {
				t.Fatalf("concurrent callers reused one-time refresh token: %d requests", requests.Load())
			}
			if _, err := refreshForTest(a, tool, staleBlob, true); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatal("stale caller must reload the vault even after the first refresh completes")
			}
		})
	}
}

func TestKeepAliveRefreshFailuresBackOff(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		for _, status := range []int{500, 400} {
			t.Run(fmt.Sprintf("%s/%d", tool, status), func(t *testing.T) {
				a, blob, now := parkedRefreshAccount(t, tool)
				hits := 0
				success := false
				a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					hits++
					code, body := status, `{"error":"invalid_grant"}`
					if status == 500 {
						body = `{"error":"temporary_failure"}`
					}
					if success {
						code, body = 200, `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":21600}`
					}
					return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
				})}
				a.KeepAlive(context.Background(), tool)
				a.Clock = clock.Fixed{T: now.Add(2 * time.Minute)}
				a.KeepAlive(context.Background(), tool)
				if hits != 1 {
					t.Fatalf("failed keepalive retried before backoff: %d requests", hits)
				}
				ac, err := a.State.GetAccount(string(tool), blob.Identity.StableID)
				wantClass := "ok"
				if status == 400 {
					wantClass = "expired"
				}
				if err != nil || ac.LastQuotaClass != wantClass || ac.LastUsedPct != 21 || ac.HTTPBackoffUntil != now.Add(a.Cfg.Backoff()).Unix() {
					t.Fatalf("wrong refresh failure state: %+v err=%v", ac, err)
				}
				success = true
				a.Clock = clock.Fixed{T: now.Add(a.Cfg.Backoff())}
				a.KeepAlive(context.Background(), tool)
				ac, err = a.State.GetAccount(string(tool), blob.Identity.StableID)
				if status == 400 {
					wantClass = "unknown"
				}
				if hits != 2 || err != nil || ac.HTTPBackoffUntil != 0 || ac.LastQuotaClass != wantClass {
					t.Fatalf("refresh did not recover after backoff: hits=%d account=%+v err=%v", hits, ac, err)
				}
				a.Clock = clock.Fixed{T: now.Add(a.Cfg.Backoff() + 2*time.Minute)}
				a.KeepAlive(context.Background(), tool)
				if hits != 2 {
					t.Fatal("successfully refreshed account must not remain forced-expired")
				}
			})
		}
	}
}

func TestProbeRefreshTemporaryFailurePreservesQuota(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		t.Run(string(tool), func(t *testing.T) {
			a, blob, _ := parkedRefreshAccount(t, tool)
			hits := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				hits++
				return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":"temporary_failure"}`)), Header: make(http.Header), Request: req}, nil
			})}
			res := a.ProbeAccount(context.Background(), tool, blob.Identity.StableID)
			if res.Class != quota.Unknown || hits != 1 {
				t.Fatalf("temporary refresh failure should stop probing: class=%s requests=%d", res.Class, hits)
			}
			ac, err := a.State.GetAccount(string(tool), blob.Identity.StableID)
			if err != nil || ac.LastQuotaClass != "ok" || ac.LastUsedPct != 21 || ac.HTTPBackoffUntil == 0 {
				t.Fatalf("temporary failure destroyed known quota or lost backoff: %+v err=%v", ac, err)
			}
		})
	}
}

func TestManualProbeCanRetryRefreshDuringBackoff(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		t.Run(string(tool), func(t *testing.T) {
			a, blob, _ := parkedRefreshAccount(t, tool)
			refreshes := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status, body := 200, `{"rate_limit":{"primary_window":{"used_percent":12}}}`
				if tool == adapter.Grok {
					body = `{"config":{"creditUsagePercent":12}}`
				}
				if strings.Contains(req.URL.Path, "/oauth") {
					refreshes++
					if refreshes == 1 {
						status, body = 500, `{"error":"temporary_failure"}`
					} else {
						body = `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":21600}`
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}
			a.KeepAlive(context.Background(), tool)
			res := a.ProbeAccount(context.Background(), tool, blob.Identity.StableID)
			if refreshes != 2 || res.Class != quota.OK {
				t.Fatalf("manual probe should bypass failed-refresh backoff: refreshes=%d class=%s", refreshes, res.Class)
			}
		})
	}
}

func TestRefreshDoesNotSpendLiveSessionToken(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		t.Run(string(tool), func(t *testing.T) {
			a, blob, _ := parkedRefreshAccount(t, tool)
			if err := a.Switch(tool, blob.Identity.StableID, adapter.RestoreOpts{}, false); err != nil {
				t.Fatal(err)
			}
			hits := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				hits++
				return nil, fmt.Errorf("live refresh must remain owned by the official client")
			})}
			if tok, err := refreshForTest(a, tool, blob, true); err != nil || tok == "" {
				t.Fatalf("expected live access token reuse: err=%v", err)
			}
			if hits != 0 {
				t.Fatalf("qswitch attempted to refresh current live login: %d requests", hits)
			}
		})
	}
}

func TestRefreshSuccessSurvivesQuotaFailure(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		t.Run(string(tool), func(t *testing.T) {
			a, blob, now := parkedRefreshAccount(t, tool)
			if err := a.State.UpdateQuotaSnapshot(string(tool), blob.Identity.StableID, "expired", 21, 0, now.Unix()); err != nil {
				t.Fatal(err)
			}
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status, body := 500, `{"error":"temporary_failure"}`
				if strings.Contains(req.URL.Path, "/oauth") {
					status, body = 200, `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":21600}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}
			if res := a.ProbeAccount(context.Background(), tool, blob.Identity.StableID); res.Class != quota.Unknown {
				t.Fatalf("quota failure should be unknown: %+v", res)
			}
			ac, err := a.State.GetAccount(string(tool), blob.Identity.StableID)
			if err != nil || ac.LastQuotaClass != "unknown" || ac.HTTPBackoffUntil == 0 {
				t.Fatalf("quota failure must not undo successful authentication recovery: %+v err=%v", ac, err)
			}
		})
	}
}

func TestKeepAliveRejectsMissingTestTransport(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		t.Run(string(tool), func(t *testing.T) {
			a, blob, _ := parkedRefreshAccount(t, tool)
			_, err := refreshForTest(a, tool, blob, true)
			if err == nil || !strings.Contains(err.Error(), "HTTP client is required in tests") {
				t.Fatalf("test refresh without fake transport must fail closed: %v", err)
			}
		})
	}
}

type refreshReadHookKeys struct {
	base   secutil.KeyProvider
	before func()
}

func (k *refreshReadHookKeys) GetOrCreate(bins []string) ([]byte, error) {
	if before := k.before; before != nil {
		k.before = nil
		before()
	}
	return k.base.GetOrCreate(bins)
}

func TestKeepAliveRechecksExpiredStateAfterLoadingNewBlob(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok} {
		t.Run(string(tool), func(t *testing.T) {
			a, blob, now := parkedRefreshAccount(t, tool)
			if err := a.State.UpdateQuotaSnapshot(string(tool), blob.Identity.StableID, "expired", 21, 0, now.Unix()); err != nil {
				t.Fatal(err)
			}
			var fresh adapter.Blob
			var err error
			if tool == adapter.Codex {
				fresh, err = codex.ApplyRefresh(blob, quota.CodexTokens{AccessToken: "access-new", RefreshToken: "refresh-new"}, now)
			} else {
				fresh, err = grok.ApplyRefresh(blob, quota.GrokTokens{AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresIn: 21600}, now)
			}
			if err != nil {
				t.Fatal(err)
			}
			// The account list still says expired, but another process finishes its
			// refresh immediately before this iteration reads the encrypted blob.
			a.Vault.Keys = &refreshReadHookKeys{base: a.Vault.Keys, before: func() {
				if err := a.saveBlob(fresh); err != nil {
					t.Fatal(err)
				}
				if err := a.State.UpdateQuotaSnapshot(string(tool), blob.Identity.StableID, "unknown", 21, 0, now.Unix()); err != nil {
					t.Fatal(err)
				}
			}}
			hits := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				hits++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"unnecessary-refresh","refresh_token":"refresh-next","expires_in":21600}`)), Header: make(http.Header), Request: req}, nil
			})}
			a.KeepAlive(context.Background(), tool)
			if hits != 0 {
				t.Fatalf("stale expired account list forced another refresh of new credentials: %d requests", hits)
			}
		})
	}
}

func TestSuccessfulQuotaResponseClearsObsoleteAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		name, previous, body, want string
		status                     int
	}{
		{"missing quota", "expired", `{"config":{"currentPeriod":{"type":"monthly"},"onDemandCap":{"val":0}}}`, "unknown", 200},
		{"known quota preserved", "ok", `{"config":{"currentPeriod":{"type":"monthly"}}}`, "ok", 200},
		{"server failure", "expired", `{}`, "expired", 503},
		{"network failure", "expired", ``, "expired", 0},
		{"authentication failure", "expired", `{}`, "expired", 401},
		{"invalid response", "expired", `<html>proxy</html>`, "expired", 200},
		{"error envelope", "expired", `{"error":"unauthorized"}`, "expired", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := setup(t)
			writeGrokLive(t, a.UserHome, "grok-live", "live@example.test")
			if _, _, err := a.Capture(adapter.Grok); err != nil {
				t.Fatal(err)
			}
			now := a.now()
			if err := a.State.UpdateQuotaSnapshot("grok", "grok-live", tc.previous, 21, now.Add(time.Hour).Unix(), now.Unix()); err != nil {
				t.Fatal(err)
			}
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "cli-chat-proxy.grok.com" {
					t.Fatalf("unexpected refresh request: %s", req.URL.Host)
				}
				if tc.status == 0 {
					return nil, fmt.Errorf("fixture connection failure")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header), Request: req}, nil
			})}
			a.ProbeAccount(context.Background(), adapter.Grok, "grok-live")
			ac, err := a.State.GetAccount("grok", "grok-live")
			if err != nil || ac.LastQuotaClass != tc.want || ac.LastUsedPct != 21 && tc.status != 401 {
				t.Fatalf("got class=%s used=%v error=%v; want %s", ac.LastQuotaClass, ac.LastUsedPct, err, tc.want)
			}
		})
	}
}
