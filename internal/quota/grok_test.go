package quota

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

const grokWeeklyPeriod = `{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-28T06:50:18.123456+00:00","end":"2026-10-05T06:50:18.123456+00:00"}`
const grokMonthlyPeriod = `{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-10-01T00:00:00Z","end":"2026-11-01T00:00:00Z"}`

func TestGrokCreditsConfig(t *testing.T) {
	weeklyEnd, _ := time.Parse(time.RFC3339Nano, "2026-10-05T06:50:18.123456+00:00")
	monthlyEnd, _ := time.Parse(time.RFC3339, "2026-11-01T00:00:00Z")
	for _, tc := range []struct {
		name, body string
		class      Class
		pct        float64
		reset      int64
		bucket     string
	}{
		{"omitted zero in unified weekly fixture", `{"config":{"currentPeriod":` + grokWeeklyPeriod + `,"prepaidBalance":{},"onDemandCap":{},"onDemandUsed":{},"isUnifiedBillingUser":true}}`, OK, 0, weeklyEnd.Unix(), "weekly"},
		{"omitted zero in unified monthly", `{"config":{"currentPeriod":` + grokMonthlyPeriod + `,"isUnifiedBillingUser":true}}`, OK, 0, monthlyEnd.Unix(), "monthly"},
		{"one percent response", `{"config":{"creditUsagePercent":1,"currentPeriod":` + grokWeeklyPeriod + `,"prepaidBalance":{},"onDemandCap":{},"onDemandUsed":{},"isUnifiedBillingUser":true}}`, OK, 1, weeklyEnd.Unix(), "weekly"},
		{"explicit zero", `{"config":{"creditUsagePercent":0}}`, OK, 0, 0, ""},
		{"explicit percent wins legacy and on-demand ratio", `{"config":{"creditUsagePercent":1,"monthlyLimit":{"val":100},"used":{"val":99},"onDemandCap":{"val":100},"onDemandUsed":{"val":100}}}`, OK, 1, 0, ""},
		{"history and products cannot override current pool", `{"config":{"creditUsagePercent":1,"currentPeriod":` + grokWeeklyPeriod + `,"history":[{"creditUsagePercent":100,"end":"2030-01-01T00:00:00Z","message":"usage balance exhausted"}],"productUsage":[{"usagePercent":99,"creditUsagePercent":99}],"billingPeriodEnd":"2031-01-01T00:00:00Z"}}`, OK, 1, weeklyEnd.Unix(), "weekly"},
		{"historical prepaid cannot make current pool available", `{"config":{"creditUsagePercent":100,"history":[{"prepaidBalance":{"val":500}}]}}`, Exhausted, 100, 0, ""},
		{"99.9 is not exhausted", `{"config":{"creditUsagePercent":99.9}}`, Soft, 99.9, 0, ""},
		{"100 exhausted", `{"config":{"creditUsagePercent":100}}`, Exhausted, 100, 0, ""},
		{"above 100 clamped", `{"config":{"creditUsagePercent":101}}`, Exhausted, 100, 0, ""},
		{"negative percent clamped", `{"config":{"creditUsagePercent":-1}}`, OK, 0, 0, ""},
		{"negative prepaid available", `{"config":{"creditUsagePercent":100,"prepaidBalance":{"val":-500}}}`, Soft, 100, 0, ""},
		{"positive prepaid available", `{"config":{"creditUsagePercent":100,"prepaidBalance":{"val":500}}}`, Soft, 100, 0, ""},
		{"zero prepaid exhausted", `{"config":{"creditUsagePercent":100,"prepaidBalance":{}}}`, Exhausted, 100, 0, ""},
		{"on-demand available", `{"config":{"creditUsagePercent":100,"onDemandCap":{"val":500},"onDemandUsed":{"val":499}}}`, Soft, 100, 0, ""},
		{"on-demand zero omitted", `{"config":{"creditUsagePercent":100,"onDemandCap":{"val":"500"},"onDemandUsed":{}}}`, Soft, 100, 0, ""},
		{"on-demand exhausted", `{"config":{"creditUsagePercent":100,"onDemandCap":{"val":500},"onDemandUsed":{"val":500}}}`, Exhausted, 100, 0, ""},
		{"legacy ratio", `{"config":{"monthlyLimit":{"val":"2000"},"used":{"val":"500"},"billingPeriodEnd":"2026-11-01T00:00:00Z"}}`, OK, 25, monthlyEnd.Unix(), ""},
		{"legacy omitted used", `{"config":{"monthlyLimit":{"val":2000}}}`, OK, 0, 0, ""},
		{"legacy omitted zero cents", `{"config":{"monthlyLimit":{"val":2000},"used":{}}}`, OK, 0, 0, ""},
		{"known percent incomplete period", `{"config":{"creditUsagePercent":1,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-05T06:50:18.123456Z"}}}`, OK, 1, weeklyEnd.Unix(), ""},
		{"invalid current end falls back to legacy end", `{"config":{"creditUsagePercent":1,"currentPeriod":{"end":"invalid"},"billingPeriodEnd":"2026-11-01T00:00:00Z"}}`, OK, 1, monthlyEnd.Unix(), ""},
		{"missing config", `{}`, Unknown, 0, 0, ""},
		{"null config", `{"config":null}`, Unknown, 0, 0, ""},
		{"empty config", `{"config":{}}`, Unknown, 0, 0, ""},
		{"wrong config type", `{"config":[]}`, Unknown, 0, 0, ""},
		{"wrong percent string", `{"config":{"creditUsagePercent":"1","isUnifiedBillingUser":true,"currentPeriod":` + grokWeeklyPeriod + `}}`, Unknown, 0, 0, ""},
		{"wrong percent null", `{"config":{"creditUsagePercent":null,"isUnifiedBillingUser":true,"currentPeriod":` + grokWeeklyPeriod + `}}`, Unknown, 0, 0, ""},
		{"wrong percent object", `{"config":{"creditUsagePercent":{"val":1}}}`, Unknown, 0, 0, ""},
		{"invalid prepaid type", `{"config":{"creditUsagePercent":100,"prepaidBalance":"500"}}`, Unknown, 0, 0, ""},
		{"invalid legacy used type", `{"config":{"monthlyLimit":{"val":2000},"used":"500"}}`, Unknown, 0, 0, ""},
		{"zero legacy limit", `{"config":{"monthlyLimit":{},"used":{}}}`, Unknown, 0, 0, ""},
		{"period without unified flag", `{"config":{"currentPeriod":` + grokWeeklyPeriod + `}}`, Unknown, 0, 0, ""},
		{"false unified flag", `{"config":{"isUnifiedBillingUser":false,"currentPeriod":` + grokWeeklyPeriod + `}}`, Unknown, 0, 0, ""},
		{"wrong unified flag type", `{"config":{"isUnifiedBillingUser":"true","currentPeriod":` + grokWeeklyPeriod + `}}`, Unknown, 0, 0, ""},
		{"wrong period type", `{"config":{"isUnifiedBillingUser":true,"currentPeriod":"weekly"}}`, Unknown, 0, 0, ""},
		{"unsupported period", `{"config":{"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_DAILY","start":"2026-10-01T00:00:00Z","end":"2026-10-02T00:00:00Z"}}}`, Unknown, 0, 0, ""},
		{"period missing start", `{"config":{"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-05T00:00:00Z"}}}`, Unknown, 0, 0, ""},
		{"period invalid start", `{"config":{"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"invalid","end":"2026-10-05T00:00:00Z"}}}`, Unknown, 0, 0, ""},
		{"period backwards", `{"config":{"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-06T00:00:00Z","end":"2026-10-05T00:00:00Z"}}}`, Unknown, 0, 0, ""},
		{"period zero duration", `{"config":{"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-05T00:00:00Z","end":"2026-10-05T00:00:00Z"}}}`, Unknown, 0, 0, ""},
		{"history-only percent ignored", `{"config":{"history":[{"creditUsagePercent":100}]}}`, Unknown, 0, 0, ""},
		{"product-only percent ignored", `{"config":{"productUsage":[{"product":"GrokBuild","usagePercent":1,"creditUsagePercent":1}]}}`, Unknown, 0, 0, ""},
		{"on-demand-only ratio ignored", `{"config":{"onDemandCap":{"val":500},"onDemandUsed":{"val":500}}}`, Unknown, 0, 0, ""},
		{"error envelope ignored", `{"error":"not available","config":{"creditUsagePercent":1}}`, Unknown, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(KindGrok, 200, []byte(tc.body))
			if got.Class != tc.class || math.Abs(got.UsedPct-tc.pct) > 1e-9 || got.ResetsAt != tc.reset {
				t.Fatalf("got %+v, want class=%s pct=%g reset=%d", got, tc.class, tc.pct, tc.reset)
			}
			if tc.bucket == "" {
				if len(got.Buckets) != 0 {
					t.Fatalf("unexpected buckets: %+v", got.Buckets)
				}
			} else if len(got.Buckets) != 1 || got.Buckets[0].ID != tc.bucket || got.Buckets[0].UsedPct != tc.pct || got.Buckets[0].ResetsAt != tc.reset {
				t.Fatalf("bucket does not match current pool: %+v", got.Buckets)
			}
		})
	}
}

