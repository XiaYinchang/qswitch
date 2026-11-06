package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/clock"
	"qswitch/internal/managed"
	"qswitch/internal/state"
)

// fakeRunner stands in for one `qswitch run claude` runner and records the
// control requests the daemon sends it.
type fakeRunner struct {
	mu                                      sync.Mutex
	ready, exited, needsContinue, exitWorks bool
	generation, reason                      string
	// busyOnPrepare reports the session busy to /prepare only, as one that
	// got busy right after a status check.
	busyOnPrepare               bool
	prepares, releases, resumes int
	committed                   bool
}

func startFakeRunner(t *testing.T, a *App, n int, s *fakeRunner) {
	t.Helper()
	root := filepath.Join(a.DataDir, "managed-claude")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%064x", n)
	token := strings.Repeat("b", 64)
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := os.OpenFile(filepath.Join(dir, "lease"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var c struct {
			Generation string `json:"generation"`
			Committed  bool   `json:"committed"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&c)
		}
		ready := s.ready
		switch r.URL.Path {
		case "/prepare":
			s.prepares++
			s.generation = c.Generation
			ready = ready && !s.busyOnPrepare
		case "/exit":
			if s.exitWorks {
				s.exited = true
			}
		case "/release":
			s.releases++
			s.committed = c.Committed
			if !s.exited {
				s.generation = ""
			}
		case "/resume":
			s.resumes++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "runner_pid": os.Getpid(), "child_pid": 7000 + n, "session_id": fmt.Sprintf("session-%d", n),
			"cwd": "/work", "ready": ready, "exited": s.exited, "needs_continue": s.needsContinue, "generation": s.generation, "reason": s.reason,
		})
	}))
	t.Cleanup(server.Close)
	reg, _ := json.Marshal(map[string]any{"id": id, "pid": os.Getpid(), "url": server.URL, "token": token})
	if err := os.WriteFile(filepath.Join(dir, "registration.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
}

// pendingClaudeSwitch queues a switch from the exhausted live account
// "source" to "target", which has quota.
func pendingClaudeSwitch(t *testing.T) *App {
	t.Helper()
	a := setupClaudeRecovery(t, "", false)
	a.Cfg.General.AutoSwitch = true
	a.Cfg.Claude.Enabled = true
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/profile" {
			t.Errorf("unexpected HTTP path %s", req.URL.Path)
		}
		id := "source"
		if req.Header.Get("Authorization") == "Bearer target-access" {
			id = "target"
		}
		return claudeRecoveryResponse(req, 200, fmt.Sprintf(`{"account":{"uuid":%q}}`, id)), nil
	})}
	if _, _, err := a.Capture(adapter.Claude); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"claudeAiOauth": claude.Creds{AccessToken: "target-access", RefreshToken: "target-refresh", ExpiresAt: a.now().Add(time.Hour).UnixMilli()}})
	target, err := claudeBlobFromRaw(raw, claude.OAuthAccount{AccountUUID: "target"}, a.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(target); err != nil {
		t.Fatal(err)
	}
	seedClaudeQuota(t, a, "source", "exhausted", 100)
	seedClaudeQuota(t, a, "target", "ok", 10)
	if err := a.State.SetPending(state.Pending{Tool: "claude", FromID: "source", ToID: "target", QueuedAt: a.now().Unix(), Reason: "exhausted"}); err != nil {
		t.Fatal(err)
	}
	a.Adapters[adapter.Claude] = claude.Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }, DataDir: a.DataDir}
	return a
}

func seedClaudeQuota(t *testing.T, a *App, id, class string, pct float64) {
	t.Helper()
	if err := a.State.UpdateQuotaSnapshot("claude", id, class, pct, a.now().Add(time.Hour).Unix(), a.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota("claude", id, class, "http", pct, a.now()); err != nil {
		t.Fatal(err)
	}
}

// applyTick runs what the daemon's apply ticker runs for Claude.
func applyTick(a *App) error {
	err := a.TryApply(adapter.Claude, false)
	a.continueInterrupted(adapter.Claude)
	return err
}

func (s *fakeRunner) resumeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumes
}

// On novix the live account's 5-hour window reset at 09:10 while a switch
// away from it still waited for a session. The probe that saw the recovery
// asked sessions to continue while the switch was still queued, which Resume
// skips; the switch was cancelled a moment later and nothing asked again.
func TestCancelledSwitchContinuesSessionsWhenTheAccountRecovers(t *testing.T) {
	a := pendingClaudeSwitch(t)
	s := &fakeRunner{needsContinue: true}
	startFakeRunner(t, a, 1, s)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	prepares, releases, resumes := s.prepares, s.releases, s.resumes
	s.ready = true
	s.mu.Unlock()
	if prepares == 0 || releases != 0 || resumes != 0 {
		t.Fatalf("setup: prepares=%d releases=%d resumes=%d", prepares, releases, resumes)
	}
	seedClaudeQuota(t, a, "source", "ok", 27)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if p, _ := a.State.GetPending("claude"); p.ToID != "" {
		t.Fatal("switch from a recovered account was not cancelled")
	}
	s.mu.Lock()
	releases, committed, resumes := s.releases, s.committed, s.resumes
	s.mu.Unlock()
	if releases != 1 || committed {
		t.Fatalf("cancellation released %d times, committed=%v", releases, committed)
	}
	if resumes != 1 {
		t.Fatalf("interrupted session was not asked to continue: resumes=%d", resumes)
	}
	// A session the cancellation released may restart long after it returns.
	a.Clock = clock.Fixed{T: a.now().Add(time.Hour)}
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if n := s.resumeCount(); n != 2 {
		t.Fatalf("a session still waiting an hour later was not asked again: resumes=%d", n)
	}
}

// On 11-03 a runner upgrade restarted a session at 09:56 while it still
// waited to continue. Its account had recovered at 09:10, before the restart,
// so no recovery came after it, and the session sat idle until the 12:16
// switch. Waiting sessions are asked whenever the live account has quota and
// no switch is queued, not only when the recovery is seen.
func TestWaitingSessionContinuesWhileTheAccountHasQuota(t *testing.T) {
	a := pendingClaudeSwitch(t)
	if err := a.State.ClearPending("claude"); err != nil {
		t.Fatal(err)
	}
	s := &fakeRunner{ready: true, needsContinue: true}
	startFakeRunner(t, a, 1, s)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if n := s.resumeCount(); n != 0 {
		t.Fatalf("asked to continue on an exhausted account: resumes=%d", n)
	}
	seedClaudeQuota(t, a, "source", "soft", 92)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if n := s.resumeCount(); n != 1 {
		t.Fatalf("not asked to continue on an account with quota: resumes=%d", n)
	}
	// A queued switch restarts the session with its continuation instead.
	if err := a.State.SetPending(state.Pending{Tool: "claude", FromID: "source", ToID: "target", QueuedAt: a.now().Unix(), Reason: "exhausted"}); err != nil {
		t.Fatal(err)
	}
	a.continueInterrupted(adapter.Claude)
	if n := s.resumeCount(); n != 1 {
		t.Fatalf("asked to continue while a switch was queued: resumes=%d", n)
	}
}

// A switch logs each change in its progress once: whom it waits for and why,
// and how it ends. On 11-04 nothing was logged, and finding out why a switch
// waited from 05:17 to 05:50 took reading every session's transcript.
func TestSwitchProgressIsLoggedOncePerChange(t *testing.T) {
	a := pendingClaudeSwitch(t)
	var lines []string
	a.Log = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	s := &fakeRunner{reason: "model turn running"}
	startFakeRunner(t, a, 1, s)
	for range 3 {
		if err := applyTick(a); err != nil {
			t.Fatal(err)
		}
	}
	// A busy session's reason changes all the time; only who is waited for counts.
	s.mu.Lock()
	s.reason = "tool call running"
	s.mu.Unlock()
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "waits for session- (model turn running)") {
		t.Fatalf("waiting logged as %q", lines)
	}
	seedClaudeQuota(t, a, "source", "ok", 27)
	if err := applyTick(a); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.Contains(lines[1], "cancelled") || !strings.Contains(lines[1], "the used-up account has quota again") {
		t.Fatalf("cancellation logged as %q", lines)
	}
}

// A session whose acknowledged exit the runner has to end is logged as
// overdue, as a change of its own: before, the log showed "exiting" until the
// switch was done and the transcript was the only record of the forced end.
func TestOverdueExitIsLogged(t *testing.T) {
	a := pendingClaudeSwitch(t)
	var lines []string
	a.Log = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	s := &fakeRunner{reason: managed.ReasonExiting}
	startFakeRunner(t, a, 1, s)
	for range 2 {
		if err := applyTick(a); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	s.reason = managed.ReasonExitOverdue
	s.mu.Unlock()
	for range 2 {
		if err := applyTick(a); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "session- (exiting)") || !strings.Contains(lines[1], "session- (exit overdue, ending it)") {
		t.Fatalf("overdue exit logged as %q", lines)
	}
}

// unansweredRunners reports the error the session listing gives when one
// runner does not answer in time, as when its terminal check is slow.
type unansweredRunners struct{ claude.Adapter }

func (unansweredRunners) AutoBlockers(string) (adapter.Holders, error) {
	return adapter.Holders{}, fmt.Errorf("runner 0002 unavailable: %w: context deadline exceeded", managed.ErrRunnerUnavailable)
}

// A runner that misses one control request must not cancel a switch that
// sessions already exited for: the cancellation restarts them on the
// exhausted account without their continuation.
func TestUnansweredRunnerDoesNotCancelTheSwitch(t *testing.T) {
	a := pendingClaudeSwitch(t)
	gone := &fakeRunner{ready: true, exitWorks: true, needsContinue: true}
	slow := &fakeRunner{ready: true}
	startFakeRunner(t, a, 1, gone)
	startFakeRunner(t, a, 2, slow)
	if err := a.TryApply(adapter.Claude, false); err != nil {
		t.Fatal(err)
	}
	if !gone.exited || slow.exited {
		t.Fatalf("setup: exited=%v/%v", gone.exited, slow.exited)
	}
	a.Adapters[adapter.Claude] = unansweredRunners{a.Adapters[adapter.Claude].(claude.Adapter)}
	if err := a.TryApply(adapter.Claude, false); !errors.Is(err, managed.ErrRunnerUnavailable) {
		t.Fatalf("TryApply error = %v, want an unavailable runner", err)
	}
	gone.mu.Lock()
	releases := gone.releases
	gone.mu.Unlock()
	if releases != 0 {
		t.Fatal("an unanswered request cancelled the switch and released an exited session")
	}
	if p, _ := a.State.GetPending("claude"); p.ToID != "target" {
		t.Fatalf("pending switch lost: %+v", p)
	}
}
