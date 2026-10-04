package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

func candidateFixture(t *testing.T) (*App, time.Time) {
	t.Helper()
	a, _ := setup(t)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@example.test")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	seedCandidateObservation(t, a, "acc-b", "exhausted", 100, now, now.Add(time.Hour).Unix())
	return a, now
}

func seedCandidateObservation(t *testing.T, a *App, id, class string, pct float64, at time.Time, reset int64) {
	t.Helper()
	if err := a.State.UpdateQuota("codex", id, class, pct, reset, at.Unix(), at.Unix(), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota("codex", id, class, "http", pct, at); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateRankRequiresRecentSuccessfulObservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		noLog     bool
		failure   bool
		backoff   bool
		status    int
		wantCalls int
		wantRank  int
	}{
		{name: "old OK revalidated", age: 72 * time.Hour, status: 200, wantCalls: 1, wantRank: 0},
		{name: "old OK unavailable", age: 72 * time.Hour, status: 503, wantCalls: 1, wantRank: 9},
		{name: "no observation revalidated", age: 72 * time.Hour, noLog: true, status: 200, wantCalls: 1, wantRank: 0},
		{name: "fresh OK reused", age: time.Minute, status: 503, wantRank: 0},
		{name: "failed latest observation", age: time.Minute, failure: true, status: 503, wantRank: 9},
		{name: "known state in backoff", age: time.Minute, backoff: true, status: 200, wantRank: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, now := candidateFixture(t)
			at := now.Add(-tc.age)
			if tc.noLog {
				if err := a.State.UpdateQuota("codex", "acc-a", "ok", 20, 0, at.Unix(), at.Unix(), 0); err != nil {
					t.Fatal(err)
				}
			} else {
				seedCandidateObservation(t, a, "acc-a", "ok", 20, at, 0)
			}
			if tc.failure {
				// A failed request updates attempt timestamps but preserves display
				// quota. A later successful refresh may have cleared its backoff.
				if err := a.State.UpdateQuota("codex", "acc-a", "ok", 20, 0, now.Unix(), now.Unix(), 0); err != nil {
					t.Fatal(err)
				}
				if err := a.State.LogQuota("codex", "acc-a", "unknown", "http", 0, now); err != nil {
					t.Fatal(err)
				}
			}
			if tc.backoff {
				if err := a.State.UpdateQuota("codex", "acc-a", "ok", 20, 0, at.Unix(), at.Unix(), now.Add(time.Hour).Unix()); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":17}}}`)), Request: req}, nil
			})}
			ac, err := a.State.GetAccount("codex", "acc-a")
			if err != nil {
				t.Fatal(err)
			}
			rank, pct := a.candidateRank(context.Background(), adapter.Codex, ac)
			if rank != tc.wantRank || calls != tc.wantCalls || (calls == 1 && tc.status == 200 && pct != 17) {
				t.Fatalf("rank=%d pct=%v calls=%d; want rank=%d calls=%d", rank, pct, calls, tc.wantRank, tc.wantCalls)
			}
		})
	}
}

