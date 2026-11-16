package app

import (
	"errors"
	"os"
	"path/filepath"

	"qswitch/internal/adapter"
	"qswitch/internal/config"
	"qswitch/internal/livefile"
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
	return a.updateConfig(func(cfg *config.Config) error { return cfg.SetToolEnabled(string(tool), false) })
}

// Reload while holding the shared lock: an independent CLI must not overwrite
// another process's tool settings using its stale in-memory Config.
func (a *App) updateConfig(change func(*config.Config) error) error {
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "config.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(a.DataDir, "config.toml")
	if _, err := os.Stat(path); err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := change(&cfg); err != nil {
		return err
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	a.cfgMu.Lock()
	a.Cfg = cfg
	a.cfgMu.Unlock()
	return nil
}

func (a *App) SetAuto(tool adapter.Tool, enabled bool) error {
	if enabled && tool == adapter.Cursor {
		if p, err := a.State.GetPointer(string(tool)); err == nil && p.Desync {
			return errors.New("cursor accounts are desynchronized; align the account before enabling automatic switching")
		}
	}
	if tool != "" {
		if err := a.CheckScope(tool); err != nil {
			return err
		}
		if enabled {
			if err := a.CheckAction(tool, "auto_switch"); err != nil {
				return err
			}
		}
	}
	return a.updateConfig(func(cfg *config.Config) error {
		if tool == "" {
			cfg.General.AutoSwitch = enabled
			return nil
		}
		return cfg.SetToolEnabled(string(tool), enabled)
	})
}

func (a *App) setCaptureError(tool adapter.Tool, err error) {
	a.fpMu.Lock()
	defer a.fpMu.Unlock()
	if a.captureErrors == nil {
		a.captureErrors = map[adapter.Tool]string{}
	}
	if err == nil || os.IsNotExist(err) {
		delete(a.captureErrors, tool)
	} else {
		a.captureErrors[tool] = err.Error()
	}
}

func (a *App) captureStatus(tool adapter.Tool) string {
	a.fpMu.Lock()
	defer a.fpMu.Unlock()
	return a.captureErrors[tool]
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

func (a *App) WebToken() string { return a.cfgSnapshot().Web.Token }

// Initial creation uses the same lock as later updates, so a second opener
// cannot overwrite the first process's changes with default settings.
func initialConfig(dataDir string) (config.Config, error) {
	lock, err := livefile.Acquire(filepath.Join(dataDir, "locks", "config.lock"))
	if err != nil {
		return config.Config{}, err
	}
	defer lock.Close()
	path := filepath.Join(dataDir, "config.toml")
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, cfg.Save(path)
	} else if err != nil {
		return cfg, err
	}
	return cfg, nil
}
