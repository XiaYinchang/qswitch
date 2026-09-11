package grok

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func (a Adapter) Name() adapter.Tool { return adapter.Grok }

func authPath(home string) string { return filepath.Join(home, ".grok", "auth.json") }

func (a Adapter) Fingerprint(home string) ([32]byte, error) {
	b, err := os.ReadFile(authPath(home))
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}
func lockPath(home string) string { return filepath.Join(home, ".grok", "auth.json.lock") }
func sessPath(home string) string { return filepath.Join(home, ".grok", "active_sessions.json") }

func (a Adapter) LivePaths(home string) []string {
	return []string{
		authPath(home),
		filepath.Join(home, snapshot.GrokBotRelRoot, "Cookies"),
	}
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
		return adapter.Holders{}, err
	}
	cli := busy.GrokCLI(list)
	seen := map[int]bool{}
	var manual []adapter.Proc
	for _, p := range cli {
		if !seen[p.PID] {
			seen[p.PID] = true
			manual = append(manual, p)
		}
	}
	for _, pid := range liveSessionPIDs(home) {
		if !seen[pid] && busy.Alive(pid) {
			seen[pid] = true
			manual = append(manual, adapter.Proc{PID: pid, Command: "grok-session"})
		}
	}
	if pid, ok := parsePIDFile(lockPath(home)); ok && busy.Alive(pid) && !seen[pid] {
		manual = append(manual, adapter.Proc{PID: pid, Command: "grok-pidfile"})
	}
	h := adapter.Holders{Manual: manual}
	if len(manual) > 0 {
		h.Why = append(h.Why, "grok cli running")
	}
	return h, nil
}

func (a Adapter) AutoBlockers(home string) (adapter.Holders, error) {
	return a.ManualBlockers(home)
}

func parsePIDFile(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, false
	}
	head, _, _ := strings.Cut(s, ":")
	pid, err := strconv.Atoi(head)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func liveSessionPIDs(home string) []int {
	b, err := os.ReadFile(sessPath(home))
	if err != nil {
		return nil
	}
	var rows []struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(b, &rows) != nil {
		return nil
	}
	var out []int
	for _, r := range rows {
		if r.PID > 0 {
			out = append(out, r.PID)
		}
	}
	return out
}

func (a Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	raw, err := os.ReadFile(authPath(home))
	if err != nil {
		if desk, e2 := captureDesktop(home); e2 == nil {
			return []adapter.Blob{desk}, nil, nil
		}
		return nil, nil, err
	}
	id, err := identityFromRaw(raw)
	if err != nil {
		return nil, nil, err
	}
	var warn []adapter.Warning
	if dualBinary(home) {
		warn = append(warn, adapter.WarnGrokDualBinary)
	}
	env := map[string]any{"kind": "grok.auth.json.v1", "identity": id, "auth_json": json.RawMessage(raw)}
	payload, err := json.Marshal(env)
	if err != nil {
		return nil, warn, err
	}
	blobs := []adapter.Blob{{Tool: adapter.Grok, Identity: id, CapturedAt: time.Now(), Payload: payload}}
	if desk, err := captureDesktop(home); err == nil {
		blobs = append(blobs, desk)
	}
	return blobs, warn, nil
}

func captureDesktop(home string) (adapter.Blob, error) {
	root := filepath.Join(home, snapshot.GrokBotRelRoot)
	z, err := snapshot.Pack(root, snapshot.GrokBotRels)
	if err != nil {
		return adapter.Blob{}, err
	}
	id := adapter.Identity{Tool: adapter.Grok, StableID: "desktop:grokbot", Email: "grok-bot.app", PlanHint: "desktop"}
	raw, err := json.Marshal(map[string]any{
		"kind": snapshot.KindDesktop, "identity": id, "app": "grokbot",
		"rel_root": snapshot.GrokBotRelRoot, "zip": z,
	})
	if err != nil {
		return adapter.Blob{}, err
	}
	return adapter.Blob{Tool: adapter.Grok, Identity: id, CapturedAt: time.Now(), Payload: raw}, nil
}

