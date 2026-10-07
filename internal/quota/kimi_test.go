package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestKimiUsageContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		class      Class
		pct        float64
		buckets    int
	}{
		{"zero", `{"usages":{"limit_5h":{"used_ratio":0}}}`, 200, OK, 0, 1},
		{"windows", `{"usages":{"limit_5h":{"used_ratio":0.3,"reset_time":"2026-10-03T00:00:00Z"},"limit_7d":{"used_ratio":"0.5"},"limit_month_total":{"used_ratio":0.4},"limit_month_code":{"used_ratio":0.25}}}`, 200, OK, 50, 4},
		{"code_is_breakdown", `{"usages":{"limit_month_total":{"used_ratio":0.2},"limit_month_code":{"used_ratio":1}}}`, 200, OK, 20, 2},
		{"near_limit", `{"usages":{"limit_5h":{"used_ratio":0.999}}}`, 200, Soft, 99.9, 1},
		{"limit", `{"usages":{"limit_7d":{"used_ratio":1}}}`, 200, Exhausted, 100, 1},
		{"wallet_enable_switch_unknown", `{"usages":{"limit_5h":{"used_ratio":1}},"boosterWallet":{"balance":{"type":"BOOSTER","amount":"200000000","amountLeft":"100000000"},"monthlyChargeLimitEnabled":false}}`, 200, Unknown, 100, 1},
		{"wallet_empty", `{"usages":{"limit_5h":{"used_ratio":1}},"boosterWallet":{"balance":{"type":"BOOSTER","amount":"200000000","amountLeft":"0"},"monthlyChargeLimitEnabled":false}}`, 200, Exhausted, 100, 1},
		{"wallet_capped", `{"usages":{"limit_5h":{"used_ratio":1}},"boosterWallet":{"balance":{"type":"BOOSTER","amount":"200000000","amountLeft":"100000000"},"monthlyChargeLimitEnabled":true,"monthlyChargeLimit":{"priceInCents":"5"},"monthlyUsed":{"priceInCents":"5"}}}`, 200, Exhausted, 100, 1},
		{"empty", `{}`, 200, Unknown, 0, 0},
		{"missing_ratio", `{"usages":{"limit_5h":{"reset_time":"2026-10-03T00:00:00Z"}}}`, 200, Unknown, 0, 0},
		{"empty_ratio", `{"usages":{"limit_5h":{"used_ratio":""}}}`, 200, Unknown, 0, 0},
		{"wrong_type", `{"usages":{"limit_5h":{"used_ratio":true}}}`, 200, Unknown, 0, 0},
		{"negative_ratio", `{"usages":{"limit_5h":{"used_ratio":-0.1}}}`, 200, Unknown, 0, 0},
		{"partial_malformed", `{"usages":{"limit_5h":{"used_ratio":0.1},"limit_7d":{"used_ratio":true}}}`, 200, Unknown, 0, 0},
		{"history", `{"history":{"usages":{"limit_5h":{"used_ratio":1}}}}`, 200, Unknown, 0, 0},
		{"error", `{"error":"bad","usages":{"limit_5h":{"used_ratio":0}}}`, 200, Unknown, 0, 0},
		{"unauthorized", `{}`, 401, Expired, 0, 0},
		{"rate_limit", `{}`, 429, Unknown, 0, 0},
		{"unavailable", `{}`, 503, Unknown, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ParseKimiUsage(tc.status, []byte(tc.body))
			if r.Class != tc.class || r.UsedPct != tc.pct || len(r.Buckets) != tc.buckets {
				t.Fatalf("got %+v", r)
			}
			if tc.name == "windows" && (r.Buckets[0].ID != "5h" || r.Buckets[0].ResetsAt != 1790985600 || r.Buckets[1].ID != "weekly" || r.Buckets[2].ID != "monthly" || r.Buckets[3].ID != "monthly_code") {
				t.Fatalf("windows: %+v", r.Buckets)
			}
		})
	}
}

