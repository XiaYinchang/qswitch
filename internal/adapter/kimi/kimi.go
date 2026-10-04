package kimi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"qswitch/internal/adapter"
	"qswitch/internal/busy"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
)

type Adapter struct {
	List func() ([]adapter.Proc, error)
}

func (Adapter) Name() adapter.Tool { return adapter.Kimi }

func dataDir(home string) string {
	if p := strings.TrimSpace(os.Getenv("KIMI_CODE_HOME")); p != "" {
		return p
	}
	return filepath.Join(home, ".kimi-code")
}
func credentialsPath(home string) string {
	return filepath.Join(dataDir(home), "credentials", "kimi-code.json")
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
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

func ParseCreds(raw []byte) (Creds, error) {
	var c Creds
	if json.Unmarshal(raw, &c) != nil || strings.TrimSpace(c.AccessToken) == "" || strings.TrimSpace(c.RefreshToken) == "" || c.ExpiresAt <= 0 {
		return Creds{}, errors.New("kimi: invalid OAuth credentials")
	}
	return c, nil
}

func (c Creds) NeedsRefresh(now time.Time) bool {
	// Match the official client's threshold and leave room for the daemon's
	// two-minute keepalive tick even for short-lived access tokens.
	threshold := int64(300)
	if c.ExpiresIn/2 > threshold {
		threshold = c.ExpiresIn / 2
	}
	return c.ExpiresAt-now.Unix() <= threshold
}

func ReadLiveCreds(home string) (Creds, error) {
	if err := validateConfig(home); err != nil {
		return Creds{}, err
	}
	b, err := os.ReadFile(credentialsPath(home))
	if err != nil {
		return Creds{}, err
	}
	return ParseCreds(b)
}

// The default managed credential slot is the supported surface. Refuse an
// environment-specific slot instead of silently switching an unrelated file.
func validateConfig(home string) error {
	for k, expected := range map[string]string{"KIMI_CODE_BASE_URL": "https://api.kimi.com/coding/v1", "KIMI_CODE_OAUTH_HOST": "https://auth.kimi.com", "KIMI_OAUTH_HOST": "https://auth.kimi.com"} {
		if v := strings.TrimRight(strings.TrimSpace(os.Getenv(k)), "/"); v != "" && v != expected {
			return errors.New("kimi: non-default OAuth environment is not supported")
		}
	}
	raw, err := os.ReadFile(filepath.Join(dataDir(home), "config.toml"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg struct {
		Providers map[string]struct {
			BaseURL string `toml:"base_url"`
			OAuth   struct {
				Storage string `toml:"storage"`
				Key     string `toml:"key"`
				Host    string `toml:"oauth_host"`
			} `toml:"oauth"`
		} `toml:"providers"`
	}
	if toml.Unmarshal(raw, &cfg) != nil {
		return errors.New("kimi: invalid config.toml")
	}
	p, ok := cfg.Providers["managed:kimi-code"]
	if !ok {
		return nil
	}
	if p.BaseURL != "" && strings.TrimRight(p.BaseURL, "/") != "https://api.kimi.com/coding/v1" {
		return errors.New("kimi: non-default managed API environment is not supported")
	}
	if (p.OAuth.Storage != "" && p.OAuth.Storage != "file") || (p.OAuth.Key != "" && p.OAuth.Key != "oauth/kimi-code" && p.OAuth.Key != "kimi-code") || (p.OAuth.Host != "" && strings.TrimRight(p.OAuth.Host, "/") != "https://auth.kimi.com") {
		return errors.New("kimi: non-default OAuth credential slot is not supported")
	}
	return nil
}

type payload struct {
	Kind        string           `json:"kind"`
	Identity    adapter.Identity `json:"identity"`
	Credentials string           `json:"credentials_json"`
}

func readPayload(blob adapter.Blob) (payload, error) {
	var p payload
	if json.Unmarshal(blob.Payload, &p) != nil || p.Kind != "kimi.oauth.v1" {
		return p, errors.New("kimi: invalid credential snapshot")
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

func (a Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	if err := validateConfig(home); err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(credentialsPath(home))
	if err != nil {
		return nil, nil, err
	}
	if _, err := ParseCreds(raw); err != nil {
		return nil, nil, err
	}
	// /me must supply the stable user_id before the application persists this.
	// Neither access nor rotating refresh tokens provide stable local identity.
	id := adapter.Identity{Tool: adapter.Kimi}
	b, err := json.Marshal(payload{Kind: "kimi.oauth.v1", Identity: id, Credentials: string(raw)})
	if err != nil {
		return nil, nil, err
	}
	return []adapter.Blob{{Tool: adapter.Kimi, Identity: id, Payload: b, CapturedAt: time.Now()}}, nil, nil
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
	id.Tool = adapter.Kimi
	return id, nil
}

func (a Adapter) Restore(home string, blob adapter.Blob, _ adapter.RestoreOpts) error {
	p, err := readPayload(blob)
	if err != nil {
		return err
	}
	if err := validateConfig(home); err != nil {
		return err
	}
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	// Official Kimi uses a proper-lockfile directory, not flock. Do not claim
	// interoperability with qswitch's flock or remove a possibly live lock.
	if _, err := os.Lstat(filepath.Join(dataDir(home), "oauth", "kimi-code.lock")); err == nil {
		return errors.New("kimi: OAuth refresh lock is present")
	} else if !os.IsNotExist(err) {
		return err
	}
	return livefile.AtomicWrite(credentialsPath(home), []byte(p.Credentials), 0600)
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
		if isKimiCLI(p.Command) {
			h.Manual = append(h.Manual, p)
		}
	}
	if len(h.Manual) > 0 {
		h.Why = []string{"kimi cli running"}
	}
	return h, nil
}
func (a Adapter) AutoBlockers(home string) (adapter.Holders, error) { return a.ManualBlockers(home) }
func (a Adapter) Idle(home string, _ time.Duration) bool {
	h, err := a.ManualBlockers(home)
	return err == nil && !h.ManualBusy()
}
func (a Adapter) KillCLI(home string) error {
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	return nil
}

func isKimiCLI(command string) bool {
	f := strings.Fields(command)
	if len(f) == 0 {
		return false
	}
	name := filepath.Base(f[0])
	if name == "kimi" {
		return true
	}
	args := f[1:]
	if strings.HasPrefix(name, "python") {
		for len(args) > 0 && (args[0] == "-u" || args[0] == "-B" || args[0] == "-E" || args[0] == "-I" || args[0] == "-s" || args[0] == "-S" || args[0] == "-O" || args[0] == "-OO") {
			args = args[1:]
		}
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) > 0 && filepath.Base(args[0]) == "kimi" {
			return true
		}
		return len(args) > 1 && args[0] == "-m" && args[1] == "kimi_cli"
	}
	if name == "node" || name == "bun" {
		for len(args) > 0 && (args[0] == "--use-system-ca" || args[0] == "--no-warnings") {
			args = args[1:]
		}
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		return len(args) > 0 && strings.HasSuffix(filepath.ToSlash(args[0]), "/@moonshot-ai/kimi-code/dist/main.mjs")
	}
	return false
}

func EnrichIdentity(id adapter.Identity, body []byte) adapter.Identity {
	uid, email, plan := quota.KimiIdentity(body)
	if uid != "" {
		id.StableID = uid
	}
	if email != "" {
		id.Email = email
	}
	if plan != "" {
		id.PlanHint = plan
	}
	var profile map[string]any
	if uid != "" && json.Unmarshal(body, &profile) == nil {
		id.DisplayName, _ = profile["nickname"].(string)
		id.DisplayName = strings.TrimSpace(id.DisplayName)
		if id.DisplayName == "" {
			id.DisplayName, _ = profile["username"].(string)
			id.DisplayName = strings.TrimSpace(id.DisplayName)
		}
		if phone, ok := profile["phone"].(map[string]any); ok {
			number, _ := phone["number"].(string)
			country, _ := phone["country_code"].(string)
			number, country = strings.TrimSpace(number), strings.TrimSpace(country)
			if number != "" {
				id.Phone = number
				if country != "" && !strings.HasPrefix(number, "+") {
					id.Phone = "+" + strings.TrimPrefix(country, "+") + " " + number
				}
			}
		}
	}
	id.Tool = adapter.Kimi
	return id
}

func WriteIdentity(blob adapter.Blob, id adapter.Identity) (adapter.Blob, error) {
	if id.StableID == "" {
		return blob, errors.New("kimi: /me user_id required before saving account")
	}
	p, err := readPayload(blob)
	if err != nil {
		return blob, err
	}
	id.Tool = adapter.Kimi
	p.Identity = id
	raw, err := json.Marshal(p)
	if err != nil {
		return blob, err
	}
	blob.Payload = raw
	blob.Identity = id
	return blob, nil
}

func ApplyRefresh(blob adapter.Blob, tokens quota.KimiTokens, now time.Time) (adapter.Blob, error) {
	if strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > int64(^uint64(0)>>1)-now.Unix() {
		return blob, errors.New("kimi: invalid refreshed credentials")
	}
	p, err := readPayload(blob)
	if err != nil {
		return blob, err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(p.Credentials), &raw); err != nil {
		return blob, fmt.Errorf("kimi: invalid original credentials")
	}
	raw["access_token"], raw["refresh_token"] = tokens.AccessToken, tokens.RefreshToken
	raw["expires_at"], raw["expires_in"] = now.Unix()+tokens.ExpiresIn, tokens.ExpiresIn
	if tokens.Scope != "" {
		raw["scope"] = tokens.Scope
	}
	if tokens.TokenType != "" {
		raw["token_type"] = tokens.TokenType
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return blob, err
	}
	p.Credentials = string(b)
	blob.Payload, err = json.Marshal(p)
	blob.StaleCLI = false
	return blob, err
}