func dualBinary(home string) bool {
	_, e1 := os.Stat(filepath.Join(home, ".local", "bin", "grok"))
	_, e2 := os.Stat(filepath.Join(home, ".grok", "bin", "grok"))
	return e1 == nil && e2 == nil
}

func identityFromRaw(raw []byte) (adapter.Identity, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return adapter.Identity{}, err
	}
	for _, v := range root {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		id := adapter.Identity{Tool: adapter.Grok}
		if s, ok := m["principal_id"].(string); ok {
			id.StableID = s
		}
		if id.StableID == "" {
			if s, ok := m["user_id"].(string); ok {
				id.StableID = s
			}
		}
		if s, ok := m["email"].(string); ok {
			id.Email = s
		}
		if id.StableID == "" {
			continue
		}
		return id, nil
	}
	return adapter.Identity{}, errors.New("grok: no principal_id")
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
		env.Identity.Tool = adapter.Grok
		return env.Identity, nil
	}
	return identityFromRaw(env.AuthJSON)
}

func (a Adapter) Restore(home string, blob adapter.Blob, _ adapter.RestoreOpts) error {
	var head struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(blob.Payload, &head)
	if head.Kind == snapshot.KindDesktop {
		list, err := a.procs()
		if err != nil {
			return err
		}
		if apps := busy.GrokBotApp(list); len(apps) > 0 {
			return &adapter.BusyError{Holders: adapter.Holders{Manual: apps, Why: []string{"quit Grok Bot.app to restore desktop session"}}}
		}
		var env struct {
			RelRoot string `json:"rel_root"`
			Zip     []byte `json:"zip"`
		}
		if err := json.Unmarshal(blob.Payload, &env); err != nil {
			return err
		}
		if env.RelRoot == "" || len(env.Zip) == 0 {
			return errors.New("grok: empty desktop snapshot")
		}
		return snapshot.Unpack(filepath.Join(home, env.RelRoot), env.Zip)
	}
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	var env struct {
		AuthJSON json.RawMessage `json:"auth_json"`
	}
	if err := json.Unmarshal(blob.Payload, &env); err != nil {
		return err
	}
	if len(env.AuthJSON) == 0 {
		return errors.New("grok: empty auth_json")
	}
	return livefile.AtomicWrite(authPath(home), env.AuthJSON, 0o600)
}

func (a Adapter) Idle(home string, grace time.Duration) bool {
	return quota.DirIdle(filepath.Join(home, ".grok", "sessions"), grace, time.Now())
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

func Bearer(blob adapter.Blob) (string, error) {
	a, err := ReadAuth(blob)
	if err != nil {
		return "", err
	}
	if a.Key == "" {
		return "", errors.New("grok: no key")
	}
	return a.Key, nil
}

const accessSkew = 5 * time.Minute

const grokIssuer = "https://auth.x.ai"

type AuthFile struct {
	Slot        string
	Key         string
	Refresh     string
	ExpiresAt   time.Time
	ClientID    string
	Issuer      string
	PrincipalID string
	UserID      string
	Email       string
}

func authJSON(blob adapter.Blob) ([]byte, error) {
	var env struct {
		AuthJSON json.RawMessage `json:"auth_json"`
	}
	if err := json.Unmarshal(blob.Payload, &env); err != nil {
		return nil, err
	}
	if len(env.AuthJSON) == 0 {
		return nil, errors.New("grok: empty auth_json")
	}
	return env.AuthJSON, nil
}

func ReadAuth(blob adapter.Blob) (AuthFile, error) {
	raw, err := authJSON(blob)
	if err != nil {
		return AuthFile{}, err
	}
	return parseAuthJSON(raw, blob.Identity.StableID)
}

func ReadAuthBytes(raw []byte) (AuthFile, error) { return parseAuthJSON(raw, "") }

func parseAuthJSON(raw []byte, preferID string) (AuthFile, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return AuthFile{}, err
	}
	var first, preferred AuthFile
	for slot, v := range root {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		a := authFromMap(slot, m)
		if a.PrincipalID == "" && a.Key == "" {
			continue
		}
		if first.Slot == "" {
			first = a
		}
		if preferID != "" && (a.PrincipalID == preferID || a.UserID == preferID) {
			preferred = a
		}
	}
	if preferred.Slot != "" {
		return preferred, nil
	}
	if first.Slot != "" {
		return first, nil
	}
	return AuthFile{}, errors.New("grok: no principal_id")
}

