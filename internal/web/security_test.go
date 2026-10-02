package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/buildinfo"
)

func TestHealthReportsVersionWithoutApp(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	r.Host = "127.0.0.1:7432"
	w := httptest.NewRecorder()
	New(nil, r.Host).Handler().ServeHTTP(w, r)
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || got["status"] != "ok" || got["version"] != buildinfo.Version {
		t.Fatalf("unexpected health response: status=%d body=%v", w.Code, got)
	}
}

func TestLocalRequestOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, host, origin string
		tls, want          bool
	}{
		{name: "cli", host: "127.0.0.1:7432", want: true},
		{name: "same origin", host: "127.0.0.1:7432", origin: "http://127.0.0.1:7432", want: true},
		{name: "different port", host: "127.0.0.1:7432", origin: "http://127.0.0.1:7433"},
		{name: "different hostname", host: "127.0.0.1:7432", origin: "http://localhost:7432"},
		{name: "different scheme", host: "127.0.0.1:7432", origin: "https://127.0.0.1:7432"},
		{name: "same tls origin", host: "localhost:7432", origin: "https://localhost:7432", tls: true, want: true},
		{name: "opaque origin", host: "127.0.0.1:7432", origin: "null"},
		{name: "userinfo", host: "127.0.0.1:7432", origin: "http://user@127.0.0.1:7432"},
		{name: "path", host: "127.0.0.1:7432", origin: "http://127.0.0.1:7432/other"},
		{name: "query", host: "127.0.0.1:7432", origin: "http://127.0.0.1:7432?other"},
		{name: "ipv6", host: "[::1]:7432", origin: "http://[::1]:7432", want: true},
		{name: "nonlocal host", host: "example.com", origin: "http://example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/capture", nil)
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := localRequest(r); got != tc.want {
				t.Fatalf("localRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoginCancellationIsTerminal(t *testing.T) {
	s := New(nil, "127.0.0.1:7432")
	cancelled := false
	s.logins["pending"] = &loginSess{ID: "pending", Status: "pending", cancel: func() { cancelled = true }}
	s.logins["completed"] = &loginSess{ID: "completed", Status: "ok", cancel: func() { t.Error("completed login cancelled") }}
	for _, id := range []string{"pending", "completed"} {
		r := httptest.NewRequest(http.MethodPost, "/api/login/codex/cancel", strings.NewReader(`{"id":"`+id+`"}`))
		s.postLoginCancel(httptest.NewRecorder(), r)
	}
	s.finishLogin("pending", adapter.Identity{StableID: "late-completion"}, nil)
	s.finishLogin("pending", adapter.Identity{}, errors.New("late error"))
	if sess := s.logins["pending"]; !cancelled || sess.Status != "error" || sess.Err != "cancelled" || sess.Identity.StableID != "" {
		t.Fatal("late completion overwrote cancellation")
	}
	if s.logins["completed"].Status != "ok" {
		t.Fatal("cancel overwrote completed login")
	}
}

func TestGetLoginConcurrentCompletion(t *testing.T) {
	s := New(nil, "127.0.0.1:7432")
	s.logins["session"] = &loginSess{ID: "session", Status: "pending"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.mu.Lock()
			s.logins["session"].Status = "pending"
			s.mu.Unlock()
			s.finishLogin("session", adapter.Identity{StableID: "account"}, nil)
		}
	}()
	for i := 0; i < 200; i++ {
		r := httptest.NewRequest(http.MethodGet, "/api/login/codex?id=session", nil)
		w := httptest.NewRecorder()
		s.getLogin(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("login status = %d", w.Code)
		}
	}
	wg.Wait()
}

func TestServerExitCancelsLoginContext(t *testing.T) {
	s := New(nil, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if s.loginContext().Err() == nil {
		t.Fatal("login context survived server shutdown")
	}
}

func TestCancelRequestBodyValidation(t *testing.T) {
	const limit = 1 << 20
	valid := `{"id":"unknown"}`
	for _, tc := range []struct {
		name, body string
		chunked    bool
		want       int
	}{
		{name: "valid", body: valid, want: http.StatusOK},
		{name: "exact limit", body: valid + strings.Repeat(" ", limit-len(valid)), want: http.StatusOK},
		{name: "oversize valid prefix", body: valid + strings.Repeat(" ", limit-len(valid)) + "x", want: http.StatusRequestEntityTooLarge},
		{name: "chunked oversize", body: valid + strings.Repeat(" ", limit-len(valid)) + "x", chunked: true, want: http.StatusRequestEntityTooLarge},
		{name: "invalid json", body: "{", want: http.StatusBadRequest},
		{name: "two documents", body: valid + valid, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/login/codex/cancel", strings.NewReader(tc.body))
			r.Host = "127.0.0.1:7432"
			if tc.chunked {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			New(nil, r.Host).Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}
