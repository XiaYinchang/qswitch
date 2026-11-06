package app

import (
	"fmt"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/managed"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

// reasonExpiring marks a switch to an account whose remaining quota is about
// to be forfeited, queued while the live account still has quota.
const reasonExpiring = "expiring"

// The user's rule is to use quota strictly by when it expires (11-03). Until
// 11-04 the rule only chose the next account after one was used up: at 18:00
// that day c2bf85c4 still had 10% of its week, due to reset at 03:00, while
// the live account's week ran until 11-08. Now an account whose quota resets
// within expiringWindow, before the live account's, with at least
// expiringMinLeft percent left, is switched to once every managed session is
// idle.
const (
	expiringWindow  = 12 * time.Hour
	expiringMinLeft = 10.0
	// expiringSettle keeps a switch, and the credential write-back that
	// follows it, from being followed at once by another.
	expiringSettle = 10 * time.Minute
)

// expiringQuota returns when the longest open window resets and the percent
// of it left. at is 0 when no window reports a reset.
func expiringQuota(buckets []quota.Bucket, now int64) (at int64, left float64) {
	for _, b := range buckets {
		if b.ResetsAt > now && b.ResetsAt > at {
			at, left = b.ResetsAt, 100-b.UsedPct
		}
	}
	return at, left
}

// expiringTarget is the account, other than live, whose quota a reset
// forfeits first among those that qualify, judged from the last observations.
// It is "" when none does.
func (a *App) expiringTarget(tool adapter.Tool, live state.Account) string {
	now := a.now().Unix()
	liveAt := quotaExpiry(quota.DecodeBuckets(live.QuotaDetail), now)
	accs, err := a.State.ListAccounts(string(tool))
	if err != nil {
		return ""
	}
	best, bestAt := "", int64(0)
	for _, ac := range accs {
		if ac.StableID == live.StableID || ac.Incomplete || ac.StaleCLI || ac.CoolingUntil > now || strings.HasPrefix(ac.StableID, "desktop:") {
			continue
		}
		if c := quota.Class(ac.LastQuotaClass); c != quota.OK && c != quota.Soft {
			continue
		}
		at, left := expiringQuota(quota.DecodeBuckets(ac.QuotaDetail), now)
		if at == 0 || at-now > int64(expiringWindow/time.Second) || left < expiringMinLeft {
			continue
		}
		// The live account's quota is used first when it expires no later;
		// an unknown expiry counts as later, as when the next account is picked.
		if liveAt != 0 && liveAt <= at {
			continue
		}
		if best == "" || at < bestAt {
			best, bestAt = ac.StableID, at
		}
	}
	return best
}

// considerExpiring queues a switch to an account whose quota is about to be
// forfeited while the live account still has quota. Only managed Claude
// sessions take part, and TryApply starts the switch only when every one of
// them is idle.
func (a *App) considerExpiring(tool adapter.Tool) {
	if tool != adapter.Claude {
		return
	}
	cfg := a.cfgSnapshot()
	if !cfg.General.AutoSwitch || !cfg.ToolEnabled(string(tool)) {
		return
	}
	if pend, err := a.State.GetPending(string(tool)); err != nil || pend.ToID != "" {
		return
	}
	p, err := a.State.GetPointer(string(tool))
	if err != nil || p.StableID == "" || p.Desync || strings.HasPrefix(p.StableID, "desktop:") {
		return
	}
	if p.LastApplyAt > 0 && a.now().Sub(time.Unix(p.LastApplyAt, 0)) < expiringSettle {
		return
	}
	live, err := a.State.GetAccount(string(tool), p.StableID)
	if err != nil {
		return
	}
	// A used-up live account is left by considerSwitch, which picks the next
	// account by expiry too.
	if c := quota.Class(live.LastQuotaClass); c != quota.OK && c != quota.Soft {
		return
	}
	next := a.expiringTarget(tool, live)
	if next == "" {
		return
	}
	pend := state.Pending{Tool: string(tool), FromID: p.StableID, ToID: next, QueuedAt: a.now().Unix(), Reason: reasonExpiring}
	if err := a.State.SetPending(pend); err != nil {
		return
	}
	a.logSwitch(tool, pendingKey(pend)+" queued", "%s%s, before %s; switching to it once every session is idle", a.accountName(tool, next), a.leftNote(tool, next), a.accountName(tool, p.StableID))
}

// expiringStillDue reports why a queued expiring switch no longer makes
// sense, or "" when it still does.
func (a *App) expiringStillDue(tool adapter.Tool, pend state.Pending) (string, error) {
	cfg := a.cfgSnapshot()
	if !cfg.General.AutoSwitch || !cfg.ToolEnabled(string(tool)) {
		return "automatic switching is off", nil
	}
	p, err := a.State.GetPointer(string(tool))
	if err != nil {
		return "", err
	}
	if p.StableID != pend.FromID {
		return "the live account changed", nil
	}
	live, err := a.State.GetAccount(string(tool), pend.FromID)
	if err != nil {
		return "", err
	}
	if a.expiringTarget(tool, live) != pend.ToID {
		return "the next account's quota is no longer about to expire unused", nil
	}
	return "", nil
}

// busySessions lists the managed sessions that are not idle.
func busySessions(dataDir string) ([]managed.Waiting, error) {
	sessions, err := managed.Status(dataDir)
	if err != nil {
		return nil, err
	}
	var busy []managed.Waiting
	for _, s := range sessions {
		if s.Live && !s.Exited && !s.Ready {
			busy = append(busy, managed.Waiting{SessionID: s.SessionID, Reason: s.Reason})
		}
	}
	return busy, nil
}

func anyExiting(waiting []managed.Waiting) bool {
	for _, w := range waiting {
		if managed.Exiting(w.Reason) {
			return true
		}
	}
	return false
}

// leftNote tells how much of an account's longest window is left and when it
// resets.
func (a *App) leftNote(tool adapter.Tool, id string) string {
	ac, err := a.State.GetAccount(string(tool), id)
	if err != nil {
		return ""
	}
	at, left := expiringQuota(quota.DecodeBuckets(ac.QuotaDetail), a.now().Unix())
	if at == 0 {
		return ""
	}
	return fmt.Sprintf(" has %.0f%% of its quota left until %s", left, time.Unix(at, 0).Format("01-02 15:04"))
}
