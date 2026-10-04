package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const KimiOAuthClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"

func (h HTTP) Kimi(ctx context.Context, token string) (Result, error) {
	body, status, retry, err := h.kimiGet(ctx, token, "/usages")
	if err != nil {
		return Result{Class: Unknown, Source: "http"}, err
	}
	r := ParseKimiUsage(status, body)
	r.RetryAfter = retry
	return r, nil
}

func (h HTTP) KimiBody(ctx context.Context, token string) ([]byte, int, error) {
	body, status, _, err := h.kimiGet(ctx, token, "/usages")
	return body, status, err
}

func (h HTTP) KimiProfile(ctx context.Context, token string) ([]byte, int, error) {
	body, status, _, err := h.kimiGet(ctx, token, "/me")
	return body, status, err
}

func (h HTTP) kimiGet(ctx context.Context, token, path string) ([]byte, int, time.Duration, error) {
	if strings.TrimSpace(token) == "" {
		return nil, 0, 0, fmt.Errorf("kimi: empty access token")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.kimi.com/coding/v1"+path, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return h.kimiRequest(req)
}

func (h HTTP) kimiRequest(req *http.Request) ([]byte, int, time.Duration, error) {
	if err := checkHost(req.URL); err != nil {
		return nil, 0, 0, err
	}
	client := *h.client()
	// Subscription credentials must not follow a server redirect to another URL.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return nil, res.StatusCode, responseRetryAfter(res), err
	}
	if len(raw) > 1<<20 {
		return nil, res.StatusCode, responseRetryAfter(res), fmt.Errorf("kimi: response too large")
	}
	return raw, res.StatusCode, responseRetryAfter(res), nil
}

type KimiTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	Permanent    bool   `json:"-"`
}

// KimiRefresh only returns rotated credentials. The caller owns vault locking
// and must never use this to race a live Kimi CLI's refresh-token rotation.
func (h HTTP) KimiRefresh(ctx context.Context, refresh string) (KimiTokens, error) {
	if strings.TrimSpace(refresh) == "" {
		return KimiTokens{}, fmt.Errorf("kimi: empty refresh token")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	form := url.Values{"client_id": {KimiOAuthClientID}, "grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://auth.kimi.com/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return KimiTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qswitch/0.1")
	raw, status, _, err := h.kimiRequest(req)
	if err != nil {
		return KimiTokens{}, err
	}
	if status != 200 {
		code := refreshErrorCode(raw)
		permanent := status == 401 || status == 403 || code == "invalid_grant" || code == "invalid_token"
		return KimiTokens{Permanent: permanent}, fmt.Errorf("kimi: refresh http %d", status)
	}
	var wire map[string]any
	if json.Unmarshal(raw, &wire) != nil {
		return KimiTokens{}, fmt.Errorf("kimi: malformed refresh response")
	}
	var out KimiTokens
	out.AccessToken, _ = wire["access_token"].(string)
	out.RefreshToken, _ = wire["refresh_token"].(string)
	out.Scope, _ = wire["scope"].(string)
	out.TokenType, _ = wire["token_type"].(string)
	seconds, ok := kimiNumber(wire["expires_in"])
	if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" || !ok || seconds <= 0 || seconds >= math.MaxInt64 || math.Trunc(seconds) != seconds {
		return KimiTokens{}, fmt.Errorf("kimi: malformed refresh response")
	}
	out.ExpiresIn = int64(seconds)
	return out, nil
}
