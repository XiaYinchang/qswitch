package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"qswitch/internal/adapter"
)

func TestReloadConfigPreservesLastValidSettings(t *testing.T) {
	a, _ := setup(t)
	cfg := a.cfgSnapshot()
	cfg.General.AutoSwitch = false
	path := filepath.Join(a.DataDir, "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if a.cfgSnapshot().General.AutoSwitch {
		t.Fatal("updated auto setting was not loaded")
	}
	if err := os.WriteFile(path, []byte("[invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err == nil {
		t.Fatal("malformed config was accepted")
	}
	if a.cfgSnapshot().General.AutoSwitch {
		t.Fatal("malformed config replaced last valid settings")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err == nil {
		t.Fatal("missing config was accepted")
	}
	if a.cfgSnapshot().General.AutoSwitch {
		t.Fatal("missing config re-enabled auto switching")
	}
}

type blockedFingerprintAdapter struct {
	adapter.Adapter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockedFingerprintAdapter) Fingerprint(home string) ([32]byte, error) {
	a.once.Do(func() { close(a.entered) })
	<-a.release
	return a.Adapter.Fingerprint(home)
}

func TestDaemonReloadsConfigWhileOperationBlocked(t *testing.T) {
	a, _ := setup(t)
	blocked := &blockedFingerprintAdapter{Adapter: a.Adapters[adapter.Codex], entered: make(chan struct{}), release: make(chan struct{})}
	a.Adapters[adapter.Codex] = blocked
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.RunDaemon(ctx) }()
	defer func() {
		close(blocked.release)
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-blocked.entered:
	case <-time.After(4 * time.Second):
		t.Fatal("daemon did not start ingest")
	}
	cfg := a.cfgSnapshot()
	cfg.General.AutoSwitch = false
	if err := cfg.Save(filepath.Join(a.DataDir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for a.cfgSnapshot().General.AutoSwitch {
		select {
		case <-deadline:
			t.Fatal("auto-off did not load while another operation was blocked")
		case <-ticker.C:
		}
	}
}

func TestOverviewConcurrentConfigChanges(t *testing.T) {
	a, _ := setup(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			if err := a.disableTool(adapter.Codex); err != nil {
				t.Error(err)
				return
			}
			if err := a.ReloadConfig(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 30; i++ {
		if len(a.Overview().Tools) != len(adapter.AllTools()) {
			t.Error("overview lost tools during config changes")
		}
	}
	wg.Wait()
	if a.cfgSnapshot().Codex.Enabled {
		t.Fatal("tool disable was lost")
	}
}

func TestConcurrentFingerprints(t *testing.T) {
	a, _ := setup(t)
	writeCursor(t, a.UserHome, "cursor-fixture", "fixture@example.test", "fake-token")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Codex, adapter.Cursor, adapter.Cursor} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 30; i++ {
				a.rememberFP(tool)
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Cursor} {
		fp, err := a.liveFingerprint(tool)
		if err != nil || !a.sameLastFP(tool, fp) {
			t.Errorf("%s fingerprint was not remembered", tool)
		}
	}
}
