package app

import (
	"os"
	"path/filepath"

	"qswitch/internal/adapter"
	"qswitch/internal/config"
)

// Config contains only values, so callers can keep this copy across I/O without
// holding the lock used by writeback recovery to disable a tool.
func (a *App) cfgSnapshot() config.Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Cfg
}

// ReloadConfig leaves the current settings intact when the file is unreadable
// or malformed. Missing runtime configuration must not re-enable defaults.
func (a *App) ReloadConfig() error {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	path := filepath.Join(a.DataDir, "config.toml")
	if _, err := os.Stat(path); err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	a.Cfg = cfg
	return nil
}

func (a *App) disableTool(tool adapter.Tool) error {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if err := a.Cfg.SetToolEnabled(string(tool), false); err != nil {
		return err
	}
	return a.Cfg.Save(filepath.Join(a.DataDir, "config.toml"))
}

func (a *App) sameLastFP(tool adapter.Tool, fp [32]byte) bool {
	a.fpMu.Lock()
	defer a.fpMu.Unlock()
	prev, ok := a.lastFP[tool]
	return ok && prev == fp
}

func (a *App) setLastFP(tool adapter.Tool, fp [32]byte) {
	a.fpMu.Lock()
	defer a.fpMu.Unlock()
	if a.lastFP == nil {
		a.lastFP = make(map[adapter.Tool][32]byte)
	}
	a.lastFP[tool] = fp
}
