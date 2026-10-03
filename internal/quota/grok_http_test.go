package quota

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGrokHTTPQuotaContract(t *testing.T) {
	start := time.Date(2026, time.September, 27, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	weeklyConfig := func(percent string) string {
		return fmt.Sprintf(`{"config":{%s"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":%q,"end":%q},"onDemandCap":{},"onDemandUsed":{},"prepaidBalance":{}}}`,
			percent, start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		class   Class
		usedPct float64
	}{
		{name: "weekly omitted zero", status: 200, body: weeklyConfig(""), class: OK},
		{name: "weekly one percent", status: 200, body: weeklyConfig(`"creditUsagePercent":1,`), class: OK, usedPct: 1},
		{name: "expired authentication", status: 401, body: `{"error":"unauthorized"}`, class: Expired},
		{name: "rate limit is not exhausted", status: 429, body: `{"error":"rate limit exceeded"}`, class: Unknown},
		{name: "upstream unavailable is not exhausted", status: 503, body: `{"error":"service unavailable"}`, class: Unknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			billingCalls, settingsCalls := 0, 0
			h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet {
					t.Fatalf("method = %q, want GET", req.Method)
				}
				if got := req.Header.Get("Authorization"); got != "Bearer fake-grok-token" {
					t.Fatalf("unexpected Authorization header: %q", got)
				}
				if got := req.Header.Get("x-xai-token-auth"); got != "xai-grok-cli" {
					t.Fatalf("token auth header = %q", got)
				}
				if req.Body != nil {
					body, err := io.ReadAll(req.Body)
					if err != nil || len(body) != 0 {
						t.Fatalf("GET body must be empty: bytes=%d err=%v", len(body), err)
					}
				}
				status, body := tt.status, tt.body
				switch req.URL.String() {
				case "https://cli-chat-proxy.grok.com/v1/billing?format=credits":
					billingCalls++
				case "https://cli-chat-proxy.grok.com/v1/settings":
					settingsCalls++
					status, body = 200, `{"subscription_tier_display":"SuperGrok Heavy"}`
				default:
					t.Fatalf("unexpected endpoint: %s", req.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}}

			got, err := h.Grok(context.Background(), "fake-grok-token")
			if err != nil {
				t.Fatal(err)
			}
			if billingCalls != 1 || settingsCalls != 1 {
				t.Fatalf("calls: billing=%d settings=%d, want one each", billingCalls, settingsCalls)
			}
			if got.Class != tt.class || got.UsedPct != tt.usedPct {
				t.Fatalf("quota = %+v, want class=%s used=%v", got, tt.class, tt.usedPct)
			}
			if tt.status != 200 {
				if got.Authenticated || len(got.Buckets) != 0 {
					t.Fatalf("unsuccessful billing response must not prove authentication or quota: %+v", got)
				}
				return
			}
			if !got.Authenticated || got.Plan != "SuperGrok Heavy" || got.ResetsAt != end.Unix() {
				t.Fatalf("billing result missing authentication, settings plan, or current reset: %+v", got)
			}
			if len(got.Buckets) != 1 || got.Buckets[0].ID != "weekly" || got.Buckets[0].UsedPct != tt.usedPct || got.Buckets[0].ResetsAt != end.Unix() {
				t.Fatalf("weekly bucket = %+v", got.Buckets)
			}
		})
	}
}
