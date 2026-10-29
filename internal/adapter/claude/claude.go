// Package claude captures and restores Claude Code's official OAuth login.
// The CLI stores credentials in ~/.claude/.credentials.json on Linux and in
// the macOS Keychain; only the file layout is managed here. Account metadata
// (email, uuid) comes from ~/.claude.json's oauthAccount, confirmed against
// the official /api/oauth/profile endpoint before enrollment.
package claude

import (
	"crypto/sha256"
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
	"qswitch/internal/quota"
)

const payloadKind = "claude.oauth.v1"

type Adapter struct {
	List func() ([]adapter.Proc, error)
}

func (Adapter) Name() adapter.Tool { return adapter.Claude }

func dataDir(home string) string {
	if p := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); p != "" {
		return p
	}
	return filepath.Join(home, ".claude")
}

func credentialsPath(home string) string {
	return filepath.Join(dataDir(home), ".credentials.json")
}

// claudeJSONPath holds the CLI's account metadata. It stays in the home
// directory even when CLAUDE_CONFIG_DIR relocates the data dir.
func claudeJSONPath(home string) string {
	return filepath.Join(home, ".claude.json")
}

func (Adapter) LivePaths(home string) []string { return []string{credentialsPath(home)} }

func (Adapter) Fingerprint(home string) ([32]byte, error) {
	b, err := os.ReadFile(credentialsPath(home))
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

type Creds struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // epoch milliseconds
	Scopes           []string `json:"scopes,omitempty"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
}

func ParseCreds(raw []byte) (Creds, error) {
	var env struct {
		OAuth *Creds `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &env) != nil || env.OAuth == nil ||
		strings.TrimSpace(env.OAuth.AccessToken) == "" ||
		strings.TrimSpace(env.OAuth.RefreshToken) == "" || env.OAuth.ExpiresAt <= 0 {
		return Creds{}, errors.New("claude: invalid OAuth credentials")
	}
	return *env.OAuth, nil
}

const refreshSkewSeconds = 300 // at least the daemon keepalive tick

func (c Creds) NeedsRefresh(now time.Time) bool {
	return c.ExpiresAt/1000-now.Unix() <= refreshSkewSeconds
}

func ReadLiveCreds(home string) (Creds, error) {
	b, err := os.ReadFile(credentialsPath(home))
	if err != nil {
		return Creds{}, err
	}
	return ParseCreds(b)
}

// OAuthAccount is the identity block the CLI keeps in ~/.claude.json.
type OAuthAccount struct {
	AccountUUID string `json:"accountUuid"`
	Email       string `json:"emailAddress"`
}

func readLiveAccount(home string) (OAuthAccount, error) {
	var doc struct {
		OAuthAccount OAuthAccount `json:"oauthAccount"`
	}
	b, err := os.ReadFile(claudeJSONPath(home))
	if err != nil {
		return OAuthAccount{}, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return OAuthAccount{}, err
	}
	return doc.OAuthAccount, nil
}

type payload struct {
	Kind         string           `json:"kind"`
	Identity     adapter.Identity `json:"identity"`
	Credentials  string           `json:"credentials_json"`
	OAuthAccount string           `json:"oauth_account_json,omitempty"`
}

func readPayload(blob adapter.Blob) (payload, error) {
	var p payload
	if json.Unmarshal(blob.Payload, &p) != nil || p.Kind != payloadKind {
		return p, errors.New("claude: invalid credential snapshot")
	}
	if _, err := ParseCreds([]byte(p.Credentials)); err != nil {
		return p, err
	}
	return p, nil
}

func CredsFromBlob(blob adapter.Blob) (Creds, error) {
	p, err := readPayload(blob)
	if err != nil {
		return Creds{}, err
	}
	return ParseCreds([]byte(p.Credentials))
}

func (Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	raw, err := os.ReadFile(credentialsPath(home))
	if err != nil {
		return nil, nil, err
	}
	if _, err := ParseCreds(raw); err != nil {
		return nil, nil, err
	}
	// /api/oauth/profile must confirm the stable account uuid before the
	// application persists this; ~/.claude.json only seeds the display hint.
	id := adapter.Identity{Tool: adapter.Claude}
	var acctJSON string
	if acct, err := readLiveAccount(home); err == nil && acct.AccountUUID != "" {
		id.StableID = acct.AccountUUID
		id.Email = acct.Email
		if b, err := json.Marshal(acct); err == nil {
			acctJSON = string(b)
		}
	}
	b, err := json.Marshal(payload{Kind: payloadKind, Identity: id, Credentials: string(raw), OAuthAccount: acctJSON})
	if err != nil {
		return nil, nil, err
	}
	return []adapter.Blob{{Tool: adapter.Claude, Identity: id, Payload: b, CapturedAt: time.Now()}}, nil, nil
}

func (Adapter) IdentityOf(blob adapter.Blob) (adapter.Identity, error) {
	p, err := readPayload(blob)
	if err != nil {
		return adapter.Identity{}, err
	}
	id := blob.Identity
	if id.StableID == "" {
		id = p.Identity
	}
	id.Tool = adapter.Claude
	return id, nil
}

