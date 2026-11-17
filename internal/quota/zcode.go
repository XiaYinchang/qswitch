package quota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func zcodeOrigin(provider string) (string, error) {
	switch provider {
	case "bigmodel":
		return "https://bigmodel.cn", nil
	case "zai":
		return "https://api.z.ai", nil
	default:
		return "", errors.New("zcode: unsupported Coding Plan provider")
	}
}

func zcodePathComponent(id string) string {
	return strings.NewReplacer("+", "%20", "%21", "!", "%27", "'", "%28", "(", "%29", ")", "%2A", "*").Replace(url.QueryEscape(id))
}

// ZCodeOAuth exchanges the current account's existing personal project locator
// for a short-lived authorization token. It never discovers/creates resources,
// refreshes OAuth, caches the project token, or retries a failed exchange.
func (h HTTP) ZCodeOAuth(ctx context.Context, accessToken, provider, organizationID, projectID, apiKeyID string) (Result, error) {
	unknown := Result{Class: Unknown, Source: "http"}
	origin, err := zcodeOrigin(provider)
	if err != nil {
		return unknown, err
	}
	accessToken = strings.TrimSpace(accessToken)
	if len(accessToken) > len("Bearer") && strings.EqualFold(accessToken[:len("Bearer")], "Bearer") && (accessToken[len("Bearer")] == ' ' || accessToken[len("Bearer")] == '\t') {
		accessToken = strings.TrimSpace(accessToken[len("Bearer"):])
	}
	if accessToken == "" || strings.EqualFold(accessToken, "Bearer") {
		return unknown, errors.New("zcode: 当前桌面账号缺少 OAuth 登录凭据")
	}
	for _, id := range []string{organizationID, projectID, apiKeyID} {
		if strings.TrimSpace(id) == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\r\n\x00") {
			return unknown, errors.New("zcode: 当前桌面账号的个人项目凭据位置无效")
		}
	}
	endpoint := origin + "/api/biz/v1/organization/" + zcodePathComponent(organizationID) + "/projects/" + zcodePathComponent(projectID) + "/api_keys/" + zcodePathComponent(apiKeyID) + "/access_tokens"
	exchangeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(exchangeCtx, http.MethodPost, endpoint, strings.NewReader(`{"clientType":"zcode","clientVersion":"3.18.2"}`))
	if err != nil {
		return unknown, err
	}
	if err := checkHost(req.URL); err != nil {
		return unknown, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	client := *h.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("zcode: 项目授权请求不允许重定向")
	}
	started := time.Now()
	response, err := client.Do(req)
	if err != nil {
		return unknown, errors.New("zcode: 无法获取当前个人项目的短期授权")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return unknown, errors.New("zcode: 个人项目授权响应无效")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		r := MarkThrottled(unknown, response.StatusCode)
		r.RetryAfter = responseRetryAfter(response)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			r.Class = Expired
		}
		if response.StatusCode == http.StatusNotFound {
			return r, errors.New("zcode: 已保存的个人项目凭据位置失效，请在 ZCode 桌面重新确认账号")
		}
		return r, errors.New("zcode: 当前个人项目的短期授权请求失败")
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Data *struct {
			Enable      json.RawMessage `json:"enable"`
			Enabled     json.RawMessage `json:"enabled"`
			AccessToken string          `json:"accessToken"`
			TokenType   string          `json:"tokenType"`
			ExpiresIn   float64         `json:"expiresIn"`
			ExpiresAt   float64         `json:"expiresAt"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data == nil {
		return unknown, errors.New("zcode: 个人项目授权响应无效")
	}
	code := string(envelope.Code)
	if code != "0" && code != "200" && code != `"0"` && code != `"200"` {
		return unknown, errors.New("zcode: 当前个人项目的短期授权未获批准")
	}
	data := envelope.Data
	// Known API variants use enable (desktop wire schema) or enabled (live
	// service). An explicit false in either wins; absent flags default to true
	// as in the desktop schema, but malformed values never authorize a token.
	for _, flag := range []json.RawMessage{data.Enable, data.Enabled} {
		if string(flag) == "false" {
			return unknown, errors.New("zcode: 当前个人项目的 API key 已停用，请在 ZCode 桌面确认")
		}
	}
	for _, flag := range []json.RawMessage{data.Enable, data.Enabled} {
		if len(flag) > 0 && string(flag) != "true" {
			return unknown, errors.New("zcode: 个人项目授权响应无效")
		}
	}
	if strings.TrimSpace(data.AccessToken) == "" || data.TokenType != "Bearer" ||
		math.IsNaN(data.ExpiresIn) || math.IsInf(data.ExpiresIn, 0) || data.ExpiresIn <= 0 ||
		math.IsNaN(data.ExpiresAt) || math.IsInf(data.ExpiresAt, 0) || data.ExpiresAt <= 0 {
		return unknown, errors.New("zcode: 个人项目授权响应缺少有效的令牌或到期信息")
	}
	if data.ExpiresAt <= float64(time.Now().UnixNano())/1e9 || data.ExpiresIn <= time.Since(started).Seconds() {
		return unknown, errors.New("zcode: 个人项目的短期授权已过期")
	}
	return h.ZCode(ctx, strings.TrimSpace(data.AccessToken), provider)
}

// ZCode reads the personal Coding Plan used by the desktop's active account.
// The official monitor endpoint takes the plan API key verbatim, not an OAuth
// token or a synthesized Bearer token. Team plans require a separate contract.
func (h HTTP) ZCode(ctx context.Context, key, provider string) (Result, error) {
	unknown := Result{Class: Unknown, Source: "http"}
	origin, err := zcodeOrigin(provider)
	if err != nil {
		return unknown, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return unknown, errors.New("zcode: missing Coding Plan API key")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, origin+"/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return unknown, err
	}
	if err := checkHost(req.URL); err != nil {
		return unknown, err
	}
	req.Header.Set("Authorization", key)
	client := *h.client()
	previousRedirectCheck := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" || next.URL.Host != req.URL.Host {
			return errors.New("zcode: quota redirect changed the credential destination")
		}
		if previousRedirectCheck != nil {
			return previousRedirectCheck(next, via)
		}
		if len(via) >= 10 {
			return errors.New("zcode: too many quota redirects")
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		return unknown, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return unknown, err
	}
	r := MarkThrottled(ClassifyZCode(res.StatusCode, body), res.StatusCode)
	r.RetryAfter = responseRetryAfter(res)
	return r, nil
}

type zcodeLimit struct {
	Type          string   `json:"type"`
	Unit          int      `json:"unit"`
	Number        int      `json:"number"`
	Usage         *float64 `json:"usage"`
	CurrentValue  *float64 `json:"currentValue"`
	Remaining     *float64 `json:"remaining"`
	Percentage    *float64 `json:"percentage"`
	NextResetTime *float64 `json:"nextResetTime"`
}

// ClassifyZCode checks both HTTP and the provider's business envelope. Model
// windows determine availability; the separate monthly MCP limit does not.
func ClassifyZCode(status int, body []byte) Result {
	r := Result{Class: Unknown, Source: "http"}
	if status == 401 || status == 403 {
		r.Class = Expired
		return r
	}
	if status != http.StatusOK {
		return r
	}
	var env struct {
		Code    *int  `json:"code"`
		Success *bool `json:"success"`
		Data    *struct {
			Level  string       `json:"level"`
			Limits []zcodeLimit `json:"limits"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil || env.Data == nil ||
		(env.Success != nil && !*env.Success) ||
		(env.Code != nil && *env.Code != 0 && *env.Code != 200) ||
		(env.Code == nil && (env.Success == nil || !*env.Success)) {
		return r
	}
	r.Authenticated = true
	r.Plan = strings.TrimSpace(env.Data.Level)
	buckets := make(map[string]Bucket)
	modelKnown := false
	var exhaustedReset int64
	exhaustedResetUnknown := false
	for _, limit := range env.Data.Limits {
		id := zcodeBucketID(limit)
		if id == "" {
			continue
		}
		if _, duplicate := buckets[id]; duplicate {
			return r
		}
		pct, ok := zcodeUsedPercent(limit)
		if !ok {
			if id != "mcp" {
				return r
			}
			continue
		}
		var reset int64
		// The desktop passes nextResetTime directly to JavaScript Date: ms.
		if limit.NextResetTime != nil && *limit.NextResetTime > 0 && *limit.NextResetTime < math.MaxInt64 {
			reset = int64(*limit.NextResetTime / 1000)
		}
		buckets[id] = Bucket{ID: id, UsedPct: pct, ResetsAt: reset}
		if id != "mcp" {
			if !modelKnown || pct > r.UsedPct || (pct == r.UsedPct && reset > r.ResetsAt) {
				r.UsedPct, r.ResetsAt = pct, reset
			}
			if pct >= 100 {
				if reset == 0 {
					exhaustedResetUnknown = true
				}
				if reset > exhaustedReset {
					exhaustedReset = reset
				}
			}
			modelKnown = true
		}
	}
	if !modelKnown {
		return r
	}
	for _, id := range []string{"5h", "weekly", "mcp"} {
		if b, ok := buckets[id]; ok {
			r.Buckets = append(r.Buckets, b)
		}
	}
	switch {
	case r.UsedPct >= 100:
		r.Class = Exhausted
		r.ResetsAt = exhaustedReset
		if exhaustedResetUnknown {
			r.ResetsAt = 0
		}
	case r.UsedPct >= 90:
		r.Class = Soft
	default:
		r.Class = OK
	}
	return r
}

