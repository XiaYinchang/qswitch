package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Claude Code's OAuth surface (undocumented but used by the official CLI
// statusline): usage/profile under api.anthropic.com, token rotation on
// platform.claude.com. All requests need the oauth beta header.
const (
	ClaudeUsageURL      = "https://api.anthropic.com/api/oauth/usage"
	ClaudeProfileURL    = "https://api.anthropic.com/api/oauth/profile"
	ClaudeTokenURL      = "https://platform.claude.com/v1/oauth/token"
	ClaudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthBeta     = "oauth-2025-04-20"
)

type ClaudeTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Permanent    bool   `json:"-"`
}

func claudeWindow(raw map[string]json.RawMessage, keys ...string) (used float64, resetsAt int64, ok bool) {
	for _, k := range keys {
		node, present := raw[k]
		if !present || string(node) == "null" {
			continue
		}
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(node, &w) != nil || w.Utilization == nil {
			continue
		}
		used = *w.Utilization
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			resetsAt = t.Unix()
		}
		return used, resetsAt, true
	}
	return 0, 0, false
}

// ParseClaudeUsage classifies an /api/oauth/usage response. The endpoint
// reports utilization on a 0-100 scale per window; an account is exhausted
// only when a binding window (5-hour or weekly) actually reaches 100%.
func ParseClaudeUsage(status int, body []byte, retryAfter time.Duration) Result {
	if status == 401 || status == 403 {
		return Result{Class: Expired, Source: "http", Authenticated: false}
	}
	if status == 429 || status == 503 {
		return Result{Class: Unknown, Source: "http_throttled", RetryAfter: retryAfter}
	}
	if status != 200 {
		return Result{Class: Unknown, Source: "http"}
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return Result{Class: Unknown, Source: "http"}
	}
	if _, hasErr := raw["error"]; hasErr {
		return Result{Class: Unknown, Source: "http", Authenticated: false}
	}
	res := Result{Class: OK, Source: "http", Authenticated: true}
	var buckets []Bucket
	if used, resets, ok := claudeWindow(raw, "five_hour"); ok {
		buckets = append(buckets, Bucket{ID: "5h", UsedPct: used, ResetsAt: resets})
	}
	if used, resets, ok := claudeWindow(raw, "seven_day"); ok {
		buckets = append(buckets, Bucket{ID: "weekly", UsedPct: used, ResetsAt: resets})
	}
	if used, resets, ok := claudeWindow(raw, "seven_day_sonnet"); ok {
		buckets = append(buckets, Bucket{ID: "weekly_sonnet", UsedPct: used, ResetsAt: resets})
	}
	// Anthropic has served the Opus weekly cap under either key.
	if used, resets, ok := claudeWindow(raw, "seven_day_opus", "seven_day_omelette"); ok {
		buckets = append(buckets, Bucket{ID: "weekly_opus", UsedPct: used, ResetsAt: resets})
	}
	if node, present := raw["extra_usage"]; present && string(node) != "null" {
		var extra struct {
			Enabled      bool     `json:"is_enabled"`
			MonthlyLimit float64  `json:"monthly_limit"` // cents
			UsedCredits  float64  `json:"used_credits"`  // cents
			Utilization  *float64 `json:"utilization"`
		}
		if json.Unmarshal(node, &extra) == nil && extra.Enabled && extra.Utilization != nil && extra.MonthlyLimit > 0 {
			used := *extra.Utilization
			if used < 0 {
				used = 0
			}
			if used > 100 {
				used = 100
			}
			buckets = append(buckets, Bucket{ID: "extra_usage", UsedPct: used,
				Plan: fmt.Sprintf("$%.0f/$%.0f", extra.UsedCredits/100, extra.MonthlyLimit/100)})
		}
	}
	res.Buckets = buckets
	// The account-level verdict follows the binding windows only; model-specific
	// and paid-overage buckets never decide a switch by themselves.
	var binding []Bucket
	for _, b := range buckets {
		if b.ID == "5h" || b.ID == "weekly" {
			binding = append(binding, b)
		}
	}
	if len(binding) == 0 {
		return Result{Class: Unknown, Source: "http", Authenticated: true, Buckets: buckets}
	}
	worst := binding[0]
	exhausted := false
	for _, b := range binding {
		if b.UsedPct >= 100 {
			exhausted = true
			// Report the binding window that recovers soonest.
			if res.ResetsAt == 0 || (b.ResetsAt > 0 && b.ResetsAt < res.ResetsAt) {
				res.ResetsAt = b.ResetsAt
			}
		}
		if b.UsedPct > worst.UsedPct {
			worst = b
		}
	}
	res.UsedPct = worst.UsedPct
	if !exhausted {
		res.ResetsAt = worst.ResetsAt
	}
	switch {
	case exhausted:
		res.Class = Exhausted
		res.UsedPct = 100
	case res.UsedPct >= 90:
		res.Class = Soft
	}
	return res
}

