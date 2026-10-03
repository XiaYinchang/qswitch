package grok

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/busy"
)

func TestKillCLIRefusesReusedPIDFromAuthLock(t *testing.T) {
	// This harmless child belongs to this test. A stale Grok lock references
	// its PID, but the current process list correctly identifies it as sleep.
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	t.Cleanup(func() { _ = child.Process.Kill(); <-exited })
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(home), []byte(fmt.Sprint(child.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	a := Adapter{List: func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: child.Process.Pid, Command: "sleep 30"}}, nil
	}}
	err := a.KillCLI(home)
	var blocked *adapter.BusyError
	if !errors.As(err, &blocked) {
		t.Fatalf("unverified PID must remain blocked, got %v", err)
	}
	if !busy.Alive(child.Process.Pid) {
		t.Fatal("unrelated child was terminated before reporting blocked")
	}
	select {
	case <-exited:
		t.Fatal("unrelated child received a termination signal before reporting blocked")
	case <-time.After(50 * time.Millisecond):
	}
}
