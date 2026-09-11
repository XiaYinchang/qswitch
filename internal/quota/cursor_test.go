package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyCursorBuckets(t *testing.T) {
	body := `{"billingCycleEnd":"1789568819000","planUsage":{"autoPercentUsed":49.556,"apiPercentUsed":97.89,"totalPercentUsed":56.46,"limit":40000}}`
	got := Classify(KindCursor, 200, []byte(body))
	if got.Class != Soft {
		t.Fatalf("class %s", got.Class)
	}
	if got.UsedPct != 97.89 {
		t.Fatalf("used %v", got.UsedPct)
	}
	if len(got.Buckets) != 2 || got.Buckets[0].ID != "auto" || got.Buckets[1].ID != "api" {
		t.Fatalf("buckets %+v", got.Buckets)
	}
	if got.ResetsAt != 1789568819 {
		t.Fatalf("resets %d", got.ResetsAt)
	}
}

func TestParseCursorBot(t *testing.T) {
	b, ok := ParseCursorBot([]byte(`{"usagePercent":12,"nextResetTimestampUtc":"2026-09-13T14:28:12.602Z","grokPlanLabel":"Grok Bot Plan"}`))
	if !ok || b.ID != "bot" || b.UsedPct != 12 || b.ResetsAt == 0 || b.Plan != "Grok Bot Plan" {
		t.Fatalf("%+v ok=%v", b, ok)
	}
}

func TestCursorHTTPMergesBot(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "GetCurrentPeriodUsage"):
			body := `{"billingCycleEnd":"1789568819000","planUsage":{"autoPercentUsed":10,"apiPercentUsed":20,"totalPercentUsed":15}}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		case strings.Contains(req.URL.Path, "GetSandUsageStatus"):
			body := `{"usagePercent":3,"nextResetTimestampUtc":"2026-09-13T14:28:12Z","grokPlanLabel":"Grok Bot Plan"}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		case strings.Contains(req.URL.Path, "GetPlanInfo"):
			body := `{"planInfo":{"planName":"Ultra","price":"$200/mo"}}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		default:
			t.Fatalf("unexpected %s", req.URL)
			return nil, nil
		}
	})}}
	res, err := h.Cursor(context.Background(), "tok")
	if err != nil || res.Class != OK || res.UsedPct != 20 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(res.Buckets) != 3 || res.Buckets[2].ID != "bot" || res.Buckets[2].UsedPct != 3 || res.Buckets[2].Plan != "Grok Bot Plan" {
		t.Fatalf("buckets %+v", res.Buckets)
	}
	if res.Plan != "Ultra" {
		t.Fatalf("plan %q", res.Plan)
	}
}

func TestGrokHTTPPlanFromSettings(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/v1/billing"):
			body := `{"config":{"creditUsagePercent":7.0,"currentPeriod":{"end":"2026-09-13T14:04:34Z"}}}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		case strings.Contains(req.URL.Path, "/v1/settings"):
			body := `{"subscription_tier_display":"SuperGrok Heavy"}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		default:
			t.Fatalf("unexpected %s", req.URL)
			return nil, nil
		}
	})}}
	res, err := h.Grok(context.Background(), "tok")
	if err != nil || res.Class != OK || res.UsedPct != 7 || res.Plan != "SuperGrok Heavy" {
		t.Fatalf("%+v %v", res, err)
	}
}