func (h HTTP) claudeGet(ctx context.Context, token, url string) ([]byte, int, time.Duration, error) {
	if strings.TrimSpace(token) == "" {
		return nil, 0, 0, fmt.Errorf("quota: empty claude access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("Accept", "application/json")
	if err := checkHost(req.URL); err != nil {
		return nil, 0, 0, err
	}
	res, err := h.client().Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var retryAfter time.Duration
	if res.StatusCode == 429 || res.StatusCode == 503 {
		retryAfter = parseRetryAfter(res.Header.Get("Retry-After"), time.Now())
	}
	return raw, res.StatusCode, retryAfter, nil
}

func (h HTTP) Claude(ctx context.Context, token string) (Result, error) {
	raw, status, retryAfter, err := h.claudeGet(ctx, token, ClaudeUsageURL)
	if err != nil {
		return Result{Class: Unknown}, err
	}
	return ParseClaudeUsage(status, raw, retryAfter), nil
}

// ClaudeProfile returns the raw /api/oauth/profile body for identity checks.
func (h HTTP) ClaudeProfile(ctx context.Context, token string) ([]byte, int, error) {
	raw, status, _, err := h.claudeGet(ctx, token, ClaudeProfileURL)
	return raw, status, err
}

// ClaudeIdentity extracts (uuid, email, display name, plan hint) from a
// /api/oauth/profile response.
func ClaudeIdentity(body []byte) (string, string, string, string) {
	var profile struct {
		Account struct {
			UUID        string `json:"uuid"`
			FullName    string `json:"full_name"`
			DisplayName string `json:"display_name"`
			Email       string `json:"email"`
		} `json:"account"`
		Organization struct {
			Name               string `json:"name"`
			Type               string `json:"organization_type"`
			RateLimitTier      string `json:"rate_limit_tier"`
			SubscriptionStatus string `json:"subscription_status"`
		} `json:"organization"`
	}
	if json.Unmarshal(body, &profile) != nil {
		return "", "", "", ""
	}
	name := strings.TrimSpace(profile.Account.FullName)
	if name == "" {
		name = strings.TrimSpace(profile.Account.DisplayName)
	}
	plan := ""
	switch strings.ToLower(strings.TrimSpace(profile.Organization.Type)) {
	case "claude_max":
		plan = "Claude Max"
	case "claude_pro":
		plan = "Claude Pro"
	case "claude_free":
		plan = "Claude Free"
	case "team":
		plan = "Claude Team"
	case "enterprise":
		plan = "Claude Enterprise"
	}
	return profile.Account.UUID, strings.TrimSpace(profile.Account.Email), name, plan
}

// ClaudeRefresh exchanges a refresh token at the official OAuth endpoint.
// The response may rotate the refresh token; the caller owns vault locking
// and must never race a live Claude CLI's own rotation.
func (h HTTP) ClaudeRefresh(ctx context.Context, refresh string) (ClaudeTokens, error) {
	if strings.TrimSpace(refresh) == "" {
		return ClaudeTokens{}, fmt.Errorf("quota: empty claude refresh token")
	}
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
		"client_id":     ClaudeOAuthClientID,
	})
	if err != nil {
		return ClaudeTokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ClaudeTokenURL, bytes.NewReader(body))
	if err != nil {
		return ClaudeTokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := checkHost(req.URL); err != nil {
		return ClaudeTokens{}, err
	}
	res, err := h.client().Do(req)
	if err != nil {
		return ClaudeTokens{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// 400/401/403 mean the refresh token is dead (rotated elsewhere or
		// revoked); anything else is transient.
		permanent := res.StatusCode == 400 || res.StatusCode == 401 || res.StatusCode == 403
		return ClaudeTokens{Permanent: permanent}, fmt.Errorf("quota: claude refresh status %d: %s", res.StatusCode, refreshErrorCode(raw))
	}
	var out ClaudeTokens
	if err := json.Unmarshal(raw, &out); err != nil {
		return ClaudeTokens{}, err
	}
	if out.AccessToken == "" {
		return ClaudeTokens{}, fmt.Errorf("quota: claude refresh missing access_token")
	}
	if out.RefreshToken == "" {
		out.RefreshToken = refresh
	}
	return out, nil
}