func TestKimiAllExhaustedWindowsMustReset(t *testing.T) {
	for _, tc := range []struct {
		weeklyReset string
		want        int64
	}{
		{`,"reset_time":"2026-10-04T00:00:00Z"`, 1791072000},
		{"", 0},
	} {
		body := `{"usages":{"limit_5h":{"used_ratio":1,"reset_time":"2026-10-03T00:00:00Z"},"limit_7d":{"used_ratio":1` + tc.weeklyReset + `}}}`
		r := ParseKimiUsage(200, []byte(body))
		if r.Class != Exhausted || r.ResetsAt != tc.want {
			t.Fatalf("%+v", r)
		}
	}
}

func TestKimiHTTPAndIdentity(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.kimi.com" || req.Method != "GET" || req.Header.Get("Authorization") != "Bearer test-access" || req.Header.Get("Accept") != "application/json" {
			t.Fatalf("request contract")
		}
		body := `{"usages":{"limit_5h":{"used_ratio":0.5}}}`
		if req.URL.Path == "/coding/v1/me" {
			body = `{"user_id":"user-1","email":"example@example.invalid","nickname":"test","user_level_name":"Allegretto"}`
		} else if req.URL.Path != "/coding/v1/usages" {
			t.Fatalf("path %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}}
	r, err := h.Kimi(context.Background(), "test-access")
	if err != nil || r.Class != OK || r.UsedPct != 50 {
		t.Fatalf("%+v %v", r, err)
	}
	b, status, err := h.KimiProfile(context.Background(), "test-access")
	uid, email, plan := KimiIdentity(b)
	if err != nil || status != 200 || uid != "user-1" || email != "example@example.invalid" || plan != "Allegretto" {
		t.Fatalf("profile contract failed")
	}
	uid, _, _ = KimiIdentity([]byte(`{"data":{"user_id":"wrong"}}`))
	if uid != "" {
		t.Fatal("must not recursively discover identity")
	}
}

func TestKimiLimitsArray(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		class      Class
		pct        float64
		ids        []string
	}{
		{
			// Live contract observed 2026-10-06: usages map plus a limits
			// array whose only window (300 minutes) duplicates limit_5h.
			"live_shape_no_duplicate",
			`{"usage":{"limit":"100","remaining":"100","resetTime":"2026-10-12T12:11:30.337175Z"},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"100","resetTime":"2026-10-07T11:11:30.337175Z"}}],"usages":{"limit_5h":{"used_ratio":0,"reset_time":"2026-10-07T11:11:30Z"},"limit_7d":{"used_ratio":0,"reset_time":"2026-10-12T12:11:30Z"}}}`,
			OK, 0, []string{"5h", "weekly"},
		},
		{
			"monthly_from_month_unit",
			`{"limits":[{"window":{"duration":1,"timeUnit":"TIME_UNIT_MONTH"},"detail":{"limit":"100","remaining":"40","resetTime":"2026-10-31T00:00:00Z"}}]}`,
			OK, 60, []string{"monthly"},
		},
		{
			"monthly_from_thirty_days",
			`{"limits":[{"window":{"duration":30,"timeUnit":"TIME_UNIT_DAY"},"detail":{"limit":200,"remaining":150,"resetTime":"2026-10-31T00:00:00Z"}}]}`,
			OK, 25, []string{"monthly"},
		},
		{
			"monthly_added_to_usages_windows",
			`{"usages":{"limit_5h":{"used_ratio":0.1},"limit_7d":{"used_ratio":0.2}},"limits":[{"window":{"duration":1,"timeUnit":"TIME_UNIT_MONTH"},"detail":{"limit":"100","remaining":"50","resetTime":"2026-10-31T00:00:00Z"}}]}`,
			OK, 50, []string{"5h", "weekly", "monthly"},
		},
		{
			"weekly_from_minutes",
			`{"limits":[{"window":{"duration":10080,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"90","resetTime":"2026-10-12T12:11:30Z"}}]}`,
			OK, 10, []string{"weekly"},
		},
		{
			"limits_fill_without_usages",
			`{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"75","resetTime":"2026-10-07T11:11:30Z"}}]}`,
			OK, 25, []string{"5h"},
		},
		{
			"malformed_entries_skipped",
			`{"usages":{"limit_5h":{"used_ratio":0.1}},"limits":[{"window":{"duration":1,"timeUnit":"TIME_UNIT_MONTH"},"detail":{"limit":"0","remaining":"0"}},{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"120"}},{"detail":{"limit":"100","remaining":"50"}},{"window":{"duration":45,"timeUnit":"TIME_UNIT_SECOND"},"detail":{"limit":"100","remaining":"50"}}]}`,
			OK, 10, []string{"5h"},
		},
		{
			"exhausted_monthly_sets_reset",
			`{"usages":{"limit_5h":{"used_ratio":1,"reset_time":"2026-10-07T11:11:30Z"}},"limits":[{"window":{"duration":1,"timeUnit":"TIME_UNIT_MONTH"},"detail":{"limit":"100","remaining":"0","resetTime":"2026-10-31T00:00:00Z"}}]}`,
			Exhausted, 100, []string{"5h", "monthly"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ParseKimiUsage(200, []byte(tc.body))
			if r.Class != tc.class || r.UsedPct != tc.pct || len(r.Buckets) != len(tc.ids) {
				t.Fatalf("got %+v", r)
			}
			for i, id := range tc.ids {
				if r.Buckets[i].ID != id {
					t.Fatalf("bucket %d = %q, want %q (%+v)", i, r.Buckets[i].ID, id, r.Buckets)
				}
			}
		})
	}
	if r := ParseKimiUsage(200, []byte(`{"limits":[{"window":{"duration":1,"timeUnit":"TIME_UNIT_MONTH"},"detail":{"limit":"100","remaining":"0","resetTime":"2026-10-31T00:00:00Z"}}],"usages":{"limit_5h":{"used_ratio":1,"reset_time":"2026-10-07T11:11:30Z"}}}`)); r.ResetsAt != 1793404800 {
		t.Fatalf("exhausted reset = %d, want the latest exhausted window", r.ResetsAt)
	}
}