func TestUnknownCandidateHonorsBackoffAcrossAutomaticSelection(t *testing.T) {
	a, now := candidateFixture(t)
	seedCandidateObservation(t, a, "acc-a", "unknown", 0, now.Add(-3*time.Hour), 0)
	calls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`)), Request: req}, nil
	})}
	for _, elapsed := range []time.Duration{0, time.Minute} {
		a.Clock = clock.Fixed{T: now.Add(elapsed)}
		if next := a.pickNext(adapter.Codex, "acc-b"); next != "" {
			t.Fatal("failed candidate was selected")
		}
	}
	if calls != 1 {
		t.Fatalf("unknown candidate bypassed backoff: calls=%d", calls)
	}
}

func TestCandidateRankReloadsStateAfterListing(t *testing.T) {
	a, now := candidateFixture(t)
	seedCandidateObservation(t, a, "acc-a", "ok", 20, now, 0)
	listed, err := a.State.GetAccount("codex", "acc-a")
	if err != nil {
		t.Fatal(err)
	}
	seedCandidateObservation(t, a, "acc-a", "expired", 0, now, 0)
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("expired candidate must not be refreshed during automatic selection")
		return nil, nil
	})}
	if rank, _ := a.candidateRank(context.Background(), adapter.Codex, listed); rank < 2 {
		t.Fatal("account list snapshot overrode newer expired state")
	}
}

func TestPendingRechecksStaleQuotaBeforeRestoring(t *testing.T) {
	for _, tc := range []struct {
		name         string
		targetOld    bool
		targetFailed bool
		sourceOld    bool
		sourceReset  bool
		status       int
		used         int
		wantCalls    int
		wantSwitch   bool
	}{
		{name: "stale target now exhausted", targetOld: true, status: 200, used: 100, wantCalls: 1},
		{name: "stale target unavailable", targetOld: true, status: 503, wantCalls: 1},
		{name: "stale target still available", targetOld: true, status: 200, used: 17, wantCalls: 1, wantSwitch: true},
		{name: "target failed after queue", targetFailed: true, status: 200},
		{name: "source recovery due", sourceOld: true, status: 200, used: 17, wantCalls: 1},
		{name: "source reset passed", sourceReset: true, status: 200, used: 17, wantCalls: 1},
		{name: "fresh observations reused", status: 503, wantSwitch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, now := candidateFixture(t)
			at := now.Add(-time.Minute)
			if tc.targetOld {
				at = now.Add(-72 * time.Hour)
			}
			seedCandidateObservation(t, a, "acc-a", "ok", 20, at, 0)
			if tc.targetFailed {
				if err := a.State.UpdateQuota("codex", "acc-a", "ok", 20, 0, now.Unix(), now.Unix(), now.Add(2*time.Hour).Unix()); err != nil {
					t.Fatal(err)
				}
				if err := a.State.LogQuota("codex", "acc-a", "unknown", "http", 0, now); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sourceOld || tc.sourceReset {
				reset := now.Add(time.Hour).Unix()
				elapsed := 31 * time.Minute
				if tc.sourceReset {
					reset = now.Add(10 * time.Minute).Unix()
					elapsed = 11 * time.Minute
				}
				if err := a.State.UpdateQuota("codex", "acc-b", "exhausted", 100, reset, now.Unix(), now.Unix(), 0); err != nil {
					t.Fatal(err)
				}
				a.Clock = clock.Fixed{T: now.Add(elapsed)}
				seedCandidateObservation(t, a, "acc-a", "ok", 20, a.now().Add(-time.Minute), 0)
			}
			if err := a.State.SetPending(state.Pending{Tool: "codex", FromID: "acc-b", ToID: "acc-a", Reason: "exhausted", QueuedAt: now.Unix()}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if current, err := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json")); err != nil || string(current) != string(before) {
					t.Fatal("credentials were restored before quota validation")
				}
				body := `{"rate_limit":{"primary_window":{"used_percent":17}}}`
				if tc.used == 100 {
					body = `{"rate_limit":{"primary_window":{"used_percent":100}}}`
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			if err := a.TryApply(adapter.Codex, false); err != nil {
				t.Fatal(err)
			}
			p, err := a.State.GetPointer("codex")
			want := "acc-b"
			if tc.wantSwitch {
				want = "acc-a"
			}
			if err != nil || p.StableID != want || calls != tc.wantCalls {
				t.Fatalf("pointer=%s calls=%d error=%v; want pointer=%s calls=%d", p.StableID, calls, err, want, tc.wantCalls)
			}
			if !tc.wantSwitch {
				if after, err := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json")); err != nil || string(after) != string(before) {
					t.Fatal("rejected automatic switch changed live credentials")
				}
			}
		})
	}
}

func TestProbeRefreshFailureIsLatestObservation(t *testing.T) {
	a, blob, now := parkedRefreshAccount(t, adapter.Codex)
	if err := a.State.LogQuota("codex", blob.Identity.StableID, "ok", "http", 21, now); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`)), Request: req}, nil
	})}
	res := a.ProbeAccount(context.Background(), adapter.Codex, blob.Identity.StableID)
	if res.Class != quota.Unknown {
		t.Fatalf("class=%s", res.Class)
	}
	points, err := a.State.RecentQuota("codex", blob.Identity.StableID, 8)
	if err != nil || len(points) != 2 || points[1].Class != "unknown" {
		t.Fatalf("refresh failure was not recorded: points=%+v error=%v", points, err)
	}
}

func TestPendingQuotaValidationPrecedesDesktopChanges(t *testing.T) {
	for _, reason := range []string{"quota failed", "source reset crossed", "auto disabled during request"} {
		t.Run(reason, func(t *testing.T) {
			a, ad, running, quits, launches := setupDesktopSwitch(t)
			now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			a.Clock = clock.Fixed{T: now}
			seedCandidateObservation(t, a, "acc-b", "exhausted", 100, now, now.Add(time.Second).Unix())
			seedCandidateObservation(t, a, "acc-a", "ok", 20, now.Add(-72*time.Hour), 0)
			if err := a.State.SetPending(state.Pending{Tool: "codex", FromID: "acc-b", ToID: "acc-a", Reason: "exhausted", QueuedAt: now.Unix()}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if !*running || *quits != 0 || *launches != 0 || ad.killed {
					t.Fatal("desktop or CLI changed before automatic quota validation")
				}
				status := 200
				switch reason {
				case "quota failed":
					status = 503
				case "source reset crossed":
					a.Clock = clock.Fixed{T: now.Add(2 * time.Second)}
				case "auto disabled during request":
					a.cfgMu.Lock()
					a.Cfg.General.AutoSwitch = false
					a.cfgMu.Unlock()
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":17}}}`)), Request: req}, nil
			})}
			if err := a.TryApply(adapter.Codex, true); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !*running || *quits != 0 || *launches != 0 || ad.killed {
				t.Fatalf("unsafe preflight outcome: calls=%d running=%v quits=%d launches=%d killed=%v", calls, *running, *quits, *launches, ad.killed)
			}
			if p, err := a.State.GetPointer("codex"); err != nil || p.StableID != "acc-b" {
				t.Fatal("rejected preflight switched accounts")
			}
		})
	}
}
