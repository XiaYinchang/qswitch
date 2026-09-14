package devin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func (a Adapter) Name() adapter.Tool { return adapter.Devin }

func credPath(home string) string {
	if x := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); x != "" {
		return filepath.Join(x, "devin", "credentials.toml")
	}
	return filepath.Join(home, ".local", "share", "devin", "credentials.toml")
}

func dataDir(home string) string {
	if x := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); x != "" {
		return filepath.Join(x, "devin")
	}
	return filepath.Join(home, ".local", "share", "devin")
}

func (a Adapter) LivePaths(home string) []string {
	return []string{credPath(home)}
}

func (a Adapter) Fingerprint(home string) ([32]byte, error) {
	b, err := os.ReadFile(credPath(home))
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
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
	cli := busy.DevinCLI(list)
	h := adapter.Holders{Manual: cli}
	if len(cli) > 0 {
		h.Why = append(h.Why, "devin cli running")
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
	apps := busy.DevinApp(list)
	if len(apps) > 0 {
		h.AutoExtra = apps
		h.DevinApp = true
		h.Why = append(h.Why, "devin.app running")
	}
	return h, nil
}

type payload struct {
	Kind        string           `json:"kind"`
	Identity    adapter.Identity `json:"identity"`
	Credentials string           `json:"credentials_toml"`
}

type Creds struct {
	APIKey string
	Server string
}

func ParseCreds(raw []byte) (Creds, error) {
	var m map[string]any
	if err := toml.Unmarshal(raw, &m); err != nil {
		return Creds{}, err
	}
	c := Creds{
		APIKey: strAny(m["windsurf_api_key"]),
		Server: strAny(m["api_server_url"]),
	}
	if c.APIKey == "" {
		return Creds{}, errors.New("devin: missing windsurf_api_key")
	}
	if c.Server == "" {
		c.Server = "https://server.codeium.com"
	}
	return c, nil
}

func strAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func CredsFromBlob(blob adapter.Blob) (Creds, error) {
	raw, err := credsTOML(blob)
	if err != nil {
		return Creds{}, err
	}
	return ParseCreds(raw)
}

func credsTOML(blob adapter.Blob) ([]byte, error) {
	var p payload
	if err := json.Unmarshal(blob.Payload, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Credentials) == "" {
		return nil, errors.New("devin: empty credentials")
	}
	return []byte(p.Credentials), nil
}

func (a Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	raw, err := os.ReadFile(credPath(home))
	if err != nil {
		return nil, nil, err
	}
	c, err := ParseCreds(raw)
	if err != nil {
		return nil, nil, err
	}
	id := adapter.Identity{Tool: adapter.Devin, StableID: keyID(c.APIKey)}
	pl, err := json.Marshal(payload{Kind: "devin.creds.v1", Identity: id, Credentials: string(raw)})
	if err != nil {
		return nil, nil, err
	}
	return []adapter.Blob{{Tool: adapter.Devin, Identity: id, CapturedAt: time.Now(), Payload: pl}}, nil, nil
}

func keyID(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return "devin-" + hex.EncodeToString(sum[:8])
}

func (a Adapter) IdentityOf(blob adapter.Blob) (adapter.Identity, error) {
	if blob.Identity.StableID != "" {
		blob.Identity.Tool = adapter.Devin
		return blob.Identity, nil
	}
	var p payload
	if err := json.Unmarshal(blob.Payload, &p); err != nil {
		return adapter.Identity{}, err
	}
	if p.Identity.StableID != "" {
		p.Identity.Tool = adapter.Devin
		return p.Identity, nil
	}
	c, err := ParseCreds([]byte(p.Credentials))
	if err != nil {
		return adapter.Identity{}, err
	}
	return adapter.Identity{Tool: adapter.Devin, StableID: keyID(c.APIKey)}, nil
}

func (a Adapter) Restore(home string, blob adapter.Blob, _ adapter.RestoreOpts) error {
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	raw, err := credsTOML(blob)
	if err != nil {
		return err
	}
	if _, err := ParseCreds(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(credPath(home)), 0o700); err != nil {
		return err
	}
	return livefile.AtomicWrite(credPath(home), raw, 0o600)
}

func (a Adapter) Idle(home string, grace time.Duration) bool {
	if len(busy.DevinCLI(mustList(a))) == 0 {
		return true
	}
	return quota.DirIdle(filepath.Join(dataDir(home), "cli"), grace, time.Now())
}

func mustList(a Adapter) []adapter.Proc {
	list, _ := a.procs()
	return list
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

func EnrichIdentity(id adapter.Identity, body []byte) adapter.Identity {
	uid, email, plan := quota.DevinIdentity(body)
	if uid != "" {
		id.StableID = uid
	}
	if email != "" {
		id.Email = email
	}
	if plan != "" {
		id.PlanHint = plan
	}
	id.Tool = adapter.Devin
	return id
}

func WriteIdentity(blob adapter.Blob, id adapter.Identity) (adapter.Blob, error) {
	var p payload
	if err := json.Unmarshal(blob.Payload, &p); err != nil {
		return blob, err
	}
	p.Identity = id
	raw, err := json.Marshal(p)
	if err != nil {
		return blob, err
	}
	blob.Payload = raw
	blob.Identity = id
	return blob, nil
}
