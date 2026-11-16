package app

import (
	"strings"
	"testing"

	"qswitch/internal/state"
)

func TestAntigravityQuotaSummaryUnknownDoesNotInventZero(t *testing.T) {
	for _, detail := range []string{"", "invalid", `[{"id":"gemini_5h"}]`} {
		text := AntigravityQuotaSummary(state.Account{Tool: "antigravity", LastQuotaClass: "unknown", QuotaDetail: detail})
		if strings.Count(text, "=用量未知") != 4 || strings.Contains(text, "已用 0%") || !strings.Contains(text, "当前额度未知，不自动切换") {
			t.Fatalf("missing usage rendered as zero: %s", text)
		}
	}
}

func TestAntigravityQuotaSummaryTwoGroupPolicy(t *testing.T) {
	ac := state.Account{Tool: "antigravity", LastQuotaClass: "ok", QuotaDetail: `[
		{"id":"gemini_5h","used_pct":100},{"id":"gemini_weekly","used_pct":20},
		{"id":"third_party_5h","used_pct":99.99},{"id":"third_party_weekly","used_pct":30}]`}
	text := AntigravityQuotaSummary(ac)
	for _, want := range []string{"Gemini 5 小时=已用 100%", "Gemini 周额度=已用 20%", "第三方 5 小时=已用 99%", "第三方周额度=已用 30%", "单组额度耗尽，保留当前账号"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	ac.QuotaDetail = strings.Replace(ac.QuotaDetail, "99.99", "100", 1)
	ac.LastQuotaClass = "exhausted"
	if text := AntigravityQuotaSummary(ac); !strings.Contains(text, "两组额度均耗尽，自动切换需等待会话空闲") {
		t.Fatalf("both exhausted: %s", text)
	}
	ac.HTTPBackoffUntil = 100
	if text := AntigravityQuotaSummary(ac); !strings.Contains(text, "当前额度未知") || strings.Contains(text, "两组额度均耗尽") {
		t.Fatalf("failed probe kept a current exhaustion claim: %s", text)
	}
}
