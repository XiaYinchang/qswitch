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
	setGrokProxyHeaders(req, bearer)
	res, err := h.do(KindGrok, req)
	if plan := h.grokSettingsPlan(ctx, bearer); plan != "" {
		res.Plan = plan
	}
	return res, err
}

func setGrokProxyHeaders(req *http.Request, bearer string) {
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("x-xai-token-auth", "xai-grok-cli")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qswitch/0.1")
}

func (h HTTP) grokSettingsPlan(ctx context.Context, bearer string) string {
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, "https://cli-chat-proxy.grok.com/v1/settings", nil)
	if err != nil {
		return ""
	}
	setGrokProxyHeaders(req, bearer)
	if err := checkHost(req.URL); err != nil {
		return ""
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return DisplayPlan("grok", PlanFromJSON(raw))
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
	res, err := h.do(KindCursor, req)
	if bot, ok := h.cursorBot(ctx, accessToken); ok {
		res.Buckets = append(res.Buckets, bot)
	}
	if plan := h.cursorPlan(ctx, accessToken); plan != "" {
		res.Plan = plan
	}
	return res, err
}

func (h HTTP) cursorPlan(ctx context.Context, accessToken string) string {
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.DashboardService/GetPlanInfo", bytes.NewReader([]byte("{}")))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qswitch/0.1")
	if err := checkHost(req.URL); err != nil {
		return ""
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return ParseCursorPlan(raw)
}

func (h HTTP) cursorBot(ctx context.Context, accessToken string) (Bucket, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.DashboardService/GetSandUsageStatus", bytes.NewReader([]byte("{}")))
	if err != nil {
		return Bucket{}, false
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qswitch/0.1")
	if err := checkHost(req.URL); err != nil {
		return Bucket{}, false
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return Bucket{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Bucket{}, false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return ParseCursorBot(raw)
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
