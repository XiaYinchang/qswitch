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

func statusOf(t *testing.T, r *runner) StatusInfo {
	t.Helper()
	var s StatusInfo
	if err := json.Unmarshal(callRunner(t, r, "/status", nil).Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExitAcknowledgementCanBeRevoked(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	if got := callRunner(t, r, "/exiting", stateFor(r)); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	// The switch log names an acknowledged session as exiting.
	if s := statusOf(t, r); s.Reason != "exiting" {
		t.Fatalf("acknowledged session reported as %q", s.Reason)
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
	if s := statusOf(t, r); s.Reason != "draft changed before final exit" {
		t.Fatalf("revoked exit reported as %q", s.Reason)
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

// Claude Code asks before an /exit stops background work, and nobody answers.
// An exit acknowledged with unfinished work is ended after a few seconds
// instead of a minute, while the other sessions wait exited.
func TestExitWithUnfinishedWorkIsEndedSoon(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	state := stateFor(r)
	state.Stopped = []string{"scheduled task 47ccd50b"}
	if got := callRunner(t, r, "/exiting", state); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	at := r.exitAckAt
	if got := r.overdueExitSignalLocked(at.Add(exitDeadlineWithWork - time.Second)); got != 0 {
		t.Fatalf("ended before its deadline: %v", got)
	}
	if got := r.overdueExitSignalLocked(at.Add(exitDeadlineWithWork)); got != syscall.SIGTERM {
		t.Fatalf("exit with unfinished work not ended after %s: %v", exitDeadlineWithWork, got)
	}
}

// The acknowledgement records the work an exit stops, for the continuation
// after the restart. A revoked acknowledgement stopped nothing.
// statusFileID identifies the current status.json. Each write replaces the
// file through a rename, so an unchanged ID means nothing was written.
func statusFileID(t *testing.T, r *runner) uint64 {
	t.Helper()
	st, err := os.Stat(filepath.Join(r.dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}

// The coordinator repeats /prepare and /exit every tick while a switch waits.
// On novix a status write could take 3 s, and eight repeated /prepare requests
// on 11-04 ran past the coordinator's 3 s timeout. Only a change is written.
func TestRepeatedControlRequestsDoNotRewriteStatus(t *testing.T) {
	r := testRunner(t)
	if got := callRunner(t, r, "/prepare", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	prepared := statusFileID(t, r)
	for _, route := range []string{"/prepare", "/prepare", "/exit", "/exit"} {
		if got := callRunner(t, r, route, control{Generation: "g"}); got.Code != 200 {
			t.Fatalf("%s: %s", route, got.Body.String())
		}
	}
	heartbeat(t, r, stateFor(r))
	if statusFileID(t, r) != prepared {
		t.Fatal("status rewritten although nothing changed")
	}
	// A change made without writing it, as the upgrade watchdog does, is
	// written with the next request.
	r.mu.Lock()
	r.status.Reason = "changed in memory only"
	r.mu.Unlock()
	if got := callRunner(t, r, "/prepare", control{Generation: "g"}); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if statusFileID(t, r) == prepared {
		t.Fatal("a change was not written")
	}
	var onDisk StatusInfo
	if err := readJSON(filepath.Join(r.dir, "status.json"), &onDisk); err != nil || onDisk.Generation != "g" || onDisk.Reason != "changed in memory only" {
		t.Fatalf("status.json holds %+v (%v)", onDisk, err)
	}
}

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

// Messages typed during a switch are held by the plugin. The acknowledgement
// hands them over and a later one from the same exiting session updates
// them. A revoked acknowledgement drops them: the plugin still holds them and
// sends them with its next acknowledgement.
func TestExitAcknowledgementRecordsHeldMessages(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	state := stateFor(r)
	state.Held = []string{"why is it stuck"}
	if got := callRunner(t, r, "/exiting", state); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if len(r.held) != 1 || r.held[0] != "why is it stuck" {
		t.Fatalf("acknowledgement did not record held messages: %q", r.held)
	}
	// Typed while Claude Code shuts down.
	state.Held = []string{"why is it stuck", "second\nline"}
	if got := callRunner(t, r, "/exiting", state); got.Code != 200 {
		t.Fatalf("update after the acknowledgement refused: %d %s", got.Code, got.Body.String())
	}
	if len(r.held) != 2 || r.held[1] != "second\nline" || !r.exitAcknowledged {
		t.Fatalf("held = %q, acknowledged = %v", r.held, r.exitAcknowledged)
	}
	other := state
	other.Generation = "h"
	other.Held = []string{"from another generation"}
	if got := callRunner(t, r, "/exiting", other); got.Code != 409 || len(r.held) != 2 {
		t.Fatalf("update for another generation: %d %q", got.Code, r.held)
	}
	revoked := stateFor(r)
	revoked.Ready = false
	revoked.Reason = "exit acknowledgement cancelled: session changed"
	callRunner(t, r, "/state", revoked)
	if r.held != nil {
		t.Fatalf("revoked exit kept held messages: %q", r.held)
	}
	bad := stateFor(r)
	bad.Held = []string{strings.Repeat("x", maxHeld+1)}
	if got := callRunner(t, r, "/state", bad); got.Code != 400 {
		t.Fatalf("oversized held messages accepted: %d", got.Code)
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
	if ok, _, err := Prepare(dataDir, "g", nil); err == nil || ok {
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
			if ready, _, err := Prepare(dataDir, "g", nil); err != nil || ready {
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
			ready, _, err := Prepare(dataDir, "g", nil)
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
			if ready, _, err := Prepare(dataDir, "g", nil); err != nil || !ready {
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

// A session that ends while the switch waits for it leaves the switch instead
// of holding it: once its Claude is gone the runner drops the generation, and
// the coordinator goes on without the participant.
func TestSessionEndingDuringDrainLeavesTheRotation(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(r *runner) bool // settles the ended session; reports whether the record is kept
		kept bool
	}{
		{"user exit", func(r *runner) bool { r.status.Exited = true; return r.unmanagedExitLocked(nil) }, false},
		{"crash", func(r *runner) bool {
			r.status.Exited = true
			return r.unmanagedExitLocked(errors.New("exit status 1"))
		}, true},
		{"runner stopped", func(r *runner) bool { return r.stoppedLocked() }, false},
		{"runner stopped after the exit", func(r *runner) bool {
			r.status.Exited, r.exitAcknowledged = true, true
			return r.stoppedLocked()
		}, true},
		{"restart failed", func(r *runner) bool {
			r.status.Exited, r.exitAcknowledged, r.released = true, true, true
			r.restartFailedLocked(errors.New("cannot verify Rex terminal"))
			return true
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			root, err := rootDir(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			staying := testRunner(t)
			serveRunner(t, root, staying)
			ending := testRunner(t)
			ending.reg.ID = strings.Repeat("c", 64)
			ending.status.ID = ending.reg.ID
			ending.status.SessionID = "session-two"
			ending.status.Ready = false // the switch waits for this session
			stop := serveRunner(t, root, ending)
			if ready, waiting, err := Prepare(dataDir, "g", nil); err != nil || ready || len(waiting) != 1 || waiting[0].SessionID != "session-two" {
				t.Fatalf("first prepare = %v, %v, %v", ready, waiting, err)
			}
			ending.mu.Lock()
			joined := ending.status.Generation == "g"
			keep := c.end(ending)
			err = ending.persistLocked()
			ending.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if !joined {
				t.Fatal("ending session had not joined the switch")
			}
			if keep != c.kept {
				t.Fatalf("record kept = %v, want %v", keep, c.kept)
			}
			// The runner process ends: Run removes the directory unless the
			// record is kept.
			stop()
			if !keep {
				if err := os.RemoveAll(ending.dir); err != nil {
					t.Fatal(err)
				}
			}
			ready, waiting, err := Prepare(dataDir, "g", nil)
			if err != nil || ready || len(waiting) != 1 || waiting[0].Reason != "exiting" {
				t.Fatalf("prepare after the session ended = %v, %v, %v", ready, waiting, err)
			}
			if got := callRunner(t, staying, "/exiting", stateFor(staying)); got.Code != 200 {
				t.Fatal(got.Body.String())
			}
			staying.mu.Lock()
			staying.status.Exited = true
			staying.mu.Unlock()
			if ready, _, err := Prepare(dataDir, "g", nil); err != nil || !ready {
				t.Fatalf("drain did not finish: %v, %v", ready, err)
			}
			if err := Release(dataDir, "g", true); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "pending.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rotation stayed pending: %v", err)
			}
			states, err := Status(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if !c.kept {
				if len(states) != 1 {
					t.Fatalf("ended session left a record: %+v", states)
				}
				return
			}
			if len(states) != 2 || states[1].Live || !states[1].Exited || states[1].Generation != "" || states[1].Recovery == "" {
				t.Fatalf("ended session's record = %+v", states)
			}
		})
	}
}

// A runner that SIGTERM or SIGHUP is ending reports its session busy, so the
// switch waits the few seconds until the runner is gone instead of failing.
func TestStoppingRunnerMakesTheSwitchWait(t *testing.T) {
	dataDir := t.TempDir()
	root, err := rootDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	r := testRunner(t)
	serveRunner(t, root, r)
	if ready, waiting, err := Prepare(dataDir, "g", nil); err != nil || ready || len(waiting) != 1 || waiting[0].Reason != "exiting" {
		t.Fatalf("prepare = %v, %v, %v", ready, waiting, err)
	}
	r.mu.Lock()
	r.stopping = true
	r.exitRequested = false
	r.mu.Unlock()
	ready, waiting, err := Prepare(dataDir, "g", nil)
	if err != nil || ready || len(waiting) != 1 || waiting[0].Reason != "runner stopping" {
		t.Fatalf("prepare while stopping = %v, %v, %v", ready, waiting, err)
	}
	// A repeated /exit does not ask the stopping runner's child for anything.
	if got := callRunner(t, r, "/exit", control{Generation: "g"}); got.Code != 200 {
		t.Fatalf("exit while stopping: %d %s", got.Code, got.Body.String())
	}
	r.mu.Lock()
	asked := r.exitRequested
	r.mu.Unlock()
	if asked {
		t.Fatal("stopping runner was asked to exit its child")
	}
	// A stopping runner that never joined does not take the generation.
	fresh := testRunner(t)
	fresh.stopping = true
	var state StatusInfo
	if got := callRunner(t, fresh, "/prepare", control{Generation: "g"}); got.Code != 200 {
		t.Fatalf("prepare on a stopping runner: %d %s", got.Code, got.Body.String())
	} else if err := json.Unmarshal(got.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Ready || state.Reason != "runner stopping" || fresh.status.Generation != "" {
		t.Fatalf("stopping runner answered %+v with generation %q", state, fresh.status.Generation)
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

// fakeClaudeRunner returns a runner whose Claude only writes its continuation
// state and arguments to the returned file, for a session whose transcript is
// saved or not.
func fakeClaudeRunner(t *testing.T, session string, saved bool) (*runner, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, ".claude", "projects", "-work")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if saved {
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
	return r, capture
}

// Messages typed during the switch reach the restarted session: as its
// prompt on their own, after the continuation when there is one, and even in
// a session that saved nothing. A runner upgrade carries them as well.
func TestRestartSendsHeldMessages(t *testing.T) {
	const session = "0b7c3f9e-5d1a-4c2b-9e8f-1a2b3c4d5e6f"
	for _, c := range []struct {
		name                      string
		mode                      startMode
		saved, committed, waiting bool
		held                      []string
		want                      string
	}{
		{"idle session", startRotation, true, true, false, []string{"why is it stuck"},
			"--resume\n" + session + "\n--\nwhy is it stuck\n"},
		{"interrupted session", startRotation, true, true, true, []string{"also check the logs"},
			"--\n" + continuation + "\n\nThe user sent this while the account was switching:\n\nalso check the logs\n"},
		// A cancelled switch does not continue, but still delivers what was typed.
		{"cancelled switch", startRotation, true, false, true, []string{"first", "second"},
			"--resume\n" + session + "\n--\nfirst\n\nsecond\n"},
		// On the command line a leading slash would run a command.
		{"leading slash", startRotation, true, true, false, []string{"/home is full"},
			"--\nThe user sent this while the account was switching:\n\n/home is full\n"},
		{"unsaved session", startRotation, false, true, false, []string{"start here"},
			"--session-id\n" + session + "\n--\nstart here\n"},
		{"runner upgrade", startHandoff, true, false, false, []string{"typed during the upgrade"},
			"--resume\n" + session + "\n--\ntyped during the upgrade\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, capture := fakeClaudeRunner(t, session, c.saved)
			if c.mode == startRotation {
				r.status.Exited = true
				r.status.Generation = "g"
				r.exitAcknowledged, r.released, r.committed = true, true, c.committed
			}
			r.status.NeedsContinue = c.waiting
			r.held = c.held
			child, err := r.startLocked(context.Background(), c.mode)
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
			// The prompt runs at once, so a later resume must not add a continuation.
			if !strings.HasPrefix(args, "prompted\n") || !strings.HasSuffix(args, c.want) {
				t.Fatalf("restart arguments:\n%s", args)
			}
			if r.held != nil {
				t.Fatal("held messages kept for a later restart")
			}
		})
	}
}

// An exit acknowledgement records the unsent text in the input box. A later
// one from the same exiting session replaces it, and a revoked one drops it:
// the plugin sends it again with its next acknowledgement.
func TestExitAcknowledgementCarriesTheDraft(t *testing.T) {
	r := testRunner(t)
	r.status.Generation = "g"
	r.exitRequested = true
	state := stateFor(r)
	state.Draft = "half written idea"
	if got := callRunner(t, r, "/exiting", state); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if r.draft != "half written idea" {
		t.Fatalf("acknowledgement did not record the draft: %q", r.draft)
	}
	// Sent while Claude Code shut down: now held, and gone from the box.
	state.Held, state.Draft = []string{"half written idea"}, ""
	if got := callRunner(t, r, "/exiting", state); got.Code != 200 || r.draft != "" {
		t.Fatalf("update after the acknowledgement: %d, draft %q", got.Code, r.draft)
	}
	state.Draft = "typed again"
	callRunner(t, r, "/exiting", state)
	revoked := stateFor(r)
	revoked.Ready = false
	revoked.Reason = "exit acknowledgement cancelled: session changed"
	callRunner(t, r, "/state", revoked)
	if r.draft != "" {
		t.Fatalf("revoked exit kept the draft: %q", r.draft)
	}
	bad := stateFor(r)
	bad.Draft = strings.Repeat("x", maxDraft+1)
	if got := callRunner(t, r, "/state", bad); got.Code != 400 {
		t.Fatalf("oversized draft accepted: %d", got.Code)
	}
}

// The restarted child is given the draft with every answer until it reports
// that it put it back in its input box.
func TestRestartPutsTheDraftBack(t *testing.T) {
	r, _ := fakeClaudeRunner(t, "0b7c3f9e-5d1a-4c2b-9e8f-1a2b3c4d5e6f", true)
	r.status.Exited = true
	r.status.Generation = "g"
	r.exitAcknowledged, r.released, r.committed = true, true, true
	r.draft = "half written idea"
	child, err := r.startLocked(context.Background(), startRotation)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if r.draft != "" || r.restore != "half written idea" {
		t.Fatalf("draft %q, restore %q after the restart", r.draft, r.restore)
	}
	state := stateFor(r)
	for range 2 {
		if a := heartbeat(t, r, state); a.Restore != "half written idea" {
			t.Fatalf("draft not handed to the new child: %+v", a)
		}
	}
	state.Restored = true
	if a := heartbeat(t, r, state); a.Restore != "" || r.restore != "" {
		t.Fatalf("draft handed out again after it was put back: %+v", a)
	}
}

// A message typed during an upgrade's exit, and the draft left in the box,
// reach the new binary's session.
func TestHandoffCarriesHeldMessages(t *testing.T) {
	old := testRunner(t)
	old.status.NeedsContinue = true
	old.held = []string{"typed during the upgrade"}
	old.draft = "half written idea"
	h := old.handoffLocked()
	h.Dir = filepath.Join(t.TempDir(), old.reg.ID)
	h.ListenerFD, h.LeaseFD = 3, 4
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(handoffEnv, string(b))
	got, err := takeHandoff()
	if err != nil {
		t.Fatal(err)
	}
	upgraded := testRunner(t)
	upgraded.inherit(got)
	if len(upgraded.held) != 1 || upgraded.held[0] != old.held[0] || upgraded.draft != old.draft || upgraded.status.SessionID != old.status.SessionID || !upgraded.status.NeedsContinue {
		t.Fatalf("handoff lost session state: held %q, draft %q, status %+v", upgraded.held, upgraded.draft, upgraded.status)
	}
}
