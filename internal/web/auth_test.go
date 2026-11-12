package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"qswitch/internal/app"
	"qswitch/internal/config"
)

const (
	testUser   = "xyc"
	testPass   = "correct horse battery"
	deckOrigin = "https://deck.xiayinchang.top"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func passwordServer(t *testing.T, addr, token string) (*Server, *fakeClock, string) {
	t.Helper()
	dir := t.TempDir()
	if err := SetPassword(dir, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	s := New(&app.App{DataDir: dir, Cfg: config.Config{Web: config.Web{Enabled: true, Addr: addr, Token: token}}}, addr)
	clk := &fakeClock{t: time.Now()}
	s.Now = clk.now
	return s, clk, dir
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func local(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:7432"
	return r
}

// Through the Deck tunnel nginx forwards X-Forwarded-* from loopback; the
// page is mounted under /qswitch on https.
func proxied(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:7432"
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("X-Forwarded-Host", "deck.xiayinchang.top")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Prefix", "/qswitch")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	return r
}

func withForm(r *http.Request, user, pass string) *http.Request {
	body := url.Values{"user": {user}, "password": {pass}}.Encode()
	r.Body = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).Body
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func proxiedLogin(user, pass, origin, from string) *http.Request {
	r := withForm(proxied(http.MethodPost, "/login"), user, pass)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if from != "" {
		r.Header.Set("X-Forwarded-For", from)
	}
	return r
}

// loginForm is what the browser sends from the Deck login page.
func loginForm(user, pass string) *http.Request { return proxiedLogin(user, pass, deckOrigin, "") }

func sessionCookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", sessionCookie, w.Header())
	return nil
}

// The loopback listener is the local page and the SSH tunnel: whoever gets
// there has authenticated to the host already, so a password never applies.
func TestLoopbackNeverAsksForPassword(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()
	if w := do(h, local(http.MethodGet, "/")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"api/overview"`) {
		t.Fatalf("page: %d", w.Code)
	}
	if w := do(h, local(http.MethodGet, "/api/session")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"login_required":false`) {
		t.Fatalf("session: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, local(http.MethodGet, "/login")); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login page: %d %q", w.Code, w.Header().Get("Location"))
	}
	w := do(h, withForm(local(http.MethodPost, "/login"), testUser, testPass))
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 0 {
		t.Fatalf("login post: %d cookies=%v", w.Code, w.Result().Cookies())
	}
	if w := do(h, local(http.MethodPost, "/logout")); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("logout: %d %q", w.Code, w.Header().Get("Location"))
	}
	r := local(http.MethodGet, "/api/health")
	r.Host = "attacker.example:7432"
	if w := do(h, r); w.Code != http.StatusForbidden {
		t.Fatalf("foreign Host must stay refused: %d", w.Code)
	}
}

func TestPasswordGatesProxiedRequests(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()

	if w := do(h, proxied(http.MethodGet, "/")); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/qswitch/login" {
		t.Fatalf("page without login: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := do(h, proxied(http.MethodGet, "/api/overview")); w.Code != http.StatusUnauthorized || w.Header().Get("X-Qswitch") != "1" {
		t.Fatalf("api without login: %d %v", w.Code, w.Header())
	}
	if w := do(h, proxied(http.MethodGet, "/api/health")); w.Code != http.StatusOK {
		t.Fatalf("health must stay open: %d", w.Code)
	}
	if w := do(h, proxied(http.MethodGet, "/login")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="password"`) || !strings.Contains(w.Body.String(), `action="/qswitch/login"`) {
		t.Fatalf("login page: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, loginForm(testUser, "wrong")); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "不对") {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if w := do(h, loginForm("someone-else", testPass)); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user: %d", w.Code)
	}
	w := do(h, loginForm(testUser, testPass))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/qswitch/" {
		t.Fatalf("login: %d %q", w.Code, w.Header().Get("Location"))
	}
	ck := sessionCookieOf(t, w)
	if !ck.HttpOnly || !ck.Secure || ck.Path != "/qswitch" || ck.SameSite != http.SameSiteLaxMode || ck.MaxAge != int(sessionTTL/time.Second) {
		t.Fatalf("cookie attributes: %+v", ck)
	}

	r := proxied(http.MethodGet, "/api/session")
	r.AddCookie(ck)
	if w := do(h, r); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"user":"xyc"`) {
		t.Fatalf("session with cookie: %d %s", w.Code, w.Body.String())
	}
	r = proxied(http.MethodGet, "/login")
	r.AddCookie(ck)
	if w := do(h, r); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/qswitch/" {
		t.Fatalf("login page while logged in: %d", w.Code)
	}
	r = proxied(http.MethodGet, "/api/session")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: ck.Value[:len(ck.Value)-2] + "xx"})
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie: %d", w.Code)
	}
	r = proxied(http.MethodPost, "/logout")
	r.AddCookie(ck)
	r.Header.Set("Origin", deckOrigin)
	w = do(h, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/qswitch/login" || sessionCookieOf(t, w).MaxAge >= 0 {
		t.Fatalf("logout: %d %v", w.Code, w.Header())
	}
}

