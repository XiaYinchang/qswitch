package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/codex"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
)

func TestScheduledQuotaRetryProgressionAndRecovery(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	hits, success, serverDelay := 0, false, ""
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hits++
		status, body := 503, `{}`
		if success {
			status, body = 200, `{"rate_limit":{"primary_window":{"used_percent":20}}}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{serverDelay}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	for _, delay := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		before := hits
		a.ProbeHTTP(context.Background(), adapter.Codex, false)
		ac, _ := a.State.GetAccount("codex", "acc-a")
		if hits != before+1 || ac.HTTPBackoffUntil != now.Add(delay).Unix() {
			t.Fatalf("retry did not use %s: requests=%d account=%+v", delay, hits-before, ac)
		}
		a.Clock = clock.Fixed{T: now.Add(delay - time.Second)}
		a.ProbeHTTP(context.Background(), adapter.Codex, false)
		if hits != before+1 {
			t.Fatal("retry bypassed its deadline")
		}
		now = now.Add(delay)
		a.Clock = clock.Fixed{T: now}
	}
	success = true
	a.ProbeHTTP(context.Background(), adapter.Codex, false)
	ac, _ := a.State.GetAccount("codex", "acc-a")
	if ac.HTTPBackoffUntil != 0 || ac.LastQuotaClass != "ok" {
		t.Fatal("success did not clear failure schedule")
	}
	success = false
	a.ProbeAccount(context.Background(), adapter.Codex, "acc-a")
	ac, _ = a.State.GetAccount("codex", "acc-a")
	if ac.HTTPBackoffUntil != now.Add(2*time.Minute).Unix() {
		t.Fatal("new failure inherited old retry progression")
	}
	serverDelay = "1800"
	a.ProbeAccount(context.Background(), adapter.Codex, "acc-a")
	ac, _ = a.State.GetAccount("codex", "acc-a")
	if ac.HTTPBackoffUntil != now.Add(30*time.Minute).Unix() {
		t.Fatal("server retry delay was shortened")
	}
	a.Clock = clock.Fixed{T: now.Add(15 * time.Minute)}
	before := hits
	a.ProbeHTTP(context.Background(), adapter.Codex, false)
	if hits != before {
		t.Fatal("scheduled probe ignored server Retry-After")
	}
}

func TestIdleQuotaBoundedFairAndDoesNotSwitch(t *testing.T) {
	a, _ := setup(t)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	for _, id := range []string{"acc-a", "acc-b", "acc-c"} {
		writeCodex(t, a.UserHome, id, id+"@example.test")
		if _, _, err := a.Capture(adapter.Codex); err != nil {
			t.Fatal(err)
		}
		b, _ := a.loadBlob(adapter.Codex, id)
		p, _ := json.Marshal(map[string]any{"exp": now.Add(24 * time.Hour).Unix()})
		b, err := codex.ApplyRefresh(b, quota.CodexTokens{AccessToken: "e30." + b64u(p) + ".x", RefreshToken: "refresh-" + id}, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.saveBlob(b); err != nil {
			t.Fatal(err)
		}
		age := time.Hour
		if id == "acc-a" {
			age = 2 * time.Hour
		}
		at := now.Add(-age)
		if err := a.State.UpdateQuota("codex", id, "ok", 20, now.Add(time.Hour).Unix(), at.Unix(), at.Unix(), 0); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(a.UserHome, ".codex", "auth.json")
	beforeAuth, _ := os.ReadFile(path)
	beforePointer, _ := a.State.GetPointer("codex")
	hits := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/wham/usage" {
			t.Fatal("healthy parked probe unnecessarily rotated credentials")
		}
		hits++
		status, body := 200, `{"rate_limit":{"primary_window":{"used_percent":25}}}`
		if hits == 1 {
			status, body = 503, `{}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	a.ProbeRecovered(context.Background(), adapter.Codex, false)
	first, _ := a.State.GetAccount("codex", "acc-a")
	if hits != 1 || first.LastHTTPAt != now.Unix() {
		t.Fatal("oldest account was not checked with bounded budget")
	}
	a.ProbeRecovered(context.Background(), adapter.Codex, false)
	second, _ := a.State.GetAccount("codex", "acc-b")
	if hits != 2 || second.LastHTTPAt != now.Unix() {
		t.Fatal("failed account starved another parked account")
	}
	a.ProbeRecovered(context.Background(), adapter.Codex, false)
	if hits != 2 {
		t.Fatal("recent parked account or current account was polled again")
	}
	a.Clock = clock.Fixed{T: now.Add(30 * time.Minute)}
	a.Cfg.Quota.ProbeIdleAccounts = false
	a.ProbeRecovered(context.Background(), adapter.Codex, false)
	if hits != 2 {
		t.Fatal("disabled idle querying was ignored")
	}
	afterAuth, _ := os.ReadFile(path)
	afterPointer, _ := a.State.GetPointer("codex")
	if !bytes.Equal(beforeAuth, afterAuth) || beforePointer != afterPointer {
		t.Fatal("idle quota query changed the live account")
	}
}

func TestQuotaBackoffDoesNotBlockRenewalOrGetClearedByIt(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Grok, adapter.Kimi} {
		t.Run(string(tool), func(t *testing.T) {
			var a *App
			var b adapter.Blob
			var now time.Time
			if tool == adapter.Kimi {
				a, b, now = liveKimiFixture(t)
			} else {
				a, b, now = parkedRefreshAccount(t, tool)
			}
			until := now.Add(45 * time.Minute).Unix()
			ac, _ := a.State.GetAccount(string(tool), b.Identity.StableID)
			if err := a.State.UpdateQuota(string(tool), b.Identity.StableID, "ok", 21, ac.LastResetsAt, now.Unix(), now.Unix(), until); err != nil {
				t.Fatal(err)
			}
			hits := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				hits++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"renewed","refresh_token":"rotated","expires_in":900}`)), Header: make(http.Header), Request: req}, nil
			})}
			a.KeepAlive(context.Background(), tool)
			ac, _ = a.State.GetAccount(string(tool), b.Identity.StableID)
			if hits != 1 || ac.HTTPBackoffUntil != until || ac.RefreshBackoffUntil != 0 {
				t.Fatalf("query and renewal backoff interfered: hits=%d account=%+v", hits, ac)
			}
			if _, fresh, err := a.cachedAutomaticQuota(tool, ac); err != nil || fresh {
				t.Fatal("renewal authorized switching with failed quota evidence")
			}
		})
	}
}

func TestBackoffCurrentQuotaDoesNotProbeSwitchCandidates(t *testing.T) {
	a, now := candidateFixture(t)
	seedCandidateObservation(t, a, "acc-a", "unknown", 0, now.Add(-time.Hour), 0)
	if err := a.State.UpdateQuota("codex", "acc-b", "exhausted", 100, now.Add(time.Hour).Unix(), now.Unix(), now.Unix(), now.Add(2*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("current quota backoff caused another account request")
		return nil, nil
	})}
	a.ProbeHTTP(context.Background(), adapter.Codex, false)
	if p, _ := a.State.GetPending("codex"); p.ToID != "" {
		t.Fatal("backoff quota queued a switch")
	}
}
