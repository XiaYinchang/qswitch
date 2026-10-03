package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestZCodeQuotaContract(t *testing.T) {
	for _, provider := range []string{"bigmodel", "zai"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				host := "bigmodel.cn"
				if provider == "zai" {
					host = "api.z.ai"
				}
				if req.Method != http.MethodGet || req.URL.String() != "https://"+host+"/api/monitor/usage/quota/limit" || req.Header.Get("Authorization") != "fake-plan-key" || req.Body != nil {
					t.Fatalf("unexpected quota request: %s %s", req.Method, req.URL)
				}
				body := `{"code":200,"success":true,"data":{"level":"max","limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1,"nextResetTime":1791009301952},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":1,"nextResetTime":1791513884984},{"type":"TIME_LIMIT","unit":5,"number":1,"usage":4000,"currentValue":511,"remaining":3489,"percentage":12,"nextResetTime":1791686684993}]}}`
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}}
			got, err := h.ZCode(context.Background(), "fake-plan-key", provider)
			if err != nil || calls != 1 || got.Class != OK || got.UsedPct != 1 || !got.Authenticated || got.Plan != "max" {
				t.Fatalf("result = %+v, calls=%d err=%v", got, calls, err)
			}
			want := []Bucket{{ID: "5h", UsedPct: 1, ResetsAt: 1791009301}, {ID: "weekly", UsedPct: 1, ResetsAt: 1791513884}, {ID: "mcp", UsedPct: 12, ResetsAt: 1791686684}}
			if len(got.Buckets) != len(want) {
				t.Fatalf("buckets = %+v", got.Buckets)
			}
			for i := range want {
				if got.Buckets[i] != want[i] {
					t.Fatalf("bucket %d = %+v, want %+v", i, got.Buckets[i], want[i])
				}
			}
		})
	}
	for _, tc := range []struct{ key, provider string }{{"", "bigmodel"}, {"key", "https://example.test"}} {
		h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid credentials/provider must not issue requests")
			return nil, nil
		})}}
		if got, err := h.ZCode(context.Background(), tc.key, tc.provider); err == nil || got.Class != Unknown {
			t.Fatalf("invalid request accepted: %+v %v", got, err)
		}
	}
}

func TestClassifyZCodeQuota(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		class      Class
		used       float64
		auth       bool
	}{
		{"five hour used", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42}]}}`, 200, OK, 42, true},
		{"weekly exhausted", `{"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":1},{"type":"CREDIT_LIMIT","unit":6,"number":1,"percentage":100}]}}`, 200, Exhausted, 100, true},
		{"99.9 still available", `{"code":0,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":99.9}]}}`, 200, Soft, 99.9, true},
		{"MCP exhausted does not stop model", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1},{"type":"TIME_LIMIT","unit":5,"number":1,"percentage":100}]}}`, 200, OK, 1, true},
		{"remaining ratio", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"usage":100,"remaining":25}]}}`, 200, OK, 75, true},
		{"explicit zero remaining", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"usage":100,"remaining":0,"percentage":99}]}}`, 200, Exhausted, 100, true},
		{"missing percentage not zero", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5}]}}`, 200, Unknown, 0, true},
		{"MCP alone is insufficient", `{"code":200,"data":{"limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"percentage":0}]}}`, 200, Unknown, 0, true},
		{"unsupported window", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":10,"percentage":0}]}}`, 200, Unknown, 0, true},
		{"business failure", `{"code":1001,"success":false,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":0}]}}`, 200, Unknown, 0, false},
		{"contradictory success", `{"code":200,"success":false,"data":{"limits":[]}}`, 200, Unknown, 0, false},
		{"no success evidence", `{"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":0}]}}`, 200, Unknown, 0, false},
		{"expired", `{"error":"unauthorized"}`, 401, Expired, 0, false},
		{"forbidden", `{"error":"forbidden"}`, 403, Expired, 0, false},
		{"rate limit", `{"error":"quota exceeded"}`, 429, Unknown, 0, false},
		{"upstream unavailable", `{"error":"unavailable"}`, 503, Unknown, 0, false},
		{"html", `<html>error</html>`, 200, Unknown, 0, false},
		{"bad percentage type", `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":"0"}]}}`, 200, Unknown, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyZCode(tc.status, []byte(tc.body))
			if got.Class != tc.class || got.UsedPct != tc.used || got.Authenticated != tc.auth {
				t.Fatalf("got %+v, want class=%s used=%v auth=%v", got, tc.class, tc.used, tc.auth)
			}
		})
	}
}

func TestZCodeWaitsForEveryExhaustedModelWindow(t *testing.T) {
	for _, tc := range []struct {
		name, reset string
		want        int64
	}{
		{"both resets known", `,"nextResetTime":1791513884984`, 1791513884},
		{"one reset unknown", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":100,"nextResetTime":1791009301952},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":100` + tc.reset + `}]}}`
			got := ClassifyZCode(200, []byte(body))
			if got.Class != Exhausted || got.ResetsAt != tc.want {
				t.Fatalf("recovery requires every exhausted window: %+v, want reset=%d", got, tc.want)
			}
		})
	}
}

func TestZCodeRefusesCrossHostQuotaRedirect(t *testing.T) {
	calls := 0
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls != 1 {
			t.Fatal("must not follow a quota redirect to another host")
		}
		return &http.Response{StatusCode: http.StatusFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": []string{"https://other.bigmodel.cn/collect"}}, Request: req}, nil
	})}}
	got, err := h.ZCode(context.Background(), "fake-plan-key", "bigmodel")
	if err == nil || got.Class != Unknown || calls != 1 {
		t.Fatalf("redirect not rejected: class=%s calls=%d err=%v", got.Class, calls, err)
	}
}
