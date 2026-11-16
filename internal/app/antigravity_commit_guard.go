package app

import (
	"fmt"
	"os"
	"path/filepath"

	"qswitch/internal/adapter"
	"qswitch/internal/config"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

// antigravityWriteGuard runs after the native adapter's target verification and
// activity RPCs. The caller still holds the tool lock. It performs only local
// reads, never another network refresh that could outlive the checked windows.
func (a *App) antigravityWriteGuard(pending state.Pending, fingerprint [32]byte) func() error {
	return func() error {
		tool := adapter.Antigravity
		reject := func(reason string) error {
			return fmt.Errorf("%w: %s", adapter.ErrRestorePrecondition, reason)
		}
		current, err := a.Adapters[tool].Fingerprint(a.UserHome)
		if err != nil || current != fingerprint {
			return reject("native credentials changed during switch preparation")
		}
		cfg := a.cfgSnapshot()
		if !cfg.General.AutoSwitch || !cfg.ToolEnabled(string(tool)) || a.CheckAction(tool, "auto_switch") != nil {
			return reject("automatic switching is no longer enabled")
		}
		// An independent CLI can persist auto-off before the daemon's next
		// reload. Observe it without replacing or saving in-memory settings.
		path := filepath.Join(a.DataDir, "config.toml")
		if _, err := os.Stat(path); err != nil {
			return reject("persisted automatic-switch settings are unavailable")
		}
		persisted, err := config.Load(path)
		if err != nil || !persisted.General.AutoSwitch || !persisted.ToolEnabled(string(tool)) {
			return reject("persisted settings no longer enable automatic switching")
		}
		pointer, err := a.State.GetPointer(string(tool))
		if err != nil || pointer.StableID != pending.FromID {
			return reject("the current account changed")
		}
		queued, err := a.State.GetPending(string(tool))
		if err != nil || queued.FromID != pending.FromID || queued.ToID != pending.ToID || queued.QueuedAt != pending.QueuedAt || queued.Reason != pending.Reason || pending.Reason != "exhausted" {
			return reject("the queued automatic switch changed")
		}
		source, err := a.State.GetAccount(string(tool), pending.FromID)
		if err != nil {
			return reject("source account state is unavailable")
		}
		known, err := a.knownExhausted(tool, source)
		if err != nil || !known {
			return reject("source quota no longer confirms both groups exhausted")
		}
		target, err := a.State.GetAccount(string(tool), pending.ToID)
		if err != nil || target.CoolingUntil > a.now().Unix() {
			return reject("target account is no longer available")
		}
		result, fresh, err := a.cachedAutomaticQuota(tool, target)
		if err != nil || !fresh || (result.Class != quota.OK && result.Class != quota.Soft) {
			return reject("target quota observation expired or became unavailable")
		}
		return nil
	}
}
