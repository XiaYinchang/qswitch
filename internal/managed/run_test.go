package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testRunner(t *testing.T) *runner {
	t.Helper()
	r := &runner{dir: t.TempDir(), wake: make(chan struct{}, 1), gate: func() error { return nil }}
	r.reg = registration{ID: strings.Repeat("a", 64), PID: os.Getpid(), Token: strings.Repeat("b", 64)}
	r.status = StatusInfo{ID: r.reg.ID, RunnerPID: r.reg.PID, ChildPID: 123, SessionID: "session-one", CWD: t.TempDir(), Ready: true, Live: true}
	r.lastState = time.Now()
	return r
}

func callRunner(t *testing.T, r *runner, route string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	method := http.MethodPost
	if route == "/status" {
		method = http.MethodGet
	}
	req := httptest.NewRequest(method, route, &payload)
	req.Header.Set("Authorization", "Bearer "+r.reg.Token)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)
	return response
}

func stateFor(r *runner) sessionState {
	return sessionState{SessionID: r.status.SessionID, CWD: r.status.CWD, Ready: true, NeedsContinue: r.status.NeedsContinue, Generation: r.status.Generation}
}

func TestEachRateLimitHitSignalsDaemonOnce(t *testing.T) {
	r := testRunner(t)
	r.options.DataDir = t.TempDir()
	// A session restarted while interrupted already carries needs_continue.
	state := stateFor(r)
	state.NeedsContinue = true
	callRunner(t, r, "/state", state)
	if TakeLimitHit(r.options.DataDir) {
		t.Fatal("inherited interruption signalled a new rate limit")
	}
	// It hits the limit again; needs_continue does not change, the count does.
	state.LimitHits = 1
	callRunner(t, r, "/state", state)
	if !TakeLimitHit(r.options.DataDir) {
		t.Fatal("rate limit hit by an already interrupted session was not signalled")
	}
	callRunner(t, r, "/state", state)
	if TakeLimitHit(r.options.DataDir) {
		t.Fatal("repeated heartbeat re-signalled the same rate limit")
	}
	state.LimitHits = 2
	callRunner(t, r, "/state", state)
	if !TakeLimitHit(r.options.DataDir) {
		t.Fatal("second rate limit was not signalled")
	}
}

func TestResumeIsDeliveredOnceToAWaitingSession(t *testing.T) {
	r := testRunner(t)
	if got := callRunner(t, r, "/resume", struct{}{}); got.Code != http.StatusConflict {
		t.Fatalf("resume accepted for a session that is not interrupted: %d", got.Code)
	}
	r.status.NeedsContinue = true
	if got := callRunner(t, r, "/resume", struct{}{}); got.Code != http.StatusOK {
		t.Fatalf("resume refused for an interrupted ready session: %s", got.Body.String())
	}
	actions := []string{}
	for range 2 {
		var a action
		if err := json.Unmarshal(callRunner(t, r, "/state", stateFor(r)).Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a.Action)
	}
	if actions[0] != "resume" || actions[1] != "run" {
		t.Fatalf("resume deliveries = %v, want one resume then run", actions)
	}
	r.status.Generation = "g"
	if got := callRunner(t, r, "/resume", struct{}{}); got.Code != http.StatusConflict {
		t.Fatal("resume accepted during an account switch")
	}
}

