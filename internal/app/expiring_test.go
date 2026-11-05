package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
)

// expiringSetup has the live account "source" with its week ending in four
// days, and "target" with left percent of a week that resets after resetsIn.
func expiringSetup(t *testing.T, left float64, resetsIn time.Duration) *App {
	t.Helper()
	a := pendingClaudeSwitch(t)
	if err := a.State.ClearPending("claude"); err != nil {
		t.Fatal(err)
	}
	p, err := a.State.GetPointer("claude")
	if err != nil {
		t.Fatal(err)
	}
	p.LastApplyAt = 0
	if err := a.State.SetPointer(p); err != nil {
		t.Fatal(err)
	}
	now := a.now()
	seedClaudeQuota(t, a, "source", "ok", 40)
	seedBuckets(t, a, "source", quota.Bucket{ID: "5h", UsedPct: 10}, quota.Bucket{ID: "weekly", UsedPct: 40, ResetsAt: now.Add(4 * 24 * time.Hour).Unix()})
	class := "ok"
	if left <= 10 {
		class = "soft"
	}
	seedClaudeQuota(t, a, "target", class, 100-left)
	seedBuckets(t, a, "target", quota.Bucket{ID: "5h"}, quota.Bucket{ID: "weekly", UsedPct: 100 - left, ResetsAt: now.Add(resetsIn).Unix()})
	return a
}

func seedBuckets(t *testing.T, a *App, id string, buckets ...quota.Bucket) {
	t.Helper()
	if err := a.State.SetQuotaDetail("claude", id, quota.EncodeBuckets(buckets)); err != nil {
		t.Fatal(err)
	}
}

func captureLog(a *App) *[]string {
	var lines []string
	a.Log = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	return &lines
}

// On 11-04 at 18:00 c2bf85c4 still had 10% of its week, due to reset at
// 03:00, while the live account's week ran until 11-08. Quota about to be
// forfeited is used first, once every session is idle.
func TestExpiringQuotaIsUsedBeforeItResets(t *testing.T) {
	a := expiringSetup(t, 10, 9*time.Hour)
	lines := captureLog(a)
	s := &fakeRunner{ready: true, exitWorks: true}
	startFakeRunner(t, a, 1, s)
	a.considerExpiring(adapter.Claude)
	if pend, _ := a.State.GetPending("claude"); pend.ToID != "target" || pend.Reason != reasonExpiring {
		t.Fatalf("expiring quota not queued: %+v", pend)
	}
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if p, _ := a.State.GetPointer("claude"); p.StableID != "target" {
		t.Fatalf("live account is still %s", p.StableID)
	}
	s.mu.Lock()
	committed := s.committed
	s.mu.Unlock()
	if !committed {
		t.Fatal("the session was not restarted for the switch")
	}
	got := *lines
	if len(got) < 2 || !strings.Contains(got[0], "target has 10% of its quota left until") || !strings.Contains(got[len(got)-1], "switched from source to target") {
		t.Fatalf("switch logged as %q", got)
	}
}

