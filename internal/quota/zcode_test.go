package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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

func TestZCodeOAuthExchangesOnlyCurrentPersonalProject(t *testing.T) {
	for _, provider := range []string{"bigmodel", "zai"} {
		t.Run(provider, func(t *testing.T) {
			origin := "https://bigmodel.cn"
			if provider == "zai" {
				origin = "https://api.z.ai"
			}
			calls := 0
			h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				body := ""
				status := http.StatusOK
				if calls%2 == 1 {
					if req.Method != http.MethodPost || req.URL.String() != origin+"/api/biz/v1/organization/org%20%2B/projects/project%3Aone/api_keys/key%40one/access_tokens" || req.Header.Get("Authorization") != "Bearer synthetic-login-token" || req.Header.Get("Content-Type") != "application/json" {
						t.Fatal("project exchange changed account, path, method, or authorization")
					}
					var payload map[string]string
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || len(payload) != 2 || payload["clientType"] != "zcode" || payload["clientVersion"] != "3.18.2" {
						t.Fatal("exchange did not use the desktop client contract")
					}
					if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) > 15*time.Second {
						t.Fatal("project exchange lacks its bounded timeout")
					}
					// Cover both known flag names and the desktop's absent-flag default.
					status = http.StatusCreated
					flags := []string{"", `"enable":true,`, `"enabled":true,`, `"enable":true,"enabled":true,`}
					body = fmt.Sprintf(`{"code":"200","data":{%s"accessToken":"synthetic-project-token","tokenType":"Bearer","expiresIn":120,"expiresAt":%d}}`, flags[(calls-1)/2], time.Now().Add(time.Hour).Unix())
				} else {
					if req.Method != http.MethodGet || req.URL.String() != origin+"/api/monitor/usage/quota/limit" || req.Header.Get("Authorization") != "synthetic-project-token" || req.Body != nil || req.Header.Get("bigmodel-organization") != "" || req.Header.Get("bigmodel-project") != "" {
						t.Fatal("quota request used the OAuth token, locator, or team scope")
					}
					body = `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":7}]}}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}}
			for i := 0; i < 4; i++ {
				got, err := h.ZCodeOAuth(context.Background(), "  bEaReR synthetic-login-token  ", provider, "org +", "project:one", "key@one")
				if err != nil || got.Class != OK || got.UsedPct != 7 || !got.Authenticated || calls != (i+1)*2 {
					t.Fatalf("OAuth quota class=%s used=%v calls=%d err=%v", got.Class, got.UsedPct, calls, err)
				}
			}
		})
	}
}

func TestZCodeOAuthRejectsFailedOrInvalidProjectAuthorization(t *testing.T) {
	valid := fmt.Sprintf(`{"code":0,"data":{"enabled":true,"accessToken":"response-secret-must-not-escape","tokenType":"Bearer","expiresIn":120,"expiresAt":%d}}`, time.Now().Add(time.Hour).Unix())
	for _, tc := range []struct {
		name, body string
		status     int
		class      Class
	}{
		{"unauthorized", valid, 401, Expired},
		{"forbidden", valid, 403, Expired},
		{"missing locator", valid, 404, Unknown},
		{"throttled", valid, 429, Unknown},
		{"unavailable", valid, 503, Unknown},
		{"redirect", valid, 302, Unknown},
		{"business failure", strings.Replace(valid, `"code":0`, `"code":1001`, 1), 200, Unknown},
		{"missing code", strings.Replace(valid, `"code":0,`, "", 1), 200, Unknown},
		{"disabled enabled", strings.Replace(valid, `"enabled":true`, `"enabled":false`, 1), 200, Unknown},
		{"disabled enable", strings.Replace(valid, `"enabled":true`, `"enable":false`, 1), 200, Unknown},
		{"null enable", strings.Replace(valid, `"enabled":true`, `"enable":null`, 1), 200, Unknown},
		{"string enable", strings.Replace(valid, `"enabled":true`, `"enable":"false"`, 1), 200, Unknown},
		{"numeric enable", strings.Replace(valid, `"enabled":true`, `"enable":0`, 1), 200, Unknown},
		{"null enabled", strings.Replace(valid, `"enabled":true`, `"enabled":null`, 1), 200, Unknown},
		{"string enabled", strings.Replace(valid, `"enabled":true`, `"enabled":"false"`, 1), 200, Unknown},
		{"numeric enabled", strings.Replace(valid, `"enabled":true`, `"enabled":0`, 1), 200, Unknown},
		{"conflicting enabled false", strings.Replace(valid, `"enabled":true`, `"enabled":false,"enable":true`, 1), 200, Unknown},
		{"conflicting enable false", strings.Replace(valid, `"enabled":true`, `"enabled":true,"enable":false`, 1), 200, Unknown},
		{"invalid enable despite enabled true", strings.Replace(valid, `"enabled":true`, `"enabled":true,"enable":null`, 1), 200, Unknown},
		{"invalid enabled despite enable true", strings.Replace(valid, `"enabled":true`, `"enabled":null,"enable":true`, 1), 200, Unknown},
		{"missing token", strings.Replace(valid, "response-secret-must-not-escape", "", 1), 200, Unknown},
		{"invalid token type", strings.Replace(valid, `"tokenType":"Bearer"`, `"tokenType":"Other"`, 1), 200, Unknown},
		{"missing expiry", `{"code":200,"data":{"enabled":true,"accessToken":"response-secret-must-not-escape","tokenType":"Bearer"}}`, 200, Unknown},
		{"expired absolute", `{"code":200,"data":{"enabled":true,"accessToken":"response-secret-must-not-escape","tokenType":"Bearer","expiresIn":120,"expiresAt":1}}`, 200, Unknown},
		{"expired relative", strings.Replace(valid, `"expiresIn":120`, `"expiresIn":1e-12`, 1), 200, Unknown},
		{"negative relative", strings.Replace(valid, `"expiresIn":120`, `"expiresIn":-1`, 1), 200, Unknown},
		{"invalid expiry type", strings.Replace(valid, `"expiresIn":120`, `"expiresIn":"120"`, 1), 200, Unknown},
		{"malformed", `not-json response-secret-must-not-escape`, 200, Unknown},
		{"oversize", strings.Repeat("x", (1<<20)+1), 200, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 || req.Method != http.MethodPost {
					t.Fatal("failed project exchange retried, redirected, or queried quota")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{"Location": []string{"https://bigmodel.cn/other"}, "Retry-After": []string{"9"}}, Request: req}, nil
			})}}
			got, err := h.ZCodeOAuth(context.Background(), "request-secret-must-not-escape", "bigmodel", "org", "project", "key-id")
			if err == nil || got.Class != tc.class || calls != 1 {
				t.Fatalf("class=%s calls=%d err=%v", got.Class, calls, err)
			}
			if strings.Contains(err.Error(), "secret-must-not-escape") {
				t.Fatal("project authorization error leaked request or response credentials")
			}
			if (tc.status == 429 || tc.status == 503) && got.RetryAfter != 9*time.Second {
				t.Fatal("exchange lost the provider retry deadline")
			}
		})
	}
}

func TestZCodeOAuthValidatesInputsBeforeRequests(t *testing.T) {
	for _, tc := range []struct{ token, provider, org, project, key string }{
		{"", "bigmodel", "org", "project", "key"}, {"Bearer ", "bigmodel", "org", "project", "key"},
		{"token", "zai_cn", "org", "project", "key"}, {"token", "bigmodel", "", "project", "key"},
		{"token", "bigmodel", "org", "", "key"}, {"token", "bigmodel", "org", "project", ""},
		{"token", "bigmodel", "org", "..", "key"}, {"token", "bigmodel", "org", "project", "../../key"},
	} {
		h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid OAuth scope made a request")
			return nil, nil
		})}}
		if got, err := h.ZCodeOAuth(context.Background(), tc.token, tc.provider, tc.org, tc.project, tc.key); err == nil || got.Class != Unknown {
			t.Fatal("invalid OAuth scope was accepted")
		}
	}
}

func TestZCodeOAuthQuotaExpiryDoesNotRefreshOrChangeProject(t *testing.T) {
	calls := 0
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		status, body := http.StatusOK, fmt.Sprintf(`{"code":200,"data":{"enabled":true,"accessToken":"project-token","tokenType":"Bearer","expiresIn":120,"expiresAt":%d}}`, time.Now().Add(time.Hour).Unix())
		if calls == 2 {
			status, body = http.StatusUnauthorized, `{"error":"expired"}`
		} else if calls != 1 {
			t.Fatal("expired quota retried OAuth or selected another project")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}}
	if got, err := h.ZCodeOAuth(context.Background(), "login-token", "bigmodel", "org", "project", "key-id"); err != nil || got.Class != Expired || calls != 2 {
		t.Fatalf("quota expiry class=%s calls=%d err=%v", got.Class, calls, err)
	}
}
