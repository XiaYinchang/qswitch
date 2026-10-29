package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"qswitch/internal/app"
	"qswitch/internal/config"
)

func tokenServer(tok string) *Server {
	return New(&app.App{Cfg: config.Config{Web: config.Web{Enabled: true, Addr: "0.0.0.0:7432", Token: tok}}}, "0.0.0.0:7432")
}

func TestPublicTokenRequired(t *testing.T) {
	s := tokenServer("secret-token")
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/api/health", nil),
		httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/", nil),
		httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/api/health?token=wrong", nil),
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status=%d want 401", r.Method, r.URL, w.Code)
		}
	}
}

func TestPublicTokenAccepted(t *testing.T) {
	s := tokenServer("secret-token")

	// Bearer header.
	r := httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/api/health", nil)
	r.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("bearer: status=%d", w.Code)
	}

	// Query token on the page plants a cookie that later requests reuse.
	r = httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/?token=secret-token", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("query token: status=%d", w.Code)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "qswitch_auth" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != "secret-token" || !cookie.HttpOnly {
		t.Fatalf("cookie not planted: %+v", cookie)
	}
	r = httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/api/health", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("cookie: status=%d", w.Code)
	}
}

func TestPublicTokenKeepsOriginCheck(t *testing.T) {
	s := tokenServer("secret-token")
	r := httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/api/health", nil)
	r.Header.Set("Authorization", "Bearer secret-token")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request: status=%d want 403", w.Code)
	}
	r.Header.Set("Origin", "http://38.69.195.202:7432")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin request: status=%d", w.Code)
	}
}

func TestPublicBindRequiresToken(t *testing.T) {
	s := tokenServer("")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("public bind without token must fail: %v", err)
	}
	var loopbackErr = CheckLoopback("127.0.0.1:7432")
	if loopbackErr != nil {
		t.Fatal(loopbackErr)
	}
	// Loopback stays open without a token.
	s = New(&app.App{Cfg: config.Default()}, "127.0.0.1:7432")
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := s.Run(ctx2); err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("loopback run: %v", err)
	}
}

// Moving a public deployment back to loopback must not leave a stale token
// locking out the local CLI probe or an SSH-tunnelled browser.
func TestLoopbackIgnoresLeftoverToken(t *testing.T) {
	s := New(&app.App{Cfg: config.Config{Web: config.Web{Enabled: true, Addr: "127.0.0.1:7432", Token: "leftover"}}}, "127.0.0.1:7432")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7432/api/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("loopback with leftover token: status=%d want 200", w.Code)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://attacker.example:7432/api/health", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("loopback with foreign Host: status=%d want 403", w.Code)
	}
}
