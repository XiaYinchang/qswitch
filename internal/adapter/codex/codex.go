package codex

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/busy"
	"qswitch/internal/identityjwt"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
	"qswitch/internal/snapshot"
)

type Adapter struct {
	List func() ([]adapter.Proc, error)
}

func (a Adapter) Name() adapter.Tool { return adapter.Codex }

func (a Adapter) LivePaths(home string) []string {
	return []string{
		filepath.Join(home, ".codex", "auth.json"),
		filepath.Join(home, snapshot.ChatGPTRelRoot, "Cookies"),
	}
}

func authPath(home string) string { return filepath.Join(home, ".codex", "auth.json") }

func (a Adapter) Fingerprint(home string) ([32]byte, error) {
	h := sha256.New()
	n := 0
	for _, p := range []string{
		authPath(home),
		filepath.Join(home, snapshot.ChatGPTRelRoot, "Cookies"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		h.Write(b)
		n++
	}
	if n == 0 {
		return [32]byte{}, os.ErrNotExist
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func (a Adapter) procs() ([]adapter.Proc, error) {
	if a.List != nil {
		return a.List()
	}
	return (busy.PS{}).List()
}

func (a Adapter) ManualBlockers(home string) (adapter.Holders, error) {
	list, err := a.procs()
	if err != nil {
		return adapter.Holders{Why: []string{"ps failed"}}, err
	}
	cli := busy.CodexCLI(list)
	h := adapter.Holders{Manual: cli}
	if len(cli) > 0 {
		h.Why = append(h.Why, "codex cli running")
	}
	return h, nil
}

func (a Adapter) AutoBlockers(home string) (adapter.Holders, error) {
	h, err := a.ManualBlockers(home)
	if err != nil {
		return h, err
	}
	list, err := a.procs()
	if err != nil {
		return h, err
	}
	cg := busy.ChatGPTApp(list)
	if len(cg) > 0 {
		h.AutoExtra = cg
		h.ChatGPTApp = true
		h.Why = append(h.Why, "chatgpt.app running")
	}
	return h, nil
}

func (a Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	raw, err := os.ReadFile(authPath(home))
	if err != nil {
		if desk, e2 := captureDesktop(home); e2 == nil {
			return []adapter.Blob{desk}, nil, nil
		}
		return nil, nil, err
	}
	id, warn, err := identityFromRaw(raw)
	if err != nil {
		return nil, warn, err
	}
	b, err := BlobFromAuthJSON(raw, time.Now())
	if err != nil {
		return nil, warn, err
	}
	b.Identity = id
	if z, zerr := snapshot.Pack(filepath.Join(home, snapshot.ChatGPTRelRoot), snapshot.ChatGPTRels); zerr == nil {
		var env map[string]any
		if json.Unmarshal(b.Payload, &env) == nil {
			env["desktop_zip"] = z
			env["desktop_rel_root"] = snapshot.ChatGPTRelRoot
			if payload, merr := json.Marshal(env); merr == nil {
				b.Payload = payload
			}
		}
	}
	return []adapter.Blob{b}, warn, nil
}

func BlobFromAuthJSON(raw []byte, now time.Time) (adapter.Blob, error) {
	id, _, err := identityFromRaw(raw)
	if err != nil {
		return adapter.Blob{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"kind":      "codex.auth.json.v1",
		"identity":  id,
		"auth_json": json.RawMessage(raw),
	})
	if err != nil {
		return adapter.Blob{}, err
	}
	return adapter.Blob{Tool: adapter.Codex, Identity: id, CapturedAt: now, Payload: payload}, nil
}

func captureDesktop(home string) (adapter.Blob, error) {
	root := filepath.Join(home, snapshot.ChatGPTRelRoot)
	z, err := snapshot.Pack(root, snapshot.ChatGPTRels)
	if err != nil {
		return adapter.Blob{}, err
	}
	id := adapter.Identity{Tool: adapter.Codex, StableID: "desktop:chatgpt", Email: "chatgpt.app", PlanHint: "desktop"}
	raw, err := json.Marshal(map[string]any{
		"kind": snapshot.KindDesktop, "identity": id, "app": "chatgpt",
		"rel_root": snapshot.ChatGPTRelRoot, "zip": z,
	})
	if err != nil {
		return adapter.Blob{}, err
	}
	return adapter.Blob{Tool: adapter.Codex, Identity: id, CapturedAt: time.Now(), Payload: raw}, nil
}

func identityFromRaw(raw []byte) (adapter.Identity, []adapter.Warning, error) {
	var warn []adapter.Warning
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return adapter.Identity{}, nil, err
	}
	mode, _ := root["auth_mode"].(string)
	if mode == "ApiKey" || mode == "api_key" {
		warn = append(warn, adapter.WarnApiKeyMode)
	}
	tokens, _ := root["tokens"].(map[string]any)
	id := adapter.Identity{Tool: adapter.Codex}
	if tokens != nil {
		if s, ok := tokens["account_id"].(string); ok {
			id.StableID = s
		}
		for _, tokName := range []string{"id_token", "access_token"} {
			s, _ := tokens[tokName].(string)
			if s == "" {
				continue
			}
			cl := identityjwt.Parse(s)
			if id.Email == "" {
				id.Email = cl.Email
			}
			if id.StableID == "" {
				id.StableID = cl.AccountID
			}
			if id.PlanHint == "" {
				id.PlanHint = cl.Plan
			}
		}
	}
	if id.StableID == "" {
		return id, warn, errors.New("codex: missing account_id")
	}
	id.DisplayName = workspaceLabel(id.Email, id.PlanHint, id.StableID)
	return id, warn, nil
}

func workspaceLabel(email, plan, accountID string) string {
	plan = strings.TrimSpace(strings.ToLower(plan))
	if plan == "" {
		plan = "unknown"
	}
	short := accountID
	if len(short) > 8 {
		short = short[:8]
	}
	if email == "" {
		email = short
	}
	return email + "/" + plan + "/" + short
}

func (a Adapter) IdentityOf(blob adapter.Blob) (adapter.Identity, error) {
	if blob.Identity.StableID != "" {
		return blob.Identity, nil
	}
	var env struct {
		Identity adapter.Identity `json:"identity"`
		AuthJSON json.RawMessage  `json:"auth_json"`
	}
	if err := json.Unmarshal(blob.Payload, &env); err != nil {
		return adapter.Identity{}, err
	}
	if env.Identity.StableID != "" {
		env.Identity.Tool = adapter.Codex
		return env.Identity, nil
	}
	id, _, err := identityFromRaw(env.AuthJSON)
	return id, err
}

func (a Adapter) Restore(home string, blob adapter.Blob, _ adapter.RestoreOpts) error {
	if isDesktop(blob) {
		list, err := a.procs()
		if err != nil {
			return err
		}
		if apps := busy.ChatGPTApp(list); len(apps) > 0 {
			return &adapter.BusyError{Holders: adapter.Holders{Manual: apps, ChatGPTApp: true, Why: []string{"quit ChatGPT.app to restore desktop session"}}}
		}
		return restoreDesktop(home, blob)
	}
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	raw, err := authJSON(blob)
	if err != nil {
		return err
	}
	if err := livefile.AtomicWrite(authPath(home), raw, 0o600); err != nil {
		return err
	}
	return restoreAttachedDesktop(home, blob, a)
}

func restoreAttachedDesktop(home string, blob adapter.Blob, a Adapter) error {
	var env struct {
		DesktopZip []byte `json:"desktop_zip"`
		RelRoot    string `json:"desktop_rel_root"`
	}
	if json.Unmarshal(blob.Payload, &env) != nil || len(env.DesktopZip) == 0 {
		return nil
	}
	list, err := a.procs()
	if err != nil {
		return nil
	}
	if len(busy.ChatGPTApp(list)) > 0 {
		return nil
	}
	if env.RelRoot == "" {
		env.RelRoot = snapshot.ChatGPTRelRoot
	}
	return snapshot.Unpack(filepath.Join(home, env.RelRoot), env.DesktopZip)
}

func isDesktop(blob adapter.Blob) bool {
	var h struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(blob.Payload, &h)
	return h.Kind == snapshot.KindDesktop
}

func restoreDesktop(home string, blob adapter.Blob) error {
	var env struct {
		RelRoot string `json:"rel_root"`
		Zip     []byte `json:"zip"`
	}
	if err := json.Unmarshal(blob.Payload, &env); err != nil {
		return err
	}
	if env.RelRoot == "" || len(env.Zip) == 0 {
		return errors.New("codex: empty desktop snapshot")
	}
	return snapshot.Unpack(filepath.Join(home, env.RelRoot), env.Zip)
}

func authJSON(blob adapter.Blob) ([]byte, error) {
	var env struct {
		AuthJSON json.RawMessage `json:"auth_json"`
	}
	if err := json.Unmarshal(blob.Payload, &env); err != nil {
		return nil, err
	}
	if len(env.AuthJSON) == 0 {
		return nil, errors.New("codex: empty auth_json")
	}
	return env.AuthJSON, nil
}

func (a Adapter) Idle(home string, grace time.Duration) bool {
	return quota.DirIdle(filepath.Join(home, ".codex", "sessions"), grace, time.Now())
}

func (a Adapter) KillCLI(home string) error {
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	var first error
	for _, p := range h.Manual {
		if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func Tokens(blob adapter.Blob) (access, account string, err error) {
	a, err := ReadAuth(blob)
	if err != nil {
		return "", "", err
	}
	if a.Access == "" {
		return "", "", errors.New("no tokens")
	}
	return a.Access, a.AccountID, nil
}

const accessSkew = 10 * time.Minute
const lastRefreshMax = 8 * 24 * time.Hour

type AuthFile struct {
	Access      string
	Refresh     string
	IDToken     string
	AccountID   string
	UserID      string
	LastRefresh time.Time
}

func ReadAuth(blob adapter.Blob) (AuthFile, error) {
	raw, err := authJSON(blob)
	if err != nil {
		return AuthFile{}, err
	}
	return parseAuthJSON(raw)
}

func ReadAuthBytes(raw []byte) (AuthFile, error) { return parseAuthJSON(raw) }

func parseAuthJSON(raw []byte) (AuthFile, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return AuthFile{}, err
	}
	tokens, _ := root["tokens"].(map[string]any)
	if tokens == nil {
		return AuthFile{}, errors.New("no tokens")
	}
	a := AuthFile{
		Access:    strAny(tokens["access_token"]),
		Refresh:   strAny(tokens["refresh_token"]),
		IDToken:   strAny(tokens["id_token"]),
		AccountID: strAny(tokens["account_id"]),
	}
	if a.IDToken != "" {
		cl := identityjwt.Parse(a.IDToken)
		a.UserID = cl.UserID
		if a.AccountID == "" {
			a.AccountID = cl.AccountID
		}
	}
	if a.UserID == "" && a.Access != "" {
		a.UserID = identityjwt.Parse(a.Access).UserID
	}
	if s, _ := root["last_refresh"].(string); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			a.LastRefresh = t
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			a.LastRefresh = t
		}
	}
	return a, nil
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}

func (a AuthFile) NeedsRefresh(now time.Time) bool {
	if a.Refresh == "" {
		return false
	}
	if a.Access != "" {
		cl := identityjwt.Parse(a.Access)
		if cl.Exp > 0 && now.Add(accessSkew).Unix() >= cl.Exp {
			return true
		}
	}
	if !a.LastRefresh.IsZero() && now.Sub(a.LastRefresh) >= lastRefreshMax {
		return true
	}
	if a.Access == "" {
		return true
	}
	return false
}

func SameOAuthUser(a, b AuthFile) bool {
	return a.UserID != "" && a.UserID == b.UserID
}

func ApplyRefresh(blob adapter.Blob, tok quota.CodexTokens, now time.Time) (adapter.Blob, error) {
	var env map[string]any
	if err := json.Unmarshal(blob.Payload, &env); err != nil {
		return blob, err
	}
	raw, err := json.Marshal(env["auth_json"])
	if err != nil {
		return blob, err
	}
	if b, ok := env["auth_json"].(json.RawMessage); ok {
		raw = b
	} else if s, ok := env["auth_json"].(string); ok {
		raw = []byte(s)
	} else if m, ok := env["auth_json"].(map[string]any); ok {
		raw, _ = json.Marshal(m)
	}
	var auth map[string]any
	if err := json.Unmarshal(raw, &auth); err != nil {
		return blob, err
	}
	tokens, _ := auth["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
		auth["tokens"] = tokens
	}
	if tok.AccessToken != "" {
		tokens["access_token"] = tok.AccessToken
	}
	if tok.IDToken != "" {
		tokens["id_token"] = tok.IDToken
	}
	if tok.RefreshToken != "" {
		tokens["refresh_token"] = tok.RefreshToken
	}
	auth["last_refresh"] = now.UTC().Format(time.RFC3339Nano)
	env["auth_json"] = auth
	if ident, _, err := identityFromRaw(mustJSONMap(auth)); err == nil {
		env["identity"] = ident
		blob.Identity = ident
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return blob, err
	}
	blob.Payload = payload
	return blob, nil
}

func mustJSONMap(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	return b
}

func BlobFromTokens(tok quota.CodexTokens, now time.Time) (adapter.Blob, error) {
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.IDToken == "" {
		return adapter.Blob{}, errors.New("codex: incomplete oauth tokens")
	}
	cl := identityjwt.Parse(tok.IDToken)
	if cl.AccountID == "" {
		cl = identityjwt.Parse(tok.AccessToken)
	}
	if cl.AccountID == "" {
		return adapter.Blob{}, errors.New("codex: no chatgpt_account_id")
	}
	auth := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      tok.IDToken,
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"account_id":    cl.AccountID,
		},
		"last_refresh": now.UTC().Format(time.RFC3339Nano),
	}
	id, _, err := identityFromRaw(mustJSONMap(auth))
	if err != nil {
		return adapter.Blob{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"kind":      "codex.auth.json.v1",
		"identity":  id,
		"auth_json": auth,
	})
	if err != nil {
		return adapter.Blob{}, err
	}
	return adapter.Blob{Tool: adapter.Codex, Identity: id, CapturedAt: now, Payload: payload}, nil
}

