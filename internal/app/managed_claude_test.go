package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/state"
)

// Exercise the app's quota gates and credential restore with three independent
// runner endpoints. The transport stands in for terminal owners, not the app.
func TestManagedClaudeRotationWaitsForEverySessionBeforeCredentialCommit(t *testing.T) {
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
	for id, class := range map[string]string{"source": "exhausted", "target": "ok"} {
		pct := 10.0
		if id == "source" {
			pct = 100
		}
		if err := a.State.UpdateQuotaSnapshot("claude", id, class, pct, a.now().Add(time.Hour).Unix(), a.now().Unix()); err != nil {
			t.Fatal(err)
		}
		if err := a.State.LogQuota("claude", id, class, "http", pct, a.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.State.SetPending(state.Pending{Tool: "claude", FromID: "source", ToID: "target", QueuedAt: a.now().Unix(), Reason: "exhausted"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	type terminal struct {
		pid                                   int
		ready, exited, continuation, released bool
		generation                            string
	}
	terminals := []*terminal{{pid: 801, ready: true}, {pid: 802, ready: true, continuation: true}, {pid: 803, continuation: true}}
	wantAccess := "target-access"
	list := func() ([]adapter.Proc, error) {
		mu.Lock()
		defer mu.Unlock()
		ps := []adapter.Proc{{PID: 900, Command: "claude"}}
		for _, s := range terminals {
			if !s.exited {
				ps = append(ps, adapter.Proc{PID: s.pid, Command: "claude"})
			}
		}
		return ps, nil
	}
	a.Adapters[adapter.Claude] = claude.Adapter{List: list, DataDir: a.DataDir}
	root := filepath.Join(a.DataDir, "managed-claude")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for i, s := range terminals {
		id := fmt.Sprintf("%064x", i+1)
		token := strings.Repeat("a", 64)
		dir := filepath.Join(root, id)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		lease, err := os.OpenFile(filepath.Join(dir, "lease"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { lease.Close() })
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var c struct {
				Generation string `json:"generation"`
				Committed  bool   `json:"committed"`
			}
			if r.Method == http.MethodPost {
				if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
					http.Error(w, "invalid", 400)
					return
				}
			}
			switch r.URL.Path {
			case "/prepare":
				s.generation = c.Generation
			case "/exit":
				if !s.ready {
					t.Error("exit sent before all terminals ready")
				}
				s.exited = true
			case "/release":
				live, err := claude.ReadLiveCreds(a.UserHome)
				if err != nil || live.AccessToken != wantAccess {
					t.Errorf("release credential check got %q, want %q (err=%v)", live.AccessToken, wantAccess, err)
				}
				if !c.Committed {
					t.Error("successful rotation released as cancelled")
				}
				s.released = true
			case "/status":
			default:
				t.Errorf("unexpected route %s", r.URL.Path)
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "runner_pid": os.Getpid(), "child_pid": s.pid, "session_id": fmt.Sprintf("session-%d", i), "ready": s.ready, "exited": s.exited, "needs_continue": s.continuation, "generation": s.generation})
		}))
		t.Cleanup(server.Close)
		reg, _ := json.Marshal(map[string]any{"id": id, "pid": os.Getpid(), "url": server.URL, "token": token})
		if err := os.WriteFile(filepath.Join(dir, "registration.json"), reg, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.TryApply(adapter.Claude, false); err != nil {
		t.Fatal(err)
	}
	for _, s := range terminals {
		if s.exited || s.released {
			t.Fatal("rotation disturbed a session while another was busy")
		}
	}
	live, err := claude.ReadLiveCreds(a.UserHome)
	if err != nil || live.AccessToken != "old-access" {
		t.Fatal("credentials changed before readiness")
	}
	mu.Lock()
	terminals[2].ready = true
	mu.Unlock()
	if err := a.TryApply(adapter.Claude, false); err != nil {
		t.Fatal(err)
	}
	p, err := a.State.GetPointer("claude")
	if err != nil || p.StableID != "target" {
		t.Fatalf("wrong current account: %+v %v", p, err)
	}
	for i, s := range terminals {
		if !s.exited || !s.released {
			t.Fatalf("terminal %d was not released after commit: %+v", i, s)
		}
	}
	if strings.Contains(a.Status(), "blocked_by_pid=[900]") {
		t.Fatal("unmanaged Claude appeared as a switch blocker")
	}
	mu.Lock()
	wantAccess = "old-access"
	for _, s := range terminals {
		s.ready = true
		s.exited = false
		s.released = false
		s.generation = ""
	}
	mu.Unlock()
	if err := a.Switch(adapter.Claude, "source", adapter.RestoreOpts{}, true); err != nil {
		t.Fatal(err)
	}
	p, err = a.State.GetPointer("claude")
	if err != nil || p.StableID != "source" {
		t.Fatalf("manual switch used the wrong account: %+v %v", p, err)
	}
	for i, s := range terminals {
		if !s.exited || !s.released {
			t.Fatalf("manual switch did not coordinate terminal %d: %+v", i, s)
		}
	}
}

func writeStaleClaudeRecord(t *testing.T, root, id string, childPID int) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lease"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"id":         id,
		"runner_pid": childPID,
		"child_pid":  childPID,
		"session_id": "stale-session",
		"cwd":        t.TempDir(),
		"ready":      false,
		"exited":     false,
		"generation": "",
		"live":       true,
	})
	if err := os.WriteFile(filepath.Join(dir, "status.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// A dead runner left exited=false on disk with a vanished child PID must not
// let Prepare drain live sessions only for switchLocked/Restore to fail with
// BusyError and bounce every session back onto the exhausted account.
func TestManagedClaudeRotationIgnoresDeadStaleRecordAndStopsBeforeDrainOnAliveOrphan(t *testing.T) {
	a := setupClaudeRecovery(t, "", false)
	a.Cfg.General.AutoSwitch = true
	a.Cfg.Claude.Enabled = true
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
	for id, class := range map[string]string{"source": "exhausted", "target": "ok"} {
		pct := 10.0
		if id == "source" {
			pct = 100
		}
		if err := a.State.UpdateQuotaSnapshot("claude", id, class, pct, a.now().Add(time.Hour).Unix(), a.now().Unix()); err != nil {
			t.Fatal(err)
		}
		if err := a.State.LogQuota("claude", id, class, "http", pct, a.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.State.SetPending(state.Pending{Tool: "claude", FromID: "source", ToID: "target", QueuedAt: a.now().Unix(), Reason: "exhausted"}); err != nil {
		t.Fatal(err)
	}
	a.Adapters[adapter.Claude] = claude.Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }, DataDir: a.DataDir}
	root := filepath.Join(a.DataDir, "managed-claude")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	prepared, exited, released, committed := 0, false, false, false
	gen := ""
	id := fmt.Sprintf("%064x", 1)
	token := strings.Repeat("b", 64)
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := os.OpenFile(filepath.Join(dir, "lease"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var c struct {
			Generation string `json:"generation"`
			Committed  bool   `json:"committed"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&c)
		}
		switch r.URL.Path {
		case "/prepare":
			prepared++
			gen = c.Generation
		case "/exit":
			exited = true
		case "/release":
			released = true
			committed = c.Committed
			gen = ""
		case "/status":
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "runner_pid": os.Getpid(), "child_pid": 801,
			"session_id": "live-session", "ready": true, "exited": exited, "generation": gen,
		})
	}))
	t.Cleanup(server.Close)
	reg, _ := json.Marshal(map[string]any{"id": id, "pid": os.Getpid(), "url": server.URL, "token": token})
	if err := os.WriteFile(filepath.Join(dir, "registration.json"), reg, 0600); err != nil {
		t.Fatal(err)
	}

	// Dead stale record must not hold up credential commit.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	writeStaleClaudeRecord(t, root, strings.Repeat("d", 64), dead.Process.Pid)

	if err := a.TryApply(adapter.Claude, false); err != nil {
		t.Fatalf("TryApply failed with dead stale record: %v", err)
	}
	pPtr, err := a.State.GetPointer("claude")
	if err != nil || pPtr.StableID != "target" {
		t.Fatalf("pointer = %+v, %v; want target", pPtr, err)
	}
	if !exited || !released || !committed {
		t.Fatalf("live runner state exited=%v released=%v committed=%v", exited, released, committed)
	}

	// Alive orphan child must stop manual switch before preparing or exiting live runners.
	mu.Lock()
	prepared, exited, released, committed, gen = 0, false, false, false, ""
	mu.Unlock()
	writeStaleClaudeRecord(t, root, strings.Repeat("e", 64), os.Getpid())
	if err := a.Switch(adapter.Claude, "source", adapter.RestoreOpts{}, true); err == nil {
		t.Fatal("manual switch succeeded past an alive orphaned Claude process")
	}
	mu.Lock()
	gotPrepared, gotExited := prepared, exited
	mu.Unlock()
	if gotPrepared != 0 || gotExited {
		t.Fatalf("alive orphan should fail before draining live runner: prepared=%d exited=%v", gotPrepared, gotExited)
	}
}
