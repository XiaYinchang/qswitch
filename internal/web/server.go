package web

import (
	"context"
	"crypto/rand"
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
	"qswitch/internal/quota"
)

type Server struct {
	App  *app.App
	Addr string

	mu      sync.Mutex
	logins  map[string]*loginSess
	httpSrv *http.Server
}

type loginSess struct {
	ID       string
	Start    quota.CodexDeviceStart
	Status   string
	Identity adapter.Identity
	Err      string
	cancel   context.CancelFunc
}

func New(a *app.App, addr string) *Server {
	return &Server{App: a, Addr: addr, logins: map[string]*loginSess{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", s.getOverview)
	mux.HandleFunc("POST /api/capture", s.postCapture)
	mux.HandleFunc("POST /api/probe", s.postProbe)
	mux.HandleFunc("POST /api/forget", s.postForget)
	mux.HandleFunc("POST /api/switch", s.postSwitch)
	mux.HandleFunc("POST /api/login/codex", s.postLoginCodex)
	mux.HandleFunc("GET /api/login/codex", s.getLoginCodex)
	mux.HandleFunc("POST /api/login/codex/cancel", s.postLoginCancel)
	mux.HandleFunc("GET /", s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) Run(ctx context.Context) error {
	if err := CheckLoopback(s.Addr); err != nil {
		return err
	}
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
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	if h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil {
			return false
		}
		oh := u.Hostname()
		return oh == "127.0.0.1" || oh == "localhost" || oh == "::1"
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, dst)
}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.App.Overview())
}

func (s *Server) postCapture(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
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
			"email": b.Identity.Email, "stable_id": b.Identity.StableID,
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
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	t, err := adapter.ParseTool(req.Tool)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if req.ID == "" {
		s.App.KeepAlive(ctx, t)
		s.App.ProbeLocal(t)
		s.App.ProbeHTTP(ctx, t, true)
		s.App.ProbeRecovered(ctx, t, true)
		writeJSON(w, 200, s.App.Overview())
		return
	}
	res := s.App.ProbeAccount(ctx, t, req.ID)
	writeJSON(w, 200, map[string]any{
		"class": res.Class, "used_pct": res.UsedPct, "resets_at": res.ResetsAt, "source": res.Source,
	})
}

func (s *Server) postForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
		ID   string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
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
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
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
	ctx, cancel := context.WithTimeout(context.Background(), 16*time.Minute)
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
		ident, err := s.App.CompleteCodexDeviceLogin(ctx, st)
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.logins[id]
		if cur == nil {
			return
		}
		if err != nil {
			cur.Status = "error"
			cur.Err = err.Error()
			return
		}
		cur.Status = "ok"
		cur.Identity = ident
	}()
	writeJSON(w, 200, map[string]any{
		"id":         id,
		"user_code":  st.UserCode,
		"url":        quota.CodexDeviceURL,
		"expires_at": st.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) getLoginCodex(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.mu.Lock()
	sess := s.logins[id]
	s.mu.Unlock()
	if sess == nil {
		writeErr(w, 404, "login session not found")
		return
	}
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
	_ = readJSON(r, &req)
	s.mu.Lock()
	sess := s.logins[req.ID]
	if sess != nil && sess.cancel != nil {
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
