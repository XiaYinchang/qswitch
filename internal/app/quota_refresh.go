package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"qswitch/internal/adapter"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
)

var ErrQuotaRefreshBusy = errors.New("该工具正在刷新额度，请稍后重试")

type QuotaRefreshSummary struct {
	Tool      adapter.Tool `json:"tool"`
	Attempted int          `json:"attempted"`
	Updated   int          `json:"updated"`
	Failed    int          `json:"failed"`
	Skipped   int          `json:"skipped"`
}

// QuotaResultUpdated excludes cached quota and authentication-only results.
func QuotaResultUpdated(result quota.Result) bool {
	if result.Class != quota.OK && result.Class != quota.Soft && result.Class != quota.Exhausted {
		return false
	}
	switch result.Source {
	case "http", "http_402", "http_usage_limit":
		return true
	default:
		return false
	}
}

// RefreshToolQuota observes only already captured accounts in the requested
// scope. It never runs the scheduler, captures accounts, or considers a switch.
// Credential renewal inside the ordinary account probe remains unchanged.
func (a *App) RefreshToolQuota(ctx context.Context, tool adapter.Tool) (QuotaRefreshSummary, error) {
	summary := QuotaRefreshSummary{Tool: tool}
	if err := a.CheckAction(tool, "probe"); err != nil {
		return summary, err
	}
	refreshLock, err := livefile.TryAcquire(filepath.Join(a.DataDir, "locks", string(tool)+"-quota-refresh.lock"))
	if errors.Is(err, livefile.ErrLocked) {
		return summary, ErrQuotaRefreshBusy
	}
	if err != nil {
		return summary, err
	}
	defer refreshLock.Close()
	accounts, err := a.State.ListAccounts(string(tool))
	if err != nil {
		return summary, err
	}
	for i, account := range accounts {
		if ctx.Err() != nil {
			summary.Skipped += len(accounts) - i
			break
		}
		if account.StableID == "" || strings.HasPrefix(account.StableID, "desktop:") {
			summary.Skipped++
			continue
		}
		lock, err := livefile.AcquireContext(ctx, filepath.Join(a.DataDir, "locks", string(tool)+".lock"))
		if err != nil {
			summary.Skipped += len(accounts) - i
			if ctx.Err() != nil {
				break
			}
			return summary, err
		}
		// Reuse ProbeAccount's implementation while holding its same tool lock;
		// acquiring it here makes this explicit refresh cancellable during waits.
		result := a.probeAccountLocked(ctx, tool, account.StableID, false)
		_ = lock.Close()
		if result.Source == "cached" || result.Source == "backoff" || result.Source == "refresh_backoff" {
			summary.Skipped++
			continue
		}
		summary.Attempted++
		if QuotaResultUpdated(result) {
			summary.Updated++
		} else {
			summary.Failed++
		}
	}
	return summary, nil
}
