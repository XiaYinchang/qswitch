package quota

import "testing"

func TestClassifyHTTP(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Class
	}{
		{"missing fields", 200, `{"hello":1}`, Unknown},
		{"429 probe", 429, `{}`, Unknown},
		{"500", 500, `{}`, Unknown},
		{"401", 401, `{}`, Expired},
		{"limit reached", 200, `{"rate_limit":{"limit_reached":true,"primary_window":{"used_percent":10}}}`, Exhausted},
		{"99.5", 200, `{"rate_limit":{"primary_window":{"used_percent":99.5}}}`, Exhausted},
		{"99.4 soft", 200, `{"rate_limit":{"primary_window":{"used_percent":99.4}}}`, Soft},
		{"ok", 200, `{"rate_limit":{"primary_window":{"used_percent":41}}}`, OK},
		{"grok missing percent", 200, `{"config":{"currentPeriod":{"end":"x"}}}`, Unknown},
		{"grok percent", 200, `{"config":{"creditUsagePercent":12}}`, OK},
		{"cursor no limit remaining", 200, `{"planUsage":{"remaining":0}}`, Unknown},
		{"cursor remaining+limit", 200, `{"planUsage":{"remaining":0,"limit":20}}`, Exhausted},
		{"cursor pct", 200, `{"planUsage":{"totalPercentUsed":50,"limit":20}}`, OK},
		{"non json", 200, `not-json`, Unknown},
		{"402 generic unknown", 402, `{"message":"payment"}`, Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyHTTP(tc.status, []byte(tc.body))
			if got.Class != tc.want {
				t.Fatalf("got %s want %s", got.Class, tc.want)
			}
		})
	}
}

func TestClassifyByKind(t *testing.T) {
	cases := []struct {
		name   string
		kind   Kind
		status int
		body   string
		want   Class
		pct    float64
	}{
		{"codex probe 429", KindCodex, 429, `{}`, Unknown, 0},
		{"codex usage_limit 429", KindCodex, 429, `{"error":{"code":"usage_limit_reached"}}`, Exhausted, 100},
		{"codex tpm 429", KindCodex, 429, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`, Unknown, 0},
		{"codex jsonl-like 200", KindCodex, 200, `{"rate_limits":{"primary":{"used_percent":16}}}`, OK, 16},
		{"codex reached type", KindCodex, 200, `{"rate_limits":{"primary":{"used_percent":40},"rate_limit_reached_type":"rate_limit_reached"}}`, Exhausted, 40},
		{"grok 402 exhausted", KindGrok, 402, `{"message":"API error (status 402 Payment Required): Grok Build usage balance exhausted"}`, Exhausted, 100},
		{"grok 402 empty", KindGrok, 402, ``, Exhausted, 100},
		{"grok x402 not quota", KindGrok, 402, `{"x402Version":1,"accepts":[{"scheme":"exact"}]}`, Unknown, 0},
		{"grok 429 not quota", KindGrok, 429, `{"error":"too many requests"}`, Unknown, 0},
		{"grok credits 2%", KindGrok, 200, `{"config":{"creditUsagePercent":2.0,"currentPeriod":{"end":"2026-08-30T14:04:34.632695+00:00"}}}`, OK, 2},
		{"grok 100% no prepaid", KindGrok, 200, `{"config":{"creditUsagePercent":100,"prepaidBalance":{"val":0},"onDemandCap":{"val":0}}}`, Exhausted, 100},
		{"grok 100% prepaid left", KindGrok, 200, `{"config":{"creditUsagePercent":100,"prepaidBalance":{"val":500}}}`, Soft, 100},
		{"grok period only", KindGrok, 200, `{"config":{"currentPeriod":{"end":"2026-08-30T14:04:34Z"}}}`, Unknown, 0},
		{"cursor remaining 0", KindCursor, 200, `{"planUsage":{"remaining":0,"limit":20}}`, Exhausted, 100},
		{"cursor on-demand still", KindCursor, 200, `{"planUsage":{"remaining":0,"limit":20,"totalPercentUsed":100},"spendLimitUsage":{"individualLimit":1000,"individualUsed":10,"individualRemaining":990}}`, Soft, 100},
		{"cursor spend hit", KindCursor, 200, `{"planUsage":{"remaining":0,"limit":20},"spendLimitUsage":{"individualLimit":1000,"individualUsed":1000,"individualRemaining":0}}`, Exhausted, 100},
		{"cursor high load", KindCursor, 503, `{"error":"ERROR_RESOURCE_EXHAUSTED","details":{"title":"High Load"}}`, Unknown, 0},
		{"cursor resource_exhausted 200", KindCursor, 200, `{"error":"resource_exhausted","detail":"high demand"}`, Unknown, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.kind, tc.status, []byte(tc.body))
			if got.Class != tc.want {
				t.Fatalf("class got %s want %s", got.Class, tc.want)
			}
			if tc.pct != 0 && got.UsedPct != tc.pct {
				t.Fatalf("pct got %v want %v", got.UsedPct, tc.pct)
			}
		})
	}
}

func TestGrokResetISO(t *testing.T) {
	got := Classify(KindGrok, 200, []byte(`{"config":{"creditUsagePercent":2.0,"currentPeriod":{"end":"2026-08-30T14:04:34Z"}}}`))
	if got.ResetsAt == 0 {
		t.Fatal("expected ISO period end as resets_at")
	}
}

func TestClassifyCodexPairsPlanWindow(t *testing.T) {
	body := `{
		"rate_limit": {
			"limit_reached": false,
			"primary_window": {"used_percent": 52, "reset_after_seconds": 326844, "reset_at": 1789435634},
			"secondary_window": null
		},
		"additional_rate_limits": [{
			"rate_limit": {
				"primary_window": {"used_percent": 0, "reset_at": 1789126790},
				"secondary_window": {"used_percent": 90, "reset_at": 1789713590}
			}
		}]
	}`
	got := Classify(KindCodex, 200, []byte(body))
	if got.Class != OK || got.UsedPct != 52 {
		t.Fatalf("class=%s pct=%v", got.Class, got.UsedPct)
	}
	if got.ResetsAt != 1789435634 {
		t.Fatalf("resets %d, mixed additional window", got.ResetsAt)
	}
}

func TestClassifyCodexHotterPlanWindow(t *testing.T) {
	body := `{"rate_limits":{"primary":{"used_percent":20,"resets_at":1000000001},"secondary":{"used_percent":80,"resets_at":1000000002}}}`
	got := Classify(KindCodex, 200, []byte(body))
	if got.Class != OK || got.UsedPct != 80 {
		t.Fatalf("class=%s pct=%v", got.Class, got.UsedPct)
	}
	if got.ResetsAt != 1000000002 {
		t.Fatalf("resets %d want secondary", got.ResetsAt)
	}
}
