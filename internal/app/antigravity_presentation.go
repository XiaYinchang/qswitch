package app

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"qswitch/internal/state"
)

// AntigravityQuotaSummary renders the same four windows as the dashboard.
// Missing values stay unknown, including a missing used_pct field; decoding
// directly into quota.Bucket would turn that missing field into zero usage.
func AntigravityQuotaSummary(ac state.Account) string {
	var rows []struct {
		ID       string   `json:"id"`
		UsedPct  *float64 `json:"used_pct"`
		ResetsAt int64    `json:"resets_at"`
	}
	_ = json.Unmarshal([]byte(ac.QuotaDetail), &rows)
	var parts []string
	complete := true
	groups := [2]float64{}
	for i, window := range []struct{ id, label string }{
		{"gemini_5h", "Gemini 5 小时"}, {"gemini_weekly", "Gemini 周额度"},
		{"third_party_5h", "第三方 5 小时"}, {"third_party_weekly", "第三方周额度"},
	} {
		var used *float64
		var reset int64
		count := 0
		for _, row := range rows {
			if row.ID == window.id {
				used, reset, count = row.UsedPct, row.ResetsAt, count+1
			}
		}
		if count != 1 || used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 || *used > 100 {
			complete = false
			parts = append(parts, window.label+"=用量未知")
			continue
		}
		groups[i/2] = math.Max(groups[i/2], *used)
		text := fmt.Sprintf("%s=已用 %.0f%%", window.label, math.Floor(*used))
		if reset > 0 {
			text += "（重置 " + time.Unix(reset, 0).UTC().Format(time.RFC3339) + "）"
		}
		parts = append(parts, text)
	}
	unknown := !complete || ac.LastQuotaClass == "" || ac.LastQuotaClass == "unknown" || ac.LastQuotaClass == "expired" || ac.HTTPBackoffUntil > ac.LastHTTPAt || ac.RefreshBackoffUntil > 0
	var policy string
	switch {
	case unknown:
		policy = "当前额度未知，不自动切换；已有数值为记录用量"
	case groups[0] >= 100 && groups[1] >= 100:
		policy = "两组额度均耗尽，自动切换需等待会话空闲"
	case groups[0] >= 100 || groups[1] >= 100:
		policy = "单组额度耗尽，保留当前账号"
	default:
		policy = "仅两组额度都耗尽时自动切号"
	}
	return strings.Join(parts, "；") + "；" + policy
}