func (a Adapter) Restore(home string, blob adapter.Blob, _ adapter.RestoreOpts) error {
	p, err := readPayload(blob)
	if err != nil {
		return err
	}
	// Saved snapshots may predate identity/metadata reconciliation. The
	// verified snapshot identity is authoritative when restoring them too.
	if p.Identity.StableID != "" {
		account, err := json.Marshal(OAuthAccount{AccountUUID: p.Identity.StableID, Email: p.Identity.Email})
		if err != nil {
			return err
		}
		p.OAuthAccount = string(account)
	}
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	// Update the CLI's account metadata first; the credentials write commits
	// the switch. A credentials failure rolls the metadata back so the live
	// pair never mixes two accounts.
	credPath := credentialsPath(home)
	metaPath := claudeJSONPath(home)
	oldMeta, metaErr := os.ReadFile(metaPath)
	metaOK := metaErr == nil
	if metaErr != nil && !os.IsNotExist(metaErr) {
		return metaErr
	}
	if p.OAuthAccount != "" {
		baseMeta := oldMeta
		if !metaOK {
			baseMeta = []byte("{}")
		}
		newMeta, err := mergeOAuthAccount(baseMeta, p.OAuthAccount)
		if err != nil {
			return err
		}
		if err := livefile.AtomicWrite(metaPath, newMeta, 0o600); err != nil {
			return err
		}
	}
	if err := livefile.AtomicWrite(credPath, []byte(p.Credentials), 0o600); err != nil {
		if p.OAuthAccount != "" {
			var rollbackErr error
			if metaOK {
				rollbackErr = livefile.AtomicWrite(metaPath, oldMeta, 0o600)
			} else {
				rollbackErr = os.Remove(metaPath)
			}
			if rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("claude: restore account metadata rollback: %w", rollbackErr))
			}
		}
		return err
	}
	return nil
}

func mergeOAuthAccount(metaRaw []byte, acctJSON string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(metaRaw, &doc); err != nil {
		return nil, fmt.Errorf("claude: invalid ~/.claude.json: %w", err)
	}
	if doc == nil {
		doc = make(map[string]json.RawMessage)
	}
	doc["oauthAccount"] = json.RawMessage(acctJSON)
	return json.Marshal(doc)
}

func (a Adapter) procs() ([]adapter.Proc, error) {
	if a.List != nil {
		return a.List()
	}
	return (busy.PS{}).List()
}

func (a Adapter) ManualBlockers(string) (adapter.Holders, error) {
	ps, err := a.procs()
	if err != nil {
		return adapter.Holders{}, err
	}
	h := adapter.Holders{}
	for _, p := range ps {
		if isClaudeCLI(p.Command) {
			h.Manual = append(h.Manual, p)
		}
	}
	if len(h.Manual) > 0 {
		h.Why = []string{"claude cli running"}
	}
	return h, nil
}

func (a Adapter) AutoBlockers(home string) (adapter.Holders, error) { return a.ManualBlockers(home) }

func (a Adapter) Idle(home string, grace time.Duration) bool {
	h, err := a.ManualBlockers(home)
	if err != nil || h.ManualBusy() {
		return false
	}
	// Sessions append to ~/.claude/projects; treat recent writes as activity.
	return quota.DirIdle(filepath.Join(dataDir(home), "projects"), grace, time.Now())
}

func (a Adapter) KillCLI(home string) error {
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	for _, p := range h.Manual {
		if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func isClaudeCLI(command string) bool {
	f := strings.Fields(command)
	if len(f) == 0 {
		return false
	}
	name := filepath.Base(f[0])
	if name == "claude" {
		return true
	}
	if name == "node" || name == "bun" {
		args := f[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			args = args[1:]
		}
		if len(args) == 0 {
			return false
		}
		script := filepath.ToSlash(args[0])
		return filepath.Base(script) == "claude" ||
			strings.Contains(script, "/@anthropic-ai/claude-code/")
	}
	return false
}

// EnrichIdentity fills identity fields from a confirmed /api/oauth/profile
// response body.
func EnrichIdentity(id adapter.Identity, body []byte) adapter.Identity {
	uuid, email, name, plan := quota.ClaudeIdentity(body)
	if uuid != "" {
		id.StableID = uuid
	}
	if email != "" {
		id.Email = email
	}
	if name != "" {
		id.DisplayName = name
	}
	if plan != "" {
		id.PlanHint = plan
	}
	id.Tool = adapter.Claude
	return id
}

func WriteIdentity(blob adapter.Blob, id adapter.Identity) (adapter.Blob, error) {
	if id.StableID == "" {
		return blob, errors.New("claude: profile account uuid required before saving account")
	}
	p, err := readPayload(blob)
	if err != nil {
		return blob, err
	}
	id.Tool = adapter.Claude
	p.Identity = id
	account, err := json.Marshal(OAuthAccount{AccountUUID: id.StableID, Email: id.Email})
	if err != nil {
		return blob, err
	}
	p.OAuthAccount = string(account)
	raw, err := json.Marshal(p)
	if err != nil {
		return blob, err
	}
	blob.Payload = raw
	blob.Identity = id
	return blob, nil
}

// ApplyRefresh writes rotated OAuth tokens back into the vault snapshot.
func ApplyRefresh(blob adapter.Blob, tokens quota.ClaudeTokens, now time.Time) (adapter.Blob, error) {
	if strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" || tokens.ExpiresIn <= 0 {
		return blob, errors.New("claude: invalid refreshed credentials")
	}
	p, err := readPayload(blob)
	if err != nil {
		return blob, err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(p.Credentials), &raw); err != nil {
		return blob, fmt.Errorf("claude: invalid original credentials")
	}
	oauth, _ := raw["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		return blob, fmt.Errorf("claude: credentials missing claudeAiOauth")
	}
	oauth["accessToken"] = tokens.AccessToken
	oauth["refreshToken"] = tokens.RefreshToken
	oauth["expiresAt"] = (now.Unix() + tokens.ExpiresIn) * 1000
	raw["claudeAiOauth"] = oauth
	b, err := json.Marshal(raw)
	if err != nil {
		return blob, err
	}
	p.Credentials = string(b)
	blob.Payload, err = json.Marshal(p)
	blob.StaleCLI = false
	return blob, err
}
