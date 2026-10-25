package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeUsage(t *testing.T) {
	body := `{
		"five_hour": {"utilization": 12.0, "resets_at": "2026-05-03T06:50:00Z"},
		"seven_day": {"utilization": 70.0, "resets_at": "2026-05-04T00:00:00Z"},
		"seven_day_sonnet": {"utilization": 38.0, "resets_at": "2026-05-04T00:00:00Z"},
		"seven_day_opus": null,
		"extra_usage": {"is_enabled": false}
	}`
	res := ParseClaudeUsage(200, []byte(body), 0)
	if res.Class != OK || !res.Authenticated {
		t.Fatalf("class=%s auth=%v", res.Class, res.Authenticated)
	}
	if res.UsedPct != 70 {
		t.Fatalf("used=%v want binding-window max 70", res.UsedPct)
	}
	if len(res.Buckets) != 3 {
		t.Fatalf("buckets=%v", res.Buckets)
	}
	if res.ResetsAt == 0 {
		t.Fatal("resets_at not parsed")
	}
}

func TestParseClaudeUsageExhaustedOnlyAt100(t *testing.T) {
	// 99.9% on the 5h window is not exhaustion.
	body := `{"five_hour": {"utilization": 99.9}, "seven_day": {"utilization": 40}}`
	if res := ParseClaudeUsage(200, []byte(body), 0); res.Class != Soft {
		t.Fatalf("class=%s want soft", res.Class)
	}
	body = `{"five_hour": {"utilization": 100, "resets_at": "2026-05-03T06:50:00Z"}, "seven_day": {"utilization": 40}}`
	res := ParseClaudeUsage(200, []byte(body), 0)
	if res.Class != Exhausted || res.UsedPct != 100 {
		t.Fatalf("class=%s used=%v", res.Class, res.UsedPct)
	}
	if res.ResetsAt == 0 {
		t.Fatal("exhausted window should report its reset time")
	}
	// Weekly exhaustion decides even when the session window is fresh.
	body = `{"five_hour": {"utilization": 3}, "seven_day": {"utilization": 100, "resets_at": "2026-05-04T00:00:00Z"}}`
	if res := ParseClaudeUsage(200, []byte(body), 0); res.Class != Exhausted {
		t.Fatalf("class=%s want exhausted", res.Class)
	}
	// A saturated model-specific bucket alone never exhausts the account.
	body = `{"five_hour": {"utilization": 10}, "seven_day": {"utilization": 20}, "seven_day_opus": {"utilization": 100}}`
	if res := ParseClaudeUsage(200, []byte(body), 0); res.Class == Exhausted {
		t.Fatal("opus-only saturation must not exhaust the account")
	}
}

func TestParseClaudeUsageStatusClasses(t *testing.T) {
	if res := ParseClaudeUsage(401, []byte(`{"type":"error"}`), 0); res.Class != Expired {
		t.Fatalf("401 class=%s", res.Class)
	}
	res := ParseClaudeUsage(429, []byte(`{}`), 3*time.Minute)
	if res.Class != Unknown || res.RetryAfter != 3*time.Minute {
		t.Fatalf("429 class=%s retry=%v", res.Class, res.RetryAfter)
	}
	if res := ParseClaudeUsage(200, []byte(`{}`), 0); res.Class != Unknown || !res.Authenticated {
		// Empty object: authenticated, but no binding window -> unknown quota.
		t.Fatalf("empty class=%s auth=%v", res.Class, res.Authenticated)
	}
	if res := ParseClaudeUsage(200, []byte(`not json`), 0); res.Class != Unknown || res.Authenticated {
		t.Fatalf("bad json class=%s auth=%v", res.Class, res.Authenticated)
	}
}

func TestClaudeIdentity(t *testing.T) {
	body := `{
		"account": {"uuid": "u-1", "full_name": "Ada Lovelace", "email": "ada@example.com"},
		"organization": {"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x"},
		"application": {"name": "Claude Code"}
	}`
	uuid, email, name, plan := ClaudeIdentity([]byte(body))
	if uuid != "u-1" || email != "ada@example.com" || name != "Ada Lovelace" || plan != "Claude Max" {
		t.Fatalf("uuid=%q email=%q name=%q plan=%q", uuid, email, name, plan)
	}
	if uuid, _, _, _ := ClaudeIdentity([]byte(`{}`)); uuid != "" {
		t.Fatal("empty profile must yield empty uuid")
	}
	if uuid, _, _, _ := ClaudeIdentity([]byte(`not json`)); uuid != "" {
		t.Fatal("invalid json must yield empty uuid")
	}
}

type roundTripFuncClaude func(*http.Request) (*http.Response, error)

func (f roundTripFuncClaude) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeHTTPUsageAndRefresh(t *testing.T) {
	var sawAuth, sawBeta, sawRefresh string
	h := HTTP{Client: &http.Client{Transport: roundTripFuncClaude(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "api.anthropic.com":
			sawAuth = r.Header.Get("Authorization")
			sawBeta = r.Header.Get("anthropic-beta")
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
				`{"five_hour":{"utilization":55}}`))}, nil
		case "platform.claude.com":
			buf, _ := io.ReadAll(r.Body)
			sawRefresh = string(buf)
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
				`{"access_token":"a2","refresh_token":"r2","expires_in":28800}`))}, nil
		}
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	res, err := h.Claude(context.Background(), "tok")
	if err != nil || res.Class != OK {
		t.Fatalf("usage: %v %+v", err, res)
	}
	if sawAuth != "Bearer tok" || sawBeta != "oauth-2025-04-20" {
		t.Fatalf("headers auth=%q beta=%q", sawAuth, sawBeta)
	}
	tok, err := h.ClaudeRefresh(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "a2" || tok.RefreshToken != "r2" || tok.ExpiresIn != 28800 {
		t.Fatalf("tokens %+v", tok)
	}
	if !strings.Contains(sawRefresh, `"refresh_token":"r1"`) || !strings.Contains(sawRefresh, ClaudeOAuthClientID) {
		t.Fatalf("refresh body %s", sawRefresh)
	}
}

func TestClaudeRefreshPermanentFailure(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFuncClaude(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"error":"invalid_grant"}`))}, nil
	})}}
	tok, err := h.ClaudeRefresh(context.Background(), "dead")
	if err == nil || !tok.Permanent {
		t.Fatalf("want permanent failure, got %v %+v", err, tok)
	}
	// A server that omits refresh_token keeps the one we sent.
	h = HTTP{Client: &http.Client{Transport: roundTripFuncClaude(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"access_token":"a3","expires_in":60}`))}, nil
	})}}
	tok, err = h.ClaudeRefresh(context.Background(), "keep-me")
	if err != nil || tok.RefreshToken != "keep-me" {
		t.Fatalf("refresh rotation fallback: %v %+v", err, tok)
	}
}
