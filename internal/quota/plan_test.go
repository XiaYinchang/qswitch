package quota

import "testing"

func TestFormatCodexPlan(t *testing.T) {
	cases := map[string]string{
		"free":     "free",
		"Go":       "go",
		"plus":     "plus",
		"pro":      "pro",
		"pro_5x":   "pro 5x",
		"pro5x":    "pro 5x",
		"pro 5x":   "pro 5x",
		"pro_20x":  "pro 20x",
		"pro-20x":  "pro 20x",
		"pro 20x":  "pro 20x",
		"team":     "team",
		"business": "business",
		"edu_plus": "edu",
	}
	for in, want := range cases {
		if got := formatCodexPlan(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestFormatGrokPlan(t *testing.T) {
	cases := map[string]string{
		"SuperGrok Heavy": "SuperGrok Heavy",
		"SuperGrokPro":    "SuperGrok Pro",
		"SuperGrokHeavy":  "SuperGrok Heavy",
		"supergrok":       "SuperGrok",
	}
	for in, want := range cases {
		if got := formatGrokPlan(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestFormatCursorPlan(t *testing.T) {
	cases := map[string]string{
		"ultra":    "Ultra",
		"Ultra":    "Ultra",
		"pro":      "Pro",
		"pro_plus": "Pro+",
		"pro+":     "Pro+",
		"hobby":    "Hobby",
		"free":     "Hobby",
	}
	for in, want := range cases {
		if got := formatCursorPlan(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestMergePlanKeepsSpecific(t *testing.T) {
	if got := MergePlan("pro 5x", "pro"); got != "pro 5x" {
		t.Fatalf("got %q", got)
	}
	if got := MergePlan("pro 5x", "plus"); got != "plus" {
		t.Fatalf("upgrade got %q", got)
	}
	if got := MergePlan("SuperGrok Heavy", ""); got != "SuperGrok Heavy" {
		t.Fatalf("empty clobber %q", got)
	}
}

func TestExtractPlanPriority(t *testing.T) {
	v := map[string]any{
		"config":                   map[string]any{"creditUsagePercent": 2},
		"subscription_tier_display": "SuperGrok Heavy",
		"subscriptionTier":         "SuperGrokPro",
	}
	if got := ExtractPlan(v); got != "SuperGrok Heavy" {
		t.Fatalf("got %q", got)
	}
}

func TestParseCursorPlan(t *testing.T) {
	got := ParseCursorPlan([]byte(`{"planInfo":{"planName":"Ultra","price":"$200/mo"}}`))
	if got != "Ultra" {
		t.Fatalf("got %q", got)
	}
}

func TestClassifyCodexPlanType(t *testing.T) {
	got := Classify(KindCodex, 200, []byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10}}}`))
	if got.Class != OK || got.Plan != "plus" {
		t.Fatalf("%+v", got)
	}
	got = Classify(KindCodex, 200, []byte(`{"plan_type":"pro_20x","rate_limit":{"primary_window":{"used_percent":10}}}`))
	if got.Plan != "pro 20x" {
		t.Fatalf("plan %q", got.Plan)
	}
}

func TestClassifyGrokPlan(t *testing.T) {
	got := Classify(KindGrok, 200, []byte(`{"config":{"creditUsagePercent":2},"subscriptionTier":"SuperGrok Heavy"}`))
	if got.Class != OK || got.Plan != "SuperGrok Heavy" {
		t.Fatalf("%+v", got)
	}
}