func zcodeBucketID(l zcodeLimit) string {
	if l.Type == "TOKENS_LIMIT" || l.Type == "CREDIT_LIMIT" {
		if l.Unit == 3 && l.Number == 5 {
			return "5h"
		}
		if l.Unit == 6 && l.Number == 1 {
			return "weekly"
		}
	}
	if l.Type == "TIME_LIMIT" && l.Unit == 5 && l.Number == 1 {
		return "mcp"
	}
	return ""
}

func zcodeUsedPercent(l zcodeLimit) (float64, bool) {
	// A precise exhausted counter takes precedence over a rounded percentage.
	if l.Usage != nil && *l.Usage > 0 && l.Remaining != nil && *l.Remaining == 0 {
		return 100, true
	}
	var pct float64
	switch {
	case l.Percentage != nil:
		pct = *l.Percentage
	case l.Usage != nil && *l.Usage > 0 && l.Remaining != nil:
		if *l.Remaining < 0 || *l.Remaining > *l.Usage {
			return 0, false
		}
		pct = (1 - *l.Remaining / *l.Usage) * 100
	case l.Usage != nil && *l.Usage > 0 && l.CurrentValue != nil:
		pct = *l.CurrentValue / *l.Usage * 100
	default:
		return 0, false
	}
	if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
		return 0, false
	}
	return math.Min(pct, 100), true
}
