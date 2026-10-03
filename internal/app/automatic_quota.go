package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

// cachedAutomaticQuota keeps historical display data separate from evidence
// that can authorize an automatic switch. The latest attempt may be unknown
// even while the account snapshot intentionally retains an earlier OK value.
func (a *App) cachedAutomaticQuota(tool adapter.Tool, ac state.Account) (quota.Result, bool, error) {
	unknown := quota.Result{Class: quota.Unknown}
	now := a.now().Unix()
	if ac.Incomplete || ac.StaleCLI || ac.HTTPBackoffUntil > now {
		return unknown, false, nil
	}
	last, err := a.State.LatestQuota(string(tool), ac.StableID)
	if errors.Is(err, sql.ErrNoRows) {
		return unknown, false, nil
	}
	if err != nil {
		return unknown, false, err
	}
	if last.Ts <= 0 || last.Ts > now || last.Class != ac.LastQuotaClass || last.UsedPct != ac.LastUsedPct {
		return unknown, false, nil
	}
	class := quota.Class(last.Class)
	var interval time.Duration
	switch class {
	case quota.OK, quota.Soft:
		interval = a.quotaInterval(string(tool), ac.StableID)
	case quota.Exhausted:
		if tool == adapter.Codex {
			interval = a.codexRecoverInterval(ac)
		} else if ac.LastResetsAt > now {
			// Non-Codex recovery already waits for the advertised reset.
			return quota.Result{Class: class, UsedPct: ac.LastUsedPct, ResetsAt: ac.LastResetsAt}, true, nil
		} else {
			interval = a.cfgSnapshot().IntervalMin()
		}
	default:
		return unknown, false, nil
	}
	// An observation from before a window boundary cannot confirm the new
	// window. A new observation taken after that boundary remains usable.
	if ac.LastResetsAt > last.Ts && ac.LastResetsAt <= now {
		return unknown, false, nil
	}
	if now-last.Ts >= int64(interval.Seconds()) {
		return unknown, false, nil
	}
	return quota.Result{Class: class, UsedPct: ac.LastUsedPct, ResetsAt: ac.LastResetsAt}, true, nil
}

// automaticQuotaLocked never bypasses HTTP or refresh backoff. Its caller owns
// the tool lock, so an accepted observation and its credential blob cannot be
// replaced by another qswitch operation before a pending switch is restored.
func (a *App) automaticQuotaLocked(ctx context.Context, tool adapter.Tool, id string) (quota.Result, error) {
	unknown := quota.Result{Class: quota.Unknown}
	if id == "" || strings.HasPrefix(id, "desktop:") {
		return unknown, nil
	}
	ac, err := a.State.GetAccount(string(tool), id)
	if err != nil {
		return unknown, err
	}
	if ac.Incomplete || ac.StaleCLI || ac.HTTPBackoffUntil > a.now().Unix() || quota.Class(ac.LastQuotaClass) == quota.Expired {
		return unknown, nil
	}
	res, fresh, err := a.cachedAutomaticQuota(tool, ac)
	if err != nil || fresh {
		return res, err
	}
	res = a.probeAccountLocked(ctx, tool, id, true)
	if res.Class == quota.Unknown || res.Class == quota.Expired {
		return res, nil
	}
	ac, err = a.State.GetAccount(string(tool), id)
	if err != nil {
		return unknown, err
	}
	res, fresh, err = a.cachedAutomaticQuota(tool, ac)
	if err != nil || fresh {
		return res, err
	}
	return unknown, nil
}
