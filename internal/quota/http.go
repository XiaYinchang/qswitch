package quota

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var allowHosts = map[string]bool{
	"chatgpt.com":             true,
	"auth.openai.com":         true,
	"auth.x.ai":               true,
	"cli-chat-proxy.grok.com": true,
	"grok.com":                true,
	"api2.cursor.sh":          true,
}

type HTTP struct {
	Client *http.Client
}

func (h HTTP) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 8 * time.Second}
}

func (h HTTP) Codex(ctx context.Context, accessToken, accountID string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return Result{Class: Unknown}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	setCodexAuthHeaders(req, true)
	return h.do(KindCodex, req)
}

func (h HTTP) Grok(ctx context.Context, bearer string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cli-chat-proxy.grok.com/v1/billing?format=credits", nil)
	if err != nil {
		return Result{Class: Unknown}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("x-xai-token-auth", "xai-grok-cli")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qswitch/0.1")
	return h.do(KindGrok, req)
}

func (h HTTP) Cursor(ctx context.Context, accessToken string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage", bytes.NewReader([]byte("{}")))
	if err != nil {
		return Result{Class: Unknown}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("User-Agent", "qswitch/0.1")
	return h.do(KindCursor, req)
}

func (h HTTP) do(kind Kind, req *http.Request) (Result, error) {
	if err := checkHost(req.URL); err != nil {
		return Result{Class: Unknown}, err
	}
	res, err := h.client().Do(req)
	if err != nil {
		return Result{Class: Unknown, Source: "http"}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return Classify(kind, res.StatusCode, body), nil
}

func checkHost(u *url.URL) error {
	if u == nil || !allowHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("quota: host not allowed")
	}
	return nil
}
