package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
	"qswitch/internal/buildinfo"
	"qswitch/internal/quota"
)

type Server struct {
	App  *app.App
	Addr string
	// Now stands in for time.Now in tests.
	Now func() time.Time

	mu      sync.Mutex
	logins  map[string]*loginSess
	httpSrv *http.Server
	ctx     context.Context
	auth    authCache
	fails   failTable
}

type loginSess struct {
	ID        string
	Start     quota.CodexDeviceStart
	GrokStart quota.GrokDeviceStart
	Status    string
	Identity  adapter.Identity
	Err       string
	cancel    context.CancelFunc
}

func New(a *app.App, addr string) *Server {
	s := &Server{App: a, Addr: addr, logins: map[string]*loginSess{}, ctx: context.Background()}
	if a != nil && a.DataDir != "" {
		s.auth.path = AuthPath(a.DataDir)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": buildinfo.Version})
	})
	mux.HandleFunc("GET /api/session", s.getSession)
	mux.HandleFunc("GET /api/overview", s.getOverview)
	mux.HandleFunc("POST /api/capture", s.postCapture)
	mux.HandleFunc("POST /api/probe", s.postProbe)
	mux.HandleFunc("POST /api/forget", s.postForget)
	mux.HandleFunc("POST /api/switch", s.postSwitch)
	mux.HandleFunc("POST /api/login/codex", s.postLoginCodex)
	mux.HandleFunc("GET /api/login/codex", s.getLogin)
	mux.HandleFunc("POST /api/login/codex/cancel", s.postLoginCancel)
	mux.HandleFunc("POST /api/login/grok", s.postLoginGrok)
	mux.HandleFunc("GET /api/login/grok", s.getLogin)
	mux.HandleFunc("POST /api/login/grok/cancel", s.postLoginCancel)
	mux.HandleFunc("GET /login", s.getLoginPage)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.postLogout)
	mux.HandleFunc("GET /", s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// Not no-referrer: under that policy browsers send `Origin: null`
		// with form posts (login, logout), which the same-origin check
		// below would reject. same-origin keeps the Referer on this site.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		if !s.admit(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// admit decides whether a request reaches the handlers:
//   - on a loopback listener the Host must be loopback (DNS rebinding) unless
//     a reverse proxy on loopback names the real host in X-Forwarded-Host;
//   - a request that carries an Origin must be same-origin as the browser
//     sees it (writes from other sites);
//   - with a password set, everything but GET /api/health and the login page
//     needs the session cookie, or the Bearer token for scripts;
//   - without one, a public listener or a proxied request needs the Bearer
//     token; a plain loopback listener stays open to the SSH tunnel.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) bool {
	public := CheckLoopback(s.Addr) != nil
	x := externalOf(r)
	if !public && !x.proxied && !loopbackHost(r.Host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && !validOrigin(r, o) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	creds, err := s.credentials()
	if err != nil {
		s.logf("%v", err)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "login configuration unreadable; see the daemon log", http.StatusServiceUnavailable)
		return false
	}
	tok := s.token()
	if creds != nil {
		if r.URL.Path == "/login" || (r.Method == http.MethodGet && r.URL.Path == "/api/health") {
			return true
		}
		if (tok != "" && bearerOK(r, tok)) || s.sessionOK(r, creds) {
			return true
		}
		s.unauthorized(w, r, x)
		return false
	}
	if public || x.proxied {
		if tok == "" {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "forbidden: set a page password (qswitch web set-password) or [web] token", http.StatusForbidden)
			return false
		}
		if !bearerOK(r, tok) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
	}
	return true
}

// token is the shared secret for scripts (Authorization: Bearer). It is the
// only way in on a public listener or through the proxy when no password is
// set; it is ignored on a plain loopback listener.
func (s *Server) token() string {
	if s.App == nil {
		return ""
	}
	return strings.TrimSpace(s.App.Cfg.Web.Token)
}

func (s *Server) Run(ctx context.Context) error {
	if err := CheckLoopback(s.Addr); err != nil {
		if creds, cerr := s.credentials(); s.token() == "" && (cerr != nil || creds == nil) {
			return fmt.Errorf("%w (set a page password with qswitch web set-password, or [web] token, to bind a public address)", err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	hs := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 8 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	s.httpSrv = hs
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hs.Shutdown(c)
	}()
	err = hs.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// AlreadyServing reports whether a qswitch page answers on addr; a login
// prompt counts.
func AlreadyServing(addr string) bool {
	if CheckLoopback(addr) != nil {
		return false
	}
	c := &http.Client{Timeout: 800 * time.Millisecond}
	res, err := c.Get("http://" + addr + "/api/overview")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != http.StatusUnauthorized {
		return false
	}
	if res.Header.Get("X-Qswitch") == "1" {
		return true
	}
	if res.StatusCode != 200 {
		return false
	}
	var body struct {
		Tools json.RawMessage `json:"tools"`
	}
	if json.NewDecoder(res.Body).Decode(&body) != nil {
		return false
	}
	return len(body.Tools) > 0
}

func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("web: bad addr %q", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("web: bind loopback only, got %s", host)
	}
	return nil
}

func localRequest(r *http.Request) bool {
	if !loopbackHost(r.Host) {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return validOrigin(r, o)
	}
	return true
}

// validOrigin compares an Origin header with the origin the browser used,
// which behind the proxy is the forwarded one.
func validOrigin(r *http.Request, o string) bool {
	u, err := url.Parse(o)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	x := externalOf(r)
	return u.Scheme == x.scheme && strings.EqualFold(u.Host, x.host)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Qswitch", "1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		status := http.StatusBadRequest
		var limitErr *http.MaxBytesError
		if errors.As(err, &limitErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, err.Error())
		return false
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, dst); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return false
		}
	}
	return true
}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.App.Overview())
}

