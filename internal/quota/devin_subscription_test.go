package quota

import "testing"

func TestDevinSubscriptionContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		class      Class
		plan       string
		used       float64
		reset      int64
		buckets    int
	}{
		{name: "empty plan status", body: `{"userStatus":{"planStatus":{}}}`, class: Unknown},
		{name: "acu has no subscription quota", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Enterprise","billingStrategy":"BILLING_STRATEGY_ACU"},"acuConsumed":12,"acuLimit":100}}}`, class: Unknown, plan: "Enterprise"},
		{name: "credits cannot reuse stale quota fields", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Teams","billingStrategy":1},"dailyQuotaRemainingPercent":0,"weeklyQuotaRemainingPercent":0}}}`, class: Unknown, plan: "Teams"},
		{name: "unrecognized strategy", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","billingStrategy":"future"},"weeklyQuotaRemainingPercent":40}}}`, class: Unknown, plan: "Pro"},
		{name: "bad percentage", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","billingStrategy":2},"dailyQuotaRemainingPercent":"invalid","weeklyQuotaRemainingPercent":40}}}`, class: Unknown, plan: "Pro"},
		{name: "out of range percentage", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","billingStrategy":2},"dailyQuotaRemainingPercent":-1,"weeklyQuotaRemainingPercent":40}}}`, class: Unknown, plan: "Pro"},
		{name: "proto omitted zero in known quota", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","billingStrategy":"BILLING_STRATEGY_QUOTA"}}}}`, class: Exhausted, plan: "Pro", used: 100, buckets: 2},
		{name: "max never has daily cap", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Max"},"weeklyQuotaRemainingPercent":80}}}`, class: OK, plan: "Max", used: 20, buckets: 1},
		{name: "nested numeric tier", body: `{"userStatus":{"planStatus":{"planInfo":{"teamsTier":17,"billingStrategy":2},"weeklyQuotaRemainingPercent":80}}}`, class: OK, plan: "Max", used: 20, buckets: 1},
		{name: "weekly bottleneck owns reset", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"},"dailyQuotaRemainingPercent":80,"weeklyQuotaRemainingPercent":0,"dailyQuotaResetAtUnix":1800000100,"weeklyQuotaResetAtUnix":1800000200}}}`, class: Exhausted, plan: "Pro", used: 100, reset: 1800000200, buckets: 2},
		{name: "all exhausted windows must reset", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","billingStrategy":2},"dailyQuotaResetAtUnix":1800000100,"weeklyQuotaResetAtUnix":1800000200}}}`, class: Exhausted, plan: "Pro", used: 100, reset: 1800000200, buckets: 2},
		{name: "do not borrow unrelated reset", body: `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"},"dailyQuotaRemainingPercent":80,"weeklyQuotaRemainingPercent":0,"dailyQuotaResetAtUnix":1800000100}}}`, class: Exhausted, plan: "Pro", used: 100, buckets: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(KindDevin, 200, []byte(tc.body))
			if got.Class != tc.class || got.Plan != tc.plan || got.UsedPct != tc.used || got.ResetsAt != tc.reset || len(got.Buckets) != tc.buckets {
				t.Fatalf("class=%s plan=%s used=%g reset=%d buckets=%d", got.Class, got.Plan, got.UsedPct, got.ResetsAt, len(got.Buckets))
			}
			if !got.Authenticated {
				t.Fatal("valid protected status response lost authentication evidence")
			}
		})
	}
}

func TestDevinIdentityUsesNestedPlanTier(t *testing.T) {
	for _, body := range []string{
		`{"userStatus":{"userId":"fixture","email":"fixture@example.test","planStatus":{"planInfo":{"teamsTier":"TEAMS_TIER_DEVIN_PRO"}}}}`,
		`{"userStatus":{"userId":"fixture","email":"fixture@example.test","planStatus":{"planInfo":{"teamsTier":16}}}}`,
		`{"userId":"fixture","email":"fixture@example.test","planStatus":{"planInfo":{"teamsTier":16}}}`,
	} {
		id, email, plan := DevinIdentity([]byte(body))
		if id != "fixture" || email != "fixture@example.test" || plan != "Pro" {
			t.Fatalf("identity incomplete: id present=%v email present=%v plan=%s", id != "", email != "", plan)
		}
	}
}
