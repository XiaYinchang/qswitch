package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	GrokDeviceURL        = "https://accounts.x.ai/oauth2/device"
	grokDeviceCodeURL    = "https://auth.x.ai/oauth2/device/code"
	grokDeviceScope      = "openid profile email offline_access grok-cli:access"
	grokDeviceDefault    = 5 * time.Second
	grokDeviceDefaultTTL = 30 * time.Minute
	grokDeviceGrant      = "urn:ietf:params:oauth:grant-type:device_code"
)

type GrokDeviceStart struct {
	DeviceCode string
	UserCode   string
	VerifyURL  string
	Interval   time.Duration
	ExpiresAt  time.Time
}

func setGrokDeviceHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	ver := GrokCLIVersion()
	req.Header.Set("User-Agent", "grok/"+ver)
	req.Header.Set("x-grok-client-version", ver)
}

func (h HTTP) deviceClient() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (h HTTP) GrokDeviceStart(ctx context.Context) (GrokDeviceStart, error) {
	form := url.Values{}
	form.Set("client_id", GrokOAuthClientID)
	form.Set("scope", grokDeviceScope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grokDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return GrokDeviceStart{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setGrokDeviceHeaders(req)
	if err := checkHost(req.URL); err != nil {
		return GrokDeviceStart{}, err
	}
	res, err := h.deviceClient().Do(req)
	if err != nil {
		return GrokDeviceStart{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return GrokDeviceStart{}, fmt.Errorf("quota: grok device http %d", res.StatusCode)
	}
	var out struct {
		DeviceCode              string          `json:"device_code"`
		UserCode                string          `json:"user_code"`
		VerificationURI         string          `json:"verification_uri"`
		VerificationURIComplete string          `json:"verification_uri_complete"`
		ExpiresIn               json.RawMessage `json:"expires_in"`
		Interval                json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return GrokDeviceStart{}, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return GrokDeviceStart{}, fmt.Errorf("quota: grok device missing fields")
	}
	verify := out.VerificationURI
	if verify == "" {
		verify = GrokDeviceURL
	}
	ttl := jsonNumberDuration(out.ExpiresIn, time.Second, grokDeviceDefaultTTL)
	interval := jsonNumberDuration(out.Interval, time.Second, grokDeviceDefault)
	if interval < time.Second {
		interval = grokDeviceDefault
	}
	return GrokDeviceStart{
		DeviceCode: out.DeviceCode,
		UserCode:   out.UserCode,
		VerifyURL:  verify,
		Interval:   interval,
		ExpiresAt:  time.Now().Add(ttl),
	}, nil
}

func (h HTTP) GrokDevicePoll(ctx context.Context, start GrokDeviceStart) (GrokTokens, error) {
	if start.DeviceCode == "" {
		return GrokTokens{}, fmt.Errorf("quota: empty grok device_code")
	}
	form := url.Values{}
	form.Set("grant_type", grokDeviceGrant)
	form.Set("device_code", start.DeviceCode)
	form.Set("client_id", GrokOAuthClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GrokTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return GrokTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setGrokDeviceHeaders(req)
	if err := checkHost(req.URL); err != nil {
		return GrokTokens{}, err
	}
	res, err := h.deviceClient().Do(req)
	if err != nil {
		return GrokTokens{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	code := refreshErrorCode(raw)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		switch code {
		case "authorization_pending":
			return GrokTokens{}, ErrDevicePending
		case "slow_down":
			return GrokTokens{}, ErrDeviceSlowDown
		case "expired_token", "access_denied", "authorization_declined":
			return GrokTokens{}, fmt.Errorf("quota: grok device poll %s", code)
		}
		if res.StatusCode == http.StatusBadRequest && code == "" {
			return GrokTokens{}, ErrDevicePending
		}
		return GrokTokens{}, fmt.Errorf("quota: grok device poll http %d", res.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return GrokTokens{}, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return GrokTokens{}, fmt.Errorf("quota: grok device missing tokens")
	}
	return GrokTokens{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, ExpiresIn: out.ExpiresIn, Email: out.Email}, nil
}

func (h HTTP) GrokUserinfo(ctx context.Context, accessToken string) (string, error) {
	if strings.TrimSpace(accessToken) == "" {
		return "", fmt.Errorf("quota: empty grok access_token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://auth.x.ai/oauth2/userinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	setGrokDeviceHeaders(req)
	if err := checkHost(req.URL); err != nil {
		return "", err
	}
	res, err := h.deviceClient().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("quota: grok userinfo http %d", res.StatusCode)
	}
	var out struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Email, nil
}
