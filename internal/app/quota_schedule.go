package app

import (
	"time"

	"qswitch/internal/config"
	"qswitch/internal/state"
)

// Reuse the previous failed attempt's deadline and time instead of maintaining
// another retry counter. A successful quota request clears this deadline.
func quotaRetryDelay(ac state.Account, serverDelay time.Duration) time.Duration {
	delay := config.QuotaRetryMin
	if ac.LastHTTPAt > 0 && ac.HTTPBackoffUntil > ac.LastHTTPAt {
		seconds := ac.HTTPBackoffUntil - ac.LastHTTPAt
		if seconds >= int64((config.QuotaRetryMax / 2).Seconds()) {
			delay = config.QuotaRetryMax
		} else if seconds >= int64(delay.Seconds()) {
			delay = 2 * time.Duration(seconds) * time.Second
		}
	}
	if serverDelay > delay {
		delay = serverDelay
	}
	return delay
}

func (a *App) refreshRetryDelay(permanent bool) time.Duration {
	if permanent {
		return a.cfgSnapshot().Backoff()
	}
	return config.QuotaRetryMin
}

func (a *App) idleQuotaDue(ac state.Account) bool {
	now := a.now().Unix()
	if ac.HTTPBackoffUntil > now || ac.RefreshBackoffUntil > now {
		return false
	}
	if ac.HTTPBackoffUntil > 0 { // A failed quota request is ready to retry.
		return true
	}
	return ac.LastHTTPAt <= 0 || now-ac.LastHTTPAt >= int64(config.IdleQuotaInterval.Seconds())
}
