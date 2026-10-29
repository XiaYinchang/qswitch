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

func TestPrepareBlocksWithoutDetachedTmux(t *testing.T) {
	r := testRunner(t)
	r.gate = func() error { return errors.New("no verified detached tmux pane") }
	response := callRunner(t, r, "/prepare", control{Generation: "g"})
	var state StatusInfo
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || state.Ready || state.Generation != "" || state.Reason == "" {
		t.Fatalf("unsafe prepare: %+v", state)
	}
	if err := unattendedTMUX("", ""); err == nil {
		t.Fatal("non-tmux run accepted")
	}
	if err := unattendedTMUX("%spoof", "/tmp/tmux,1,0"); err == nil {
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

func TestRestartContinuesOnlyAfterCommittedRelease(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "abort", true: "commit"}[committed], func(t *testing.T) {
			r := testRunner(t)
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
			r.exitAcknowledged, r.released, r.committed = true, true, committed
			child, err := r.startLocked(context.Background(), true)
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
			if !strings.Contains(args, "--resume\nsession-one\n") || !strings.Contains(args, "--permission-mode\nplan\n") || strings.Contains(args, "original prompt") {
				t.Fatalf("incorrect restart arguments: %s", args)
			}
			if strings.Contains(args, continuation) != committed {
				t.Fatalf("continuation decision did not match release: %s", args)
			}
			if !strings.HasPrefix(args, "continue\n") {
				t.Fatalf("pending work state not preserved: %s", args)
			}
			var reg registration
			if err := readJSON(filepath.Join(r.dir, "registration.json"), &reg); err != nil || reg.ID != r.reg.ID {
				t.Fatalf("child not registered: %+v %v", reg, err)
			}
		})
	}
}
