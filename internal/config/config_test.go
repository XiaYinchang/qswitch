package config

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestIntervalBounds(t *testing.T) {
	c := Default()
	if c.IntervalMin() != 10*time.Minute {
		t.Fatalf("min %s", c.IntervalMin())
	}
	if c.IntervalMax() != 2*time.Hour {
		t.Fatalf("max %s", c.IntervalMax())
	}
	if c.ETAChecks() != 6 {
		t.Fatalf("eta %v", c.ETAChecks())
	}
	if !c.Web.Enabled || c.WebAddr() != DefaultWebAddr {
		t.Fatalf("web %+v", c.Web)
	}
}

func TestSaveConcurrentReadersSeeCompleteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.General.AutoSwitch = false
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			cfg.General.AutoSwitch = !cfg.General.AutoSwitch
			cfg.Codex.Enabled = !cfg.General.AutoSwitch
			if err := cfg.Save(path); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var readErr error
	for i := 0; i < 100; i++ {
		got, err := Load(path)
		if err != nil {
			readErr = err
			break
		}
		if got.General.AutoSwitch == got.Codex.Enabled {
			readErr = fmt.Errorf("reader saw a partial config")
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
}
