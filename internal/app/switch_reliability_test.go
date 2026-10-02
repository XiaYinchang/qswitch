package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
)

type switchFailureAdapter struct {
	adapter.Adapter
	holders    adapter.Holders
	listErr    error
	restoreErr error
	idle       bool
	killed     bool
}

func (s *switchFailureAdapter) ManualBlockers(string) (adapter.Holders, error) {
	return s.holders, s.listErr
}

func (s *switchFailureAdapter) Idle(string, time.Duration) bool { return s.idle }

func (s *switchFailureAdapter) KillCLI(string) error {
	s.killed = true
	s.holders = adapter.Holders{}
	return nil
}

func (s *switchFailureAdapter) Restore(home string, b adapter.Blob, opts adapter.RestoreOpts) error {
	if s.restoreErr != nil {
		return s.restoreErr
	}
	return s.Adapter.Restore(home, b, opts)
}

func setupDesktopSwitch(t *testing.T) (*App, *switchFailureAdapter, *bool, *int, *int) {
	t.Helper()
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	running, quits, launches := true, 0, 0
	a.ListProcs = func() ([]adapter.Proc, error) {
		if running {
			return []adapter.Proc{{PID: 42, Command: "/Applications/ChatGPT.app/Contents/MacOS/ChatGPT"}}, nil
		}
		return nil, nil
	}
	a.Host.QuitFn = func(string) error { quits++; running = false; return nil }
	a.Host.LaunchFn = func(string) error { launches++; running = true; return nil }
	s := &switchFailureAdapter{Adapter: a.Adapters[adapter.Codex]}
	a.Adapters[adapter.Codex] = s
	return a, s, &running, &quits, &launches
}

func TestSwitchRejectsBusyBeforeQuittingDesktop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kill    bool
		cliOnly bool
		listErr error
	}{
		{name: "CLI running"},
		{name: "CLI generating", kill: true},
		{name: "CLI only generating", kill: true, cliOnly: true},
		{name: "process check failed", listErr: errors.New("ps unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s, running, quits, launches := setupDesktopSwitch(t)
			s.holders = adapter.Holders{Manual: []adapter.Proc{{PID: 7, Command: "/opt/homebrew/bin/codex"}}}
			s.listErr = tc.listErr
			path := filepath.Join(a.UserHome, ".codex", "auth.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Switch(adapter.Codex, "acc-a", adapter.RestoreOpts{CLIOnly: tc.cliOnly}, tc.kill); err == nil {
				t.Error("switch must fail while CLI is busy or its state is unknown")
			}
			if !*running || *quits != 0 || *launches != 0 || s.killed {
				t.Errorf("blocked switch changed processes: running=%v quits=%d launches=%d killed=%v", *running, *quits, *launches, s.killed)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("blocked switch changed credentials")
			}
		})
	}
}

func TestSwitchFailureRelaunchesDesktop(t *testing.T) {
	for _, launchFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "reopened", true: "reopen failed"}[launchFails], func(t *testing.T) {
			a, s, running, quits, launches := setupDesktopSwitch(t)
			restoreErr := errors.New("restore failed")
			launchErr := errors.New("launch failed")
			s.restoreErr = restoreErr
			if launchFails {
				a.Host.LaunchFn = func(string) error { *launches++; return launchErr }
			}
			err := a.Switch(adapter.Codex, "acc-a", adapter.RestoreOpts{}, false)
			if !errors.Is(err, restoreErr) {
				t.Errorf("lost restore error: %v", err)
			}
			if launchFails && !errors.Is(err, launchErr) {
				t.Errorf("lost relaunch error: %v", err)
			}
			if *quits != 1 || *launches != 1 || *running == launchFails {
				t.Errorf("desktop recovery: running=%v quits=%d launches=%d", *running, *quits, *launches)
			}
			p, err := a.State.GetPointer("codex")
			if err != nil {
				t.Fatal(err)
			}
			if p.StableID != "acc-b" {
				t.Errorf("failed restore changed pointer: %s", p.StableID)
			}
		})
	}
}
