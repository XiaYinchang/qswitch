package web

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"qswitch/internal/livefile"
)

// Browser login for the page. `qswitch web set-password <user>` writes
// web-auth.json into the data dir; from then on every request that comes
// through a reverse proxy or a public listener, except GET /api/health,
// needs a session cookie from POST /login, or the Bearer token for scripts.
// The loopback listener itself (the local page, an SSH tunnel) never asks. The cookie is an HMAC over its expiry under a key that
// changes with the password, so a new password logs every browser out and a
// daemon restart logs nobody out. Five wrong passwords in a row from one
// address pause its logins for fifteen minutes.
const (
	AuthFile       = "web-auth.json"
	MinPasswordLen = 10
	sessionCookie  = "qswitch_session"
	sessionTTL     = 30 * 24 * time.Hour
	lockAfter      = 5
	lockFor        = 15 * time.Minute
	pbkdf2Iters    = 600_000
	failTableCap   = 4096
)

type Credentials struct {
	User       string    `json:"user"`
	Hash       string    `json:"hash"`        // pbkdf2-sha256$<iterations>$<salt>$<key>, base64
	SessionKey string    `json:"session_key"` // hex; rotated with the password
	UpdatedAt  time.Time `json:"updated_at"`
}

var userRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func AuthPath(dataDir string) string { return filepath.Join(dataDir, AuthFile) }

// SetPassword replaces the page login. Existing sessions stop working.
func SetPassword(dataDir, user, password string) error {
	if !userRe.MatchString(user) {
		return errors.New("用户名只能用 1–64 个字母、数字或 . _ @ -")
	}
	if len(password) < MinPasswordLen {
		return fmt.Errorf("密码至少 %d 个字符", MinPasswordLen)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iters, 32)
	if err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	c := Credentials{
		User:       user,
		Hash:       fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iters, b64(salt), b64(dk)),
		SessionKey: hex.EncodeToString(key),
		UpdatedAt:  time.Now().UTC().Truncate(time.Second),
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return livefile.AtomicWrite(AuthPath(dataDir), append(b, '\n'), 0o600)
}

func readCredentials(path string) (*Credentials, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s permissions %04o too open; want 0600", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !userRe.MatchString(c.User) || len(c.key()) != 32 {
		return nil, fmt.Errorf("%s: malformed", path)
	}
	if _, _, _, err := c.hashParts(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Credentials) key() []byte {
	k, err := hex.DecodeString(c.SessionKey)
	if err != nil {
		return nil
	}
	return k
}

func (c *Credentials) hashParts() (iters int, salt, want []byte, err error) {
	p := strings.Split(c.Hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return 0, nil, nil, errors.New("unsupported password hash")
	}
	iters, err = strconv.Atoi(p[1])
	if err != nil || iters < 1000 {
		return 0, nil, nil, errors.New("bad hash iterations")
	}
	if salt, err = unb64(p[2]); err != nil {
		return 0, nil, nil, err
	}
	if want, err = unb64(p[3]); err != nil {
		return 0, nil, nil, err
	}
	return iters, salt, want, nil
}

// verify always derives the key, so a wrong user name costs as much as a
// wrong password.
func (c *Credentials) verify(user, password string) bool {
	iters, salt, want, err := c.hashParts()
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iters, len(want))
	if err != nil {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(c.User)) == 1
	return subtle.ConstantTimeCompare(got, want) == 1 && userOK
}

func (c *Credentials) issue(now time.Time) string {
	var p [16]byte
	binary.BigEndian.PutUint64(p[:8], uint64(now.Add(sessionTTL).Unix()))
	_, _ = rand.Read(p[8:])
	return b64url(p[:]) + "." + b64url(c.sign(p[:]))
}

func (c *Credentials) valid(cookie string, now time.Time) bool {
	i := strings.IndexByte(cookie, '.')
	if i < 0 {
		return false
	}
	p, err := base64.RawURLEncoding.DecodeString(cookie[:i])
	if err != nil || len(p) != 16 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(cookie[i+1:])
	if err != nil || !hmac.Equal(sig, c.sign(p)) {
		return false
	}
	return now.Unix() < int64(binary.BigEndian.Uint64(p[:8]))
}

func (c *Credentials) sign(p []byte) []byte {
	m := hmac.New(sha256.New, c.key())
	m.Write(p)
	return m.Sum(nil)
}

