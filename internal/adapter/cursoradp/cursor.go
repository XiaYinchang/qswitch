package cursoradp

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/busy"
	"qswitch/internal/livefile"
	"qswitch/internal/secutil"

	_ "modernc.org/sqlite"
)

const liveVSCDBSuffix = "Library/Application Support/Cursor/User/globalStorage/state.vscdb"

var cursorAuthKeys = []string{
	"cursorAuth/accessToken",
	"cursorAuth/refreshToken",
	"cursorAuth/cachedEmail",
	"cursorAuth/cachedScopedProfile",
	"cursorAuth/cachedSignUpType",
	"cursorAuth/stripeMembershipAuthId",
	"cursorAuth/stripeMembershipType",
	"cursorAuth/stripeSubscriptionStatus",
}

type Adapter struct {
	List func() ([]adapter.Proc, error)
}

func (a Adapter) Name() adapter.Tool { return adapter.Cursor }

func cliAuth(home string) string { return filepath.Join(home, ".cursor", "auth.json") }
func cliCfg(home string) string  { return filepath.Join(home, ".cursor", "cli-config.json") }
func vscdb(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
}

func (a Adapter) LivePaths(home string) []string {
	return []string{cliAuth(home), cliCfg(home), vscdb(home)}
}

func HashCLI(home string) ([32]byte, error) {
	h := sha256.New()
	n := 0
	for _, p := range []string{cliAuth(home), cliCfg(home)} {
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

func HashDesktop(home string) ([32]byte, error) {
	desk, err := readDesktop(home)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	for _, k := range []string{
		"cursorAuth/accessToken",
		"cursorAuth/refreshToken",
		"cursorAuth/cachedEmail",
		"cursorAuth/stripeMembershipAuthId",
	} {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(desk[k]))
		h.Write([]byte{0})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func (a Adapter) Fingerprint(home string) ([32]byte, error) {
	cli, cliErr := HashCLI(home)
	desk, deskErr := HashDesktop(home)
	return CombineFP(cli, cliErr, desk, deskErr)
}

func CombineFP(cli [32]byte, cliErr error, desk [32]byte, deskErr error) ([32]byte, error) {
	if cliErr != nil && deskErr != nil {
		return [32]byte{}, cliErr
	}
	h := sha256.New()
	if cliErr == nil {
		h.Write(cli[:])
	}
	if deskErr == nil {
		h.Write(desk[:])
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
		return adapter.Holders{}, err
	}
	app := busy.CursorApp(list)
	ag := busy.CursorAgent(list)
	h := adapter.Holders{Manual: append(app, ag...), CursorApp: len(app) > 0}
	if len(app) > 0 {
		h.Why = append(h.Why, "cursor.app running")
	}
	if len(ag) > 0 {
		h.Why = append(h.Why, "cursor-agent running")
	}
	return h, nil
}

func (a Adapter) AutoBlockers(home string) (adapter.Holders, error) {
	return a.ManualBlockers(home)
}

type payload struct {
	Kind     string            `json:"kind"`
	Identity adapter.Identity  `json:"identity"`
	Desktop  map[string]string `json:"desktop"`
	CLIAuth  map[string]string `json:"cli_auth_json"`
	CLIInfo  map[string]any    `json:"cli_auth_info"`
	StaleCLI bool              `json:"stale_cli,omitempty"`
}

func (a Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	desk, err := readDesktop(home)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	cliTok, _ := readJSONMap(cliAuth(home))
	cfg, _ := readObject(cliCfg(home))
	info, _ := asMap(cfg["authInfo"])

	deskID := str(desk["cursorAuth/stripeMembershipAuthId"])
	if deskID == "" {
		deskID = str(desk["cursorAuth/cachedEmail"])
	}
	cliID := str(info["authId"])
	if cliID == "" {
		cliID = str(info["email"])
	}
	stale := isStaleCLI(home)

	var warn []adapter.Warning
	if deskID != "" && cliID != "" && deskID != cliID {
		warn = append(warn, adapter.WarnDesyncDesktop, adapter.WarnDesyncCLI)
		deskIdent := adapter.Identity{Tool: adapter.Cursor, StableID: deskID, Email: str(desk["cursorAuth/cachedEmail"]), PlanHint: str(desk["cursorAuth/stripeMembershipType"])}
		cliIdent := adapter.Identity{Tool: adapter.Cursor, StableID: cliID, Email: str(info["email"])}
		// Vault snapshots only: copy each side onto the empty plane so both
		// accounts are restorable. Does not write live files.
		dSnap := align(payload{Kind: "cursor.auth.v1", Identity: deskIdent, Desktop: desk})
		cSnap := align(payload{Kind: "cursor.auth.v1", Identity: cliIdent, CLIAuth: cliTok, CLIInfo: info, StaleCLI: stale})
		if stale {
			warn = append(warn, adapter.WarnStaleCLITokens)
		}
		return []adapter.Blob{
			blobFrom(dSnap.Desktop, dSnap.CLIAuth, dSnap.CLIInfo, deskIdent, incomplete(dSnap), false),
			blobFrom(cSnap.Desktop, cSnap.CLIAuth, cSnap.CLIInfo, cliIdent, incomplete(cSnap), stale),
		}, warn, nil
	}
	id := adapter.Identity{Tool: adapter.Cursor, StableID: first(deskID, cliID), Email: first(str(desk["cursorAuth/cachedEmail"]), str(info["email"])), PlanHint: str(desk["cursorAuth/stripeMembershipType"])}
	if id.StableID == "" {
		return nil, warn, errors.New("cursor: no identity")
	}
	inc := str(desk["cursorAuth/accessToken"]) == "" || str(cliTok["accessToken"]) == ""
	if inc {
		warn = append(warn, adapter.WarnIncompleteBlob)
	}
	if stale {
		warn = append(warn, adapter.WarnStaleCLITokens)
	}
	return []adapter.Blob{blobFrom(desk, cliTok, info, id, inc, stale)}, warn, nil
}

func blobFrom(desk, cliTok map[string]string, info map[string]any, id adapter.Identity, inc, stale bool) adapter.Blob {
	pl, _ := json.Marshal(payload{Kind: "cursor.auth.v1", Identity: id, Desktop: desk, CLIAuth: cliTok, CLIInfo: info, StaleCLI: stale})
	return adapter.Blob{Tool: adapter.Cursor, Identity: id, CapturedAt: time.Now(), Incomplete: inc, StaleCLI: stale, Payload: pl}
}

func isStaleCLI(home string) bool {
	ai, err1 := os.Stat(cliAuth(home))
	ci, err2 := os.Stat(cliCfg(home))
	if err1 != nil || err2 != nil {
		return false
	}
	return ci.ModTime().Sub(ai.ModTime()) > 24*time.Hour
}

func (a Adapter) IdentityOf(blob adapter.Blob) (adapter.Identity, error) {
	if blob.Identity.StableID != "" {
		return blob.Identity, nil
	}
	var p payload
	if err := json.Unmarshal(blob.Payload, &p); err != nil {
		return adapter.Identity{}, err
	}
	p.Identity.Tool = adapter.Cursor
	return p.Identity, nil
}

func (a Adapter) Restore(home string, blob adapter.Blob, opts adapter.RestoreOpts) error {
	var p payload
	if err := json.Unmarshal(blob.Payload, &p); err != nil {
		return err
	}
	p = align(p)
	if incomplete(p) {
		return errors.New("cursor: snapshot has no usable access token")
	}
	if opts.CLIOnly {
		list, err := a.procs()
		if err != nil {
			return err
		}
		if ag := busy.CursorAgent(list); len(ag) > 0 {
			return &adapter.BusyError{Holders: adapter.Holders{Manual: ag, Why: []string{"cursor-agent running"}}}
		}
		return writeCLI(home, p)
	}
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	if err := guardLiveDB(vscdb(home)); err != nil {
		return err
	}
	preDesk, _ := readDesktop(home)
	preCLI, _ := os.ReadFile(cliAuth(home))
	preCfg, _ := os.ReadFile(cliCfg(home))

	if err := writeDesktop(home, p.Desktop); err != nil {
		return err
	}
	if err := writeCLI(home, p); err != nil {
		_ = writeDesktop(home, preDesk)
		if len(preCLI) > 0 {
			_ = livefile.AtomicWrite(cliAuth(home), preCLI, 0o600)
		}
		if len(preCfg) > 0 {
			_ = livefile.AtomicWrite(cliCfg(home), preCfg, 0o600)
		}
		return err
	}
	return nil
}

func emptyDesktop(p payload) bool {
	return str(p.Desktop["cursorAuth/accessToken"]) == ""
}

func incomplete(p payload) bool {
	return str(p.Desktop["cursorAuth/accessToken"]) == "" || str(p.CLIAuth["accessToken"]) == ""
}

func align(p payload) payload {
	dTok := str(p.Desktop["cursorAuth/accessToken"])
	cTok := str(p.CLIAuth["accessToken"])
	if p.CLIAuth == nil {
		p.CLIAuth = map[string]string{}
	}
	if p.Desktop == nil {
		p.Desktop = map[string]string{}
	}
	if dTok != "" && cTok == "" {
		p.CLIAuth["accessToken"] = dTok
		if r := str(p.Desktop["cursorAuth/refreshToken"]); r != "" {
			p.CLIAuth["refreshToken"] = r
		}
		if p.CLIInfo == nil {
			p.CLIInfo = map[string]any{}
		}
		if p.CLIInfo["email"] == nil || p.CLIInfo["email"] == "" {
			p.CLIInfo["email"] = str(p.Desktop["cursorAuth/cachedEmail"])
		}
		if p.CLIInfo["authId"] == nil || p.CLIInfo["authId"] == "" {
			p.CLIInfo["authId"] = str(p.Desktop["cursorAuth/stripeMembershipAuthId"])
		}
	}
	if cTok != "" && dTok == "" {
		p.Desktop["cursorAuth/accessToken"] = cTok
		if r := str(p.CLIAuth["refreshToken"]); r != "" {
			p.Desktop["cursorAuth/refreshToken"] = r
		}
		if p.Desktop["cursorAuth/cachedEmail"] == "" {
			p.Desktop["cursorAuth/cachedEmail"] = str(p.CLIInfo["email"])
		}
		if p.Desktop["cursorAuth/stripeMembershipAuthId"] == "" {
			p.Desktop["cursorAuth/stripeMembershipAuthId"] = str(p.CLIInfo["authId"])
		}
	}
	p.StaleCLI = false
	return p
}

func guardLiveDB(path string) error {
	if os.Getenv("QSWITCH_ALLOW_LIVE_VSCDB") == "1" {
		return nil
	}
	if !secutil.InTest() {
		return nil
	}
	abs, _ := filepath.Abs(path)
	uh, _ := os.UserHomeDir()
	live, _ := filepath.Abs(filepath.Join(uh, "Library", "Application Support", "Cursor"))
	if live != "" && strings.HasPrefix(abs, live) {
		return errors.New("refusing live Cursor state.vscdb in tests")
	}
	st, err := os.Stat(path)
	if err == nil && st.Size() > 10<<20 {
		return errors.New("refusing large vscdb in tests")
	}
	return nil
}

func readDesktop(home string) (map[string]string, error) {
	path := vscdb(home)
	if err := guardLiveDB(path); err != nil && secutil.InTest() {
		// tests with tiny fixture are fine; guard only live/large
		if strings.Contains(err.Error(), "live Cursor") || strings.Contains(err.Error(), "large vscdb") {
			return nil, err
		}
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", path))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]string{}
	rows, err := db.Query(`SELECT key, value FROM ItemTable WHERE key LIKE 'cursorAuth/%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = string(v)
	}
	return out, rows.Err()
}

func writeDesktop(home string, kv map[string]string) error {
	path := vscdb(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := initMiniDB(path); err != nil {
			return err
		}
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, k := range cursorAuthKeys {
		val, ok := kv[k]
		if !ok {
			continue
		}
		if (k == "cursorAuth/accessToken" || k == "cursorAuth/refreshToken") && val == "" {
			_ = tx.Rollback()
			return errors.New("cursor: refusing empty token write")
		}
		if _, err := tx.Exec(`INSERT INTO ItemTable(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, []byte(val)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func initMiniDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`)
	return err
}

func writeCLI(home string, p payload) error {
	tok := map[string]string{}
	if t := str(p.CLIAuth["accessToken"]); t != "" {
		tok["accessToken"] = t
	} else {
		return errors.New("cursor: empty CLI accessToken")
	}
	if t := str(p.CLIAuth["refreshToken"]); t != "" {
		tok["refreshToken"] = t
	}
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	if err := livefile.AtomicWrite(cliAuth(home), b, 0o600); err != nil {
		return err
	}
	cfg, _ := readObject(cliCfg(home))
	if cfg == nil {
		cfg = map[string]any{}
	}
	if p.CLIInfo != nil {
		cfg["authInfo"] = p.CLIInfo
	}
	cb, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return livefile.AtomicWrite(cliCfg(home), cb, 0o644)
}

func readObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func readJSONMap(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a Adapter) Idle(home string, grace time.Duration) bool {
	if len(busy.CursorAgent(mustList(a))) == 0 {
		return true
	}
	return time.Since(mtime(filepath.Join(home, ".cursor"))) >= grace
}

func mustList(a Adapter) []adapter.Proc {
	list, _ := a.procs()
	return list
}

func mtime(root string) time.Time {
	var newest time.Time
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

func (a Adapter) KillCLI(home string) error {
	list, err := a.procs()
	if err != nil {
		return err
	}
	var first error
	for _, p := range busy.CursorAgent(list) {
		if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func AccessToken(blob adapter.Blob) (string, error) {
	var p payload
	if err := json.Unmarshal(blob.Payload, &p); err != nil {
		return "", err
	}
	if t := str(p.Desktop["cursorAuth/accessToken"]); t != "" {
		return t, nil
	}
	if t := str(p.CLIAuth["accessToken"]); t != "" {
		return t, nil
	}
	return "", errors.New("no cursor token")
}
