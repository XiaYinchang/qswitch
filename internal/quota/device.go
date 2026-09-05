package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	CodexDeviceURL         = "https://auth.openai.com/codex/device"
	codexDeviceUsercodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	codexDeviceTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token"
	codexDeviceRedirectURI = "https://auth.openai.com/deviceauth/callback"
	codexDeviceDefaultWait = 5 * time.Second
	codexDeviceDefaultTTL  = 15 * time.Minute
)

var (
	ErrDevicePending  = errors.New("quota: device authorization pending")
	ErrDeviceSlowDown = errors.New("quota: device slow_down")
)

type CodexDeviceStart struct {
	DeviceAuthID string
	UserCode     string
	Interval     time.Duration
	ExpiresAt    time.Time
}

func (h HTTP) CodexDeviceStart(ctx context.Context) (CodexDeviceStart, error) {
	body, err := json.Marshal(map[string]string{"client_id": CodexOAuthClientID})
	if err != nil {
		return CodexDeviceStart{}, err
	}
	raw, status, err := h.postJSON(ctx, codexDeviceUsercodeURL, body)
	if err != nil {
		return CodexDeviceStart{}, err
	}
	if status < 200 || status >= 300 {
		return CodexDeviceStart{}, fmt.Errorf("quota: device usercode http %d", status)
	}
	var out struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return CodexDeviceStart{}, err
	}
	if out.DeviceAuthID == "" || out.UserCode == "" {
		return CodexDeviceStart{}, fmt.Errorf("quota: device usercode missing fields")
	}
	ttl := jsonNumberDuration(out.ExpiresIn, time.Second, codexDeviceDefaultTTL)
	interval := jsonNumberDuration(out.Interval, time.Second, codexDeviceDefaultWait)
	if interval < time.Second {
		interval = codexDeviceDefaultWait
	}
	return CodexDeviceStart{
		DeviceAuthID: out.DeviceAuthID,
		UserCode:     out.UserCode,
		Interval:     interval,
		ExpiresAt:    time.Now().Add(ttl),
	}, nil
}

func (h HTTP) CodexDevicePoll(ctx context.Context, start CodexDeviceStart) (authCode, verifier string, err error) {
	if start.DeviceAuthID == "" || start.UserCode == "" {
		return "", "", fmt.Errorf("quota: empty device poll")
	}
	body, err := json.Marshal(map[string]string{
		"device_auth_id": start.DeviceAuthID,
		"user_code":      start.UserCode,
	})
	if err != nil {
		return "", "", err
	}
	raw, status, err := h.postJSON(ctx, codexDeviceTokenURL, body)
	if err != nil {
		return "", "", err
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		return "", "", ErrDevicePending
	}
	if status == http.StatusGone {
		return "", "", fmt.Errorf("quota: device code expired")
	}
	if status < 200 || status >= 300 {
		switch strings.ToLower(refreshErrorCode(raw)) {
		case "authorization_pending":
			return "", "", ErrDevicePending
		case "slow_down":
			return "", "", ErrDeviceSlowDown
		case "expired_token", "access_denied", "authorization_declined":
			return "", "", fmt.Errorf("quota: device poll %s", refreshErrorCode(raw))
		}
		return "", "", fmt.Errorf("quota: device poll http %d", status)
	}
	var out struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", err
	}
	if out.AuthorizationCode == "" || out.CodeVerifier == "" {
		return "", "", fmt.Errorf("quota: device poll missing code")
	}
	return out.AuthorizationCode, out.CodeVerifier, nil
}

func (h HTTP) CodexExchangeCode(ctx context.Context, code, verifier string) (CodexTokens, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(verifier) == "" {
		return CodexTokens{}, fmt.Errorf("quota: empty authorization_code")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", codexDeviceRedirectURI)
	form.Set("client_id", CodexOAuthClientID)
	form.Set("code_verifier", verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://auth.openai.com/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return CodexTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setCodexAuthHeaders(req, false)
	if err := checkHost(req.URL); err != nil {
		return CodexTokens{}, err
	}
	res, err := h.client().Do(req)
	if err != nil {
		return CodexTokens{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return CodexTokens{}, fmt.Errorf("quota: code exchange http %d", res.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return CodexTokens{}, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.IDToken == "" {
		return CodexTokens{}, fmt.Errorf("quota: code exchange missing tokens")
	}
	return CodexTokens{AccessToken: out.AccessToken, IDToken: out.IDToken, RefreshToken: out.RefreshToken}, nil
}

func (h HTTP) postJSON(ctx context.Context, rawURL string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	setCodexAuthHeaders(req, false)
	if err := checkHost(req.URL); err != nil {
		return nil, 0, err
	}
	res, err := h.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return raw, res.StatusCode, nil
}

func jsonNumberDuration(raw json.RawMessage, unit, fallback time.Duration) time.Duration {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return time.Duration(n * float64(unit))
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && v > 0 {
			return time.Duration(v * float64(unit))
		}
	}
	return fallback
}