func TestPublicBindWithPasswordAsksLogin(t *testing.T) {
	s, _, _ := passwordServer(t, "0.0.0.0:7432", "")
	h := s.Handler()
	if w := do(h, httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/", nil)); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("public page: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := do(h, httptest.NewRequest(http.MethodGet, "http://38.69.195.202:7432/api/health", nil)); w.Code != http.StatusOK {
		t.Fatalf("public health: %d", w.Code)
	}
	r := withForm(httptest.NewRequest(http.MethodPost, "http://38.69.195.202:7432/login", nil), testUser, testPass)
	w := do(h, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("public login: %d %q", w.Code, w.Header().Get("Location"))
	}
	if ck := sessionCookieOf(t, w); ck.Secure || ck.Path != "/" {
		t.Fatalf("plain-http public cookie: %+v", ck)
	}
}

func TestSessionOutlivesRestartNotPasswordChange(t *testing.T) {
	s, clk, dir := passwordServer(t, "127.0.0.1:7432", "")
	ck := sessionCookieOf(t, do(s.Handler(), loginForm(testUser, testPass)))

	again := New(s.App, s.Addr)
	again.Now = clk.now
	r := proxied(http.MethodGet, "/api/session")
	r.AddCookie(ck)
	if w := do(again.Handler(), r); w.Code != http.StatusOK {
		t.Fatalf("cookie after restart: %d", w.Code)
	}

	time.Sleep(20 * time.Millisecond) // a distinct mtime on coarse filesystems
	if err := SetPassword(dir, testUser, "another password!"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(2 * time.Second)
	r = proxied(http.MethodGet, "/api/session")
	r.AddCookie(ck)
	if w := do(s.Handler(), r); w.Code != http.StatusUnauthorized {
		t.Fatalf("cookie after password change: %d", w.Code)
	}
	if w := do(s.Handler(), loginForm(testUser, "another password!")); w.Code != http.StatusSeeOther {
		t.Fatalf("new password: %d", w.Code)
	}
}

func TestLoginLockout(t *testing.T) {
	s, clk, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()
	for i := 1; i < lockAfter; i++ {
		if w := do(h, loginForm(testUser, "wrong")); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := do(h, loginForm(testUser, "wrong")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: %d", lockAfter, w.Code)
	}
	if w := do(h, loginForm(testUser, testPass)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("right password while paused: %d", w.Code)
	}
	clk.t = clk.t.Add(lockFor + time.Second)
	if w := do(h, loginForm(testUser, testPass)); w.Code != http.StatusSeeOther {
		t.Fatalf("after the pause: %d", w.Code)
	}
}

func TestProxiedLockoutIsPerClient(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()
	for i := 0; i < lockAfter; i++ {
		do(h, proxiedLogin(testUser, "wrong", deckOrigin, "203.0.113.9"))
	}
	if w := do(h, proxiedLogin(testUser, testPass, deckOrigin, "203.0.113.9")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked client: %d", w.Code)
	}
	if w := do(h, proxiedLogin(testUser, testPass, deckOrigin, "203.0.113.10")); w.Code != http.StatusSeeOther {
		t.Fatalf("other client: %d", w.Code)
	}
}

func TestBearerTokenSkipsLogin(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "script-token")
	r := proxied(http.MethodGet, "/api/session")
	r.Header.Set("Authorization", "Bearer script-token")
	if w := do(s.Handler(), r); w.Code != http.StatusOK {
		t.Fatalf("bearer: %d", w.Code)
	}
	r.Header.Set("Authorization", "Bearer wrong")
	if w := do(s.Handler(), r); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: %d", w.Code)
	}
}

func TestUnreadableCredentialsFailClosed(t *testing.T) {
	s, _, dir := passwordServer(t, "127.0.0.1:7432", "")
	if err := os.Chmod(AuthPath(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(s.Handler(), proxied(http.MethodGet, "/api/health")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("world-readable credentials, proxied: %d", w.Code)
	}
	if w := do(s.Handler(), local(http.MethodGet, "/api/health")); w.Code != http.StatusOK {
		t.Fatalf("loopback does not depend on the credentials file: %d", w.Code)
	}
}

func TestSetPasswordRules(t *testing.T) {
	dir := t.TempDir()
	if err := SetPassword(dir, "bad user", testPass); err == nil {
		t.Fatal("space in user accepted")
	}
	if err := SetPassword(dir, testUser, "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := SetPassword(dir, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(AuthPath(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode: %v %v", info, err)
	}
	c, err := readCredentials(AuthPath(dir))
	if err != nil || !c.verify(testUser, testPass) || c.verify(testUser, testPass+"x") || c.verify("other", testPass) {
		t.Fatalf("verify: %v", err)
	}
}

func TestProxiedThroughDeck(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()
	if w := do(h, proxiedLogin(testUser, testPass, "https://evil.example", "")); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site login: %d", w.Code)
	}
	if w := do(h, proxiedLogin(testUser, testPass, "http://deck.xiayinchang.top", "")); w.Code != http.StatusForbidden {
		t.Fatalf("http origin for https page: %d", w.Code)
	}
	ck := sessionCookieOf(t, do(h, loginForm(testUser, testPass)))
	r := proxied(http.MethodGet, "/")
	r.AddCookie(ck)
	w := do(h, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"api/overview"`) || strings.Contains(w.Body.String(), `"/api/`) {
		t.Fatalf("page must use relative api paths: %d", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	nonce := strings.SplitN(csp, "'nonce-", 2)[1]
	if i := strings.Index(nonce, "'"); i >= 0 {
		nonce = nonce[:i]
	}
	if nonce == "" || !strings.Contains(w.Body.String(), `<script nonce="`+nonce+`">`) {
		t.Fatalf("csp %q does not match page", csp)
	}
}

// A no-referrer policy would make browsers post the login form with
// `Origin: null`, which the same-origin check rejects.
func TestReferrerPolicyKeepsFormOrigin(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	for _, r := range []*http.Request{local(http.MethodGet, "/"), proxied(http.MethodGet, "/login")} {
		w := do(s.Handler(), r)
		if got := w.Header().Values("Referrer-Policy"); len(got) != 1 || got[0] != "same-origin" {
			t.Fatalf("Referrer-Policy = %v", got)
		}
	}
	if w := do(s.Handler(), proxiedLogin(testUser, testPass, "null", "")); w.Code != http.StatusForbidden {
		t.Fatalf("null origin must stay rejected: %d", w.Code)
	}
}

func TestProxiedWithoutPasswordNeedsToken(t *testing.T) {
	s := New(&app.App{Cfg: config.Default()}, "127.0.0.1:7432")
	if w := do(s.Handler(), proxied(http.MethodGet, "/api/health")); w.Code != http.StatusForbidden {
		t.Fatalf("proxied, nothing configured: %d", w.Code)
	}
	s = New(&app.App{Cfg: config.Config{Web: config.Web{Enabled: true, Addr: "127.0.0.1:7432", Token: "tok"}}}, "127.0.0.1:7432")
	if w := do(s.Handler(), proxied(http.MethodGet, "/api/health")); w.Code != http.StatusUnauthorized {
		t.Fatalf("proxied, token not sent: %d", w.Code)
	}
	r := proxied(http.MethodGet, "/api/health")
	r.Header.Set("Authorization", "Bearer tok")
	if w := do(s.Handler(), r); w.Code != http.StatusOK {
		t.Fatalf("proxied with bearer: %d", w.Code)
	}
}

// Forwarded headers from a peer that is not loopback are ignored: the request
// is judged by its own Host and scheme.
func TestForwardedHeadersOnlyFromLoopback(t *testing.T) {
	s := New(&app.App{Cfg: config.Default()}, "127.0.0.1:7432")
	r := proxied(http.MethodGet, "/api/health")
	r.RemoteAddr = "192.0.2.1:1234"
	if w := do(s.Handler(), r); w.Code != http.StatusOK {
		t.Fatalf("plain loopback request: %d", w.Code)
	}
	r.Header.Set("Origin", deckOrigin)
	if w := do(s.Handler(), r); w.Code != http.StatusForbidden {
		t.Fatalf("forwarded origin from a stranger: %d", w.Code)
	}
}

func TestCleanPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "/": "", "/qswitch": "/qswitch", "/qswitch/": "/qswitch", "qswitch": "",
		"/a/b": "/a/b", "/../x": "", "/q?x": "", "/q#f": "", "/.hidden": "", "//x": "",
	} {
		if got := cleanPrefix(in); got != want {
			t.Errorf("cleanPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAlreadyServingWithPassword(t *testing.T) {
	a := setupApp(t)
	if err := SetPassword(a.DataDir, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(a, "127.0.0.1:7432").Handler())
	defer ts.Close()
	if !AlreadyServing(ts.Listener.Addr().String()) {
		t.Fatal("the local page is still qswitch")
	}
}