func authFromMap(slot string, m map[string]any) AuthFile {
	a := AuthFile{
		Slot:        slot,
		Key:         strAny(m["key"]),
		Refresh:     strAny(m["refresh_token"]),
		ClientID:    strAny(m["oidc_client_id"]),
		Issuer:      strAny(m["oidc_issuer"]),
		PrincipalID: strAny(m["principal_id"]),
		UserID:      strAny(m["user_id"]),
		Email:       strAny(m["email"]),
	}
	if a.PrincipalID == "" {
		a.PrincipalID = a.UserID
	}
	if a.ClientID == "" {
		if _, id, ok := strings.Cut(slot, "::"); ok {
			a.ClientID = id
		}
	}
	if a.Issuer == "" {
		if iss, _, ok := strings.Cut(slot, "::"); ok {
			a.Issuer = iss
		}
	}
	if s := strAny(m["expires_at"]); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			a.ExpiresAt = t
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			a.ExpiresAt = t
		}
	}
	return a
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}

func (a AuthFile) Refreshable() bool {
	if a.Refresh == "" {
		return false
	}
	iss := strings.TrimRight(strings.ToLower(a.Issuer), "/")
	if iss == "" {
		iss = strings.ToLower(a.Slot)
	}
	return strings.HasPrefix(iss, grokIssuer)
}

func (a AuthFile) NeedsRefresh(now time.Time) bool {
	if !a.Refreshable() {
		return false
	}
	if a.Key == "" {
		return true
	}
	if !a.ExpiresAt.IsZero() && !now.Add(accessSkew).Before(a.ExpiresAt) {
		return true
	}
	cl := identityjwt.Parse(a.Key)
	if cl.Exp > 0 && now.Add(accessSkew).Unix() >= cl.Exp {
		return true
	}
	return false
}

func SameOIDCUser(a, b AuthFile) bool {
	if a.PrincipalID != "" && a.PrincipalID == b.PrincipalID {
		return true
	}
	return a.UserID != "" && a.UserID == b.UserID
}

