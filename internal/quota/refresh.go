package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const CodexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

type CodexTokens struct {
	AccessToken  string
	IDToken      string
	RefreshToken string
	Permanent    bool
}

func (h HTTP) CodexRefresh(ctx context.Context, refreshToken string) (CodexTokens, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return CodexTokens{}, fmt.Errorf("quota: empty refresh_token")
	}
	body, err := json.Marshal(map[string]string{
		"client_id":     CodexOAuthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return CodexTokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://auth.openai.com/oauth/token", bytes.NewReader(body))
	if err != nil {
		return CodexTokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setCodexAuthHeaders(req, true)
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
		code := refreshErrorCode(raw)
		permanent := res.StatusCode == 401 || res.StatusCode == 400 ||
			code == "refresh_token_expired" || code == "refresh_token_reused" ||
			code == "refresh_token_invalidated" || code == "invalid_grant"
		return CodexTokens{Permanent: permanent}, fmt.Errorf("quota: refresh http %d", res.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return CodexTokens{}, err
	}
	if out.AccessToken == "" {
		return CodexTokens{}, fmt.Errorf("quota: refresh missing access_token")
	}
	return CodexTokens{AccessToken: out.AccessToken, IDToken: out.IDToken, RefreshToken: out.RefreshToken}, nil
}

const (
	GrokOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	GrokTokenURL      = "https://auth.x.ai/oauth2/token"
)

type GrokTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Permanent    bool
}

func (h HTTP) GrokRefresh(ctx context.Context, refreshToken, clientID string) (GrokTokens, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return GrokTokens{}, fmt.Errorf("quota: empty grok refresh_token")
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = GrokOAuthClientID
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GrokTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return GrokTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qswitch/0.1")
	if err := checkHost(req.URL); err != nil {
		return GrokTokens{}, err
	}
	res, err := h.client().Do(req)
	if err != nil {
		return GrokTokens{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		code := refreshErrorCode(raw)
		permanent := res.StatusCode == 401 || res.StatusCode == 400 ||
			code == "invalid_grant" || code == "invalid_client" ||
			code == "unauthorized_client"
		return GrokTokens{Permanent: permanent}, fmt.Errorf("quota: grok refresh http %d", res.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return GrokTokens{}, err
	}
	if out.AccessToken == "" {
		return GrokTokens{}, fmt.Errorf("quota: grok refresh missing access_token")
	}
	return GrokTokens{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, ExpiresIn: out.ExpiresIn}, nil
}

func refreshErrorCode(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if s, ok := m["code"].(string); ok {
		return strings.ToLower(s)
	}
	switch errv := m["error"].(type) {
	case string:
		return strings.ToLower(errv)
	case map[string]any:
		if s, ok := errv["code"].(string); ok {
			return strings.ToLower(s)
		}
	}
	return ""
}