// replaceSelf points the runner at a binary file and then replaces that path
// with a new file, as an install script's rename does.
func replaceSelf(t *testing.T, r *runner) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qswitch")
	if err := os.WriteFile(path, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	id, err := identify(path)
	if err != nil {
		t.Fatal(err)
	}
	r.self, r.selfID = path, id
	next := path + ".new"
	if err := os.WriteFile(next, []byte("new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

func heartbeat(t *testing.T, r *runner, state sessionState) action {
	t.Helper()
	var a action
	if err := json.Unmarshal(callRunner(t, r, "/state", state).Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestReplacedBinaryRequestsUpgradeExitOnlyWhenIdle(t *testing.T) {
	r := testRunner(t)
	state := stateFor(r)
	if a := heartbeat(t, r, state); a.Action != "run" {
		t.Fatalf("unchanged binary requested %q", a.Action)
	}
	replaceSelf(t, r)
	busy := state
	busy.Ready = false
	busy.Reason = "model turn running"
	if a := heartbeat(t, r, busy); a.Action != "run" {
		t.Fatalf("busy session asked to exit for upgrade: %q", a.Action)
	}
	// freshLocked needs a ready heartbeat before an idle one can qualify.
	heartbeat(t, r, state)
	a := heartbeat(t, r, state)
	if a.Action != "exit" || a.Generation != upgradeGeneration {
		t.Fatalf("idle session with replaced binary: %+v", a)
	}
	// The plugin's acknowledgement uses the normal exit route.
	ack := state
	ack.Generation = upgradeGeneration
	if got := callRunner(t, r, "/exiting", ack); got.Code != 200 {
		t.Fatalf("upgrade exit not acknowledged: %s", got.Body.String())
	}
}

func TestUpgradeIsAbandonedWhenTheSessionBecomesBusy(t *testing.T) {
	r := testRunner(t)
	replaceSelf(t, r)
	state := stateFor(r)
	heartbeat(t, r, state)
	if a := heartbeat(t, r, state); a.Action != "exit" {
		t.Fatalf("upgrade not requested: %q", a.Action)
	}
	busy := state
	busy.Ready = false
	busy.Reason = "text draft is present"
	if a := heartbeat(t, r, busy); a.Action != "run" {
		t.Fatalf("busy session kept the upgrade exit: %q", a.Action)
	}
	if r.upgrading || r.status.Generation != "" || r.exitRequested {
		t.Fatalf("upgrade state not cleared: upgrading=%v generation=%q", r.upgrading, r.status.Generation)
	}
}

func TestUpgradeWaitsForDaemonGenerationAndBlocksPrepare(t *testing.T) {
	r := testRunner(t)
	replaceSelf(t, r)
	state := stateFor(r)
	if got := callRunner(t, r, "/prepare", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	heartbeat(t, r, state)
	if a := heartbeat(t, r, state); a.Generation != "g" {
		t.Fatalf("upgrade pre-empted a daemon generation: %+v", a)
	}
	callRunner(t, r, "/release", control{Generation: "g"})
	heartbeat(t, r, state)
	if a := heartbeat(t, r, state); a.Action != "exit" || a.Generation != upgradeGeneration {
		t.Fatalf("upgrade not requested after release: %+v", a)
	}
	var prepared StatusInfo
	if err := json.Unmarshal(callRunner(t, r, "/prepare", control{Generation: "h"}).Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Ready || prepared.Generation != "" {
		t.Fatalf("upgrading runner joined a daemon generation: %+v", prepared)
	}
}

// A session stopped by the usage limit, or restarted with the continuation,
// is not upgraded until its continuation has run. On novix a session hit the
// limit at 05:45:32 and its runner upgraded it two seconds later; the switch
// that followed found a freshly restarted Claude that never ran its /exit,
// and every session waited until the account recovered at 09:10.
func TestUpgradeWaitsWhileSessionNeedsContinuation(t *testing.T) {
	r := testRunner(t)
	r.options.DataDir = t.TempDir()
	r.status.NeedsContinue = true
	replaceSelf(t, r)
	state := stateFor(r)
	for range 3 {
		if a := heartbeat(t, r, state); a.Action != "run" {
			t.Fatalf("upgrade requested while the session waits for its continuation: %+v", a)
		}
	}
	// The continuation hits the limit again: still interrupted, still waiting.
	busy := state
	busy.Ready = false
	busy.Reason = "model turn running"
	heartbeat(t, r, busy)
	state.LimitHits = 1
	for range 2 {
		if a := heartbeat(t, r, state); a.Action != "run" {
			t.Fatalf("upgrade requested after another usage-limit failure: %+v", a)
		}
	}
	TakeLimitHit(r.options.DataDir)
	// Once a continuation is answered, the next idle heartbeat may upgrade.
	state.NeedsContinue = false
	heartbeat(t, r, state)
	if a := heartbeat(t, r, state); a.Action != "exit" || a.Generation != upgradeGeneration {
		t.Fatalf("upgrade not requested after the continuation ended: %+v", a)
	}
}

func TestExitRechecksTmuxAndFreshSession(t *testing.T) {
	r := testRunner(t)
	if got := callRunner(t, r, "/prepare", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if got := callRunner(t, r, "/exit", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	r.gate = func() error { return errors.New("client attached after prepare") }
	if got := callRunner(t, r, "/exiting", stateFor(r)); got.Code != 409 || r.exitAcknowledged {
		t.Fatalf("attached client allowed exit: %d", got.Code)
	}
	r.gate = func() error { return nil }
	state := stateFor(r)
	if got := callRunner(t, r, "/state", state); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	state.Generation = "wrong"
	if got := callRunner(t, r, "/exiting", state); got.Code != 409 {
		t.Fatal("accepted stale generation")
	}
	state.Generation = "g"
	state.SessionID = "different-session"
	if got := callRunner(t, r, "/exiting", state); got.Code != 409 {
		t.Fatal("accepted replaced transcript")
	}
	state = stateFor(r)
	r.lastState = time.Now().Add(-6 * time.Second)
	if got := callRunner(t, r, "/exiting", state); got.Code != 409 {
		t.Fatal("accepted stale readiness")
	}
}

func TestPrepareBlocksWithoutVerifiedTmuxPane(t *testing.T) {
	r := testRunner(t)
	r.gate = func() error { return errors.New("no verified tmux pane") }
	response := callRunner(t, r, "/prepare", control{Generation: "g"})
	var state StatusInfo
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || state.Ready || state.Generation != "" || state.Reason == "" {
		t.Fatalf("unsafe prepare: %+v", state)
	}
	if err := verifiedTMUXPane("", ""); err == nil {
		t.Fatal("non-tmux run accepted")
	}
	if err := verifiedTMUXPane("%spoof", "/tmp/tmux,1,0"); err == nil {
		t.Fatal("spoofed pane accepted")
	}
}

func TestReleaseDoesNotWaitOnClaudeLock(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.status.Exited = true
	r.exitAcknowledged = true
	r.options.DataDir = t.TempDir()
	lock, err := acquireClaudeLock(context.Background(), r.options.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- callRunner(t, r, "/release", control{Generation: "g", Committed: false}) }()
	select {
	case response := <-done:
		if response.Code != 200 || !r.released || r.committed {
			t.Fatalf("abort release failed: %s", response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("release waited for coordinator's Claude lock")
	}
}

func TestCommittedReleaseRequiresWaitConfirmedExit(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	r.exitAcknowledged = true
	if got := callRunner(t, r, "/release", control{Generation: "g", Committed: true}); got.Code != 409 || r.released {
		t.Fatal("committed before child Wait completed")
	}
}

func TestExitAcknowledgementCanBeRevoked(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	if got := callRunner(t, r, "/exiting", stateFor(r)); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	state := stateFor(r)
	state.Ready = false
	state.Reason = "draft changed before final exit"
	if got := callRunner(t, r, "/state", state); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if r.exitAcknowledged || r.exitRequested || r.status.Generation != "g" || r.status.Ready {
		t.Fatalf("exit acknowledgement stayed armed: %+v", r.status)
	}
	if got := callRunner(t, r, "/exiting", stateFor(r)); got.Code != 409 {
		t.Fatal("exit rearmed without coordinator")
	}
}

// The coordinator repeats /exit each tick until every child has exited. On
// novix one Claude took longer than the heartbeat freshness window, the
// repeat was refused, and the failed switch restarted the six sessions that
// had already exited, without their continuation.
func TestRepeatedExitAfterAcknowledgementSucceeds(t *testing.T) {
	r := testRunner(t)
	if got := callRunner(t, r, "/prepare", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if got := callRunner(t, r, "/exit", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if got := callRunner(t, r, "/exiting", stateFor(r)); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	// Claude shuts down; the plugin stays quiet and the session goes stale.
	r.lastState = time.Now().Add(-10 * time.Second)
	r.gate = func() error { return errors.New("terminal check is slow") }
	if got := callRunner(t, r, "/exit", control{Generation: "g"}); got.Code != 200 {
		t.Fatalf("repeated /exit after the acknowledgement was refused: %d %s", got.Code, got.Body.String())
	}
	// Before the acknowledgement, a stale session is still refused.
	other := testRunner(t)
	callRunner(t, other, "/prepare", control{Generation: "g"})
	other.lastState = time.Now().Add(-10 * time.Second)
	if got := callRunner(t, other, "/exit", control{Generation: "g"}); got.Code != 409 {
		t.Fatalf("stale unacknowledged session accepted /exit: %d", got.Code)
	}
}

// An acknowledged exit that Claude never carries out is ended by the runner:
// SIGTERM after exitDeadline, then SIGKILL while it still runs.
func TestOverdueAcknowledgedExitIsEnded(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	if got := callRunner(t, r, "/exiting", stateFor(r)); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	at := r.exitAckAt
	for _, step := range []struct {
		after time.Duration
		want  syscall.Signal
	}{
		{exitDeadline - time.Second, 0},
		{exitDeadline, syscall.SIGTERM},
		{exitDeadline + time.Second, 0},
		{exitDeadline + exitKillGrace, syscall.SIGKILL},
		{exitDeadline + exitKillGrace + time.Second, 0},
	} {
		if got := r.overdueExitSignalLocked(at.Add(step.after)); got != step.want {
			t.Fatalf("after %s: signal %v, want %v", step.after, got, step.want)
		}
	}
	// Once the runner is ending the child, a late heartbeat keeps the
	// acknowledgement, so the exit still counts for the switch.
	state := stateFor(r)
	state.Ready = false
	state.Reason = "waiting for Claude Code to exit"
	callRunner(t, r, "/state", state)
	if !r.exitAcknowledged {
		t.Fatal("a heartbeat revoked an exit the runner is ending")
	}
	// A revoked acknowledgement is never ended.
	other := testRunner(t)
	other.status.Generation = "g"
	other.exitRequested = true
	callRunner(t, other, "/exiting", stateFor(other))
	revoked := stateFor(other)
	revoked.Ready = false
	revoked.Reason = "exit acknowledgement cancelled: session changed"
	callRunner(t, other, "/state", revoked)
	if got := other.overdueExitSignalLocked(time.Now().Add(2 * exitDeadline)); got != 0 {
		t.Fatalf("revoked exit was ended with %v", got)
	}
}

// The acknowledgement records the work an exit stops, for the continuation
// after the restart. A revoked acknowledgement stopped nothing.
func TestExitAcknowledgementRecordsStoppedWork(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	state := stateFor(r)
	state.Stopped = []string{"background task bg1 (wait for the build)", "agent a1"}
	if got := callRunner(t, r, "/exiting", state); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if len(r.stopped) != 2 || r.stopped[0] != state.Stopped[0] {
		t.Fatalf("stopped work = %q", r.stopped)
	}
	revoked := stateFor(r)
	revoked.Ready = false
	revoked.Reason = "exit acknowledgement cancelled: session changed"
	callRunner(t, r, "/state", revoked)
	if r.stopped != nil {
		t.Fatalf("revoked exit kept stopped work: %q", r.stopped)
	}
	bad := stateFor(r)
	bad.Stopped = []string{"line\nbreak"}
	if got := callRunner(t, r, "/state", bad); got.Code != 400 {
		t.Fatalf("control characters in stopped work accepted: %d", got.Code)
	}
}

func TestAbortBeforeAcknowledgementKeepsChild(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	if got := callRunner(t, r, "/release", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if r.status.Generation != "" || r.exitRequested || r.released || r.status.Exited {
		t.Fatalf("abort changed active child: %+v", r.status)
	}
}

func TestLiveDispositionReplacesAdoptionSeed(t *testing.T) {
	r := testRunner(t)
	r.status.NeedsContinue = true
	state := stateFor(r)
	state.NeedsContinue = false
	state.Reason = "user cancelled"
	if got := callRunner(t, r, "/state", state); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if r.status.NeedsContinue {
		t.Fatal("adoption seed overrode user cancellation")
	}
}

func TestControlRejectsUnauthenticatedRequests(t *testing.T) {
	r := testRunner(t)
	request := httptest.NewRequest(http.MethodPost, "/prepare", strings.NewReader(`{"generation":"g"}`))
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if response.Code != 401 || r.status.Generation != "" {
		t.Fatal("unauthenticated request changed generation")
	}
}

func TestAcquireClaudeLockHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireClaudeLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if second, err := acquireClaudeLock(ctx, dir); !errors.Is(err, context.Canceled) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("lock ignored cancellation: %v", err)
	}
}

func TestLostRunnerBlocksPrepareAndRelease(t *testing.T) {
	dataDir := t.TempDir()
	root, err := rootDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	dir := filepath.Join(root, id)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	lease, err := privateFile(filepath.Join(dir, "lease"), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err := atomicJSON(filepath.Join(dir, "status.json"), StatusInfo{ID: id, Generation: "g", SessionID: "session"}); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(root, "pending.json"), transaction{Generation: "g", IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Prepare(dataDir, "g", nil); err == nil || ok {
		t.Fatal("crashed participant counted as exited")
	}
	if err := Release(dataDir, "", false); err == nil {
		t.Fatal("lost participant was silently released")
	}
	var txn transaction
	if err := readJSON(filepath.Join(root, "pending.json"), &txn); err != nil || txn.Generation != "g" {
		t.Fatal("recovery transaction was discarded")
	}
	states, err := Status(dataDir)
	if err != nil || len(states) != 1 || states[0].Live || states[0].Recovery == "" {
		t.Fatalf("stale status unavailable: %+v %v", states, err)
	}
}

func TestScanSkipsADirectoryBeingRemoved(t *testing.T) {
	dataDir := t.TempDir()
	root, err := rootDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	// The ending runner has removed its lease but not yet its status.
	id := strings.Repeat("a", 64)
	if err := privateDir(filepath.Join(root, id)); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(root, id, "status.json"), StatusInfo{ID: id, Exited: true}); err != nil {
		t.Fatal(err)
	}
	if states, err := Status(dataDir); err != nil || len(states) != 0 {
		t.Fatalf("status during removal = %+v, %v", states, err)
	}
}

// serveRunner registers r under root the way Run does and serves its control
// API. The returned function ends the runner process: it frees the lease.
func serveRunner(t *testing.T, root string, r *runner) func() {
	t.Helper()
	r.dir = filepath.Join(root, r.reg.ID)
	if err := privateDir(r.dir); err != nil {
		t.Fatal(err)
	}
	lease, err := privateFile(filepath.Join(r.dir, "lease"), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := r.persistLocked(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(r)
	r.reg.URL = server.URL
	if err := atomicJSON(filepath.Join(r.dir, "registration.json"), r.reg); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { server.Close(); lease.Close() }) }
	t.Cleanup(stop)
	return stop
}

func TestRotationOutlivesARunnerThatNeverJoined(t *testing.T) {
	for _, c := range []struct {
		ending string
		blocks bool
	}{
		{"exited", false}, // the runner removed its directory
		{"failed", false}, // Claude failed; the record stays for manual resume
		{"killed", true},  // the runner died while its Claude may still run
	} {
		t.Run(c.ending, func(t *testing.T) {
			dataDir := t.TempDir()
			root, err := rootDir(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			joined := testRunner(t)
			serveRunner(t, root, joined)
			outside := testRunner(t)
			outside.reg.ID = strings.Repeat("c", 64)
			outside.status.ID = outside.reg.ID
			outside.gate = func() error { return errors.New("automatic rotation requires a verified tmux pane or Rex terminal") }
			stop := serveRunner(t, root, outside)
			if ready, err := Prepare(dataDir, "g", nil); err != nil || ready {
				t.Fatalf("first prepare = %v, %v", ready, err)
			}
			joined.mu.Lock()
			joinedGeneration := joined.status.Generation
			joined.mu.Unlock()
			outside.mu.Lock()
			outsideGeneration := outside.status.Generation
			if c.ending == "failed" {
				outside.status.Exited = true
				outside.failLocked(errors.New("Claude exited without a confirmed successful rotation exit"))
				err = outside.persistLocked()
			}
			outside.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if joinedGeneration != "g" || outsideGeneration != "" {
				t.Fatalf("generations = %q, %q", joinedGeneration, outsideGeneration)
			}
			stop()
			if c.ending == "exited" {
				if err := os.RemoveAll(outside.dir); err != nil {
					t.Fatal(err)
				}
			}
			ready, err := Prepare(dataDir, "g", nil)
			if c.blocks {
				if err == nil || ready {
					t.Fatal("participant lost with a running Claude was dropped")
				}
				if err := Release(dataDir, "", false); err == nil {
					t.Fatal("participant lost with a running Claude was released")
				}
				if _, err := os.Stat(filepath.Join(root, "pending.json")); err != nil {
					t.Fatal("recovery transaction was discarded")
				}
				return
			}
			if err != nil || ready {
				t.Fatalf("prepare after the session ended = %v, %v", ready, err)
			}
			joined.mu.Lock()
			asked := joined.exitRequested
			joined.mu.Unlock()
			if !asked {
				t.Fatal("remaining participant was not asked to exit")
			}
			// The plugin acknowledges the exit, then Claude ends.
			if got := callRunner(t, joined, "/exiting", stateFor(joined)); got.Code != 200 {
				t.Fatal(got.Body.String())
			}
			joined.mu.Lock()
			joined.status.Exited = true
			joined.mu.Unlock()
			if ready, err := Prepare(dataDir, "g", nil); err != nil || !ready {
				t.Fatalf("drain did not finish: %v, %v", ready, err)
			}
			if err := Release(dataDir, "g", true); err != nil {
				t.Fatal(err)
			}
			joined.mu.Lock()
			released := joined.released && joined.committed
			joined.mu.Unlock()
			if !released {
				t.Fatal("remaining participant was not released to restart")
			}
			if _, err := os.Stat(filepath.Join(root, "pending.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rotation stayed pending: %v", err)
			}
			states, err := Status(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if c.ending == "failed" && (len(states) != 2 || states[1].Live || states[1].Recovery == "") {
				t.Fatalf("failed session's recovery record missing: %+v", states)
			}
		})
	}
}

func TestRestartContinuesOnlyAfterCommittedRelease(t *testing.T) {
	const session = "0b7c3f9e-5d1a-4c2b-9e8f-1a2b3c4d5e6f"
	stopped := []string{"background task bg1 (wait for the build)"}
	for _, c := range []struct {
		name             string
		saved, committed bool
		stopped          []string
	}{
		{"abort", true, false, nil},
		{"commit", true, true, nil},
		// A session that saved nothing starts again under its ID.
		{"unsaved", false, true, nil},
		// The exit stopped background work either way; the restarted session
		// is told what, though only a committed switch continues the work
		// interrupted by the usage limit.
		{"abort with stopped work", true, false, stopped},
		{"commit with stopped work", true, true, stopped},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			project := filepath.Join(home, ".claude", "projects", "-work")
			if err := os.MkdirAll(project, 0700); err != nil {
				t.Fatal(err)
			}
			if c.saved {
				if err := os.WriteFile(filepath.Join(project, session+".jsonl"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := testRunner(t)
			r.status.SessionID = session
			capture := filepath.Join(r.dir, "child-arguments")
			binary := filepath.Join(r.dir, "fake-claude")
			script := "#!/bin/sh\nprintf '%s\\n' \"$QSWITCH_INITIAL_CONTINUATION\" \"$@\" > " + shellQuote(capture) + "\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			r.options.ClaudeBin = binary
			r.options.Args = []string{"--permission-mode", "plan", "original prompt"}
			r.status.Exited = true
			r.status.Generation = "g"
			r.status.NeedsContinue = true
			r.exitAcknowledged, r.released, r.committed = true, true, c.committed
			r.stopped = c.stopped
			child, err := r.startLocked(context.Background(), startRotation)
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			args := string(got)
			want := "--resume\n" + session + "\n"
			if !c.saved {
				want = "--session-id\n" + session + "\n"
			}
			if !strings.Contains(args, want) || !strings.Contains(args, "--permission-mode\nplan\n") || strings.Contains(args, "original prompt") {
				t.Fatalf("incorrect restart arguments: %s", args)
			}
			if strings.Contains(args, continuation) != (c.saved && c.committed) {
				t.Fatalf("continuation decision did not match release: %s", args)
			}
			if strings.Contains(args, "bg1 (wait for the build)") != (c.saved && c.stopped != nil) {
				t.Fatalf("stopped work not reported to the restarted session: %s", args)
			}
			if r.stopped != nil {
				t.Fatal("stopped work kept for a later restart")
			}
			// A continuation on the command line is marked so that a later
			// resume does not send a second one.
			want = "continue\n"
			if c.saved && (c.committed || c.stopped != nil) {
				want = "prompted\n"
			}
			if !strings.HasPrefix(args, want) {
				t.Fatalf("pending work state not preserved: %s", args)
			}
			var reg registration
			if err := readJSON(filepath.Join(r.dir, "registration.json"), &reg); err != nil || reg.ID != r.reg.ID {
				t.Fatalf("child not registered: %+v %v", reg, err)
			}
		})
	}
}
