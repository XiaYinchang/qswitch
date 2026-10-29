package app

import (
	"encoding/json"
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
		return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"source"}}`), nil
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
	list := func() ([]adapter.Proc, error) {
		mu.Lock()
		defer mu.Unlock()
		var ps []adapter.Proc
		for _, s := range terminals {
			if !s.exited {
				ps = append(ps, adapter.Proc{PID: s.pid, Command: "claude"})
			}
		}
		return ps, nil
	}
	a.Adapters[adapter.Claude] = claude.Adapter{List: list}
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
				http.Error(w, "unauthorized", 401)
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
				if err != nil || live.AccessToken != "target-access" {
					t.Error("released before credential commit")
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
}