func (s *Server) postCapture(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	t, err := adapter.ParseTool(req.Tool)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	blobs, warn, err := s.App.Capture(t)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var ids []map[string]any
	for _, b := range blobs {
		ids = append(ids, map[string]any{
			"tool": b.Tool, "email": b.Identity.Email, "phone": b.Identity.Phone, "display_name": b.Identity.DisplayName, "stable_id": b.Identity.StableID,
			"incomplete": b.Incomplete, "stale_cli": b.StaleCLI,
		})
	}
	ws := make([]string, 0, len(warn))
	for _, x := range warn {
		ws = append(ws, string(x))
	}
	writeJSON(w, 200, map[string]any{"captured": ids, "warnings": ws})
}

func (s *Server) postProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
		ID   string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if req.ID == "" {
		var tools []adapter.Tool
		if strings.TrimSpace(req.Tool) == "" {
			tools = adapter.AllTools()
		} else {
			t, err := adapter.ParseTool(req.Tool)
			if err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			tools = []adapter.Tool{t}
		}
		for _, t := range tools {
			s.App.KeepAlive(ctx, t)
			s.App.ProbeLocal(t)
			s.App.ProbeHTTP(ctx, t, true)
			s.App.ProbeRecovered(ctx, t, true)
		}
		writeJSON(w, 200, s.App.Overview())
		return
	}
	t, err := adapter.ParseTool(req.Tool)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	res := s.App.ProbeAccount(ctx, t, req.ID)
	writeJSON(w, 200, map[string]any{
		"buckets": res.Buckets,
		"updated": res.Class == quota.OK || res.Class == quota.Soft || res.Class == quota.Exhausted,
		"class":   res.Class, "used_pct": res.UsedPct, "resets_at": res.ResetsAt, "source": res.Source,
	})
}

func (s *Server) postForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
		ID   string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	t, err := adapter.ParseTool(req.Tool)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeErr(w, 400, "missing id")
		return
	}
	if err := s.App.Forget(t, req.ID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "forgotten"})
}

func (s *Server) postSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool    string `json:"tool"`
		ID      string `json:"id"`
		KillCLI bool   `json:"kill_cli"`
		CLIOnly bool   `json:"cli_only"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	t, err := adapter.ParseTool(req.Tool)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	opts := adapter.RestoreOpts{CLIOnly: req.CLIOnly}
	if err := s.App.Switch(t, req.ID, opts, req.KillCLI); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "switched"})
}

func (s *Server) postLoginCodex(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(s.loginContext(), 16*time.Minute)
	st, err := s.App.BeginCodexDeviceLogin(ctx)
	if err != nil {
		cancel()
		writeErr(w, 400, err.Error())
		return
	}
	id := newID()
	sess := &loginSess{ID: id, Start: st, Status: "pending", cancel: cancel}
	s.mu.Lock()
	s.logins[id] = sess
	s.mu.Unlock()
	go func() {
		defer cancel()
		ident, err := s.App.CompleteCodexDeviceLogin(ctx, st)
		s.finishLogin(id, ident, err)
	}()
	writeJSON(w, 200, map[string]any{
		"id":         id,
		"user_code":  st.UserCode,
		"url":        quota.CodexDeviceURL,
		"expires_at": st.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) postLoginGrok(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(s.loginContext(), 16*time.Minute)
	st, err := s.App.BeginGrokDeviceLogin(ctx)
	if err != nil {
		cancel()
		writeErr(w, 400, err.Error())
		return
	}
	id := newID()
	sess := &loginSess{ID: id, GrokStart: st, Status: "pending", cancel: cancel}
	s.mu.Lock()
	s.logins[id] = sess
	s.mu.Unlock()
	go func() {
		defer cancel()
		ident, err := s.App.CompleteGrokDeviceLogin(ctx, st)
		s.finishLogin(id, ident, err)
	}()
	url := st.VerifyURL
	if url == "" {
		url = quota.GrokDeviceURL
	}
	writeJSON(w, 200, map[string]any{
		"id":         id,
		"user_code":  st.UserCode,
		"url":        url,
		"expires_at": st.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) loginContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

func (s *Server) finishLogin(id string, ident adapter.Identity, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.logins[id]
	if cur == nil || cur.Status != "pending" {
		return
	}
	if err != nil {
		cur.Status = "error"
		cur.Err = err.Error()
		return
	}
	cur.Status = "ok"
	cur.Identity = ident
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.mu.Lock()
	sess := s.logins[id]
	if sess == nil {
		s.mu.Unlock()
		writeErr(w, 404, "login session not found")
		return
	}
	snapshot := *sess
	s.mu.Unlock()
	sess = &snapshot
	out := map[string]any{"id": sess.ID, "status": sess.Status}
	if sess.Status == "ok" {
		out["email"] = sess.Identity.Email
		out["stable_id"] = sess.Identity.StableID
		out["plan"] = sess.Identity.PlanHint
	}
	if sess.Status == "error" {
		out["error"] = sess.Err
	}
	writeJSON(w, 200, out)
}

func (s *Server) postLoginCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	sess := s.logins[req.ID]
	if sess != nil && sess.Status == "pending" && sess.cancel != nil {
		sess.cancel()
		sess.Status = "error"
		sess.Err = "cancelled"
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]string{"ok": "cancelled"})
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		http.Error(w, "ui missing", 500)
		return
	}
	nonce := newNonce()
	page := strings.Replace(string(b), "<script>", `<script nonce="`+nonce+`">`, 1)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Qswitch", "1")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	_, _ = io.WriteString(w, page)
}

func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawStdEncoding.EncodeToString(b[:])
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
