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
	generation                              string
	prepares, releases, resumes             int
	committed                               bool
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
		switch r.URL.Path {
		case "/prepare":
			s.prepares++
			s.generation = c.Generation
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
			"cwd": "/work", "ready": s.ready, "exited": s.exited, "needs_continue": s.needsContinue, "generation": s.generation,
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

// On novix the live account's 5-hour window reset at 09:10 while a switch
// away from it still waited for a session. The probe that saw the recovery
// asked sessions to continue while the switch was still queued, which Resume
// skips; the switch was cancelled a moment later and nothing asked again.
func TestCancelledSwitchContinuesSessionsWhenTheAccountRecovers(t *testing.T) {
	a := pendingClaudeSwitch(t)
	s := &fakeRunner{needsContinue: true}
	startFakeRunner(t, a, 1, s)
	if err := a.TryApply(adapter.Claude, false); err != nil {
		t.Fatal(err)
	}
	if s.prepares == 0 || s.releases != 0 {
		t.Fatalf("setup: prepares=%d releases=%d", s.prepares, s.releases)
	}
	seedClaudeQuota(t, a, "source", "ok", 27)
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	if err := a.TryApply(adapter.Claude, false); err != nil {
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
	// Sessions the cancellation released restart after it returns.
	if !a.resumeDue() {
		t.Fatal("no resume window for sessions that are still restarting")
	}
	a.Clock = clock.Fixed{T: a.now().Add(resumeWindow + time.Second)}
	if a.resumeDue() {
		t.Fatal("resume window did not end")
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
