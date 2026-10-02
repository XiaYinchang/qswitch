package web

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"qswitch/internal/notify"
	"qswitch/internal/secutil"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func lifecycleApp(t *testing.T) *app.App {
	t.Helper()
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	a, err := app.Open(home, filepath.Join(home, ".qswitch"), &secutil.Memory{}, nil, &notify.Log{}, func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Cfg.General.AutoSwitch = false
	a.HTTP.Client = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" || r.URL.Path != "/backend-api/wham/usage" {
			return nil, fmt.Errorf("fixture blocks unexpected upstream: %s %s", r.Method, r.URL.Host)
		}
		used := 25
		if r.Header.Get("ChatGPT-Account-ID") == "demo-b" {
			used = 60
		}
		body := fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800}}}`, used)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"demo-b", "demo-a"} {
		writeFixtureAuth(t, a, id)
		if _, _, err := a.Capture(adapter.Codex); err != nil {
			t.Fatal(err)
		}
		if r := a.ProbeAccount(context.Background(), adapter.Codex, id); r.Class != "ok" {
			t.Fatalf("fixture quota: %+v", r)
		}
	}
	return a
}

func writeFixtureAuth(t *testing.T, a *app.App, id string) {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"email": id + "@example.test", "sub": id, "exp": time.Now().Add(24 * time.Hour).Unix()})
	jwt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	raw, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": time.Now().Format(time.RFC3339), "tokens": map[string]string{"account_id": id, "access_token": jwt, "id_token": jwt, "refresh_token": "fixture-refresh-" + id}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.UserHome, ".codex", "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAccountLifecycleHTTP(t *testing.T) {
	a := lifecycleApp(t)
	ts := httptest.NewServer(New(a, "127.0.0.1:0").Handler())
	defer ts.Close()
	request := func(method, path string, body any, want int) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", ts.URL)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s: status=%d body=%s", method, path, res.StatusCode, out)
		}
		if bytes.Contains(out, []byte("fixture-refresh")) || bytes.Contains(out, []byte("access_token")) {
			t.Fatal("API leaked credentials")
		}
		return out
	}
	if raw := request("GET", "/", nil, 200); !bytes.Contains(raw, []byte("data-act=\"reload-all\"")) {
		t.Fatal("embedded UI missing")
	}
	request("POST", "/api/probe", map[string]string{"tool": "codex", "id": "demo-b"}, 200)
	request("POST", "/api/switch", map[string]any{"tool": "codex", "id": "demo-b", "cli_only": true}, 200)
	p, err := a.State.GetPointer("codex")
	if err != nil || p.StableID != "demo-b" {
		t.Fatalf("switch pointer: %+v %v", p, err)
	}
	blobs, _, err := a.Adapters[adapter.Codex].Capture(a.UserHome)
	if err != nil || len(blobs) != 1 || blobs[0].Identity.StableID != "demo-b" {
		t.Fatal("HTTP switch did not update live file")
	}
	request("POST", "/api/forget", map[string]string{"tool": "codex", "id": "demo-b"}, 400)
	request("POST", "/api/forget", map[string]string{"tool": "codex", "id": "demo-a"}, 200)
	if _, err := os.Stat(filepath.Join(a.DataDir, "vault", "codex", "demo-a.enc")); !os.IsNotExist(err) {
		t.Fatal("forgotten vault entry remains")
	}
	writeFixtureAuth(t, a, "demo-c")
	request("POST", "/api/capture", map[string]string{"tool": "codex"}, 200)
	var overview app.Overview
	if err := json.Unmarshal(request("GET", "/api/overview", nil, 200), &overview); err != nil {
		t.Fatal(err)
	}
	for _, tool := range overview.Tools {
		if tool.Tool == "codex" && (tool.LiveID != "demo-c" || len(tool.Accounts) != 2) {
			t.Fatalf("capture overview: %+v", tool)
		}
	}
	if _, err := a.Vault.Get("codex", "demo-c"); err != nil {
		t.Fatalf("captured vault cannot decrypt: %v", err)
	}
	request("POST", "/api/login/codex", map[string]string{}, 400) // Offline failure is explicit, not a stuck login.
}

// Opt-in fixture for a real browser. Every file and upstream is isolated.
func TestBrowserFixture(t *testing.T) {
	addr := os.Getenv("QSWITCH_BROWSER_TEST_ADDR")
	if addr == "" {
		t.Skip("browser fixture not requested")
	}
	a := lifecycleApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	t.Logf("browser fixture: http://%s", addr)
	if err := New(a, addr).Run(ctx); err != nil {
		t.Fatal(err)
	}
}