func ApplyRefresh(blob adapter.Blob, tok quota.GrokTokens, now time.Time) (adapter.Blob, error) {
	var env map[string]any
	if err := json.Unmarshal(blob.Payload, &env); err != nil {
		return blob, err
	}
	raw := marshalAuthJSON(env["auth_json"])
	if len(raw) == 0 {
		return blob, errors.New("grok: empty auth_json")
	}
	var auth map[string]any
	if err := json.Unmarshal(raw, &auth); err != nil {
		return blob, err
	}
	stored, err := parseAuthJSON(raw, blob.Identity.StableID)
	if err != nil {
		return blob, err
	}
	entry, _ := auth[stored.Slot].(map[string]any)
	if entry == nil {
		return blob, errors.New("grok: missing slot")
	}
	if tok.AccessToken != "" {
		entry["key"] = tok.AccessToken
	}
	if tok.RefreshToken != "" {
		entry["refresh_token"] = tok.RefreshToken
	}
	exp := now.Add(6 * time.Hour)
	if tok.ExpiresIn > 0 {
		exp = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else if cl := identityjwt.Parse(tok.AccessToken); cl.Exp > 0 {
		exp = time.Unix(cl.Exp, 0).UTC()
	}
	entry["expires_at"] = exp.UTC().Format(time.RFC3339Nano)
	entry["create_time"] = now.UTC().Format(time.RFC3339Nano)
	auth[stored.Slot] = entry
	env["auth_json"] = auth
	if ident, err := identityFromRaw(mustJSONMap(auth)); err == nil {
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

func marshalAuthJSON(v any) []byte {
	switch t := v.(type) {
	case json.RawMessage:
		return t
	case string:
		return []byte(t)
	case map[string]any:
		b, _ := json.Marshal(t)
		return b
	default:
		b, _ := json.Marshal(t)
		return b
	}
}

func mustJSONMap(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	return b
}

func BlobFromAuthJSON(raw []byte, now time.Time) (adapter.Blob, error) {
	id, err := identityFromRaw(raw)
	if err != nil {
		return adapter.Blob{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"kind":      "grok.auth.json.v1",
		"identity":  id,
		"auth_json": json.RawMessage(raw),
	})
	if err != nil {
		return adapter.Blob{}, err
	}
	return adapter.Blob{Tool: adapter.Grok, Identity: id, CapturedAt: now, Payload: payload}, nil
}

func BlobFromTokens(tok quota.GrokTokens, now time.Time) (adapter.Blob, error) {
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return adapter.Blob{}, errors.New("grok: incomplete oauth tokens")
	}
	cl := grokAccessClaims(tok.AccessToken)
	if cl.principalID == "" {
		return adapter.Blob{}, errors.New("grok: no principal_id")
	}
	clientID := cl.clientID
	if clientID == "" {
		clientID = quota.GrokOAuthClientID
	}
	exp := now.Add(6 * time.Hour)
	if tok.ExpiresIn > 0 {
		exp = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else if cl.exp > 0 {
		exp = time.Unix(cl.exp, 0).UTC()
	}
	email := tok.Email
	if email == "" {
		email = cl.email
	}
	ptype := cl.principalType
	if ptype == "" {
		ptype = "User"
	}
	slot := grokIssuer + "::" + clientID
	entry := map[string]any{
		"auth_mode":      "oidc",
		"key":            tok.AccessToken,
		"refresh_token":  tok.RefreshToken,
		"expires_at":     exp.UTC().Format(time.RFC3339Nano),
		"create_time":    now.UTC().Format(time.RFC3339Nano),
		"principal_id":   cl.principalID,
		"user_id":        cl.principalID,
		"principal_type": ptype,
		"oidc_issuer":    grokIssuer,
		"oidc_client_id": clientID,
		"email":          email,
	}
	if cl.teamID != "" {
		entry["team_id"] = cl.teamID
	}
	auth := map[string]any{slot: entry}
	raw, err := json.Marshal(auth)
	if err != nil {
		return adapter.Blob{}, err
	}
	return BlobFromAuthJSON(raw, now)
}

type grokClaims struct {
	principalID, principalType, teamID, clientID, email string
	exp                                                 int64
}

func grokAccessClaims(access string) grokClaims {
	var c grokClaims
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return c
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		b, err2 := base64.StdEncoding.DecodeString(parts[1])
		if err2 != nil {
			return c
		}
		raw = b
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return c
	}
	c.email, _ = m["email"].(string)
	c.principalType, _ = m["principal_type"].(string)
	c.teamID, _ = m["team_id"].(string)
	c.clientID, _ = m["client_id"].(string)
	if c.clientID == "" {
		if aud, ok := m["aud"].(string); ok {
			c.clientID = aud
		}
	}
	c.principalID, _ = m["principal_id"].(string)
	if c.principalID == "" {
		c.principalID, _ = m["sub"].(string)
	}
	if n, ok := m["exp"].(float64); ok {
		c.exp = int64(n)
	}
	return c
}

func MergeLiveTokens(blob adapter.Blob, liveAuth []byte) adapter.Blob {
	stored, err := ReadAuth(blob)
	if err != nil {
		return blob
	}
	live, err := parseAuthJSON(liveAuth, stored.PrincipalID)
	if err != nil || live.Key == "" || !SameOIDCUser(stored, live) {
		return blob
	}
	tok := quota.GrokTokens{AccessToken: live.Key, RefreshToken: live.Refresh}
	if !live.ExpiresAt.IsZero() {
		tok.ExpiresIn = int(live.ExpiresAt.Sub(time.Now()).Seconds())
		if tok.ExpiresIn < 1 {
			tok.ExpiresIn = 1
		}
	}
	out, err := ApplyRefresh(blob, tok, time.Now())
	if err != nil {
		return blob
	}
	return out
}
