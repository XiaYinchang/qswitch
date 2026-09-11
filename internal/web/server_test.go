package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
	"qswitch/internal/secutil"
)

func setupApp(t *testing.T) *app.App {
	t.Helper()
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	data := filepath.Join(home, ".qswitch")
	a, err := app.Open(home, data, &secutil.Memory{}, nil, nil, func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	auth := []byte(`{"auth_mode":"chatgpt","tokens":{"account_id":"acc-a","access_token":"t","refresh_token":"r"}}`)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), auth, 0o600)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCheckLoopback(t *testing.T) {
	if err := CheckLoopback("127.0.0.1:7432"); err != nil {
		t.Fatal(err)
	}
	if err := CheckLoopback("0.0.0.0:80"); err == nil {
		t.Fatal("want reject")
	}
}

func TestOverviewAndForgetHTTP(t *testing.T) {
	a := setupApp(t)
	h := New(a, "127.0.0.1:7432").Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.Host = "127.0.0.1:7432"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("overview %d %s", w.Code, w.Body.Bytes())
	}
	var ov app.Overview
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if len(ov.Tools) != 3 {
		t.Fatalf("tools %d", len(ov.Tools))
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/forget", strings.NewReader(`{"tool":"codex","id":"acc-a"}`))
	req2.Host = "127.0.0.1:7432"
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != 400 {
		t.Fatalf("forget live should fail %d %s", w2.Code, w2.Body.Bytes())
	}
}

func TestForbiddenNonLocalHost(t *testing.T) {
	a := setupApp(t)
	h := New(a, "127.0.0.1:7432").Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.Host = "example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code %d", w.Code)
	}
}

func TestIndex(t *testing.T) {
	a := setupApp(t)
	h := New(a, "127.0.0.1:7432").Handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:7432"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "qswitch") {
		t.Fatalf("%d %s", w.Code, w.Body.String()[:80])
	}
	if w.Header().Get("X-Qswitch") != "1" {
		t.Fatal("missing X-Qswitch")
	}
}

func TestAlreadyServing(t *testing.T) {
	a := setupApp(t)
	ts := httptest.NewServer(New(a, "127.0.0.1:7432").Handler())
	defer ts.Close()
	host := ts.Listener.Addr().String()
	if !AlreadyServing(host) {
		t.Fatalf("want already serving %s", host)
	}
	if AlreadyServing("127.0.0.1:1") {
		t.Fatal("unused port should not look like qswitch")
	}
}