// Nothing is used up, so the switch holds no turn and no message: it prepares
// no session until every one is idle.
func TestExpiringSwitchWaitsUntilEverySessionIsIdle(t *testing.T) {
	a := expiringSetup(t, 30, 6*time.Hour)
	busy := &fakeRunner{reason: "model turn running", exitWorks: true}
	idle := &fakeRunner{ready: true, exitWorks: true}
	startFakeRunner(t, a, 1, busy)
	startFakeRunner(t, a, 2, idle)
	a.considerExpiring(adapter.Claude)
	for range 3 {
		if err := applyTick(a); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []*fakeRunner{busy, idle} {
		s.mu.Lock()
		prepares := s.prepares
		s.mu.Unlock()
		if prepares != 0 {
			t.Fatal("a session was prepared while another was busy")
		}
	}
	if pend, _ := a.State.GetPending("claude"); pend.Reason != reasonExpiring {
		t.Fatalf("switch dropped while waiting: %+v", pend)
	}
	busy.mu.Lock()
	busy.ready = true
	busy.mu.Unlock()
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if p, _ := a.State.GetPointer("claude"); p.StableID != "target" {
		t.Fatalf("not switched once every session was idle: %s", p.StableID)
	}
}

// A session that gets busy between the idle check and the drain is released
// at once instead of having its input held; the switch tries again later.
func TestExpiringSwitchLetsGoOfASessionThatGotBusy(t *testing.T) {
	a := expiringSetup(t, 30, 6*time.Hour)
	s := &fakeRunner{ready: true, busyOnPrepare: true, exitWorks: true}
	startFakeRunner(t, a, 1, s)
	a.considerExpiring(adapter.Claude)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	prepares, releases, generation, exited := s.prepares, s.releases, s.generation, s.exited
	s.mu.Unlock()
	if prepares != 1 || releases != 1 || generation != "" || exited {
		t.Fatalf("busy session kept in the drain: prepares=%d releases=%d generation=%q exited=%v", prepares, releases, generation, exited)
	}
	if pend, _ := a.State.GetPending("claude"); pend.Reason != reasonExpiring {
		t.Fatalf("switch dropped: %+v", pend)
	}
}

func TestExpiringQuotaConditions(t *testing.T) {
	for _, c := range []struct {
		name     string
		left     float64
		resetsIn time.Duration
		setup    func(*testing.T, *App)
		want     bool
	}{
		{"ten percent left, reset in nine hours", 10, 9 * time.Hour, nil, true},
		{"less than ten percent left", 9, 9 * time.Hour, nil, false},
		{"reset more than twelve hours away", 50, 13 * time.Hour, nil, false},
		{"reset after the live account's", 50, 9 * time.Hour, func(t *testing.T, a *App) {
			seedBuckets(t, a, "source", quota.Bucket{ID: "weekly", UsedPct: 40, ResetsAt: a.now().Add(5 * time.Hour).Unix()})
		}, false},
		{"live account used up", 50, 9 * time.Hour, func(t *testing.T, a *App) {
			seedClaudeQuota(t, a, "source", "exhausted", 100)
		}, false},
		{"right after a switch", 50, 9 * time.Hour, func(t *testing.T, a *App) {
			p, _ := a.State.GetPointer("claude")
			p.LastApplyAt = a.now().Add(-5 * time.Minute).Unix()
			if err := a.State.SetPointer(p); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := expiringSetup(t, c.left, c.resetsIn)
			if c.setup != nil {
				c.setup(t, a)
			}
			a.considerExpiring(adapter.Claude)
			pend, _ := a.State.GetPending("claude")
			if got := pend.Reason == reasonExpiring && pend.ToID == "target"; got != c.want {
				t.Fatalf("queued = %v, want %v (%+v)", got, c.want, pend)
			}
		})
	}
}

// A queued switch is dropped, and says why, when the next account's quota
// stops qualifying while it waits.
func TestQueuedExpiringSwitchIsDroppedWhenItNoLongerQualifies(t *testing.T) {
	a := expiringSetup(t, 30, 6*time.Hour)
	lines := captureLog(a)
	startFakeRunner(t, a, 1, &fakeRunner{reason: "model turn running"})
	a.considerExpiring(adapter.Claude)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	// Used from another device meanwhile.
	seedBuckets(t, a, "target", quota.Bucket{ID: "weekly", UsedPct: 95, ResetsAt: a.now().Add(6 * time.Hour).Unix()})
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if pend, _ := a.State.GetPending("claude"); pend.ToID != "" {
		t.Fatalf("switch kept: %+v", pend)
	}
	got := *lines
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "no longer about to expire unused") {
		t.Fatalf("cancellation logged as %q", got)
	}
}

// Sessions waiting to continue are still asked to while an expiring switch
// waits for every session to be idle.
func TestWaitingSessionContinuesWhileAnExpiringSwitchIsQueued(t *testing.T) {
	a := expiringSetup(t, 30, 6*time.Hour)
	waiting := &fakeRunner{ready: true, needsContinue: true}
	startFakeRunner(t, a, 1, waiting)
	startFakeRunner(t, a, 2, &fakeRunner{reason: "model turn running"})
	a.considerExpiring(adapter.Claude)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if n := waiting.resumeCount(); n != 1 {
		t.Fatalf("waiting session asked to continue %d times", n)
	}
}