func TestKimiRefreshContract(t *testing.T) {
	for _, tc := range []struct {
		status         int
		body           string
		bad, permanent bool
	}{
		{200, `{"access_token":"next-access","refresh_token":"next-refresh","expires_in":900,"scope":"code","token_type":"Bearer"}`, false, false},
		{200, `{"access_token":"next-access","refresh_token":"next-refresh","expires_in":"900"}`, false, false},
		{200, `{"access_token":"next-access","expires_in":900}`, true, false},
		{200, `{"access_token":"next-access","refresh_token":"next-refresh","expires_in":0}`, true, false},
		{401, `{"error":"unauthorized"}`, true, true},
		{400, `{"error":"invalid_grant"}`, true, true},
		{503, `{"error":"unavailable"}`, true, false},
	} {
		h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://auth.kimi.com/api/oauth/token" || req.Method != "POST" {
				t.Fatalf("refresh URL")
			}
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.Form.Get("grant_type") != "refresh_token" || req.Form.Get("refresh_token") != "old-refresh" || req.Form.Get("client_id") != KimiOAuthClientID {
				t.Fatal("refresh form")
			}
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header), Request: req}, nil
		})}}
		tok, err := h.KimiRefresh(context.Background(), "old-refresh")
		if (err != nil) != tc.bad || tok.Permanent != tc.permanent {
			t.Fatalf("status %d: %+v %v", tc.status, tok, err)
		}
		if !tc.bad && (tok.AccessToken != "next-access" || tok.RefreshToken != "next-refresh" || tok.ExpiresIn != 900) {
			t.Fatal("tokens")
		}
	}
}