func b64(b []byte) string            { return base64.StdEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
func b64url(b []byte) string         { return base64.RawURLEncoding.EncodeToString(b) }

// authCache re-reads web-auth.json when it changes, checking at most once a
// second, so set-password takes effect without a restart.
type authCache struct {
	mu      sync.Mutex
	path    string
	creds   *Credentials
	err     error
	mtime   time.Time
	size    int64
	checked time.Time
}

func (a *authCache) load(now time.Time) (*Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.path == "" {
		return nil, nil
	}
	if !a.checked.IsZero() && now.Sub(a.checked) < time.Second && now.After(a.checked) {
		return a.creds, a.err
	}
	a.checked = now
	info, err := os.Stat(a.path)
	if err != nil {
		a.creds, a.err, a.mtime, a.size = nil, nil, time.Time{}, 0
		if !os.IsNotExist(err) {
			a.err = err
		}
		return a.creds, a.err
	}
	if a.err == nil && a.creds != nil && info.ModTime().Equal(a.mtime) && info.Size() == a.size {
		return a.creds, nil
	}
	a.mtime, a.size = info.ModTime(), info.Size()
	a.creds, a.err = readCredentials(a.path)
	return a.creds, a.err
}

// failTable counts wrong passwords per client address.
type failTable struct {
	mu   sync.Mutex
	recs map[string]*failRec
}

type failRec struct {
	n     int
	last  time.Time
	until time.Time
}

// locked reports how much longer a client has to wait.
func (f *failTable) locked(client string, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.recs[client]
	if r == nil || !now.Before(r.until) {
		return 0
	}
	return r.until.Sub(now)
}

// fail records a wrong password and returns the lock it caused, if any.
func (f *failTable) fail(client string, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recs == nil {
		f.recs = map[string]*failRec{}
	}
	if len(f.recs) >= failTableCap {
		for k, r := range f.recs {
			if now.Sub(r.last) > lockFor && !now.Before(r.until) {
				delete(f.recs, k)
			}
		}
	}
	r := f.recs[client]
	if r == nil {
		r = &failRec{}
		f.recs[client] = r
	}
	if !r.until.IsZero() && !now.Before(r.until) {
		r.n, r.until = 0, time.Time{}
	}
	r.n++
	r.last = now
	if r.n >= lockAfter {
		r.until = now.Add(lockFor)
		return lockFor
	}
	return 0
}

func (f *failTable) reset(client string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.recs, client)
}

// external is the request as the browser sees it. Behind a reverse proxy on
// loopback (the Deck tunnel) that is X-Forwarded-Host / -Proto / -Prefix /
// -For; those headers are ignored from any other peer.
type external struct {
	proxied bool
	scheme  string
	host    string
	prefix  string // "" or "/qswitch": where the page is mounted
	client  string // address for the lockout table
}

func externalOf(r *http.Request) external {
	x := external{scheme: "http", host: r.Host, client: ipOf(r.RemoteAddr)}
	if r.TLS != nil {
		x.scheme = "https"
	}
	fh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if fh == "" || !isLoopbackIP(x.client) || !validHost(fh) {
		return x
	}
	x.proxied, x.host = true, fh
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		x.scheme = "https"
	} else {
		x.scheme = "http"
	}
	x.prefix = cleanPrefix(r.Header.Get("X-Forwarded-Prefix"))
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		last := strings.TrimSpace(xff[strings.LastIndexByte(xff, ',')+1:])
		if net.ParseIP(last) != nil {
			x.client = last
		}
	}
	return x
}

// path returns an absolute path under the mount point.
func (x external) path(p string) string { return x.prefix + p }

func ipOf(remote string) string {
	if h, _, err := net.SplitHostPort(remote); err == nil {
		return h
	}
	return remote
}

func isLoopbackIP(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.IsLoopback()
}

func loopbackHost(h string) bool {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func validHost(h string) bool {
	u, err := url.Parse("http://" + h + "/")
	return err == nil && u.Host == h && !strings.ContainsAny(h, " /\\?#@")
}

var prefixRe = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

func cleanPrefix(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" || !prefixRe.MatchString(p) || strings.Contains(p, "/.") {
		return ""
	}
	return p
}

func bearerOK(r *http.Request, tok string) bool {
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, "Bearer ") && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(tok)) == 1
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) credentials() (*Credentials, error) { return s.auth.load(s.now()) }

func (s *Server) logf(format string, args ...any) {
	if s.App != nil && s.App.Log != nil {
		s.App.Log("web: "+format, args...)
	}
}

// loginRequired reports whether this request must carry a session: a
// password is set and the request came through the proxy or a public
// listener. The loopback listener (the local page, the SSH tunnel) never asks.
func (s *Server) loginRequired(r *http.Request) (*Credentials, bool) {
	c, err := s.credentials()
	if err != nil || c == nil {
		return nil, false
	}
	return c, CheckLoopback(s.Addr) != nil || externalOf(r).proxied
}

func (s *Server) sessionOK(r *http.Request, c *Credentials) bool {
	ck, err := r.Cookie(sessionCookie)
	return err == nil && c.valid(ck.Value, s.now())
}