func TestGrokLocalBillingUsesCreditsConfig(t *testing.T) {
	for _, pct := range []string{"", `"creditUsagePercent":1,`} {
		body := `{"config":{` + pct + `"currentPeriod":` + grokWeeklyPeriod + `,"isUnifiedBillingUser":true}}`
		var ctx map[string]any
		if err := json.Unmarshal([]byte(body), &ctx); err != nil {
			t.Fatal(err)
		}
		ctx["subscriptionTier"] = "SuperGrok Heavy"
		line, err := json.Marshal(map[string]any{"msg": "billing: fetched credits config", "ctx": ctx, "timestamp": "2026-10-02T08:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := classifyGrokLine(string(line))
		want := Classify(KindGrok, 200, []byte(body))
		if !ok || got.Class != OK || got.UsedPct != want.UsedPct || got.ResetsAt != want.ResetsAt || len(got.Buckets) != 1 || got.Buckets[0] != want.Buckets[0] || got.Plan != "SuperGrok Heavy" || !got.Timestamped {
			t.Fatalf("HTTP/local disagree or metadata lost: local=%+v ok=%v HTTP=%+v", got, ok, want)
		}
	}
	if got, ok := classifyGrokLine(`{"msg":"billing: fetched credits config","ctx":{"history":[{"config":{"creditUsagePercent":100}}]}}`); ok {
		t.Fatalf("historical config must not become current quota: %+v", got)
	}
}
