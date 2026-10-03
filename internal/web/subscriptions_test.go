package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
)

func subscriptionApp(t *testing.T) *app.App {
	t.Helper()
	t.Setenv("KIMI_CODE_HOME", "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	a := setupApp(t)
	a.Cfg.General.AutoSwitch = false
	a.HTTP.Client = &http.Client{Transport: fixtureTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Host + req.URL.Path {
		case "api.kimi.com/coding/v1/me":
			id := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer fixture-access-")
			if id != "kimi-a" && id != "kimi-b" {
				return nil, fmt.Errorf("unexpected fixture token")
			}
			body = fmt.Sprintf(`{"user_id":%q,"nickname":%q,"phone":{"country_code":"86","number":"176****0000"},"user_level_name":"Allegretto"}`, id, id)
		case "api.kimi.com/coding/v1/usages":
			body = `{"usages":{"limit_5h":{"used_ratio":0.25,"reset_time":"2027-01-02T08:00:00Z"},"limit_7d":{"used_ratio":0.40,"reset_time":"2027-01-07T08:00:00Z"},"limit_month_total":{"used_ratio":0.10,"reset_time":"2027-02-01T08:00:00Z"},"limit_month_code":{"used_ratio":0.99}}}`
		default:
			return nil, fmt.Errorf("fixture rejects %s %s", req.Method, req.URL.Host)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	path := filepath.Join(a.UserHome, ".kimi-code", "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.UserHome, ".kimi-code", "config.toml"), []byte("hooks = []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"kimi-b", "kimi-a"} {
		raw, _ := json.Marshal(map[string]any{"access_token": "fixture-access-" + id, "refresh_token": "fixture-refresh-" + id, "expires_at": 4102444800, "scope": "code", "token_type": "Bearer"})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.Capture(adapter.Kimi); err != nil {
			t.Fatal(err)
		}
		res := a.ProbeAccount(context.Background(), adapter.Kimi, id)
		if res.Class != "ok" || res.UsedPct != 40 {
			t.Fatalf("quota integration: %+v", res)
		}
	}
	return a
}

func TestKimiSubscriptionLifecycleHTTP(t *testing.T) {
	a := subscriptionApp(t)
	ts := httptest.NewServer(New(a, "127.0.0.1:0").Handler())
	defer ts.Close()
	call := func(path string, body any, code int) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Origin", ts.URL)
		req.Header.Set("Content-Type", "application/json")
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		if res.StatusCode != code {
			t.Fatalf("%s: status %d, %s", path, res.StatusCode, out)
		}
		if bytes.Contains(out, []byte("fixture-access")) || bytes.Contains(out, []byte("fixture-refresh")) {
			t.Fatal("credential exposure")
		}
		return out
	}
	call("/api/probe", map[string]string{"tool": "kimi", "id": "kimi-b"}, 200)
	call("/api/switch", map[string]any{"tool": "kimi", "id": "kimi-b", "kill_cli": true}, 200)
	ptr, err := a.State.GetPointer("kimi")
	if err != nil || ptr.StableID != "kimi-b" {
		t.Fatal("switch did not change live pointer")
	}
	raw, _ := os.ReadFile(filepath.Join(a.UserHome, ".kimi-code", "credentials", "kimi-code.json"))
	if !bytes.Contains(raw, []byte("fixture-access-kimi-b")) {
		t.Fatal("switch did not restore target credentials")
	}
	cfg, _ := os.ReadFile(filepath.Join(a.UserHome, ".kimi-code", "config.toml"))
	if string(cfg) != "hooks = []\n" {
		t.Fatal("switch altered hooks")
	}
	call("/api/capture", map[string]string{"tool": "kimi"}, 200)
	call("/api/forget", map[string]string{"tool": "kimi", "id": "kimi-b"}, 400)
	call("/api/forget", map[string]string{"tool": "kimi", "id": "kimi-a"}, 200)
	call("/api/switch", map[string]any{"tool": "zcode", "id": "any"}, 400)
	for _, tool := range a.Overview().Tools {
		if tool.Tool == "kimi" && (len(tool.Accounts) != 1 || len(tool.Accounts[0].Buckets) != 4 || tool.Accounts[0].UsedPct != 40 || tool.Accounts[0].Phone != "+86 176****0000" || tool.Accounts[0].Email != "" || !tool.Switchable) {
			t.Fatalf("Kimi overview: %+v", tool)
		}
		if tool.Tool == "zcode" && (tool.Switchable || tool.Auto) {
			t.Fatal("ZCode read-only guard absent")
		}
	}
}

func TestSubscriptionBrowserFixture(t *testing.T) {
	addr := os.Getenv("QSWITCH_SUBSCRIPTION_BROWSER_ADDR")
	if addr == "" {
		t.Skip("browser fixture not requested")
	}
	a := subscriptionApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	t.Logf("subscription browser fixture http://%s", addr)
	if err := New(a, addr).Run(ctx); err != nil {
		t.Fatal(err)
	}
}