func (s *Server) setSession(w http.ResponseWriter, x external, value string, maxAge int) {
	path := x.prefix
	if path == "" {
		path = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   x.scheme == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// unauthorized sends a browser to the login page and everything else a 401.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request, x external) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && (r.URL.Path == "/" || strings.Contains(r.Header.Get("Accept"), "text/html")) {
		http.Redirect(w, r, x.path("/login"), http.StatusSeeOther)
		return
	}
	writeErr(w, http.StatusUnauthorized, "unauthorized")
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	c, need := s.loginRequired(r)
	out := map[string]any{"login_required": need}
	if need {
		out["user"] = c.User
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getLoginPage(w http.ResponseWriter, r *http.Request) {
	c, need := s.loginRequired(r)
	if c == nil {
		http.NotFound(w, r)
		return
	}
	x := externalOf(r)
	if !need || s.sessionOK(r, c) {
		http.Redirect(w, r, x.path("/"), http.StatusSeeOther)
		return
	}
	s.loginPage(w, http.StatusOK, x, "", "")
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	c, need := s.loginRequired(r)
	if c == nil {
		http.NotFound(w, r)
		return
	}
	x := externalOf(r)
	if !need {
		http.Redirect(w, r, x.path("/"), http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.loginPage(w, http.StatusBadRequest, x, "", "表单无法读取")
		return
	}
	user := strings.TrimSpace(r.PostForm.Get("user"))
	password := r.PostForm.Get("password")
	now := s.now()
	if wait := s.fails.locked(x.client, now); wait > 0 {
		s.loginPage(w, http.StatusTooManyRequests, x, user, lockMessage(wait))
		return
	}
	if !c.verify(user, password) {
		if lock := s.fails.fail(x.client, now); lock > 0 {
			s.logf("login paused for %s after %d wrong passwords", x.client, lockAfter)
			s.loginPage(w, http.StatusTooManyRequests, x, user, lockMessage(lock))
			return
		}
		s.loginPage(w, http.StatusUnauthorized, x, user, "用户名或密码不对")
		return
	}
	s.fails.reset(x.client)
	s.setSession(w, x, c.issue(now), int(sessionTTL/time.Second))
	s.logf("login ok user=%s from %s", c.User, x.client)
	http.Redirect(w, r, x.path("/"), http.StatusSeeOther)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	x := externalOf(r)
	s.setSession(w, x, "", -1)
	if _, need := s.loginRequired(r); need {
		http.Redirect(w, r, x.path("/login"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, x.path("/"), http.StatusSeeOther)
}

func lockMessage(wait time.Duration) string {
	m := int((wait + time.Minute - 1) / time.Minute)
	if m < 1 {
		m = 1
	}
	return fmt.Sprintf("连续输错 %d 次，%d 分钟后再试", lockAfter, m)
}

func (s *Server) loginPage(w http.ResponseWriter, status int, x external, user, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Qswitch", "1")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	errLine := ""
	if msg != "" {
		errLine = `<p class="err" role="alert">` + html.EscapeString(msg) + `</p>`
	}
	fmt.Fprintf(w, loginHTML, x.path("/login"), html.EscapeString(user), errLine, lockAfter, int(lockFor/time.Minute))
}

const loginHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>登录 · qswitch</title>
<style>
  :root { --bg: #111318; --card: #1a1d24; --line: #2a2e38; --text: #e8eaed; --muted: #9aa0a6; --bad: #f07178; --live: #7aa2f7; }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--text);
    font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, "PingFang SC", sans-serif; }
  main { width: min(360px, calc(100vw - 32px)); background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 22px 22px 18px; }
  h1 { margin: 0 0 4px; font-size: 18px; }
  .lead { margin: 0 0 16px; color: var(--muted); font-size: 13px; }
  label { display: block; margin-bottom: 12px; color: var(--muted); font-size: 12px; }
  input { display: block; width: 100%%; margin-top: 4px; padding: 8px 10px; font: inherit; font-size: 14px; color: var(--text);
    background: var(--bg); border: 1px solid var(--line); border-radius: 8px; }
  input:focus { outline: none; border-color: var(--live); }
  button { width: 100%%; margin-top: 4px; padding: 8px 10px; font: inherit; color: var(--live); background: transparent;
    border: 1px solid var(--live); border-radius: 8px; cursor: pointer; }
  button:hover { background: #7aa2f71a; }
  .err { margin: 0 0 12px; color: var(--bad); font-size: 13px; }
  .note { margin: 14px 0 0; color: var(--muted); font-size: 12px; }
</style>
</head>
<body>
<main>
  <h1>qswitch</h1>
  <p class="lead">账号余量页面。登录后继续。</p>
  <form method="post" action="%s">
    <label>用户名<input name="user" value="%s" autocomplete="username" autocapitalize="none" spellcheck="false" required autofocus></label>
    <label>密码<input type="password" name="password" autocomplete="current-password" required></label>
    %s
    <button type="submit">登录</button>
  </form>
  <p class="note">同一来源连续输错 %d 次密码会暂停登录 %d 分钟。</p>
</main>
</body>
</html>
`
