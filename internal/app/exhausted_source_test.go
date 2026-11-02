package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/clock"
	"qswitch/internal/state"
)

// claudeRotationFixture makes "source" the current Claude account, which a
// usage request found exhausted a minute ago until sourceReset from now, and
// parks "target" with quota. Usage requests are counted and throttled.
func claudeRotationFixture(t *testing.T, sourceReset time.Duration) (*App, *int) {
	t.Helper()
	a := setupClaudeRecovery(t, "", false)
	a.Cfg.General.AutoSwitch = true
	a.Cfg.Claude.Enabled = true
	usage := new(int)
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"source"}}`), nil
		}
		*usage++
		resp := claudeRecoveryResponse(req, http.StatusTooManyRequests, `{"error":"rate limited"}`)
		resp.Header.Set("Retry-After", "300")
		return resp, nil
	})}
	if _, _, err := a.Capture(adapter.Claude); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"claudeAiOauth": claude.Creds{AccessToken: "target-access", RefreshToken: "target-refresh", ExpiresAt: a.now().Add(time.Hour).UnixMilli()}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := claudeBlobFromRaw(raw, claude.OAuthAccount{AccountUUID: "target"}, a.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(target); err != nil {
		t.Fatal(err)
	}
	observed := a.now().Add(-time.Minute)
	for _, o := range []struct {
		id, class string
		pct       float64
		reset     time.Time
	}{
		{"source", "exhausted", 100, a.now().Add(sourceReset)},
		{"target", "ok", 10, a.now().Add(4 * time.Hour)},
	} {
		if err := a.State.UpdateQuota("claude", o.id, o.class, o.pct, o.reset.Unix(), observed.Unix(), observed.Unix(), 0); err != nil {
			t.Fatal(err)
		}
		if err := a.State.LogQuota("claude", o.id, o.class, "http", o.pct, observed); err != nil {
			t.Fatal(err)
		}
	}
	*usage = 0
	return a, usage
}

// throttle records a usage request for the current account that the server
// answered with 429, as a manual refresh or a scheduled check would.
func throttle(t *testing.T, a *App, usage *int) {
	t.Helper()
	if res := a.ProbeAccount(context.Background(), adapter.Claude, "source"); res.Source != "http_throttled" || *usage != 1 {
		t.Fatalf("setup: source=%s requests=%d", res.Source, *usage)
	}
	if ac, err := a.State.GetAccount("claude", "source"); err != nil || ac.HTTPBackoffUntil <= a.now().Unix() || ac.LastQuotaClass != "exhausted" {
		t.Fatalf("setup: throttled account=%+v err=%v", ac, err)
	}
}

// On novix five managed sessions hit the usage limit within 27 s. Each hit
// re-checked the account the first hit had already found exhausted, and the
// fourth request within a minute was throttled.
func TestLimitHitsDoNotRecheckKnownExhaustedAccount(t *testing.T) {
	a, usage := claudeRotationFixture(t, 4*time.Hour)
	a.ProbeLimitHit(context.Background(), adapter.Claude)
	if *usage != 0 {
		t.Fatalf("limit hit re-checked an account known to be exhausted: requests=%d", *usage)
	}
	if p, _ := a.State.GetPointer("claude"); p.StableID != "target" {
		t.Fatalf("limit hit did not switch: pointer=%s", p.StableID)
	}
}

// The throttled request then cancelled the queued switch, and nothing queued
// it again until the throttle expired seven minutes later. A failed request is
// no evidence of recovery, so the exhaustion stands until its reset.
func TestThrottledRecheckKeepsSwitchFromExhaustedAccount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reset      time.Duration
		elapsed    time.Duration
		wantSwitch bool
	}{
		{name: "before reset", reset: 4 * time.Hour, elapsed: 5 * time.Second, wantSwitch: true},
		// Past the reset only a new request can confirm the account; the
		// throttle's backoff still defers it.
		{name: "reset passed during backoff", reset: time.Minute, elapsed: 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, usage := claudeRotationFixture(t, tc.reset)
			now := a.now()
			if err := a.State.SetPending(state.Pending{Tool: "claude", FromID: "source", ToID: "target", Reason: "exhausted", QueuedAt: now.Unix()}); err != nil {
				t.Fatal(err)
			}
			throttle(t, a, usage)
			a.Clock = clock.Fixed{T: now.Add(tc.elapsed)}
			if err := a.TryApply(adapter.Claude, false); err != nil {
				t.Fatal(err)
			}
			want := "source"
			if tc.wantSwitch {
				want = "target"
			}
			if p, _ := a.State.GetPointer("claude"); p.StableID != want || *usage != 1 {
				t.Fatalf("pointer=%s requests=%d; want pointer=%s requests=1", p.StableID, *usage, want)
			}
		})
	}
}

// A switch dropped for another reason, such as a target that failed its own
// check, is queued again by the next scheduled check without waiting for the
// current account's backoff.
func TestBackoffStillQueuesSwitchFromKnownExhaustedAccount(t *testing.T) {
	a, usage := claudeRotationFixture(t, 4*time.Hour)
	throttle(t, a, usage)
	a.ProbeHTTP(context.Background(), adapter.Claude, false)
	if p, _ := a.State.GetPointer("claude"); p.StableID != "target" || *usage != 1 {
		t.Fatalf("pointer=%s requests=%d; want the switch without another request", p.StableID, *usage)
	}
}