func MergeLiveTokens(blob adapter.Blob, liveAuth []byte) adapter.Blob {
	stored, err := ReadAuth(blob)
	if err != nil || stored.AccountID == "" {
		return blob
	}
	live, err := parseAuthJSON(liveAuth)
	if err != nil || live.Access == "" || !SameOAuthUser(stored, live) {
		return blob
	}
	tok := quota.CodexTokens{AccessToken: live.Access, IDToken: live.IDToken, RefreshToken: live.Refresh}
	out, err := ApplyRefresh(blob, tok, time.Now())
	if err != nil {
		return blob
	}
	return forceAccountID(out, stored.AccountID)
}

func forceAccountID(blob adapter.Blob, accountID string) adapter.Blob {
	var env map[string]any
	if json.Unmarshal(blob.Payload, &env) != nil {
		return blob
	}
	auth, _ := env["auth_json"].(map[string]any)
	if auth == nil {
		return blob
	}
	tokens, _ := auth["tokens"].(map[string]any)
	if tokens == nil {
		return blob
	}
	tokens["account_id"] = accountID
	if ident, ok := env["identity"].(map[string]any); ok {
		ident["stable_id"] = accountID
	}
	blob.Identity.StableID = accountID
	payload, err := json.Marshal(env)
	if err != nil {
		return blob
	}
	blob.Payload = payload
	return blob
}
