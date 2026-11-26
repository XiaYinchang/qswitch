//go:build !windows

package claude

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/managed"
)

func writeStaleManagedRecord(t *testing.T, dataDir, id string, pid int) {
	t.Helper()
	dir := filepath.Join(dataDir, "managed-claude", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lease"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(managed.StatusInfo{
		ID:        id,
		RunnerPID: pid,
		ChildPID:  pid,
		SessionID: "session-" + id[:8],
		CWD:       t.TempDir(),
		Exited:    false,
		Live:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAutoBlockersAndRestoreHandleStaleManagedRecords(t *testing.T) {
	home := t.TempDir()
	writeCreds(t, home, validOAuth())

	// Obtain a PID guaranteed to have exited and been reaped.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := dead.Process.Pid

	dataDir := t.TempDir()
	writeStaleManagedRecord(t, dataDir, strings.Repeat("a", 64), deadPID)

	a := Adapter{DataDir: dataDir, List: func() ([]adapter.Proc, error) { return nil, nil }}
	blobs, _, err := a.Capture(home)
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.AutoBlockers(home)
	if err != nil {
		t.Fatal(err)
	}
	if h.ManualBusy() {
		t.Fatalf("dead stale managed record should not block: %+v", h)
	}
	other := validOAuth()
	other["accessToken"] = "sk-ant-oat-other"
	writeCreds(t, home, other)
	if err := a.Restore(home, blobs[0], adapter.RestoreOpts{}); err != nil {
		t.Fatalf("restore failed with dead stale managed record: %v", err)
	}
	c, err := ReadLiveCreds(home)
	if err != nil || c.AccessToken != "sk-ant-oat-access" {
		t.Fatalf("credentials not restored: %+v %v", c, err)
	}

	// An orphaned child that still exists must continue to block.
	writeStaleManagedRecord(t, dataDir, strings.Repeat("b", 64), os.Getpid())
	h, err = a.AutoBlockers(home)
	if err != nil {
		t.Fatal(err)
	}
	if pids := h.ManualPIDs(); len(pids) != 1 || pids[0] != os.Getpid() {
		t.Fatalf("alive stale child should block, got %v", pids)
	}
	var busyErr *adapter.BusyError
	if err := a.Restore(home, blobs[0], adapter.RestoreOpts{}); !errors.As(err, &busyErr) {
		t.Fatalf("expected BusyError for alive stale child, got %v", err)
	}
}
