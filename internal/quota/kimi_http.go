package quota

import (
	"context"
	"encoding/base64"
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

// KimiWebSession describes the Kimi desktop app's web session token. It is
// read from the app's localStorage and used as-is; qswitch never refreshes
// or rotates the desktop login.
type KimiWebSession struct {
	Token     string
	Sub       string
	DeviceID  string
	SessionID string
	ExpiresAt int64
}

// ParseKimiWebToken decodes the web session JWT's claims without verifying
// the signature: the claims only pick request headers, the server validates
// the token itself.
func ParseKimiWebToken(token string) (KimiWebSession, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return KimiWebSession{}, fmt.Errorf("kimi: malformed web token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) > 8192 {
		return KimiWebSession{}, fmt.Errorf("kimi: malformed web token")
	}
	var claims struct {
		Sub      string `json:"sub"`
		DeviceID string `json:"device_id"`
		Ssid     string `json:"ssid"`
		Exp      int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || strings.TrimSpace(claims.Sub) == "" {
		return KimiWebSession{}, fmt.Errorf("kimi: web token missing identity")
	}
	return KimiWebSession{Token: strings.TrimSpace(token), Sub: claims.Sub, DeviceID: claims.DeviceID, SessionID: claims.Ssid, ExpiresAt: claims.Exp}, nil
}

// KimiWebStats calls the membership gateway the Kimi desktop app uses for
// its subscription page; host is the origin the web session belongs to.
func (h HTTP) KimiWebStats(ctx context.Context, host string, s KimiWebSession) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := "https://" + host + "/apiv2/kimi.gateway.membership.v2.MembershipService/GetSubscriptionStats"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"mute_notice":false}`))
	if err != nil {
		return Result{Class: Unknown, Source: "web_subscription"}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-msh-platform", "web")
	req.Header.Set("x-msh-version", "2.3.0")
	if s.Sub != "" {
		req.Header.Set("X-Traffic-Id", s.Sub)
	}
	if s.DeviceID != "" {
		req.Header.Set("x-msh-device-id", s.DeviceID)
	}
	if s.SessionID != "" {
		req.Header.Set("x-msh-session-id", s.SessionID)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36")
	body, status, err := h.kimiWebRequest(req)
	if err != nil {
		return Result{Class: Unknown, Source: "web_subscription"}, err
	}
	r, ok := ParseKimiSubscriptionStats(status, body)
	if !ok {
		return Result{Class: Unknown, Source: "web_subscription"}, nil
	}
	return r, nil
}

func (h HTTP) kimiWebRequest(req *http.Request) ([]byte, int, error) {
	if err := checkHost(req.URL); err != nil {
		return nil, 0, err
	}
	client := *h.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return nil, res.StatusCode, err
	}
	if len(raw) > 1<<20 {
		return nil, res.StatusCode, fmt.Errorf("kimi: response too large")
	}
	return raw, res.StatusCode, nil
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
