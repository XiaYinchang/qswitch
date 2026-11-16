package antigravity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/busy"
)

// Adapter manages the native shared consumer login. It never logs out and
// never replaces a conversation or sends a prompt while changing accounts.
type Adapter struct {
	List func() ([]adapter.Proc, error)
	deps *adapterDependencies
}

// Per-instance seams keep orchestration tests on synthetic credentials and
// the real file transaction, without changing package-global auth behavior.
type adapterDependencies struct {
	read     func(string) ([]Candidate, error)
	validate func(context.Context, []byte) ([]byte, adapter.Identity, error)
	write    func(string, []byte) (func() error, error)
	sync     func(context.Context, []Endpoint, string) error
}

func (a *Adapter) dependencies() adapterDependencies {
	d := adapterDependencies{read: ReadCredentials, validate: ValidatedCredential, write: WriteCredentialWithRollback, sync: SyncEndpoints}
	if a.deps != nil {
		if a.deps.read != nil {
			d.read = a.deps.read
		}
		if a.deps.validate != nil {
			d.validate = a.deps.validate
		}
		if a.deps.write != nil {
			d.write = a.deps.write
		}
		if a.deps.sync != nil {
			d.sync = a.deps.sync
		}
	}
	return d
}

type payload struct {
	Kind        string           `json:"kind"`
	Identity    adapter.Identity `json:"identity"`
	Credentials json.RawMessage  `json:"credentials"`
}

func (*Adapter) Name() adapter.Tool             { return adapter.Antigravity }
func (*Adapter) LivePaths(home string) []string { return CredentialPaths(home) }
func (a *Adapter) Fingerprint(home string) ([32]byte, error) {
	candidates, err := a.dependencies().read(home)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	for _, c := range candidates {
		h.Write([]byte(c.Source))
		h.Write([]byte{0})
		h.Write(c.Raw)
		h.Write([]byte{0})
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func (a *Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	d := a.dependencies()
	endpoints, uncovered, err := a.nativeClients(home)
	if err != nil {
		return nil, nil, err
	}
	if len(uncovered) != 0 {
		return nil, nil, errors.New("antigravity: a running native client has no verifiable control endpoint; its active account is unknown")
	}
	candidates, err := d.read(home)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var blobs []adapter.Blob
	var warnings []adapter.Warning
	seen := map[string]bool{}
	keyringID, keyringPresent := "", false
	for _, candidate := range candidates {
		isKeyring := strings.HasPrefix(candidate.Source, "keyring:")
		keyringPresent = keyringPresent || isKeyring
		raw, id, err := d.validate(ctx, candidate.Raw)
		if err != nil {
			warnings = append(warnings, adapter.Warning("antigravity_login_not_verified"))
			continue
		}
		if isKeyring {
			keyringID = id.StableID
		}
		if seen[id.StableID] {
			continue
		}
		seen[id.StableID] = true
		blob, err := NewBlob(raw, id)
		if err != nil {
			return nil, warnings, err
		}
		blobs = append(blobs, blob)
	}
	if len(blobs) == 0 {
		return nil, warnings, errors.New("antigravity: no saved login passed identity verification")
	}
	// Fallback files can be old accounts. Prefer the account actually loaded
	// by a running native client over their directory or file order.
	activeEmail := ""
	for _, endpoint := range endpoints {
		email, err := endpoint.UserEmail(ctx)
		if err != nil {
			return nil, warnings, errors.New("antigravity: cannot verify the running client's identity")
		}
		if activeEmail != "" && !strings.EqualFold(email, activeEmail) {
			return nil, warnings, errors.New("antigravity: running clients use different accounts; finish their sessions before aligning accounts")
		}
		activeEmail = email
	}
	if activeEmail != "" {
		found := false
		for i := range blobs {
			if strings.EqualFold(blobs[i].Identity.Email, activeEmail) {
				blobs[0], blobs[i] = blobs[i], blobs[0]
				found = true
				break
			}
		}
		if !found {
			return nil, warnings, errors.New("antigravity: the running account has no verified saved credential")
		}
	} else if keyringID != "" {
		for i := range blobs {
			if blobs[i].Identity.StableID == keyringID {
				blobs[0], blobs[i] = blobs[i], blobs[0]
				break
			}
		}
	} else if keyringPresent || len(blobs) > 1 {
		return nil, warnings, errors.New("antigravity: saved fallback accounts do not establish one active login")
	}
	return blobs, warnings, nil
}

func NewBlob(raw []byte, id adapter.Identity) (adapter.Blob, error) {
	if id.StableID == "" || id.Email == "" {
		return adapter.Blob{}, errors.New("antigravity: verified account ID and email required")
	}
	c, err := parseConsumerCredential(raw)
	if err != nil {
		return adapter.Blob{}, err
	}
	if c.IDTokenEmail != "" && !strings.EqualFold(c.IDTokenEmail, id.Email) {
		return adapter.Blob{}, errors.New("antigravity: stored and verified account identities differ")
	}
	id.Tool = adapter.Antigravity
	b, err := json.Marshal(payload{Kind: "antigravity.oauth.v1", Identity: id, Credentials: raw})
	return adapter.Blob{Tool: adapter.Antigravity, Identity: id, Payload: b, CapturedAt: time.Now()}, err
}

func CredentialsFromBlob(blob adapter.Blob) ([]byte, error) {
	var p payload
	if json.Unmarshal(blob.Payload, &p) != nil || p.Kind != "antigravity.oauth.v1" {
		return nil, errors.New("antigravity: invalid account snapshot")
	}
	if _, err := parseConsumerCredential(p.Credentials); err != nil {
		return nil, err
	}
	return p.Credentials, nil
}

func (*Adapter) IdentityOf(blob adapter.Blob) (adapter.Identity, error) {
	var p payload
	if _, err := CredentialsFromBlob(blob); err != nil {
		return adapter.Identity{}, err
	}
	_ = json.Unmarshal(blob.Payload, &p)
	if p.Identity.StableID == "" || p.Identity.Email == "" {
		return adapter.Identity{}, errors.New("antigravity: snapshot has no verified identity")
	}
	p.Identity.Tool = adapter.Antigravity
	return p.Identity, nil
}

func parseConsumerCredential(raw []byte) (Credential, error) {
	c, err := ParseCredential(raw)
	if err != nil {
		return c, err
	}
	if c.AuthMethod != "consumer" {
		return c, errors.New("antigravity: only personal Google account subscriptions can rotate")
	}
	return c, nil
}

func ValidatedCredential(ctx context.Context, raw []byte) ([]byte, adapter.Identity, error) {
	c, err := parseConsumerCredential(raw)
	if err != nil {
		return nil, adapter.Identity{}, err
	}
	if c.Token.Expiry.IsZero() || time.Until(c.Token.Expiry) < 5*time.Minute {
		raw, err = Refresh(ctx, raw)
		if err != nil {
			return nil, adapter.Identity{}, err
		}
	}
	id, err := Verify(ctx, raw)
	if err != nil {
		return nil, adapter.Identity{}, err
	}
	if c.IDTokenEmail != "" && !strings.EqualFold(c.IDTokenEmail, id.Email) {
		return nil, adapter.Identity{}, errors.New("antigravity: stored and verified account identities differ")
	}
	return raw, id, nil
}

func (a *Adapter) Restore(home string, blob adapter.Blob, opts adapter.RestoreOpts) error {
	_, err := a.RestoreWithRollback(home, blob, opts)
	return err
}

// RestoreWithRollback keeps native changes reversible until qswitch commits
// its account pointer. The returned undo is idempotent and retains exact old
// storage bytes and the verified previous native identity only in memory.
func (a *Adapter) RestoreWithRollback(home string, blob adapter.Blob, opts adapter.RestoreOpts) (func() error, error) {
	if opts.CLIOnly {
		return nil, errors.New("antigravity: desktop and CLI share one login; cli-only switching is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	d := a.dependencies()
	savedID, err := a.IdentityOf(blob)
	if err != nil {
		return nil, err
	}
	if blob.Tool != adapter.Antigravity || savedID.StableID != blob.Identity.StableID || !strings.EqualFold(savedID.Email, blob.Identity.Email) {
		return nil, errors.New("antigravity: snapshot identity metadata differs")
	}
	raw, err := CredentialsFromBlob(blob)
	if err != nil {
		return nil, err
	}
	raw, id, err := d.validate(ctx, raw)
	if err != nil {
		return nil, err
	}
	if id.StableID != savedID.StableID || !strings.EqualFold(id.Email, savedID.Email) {
		return nil, errors.New("antigravity: target identity changed")
	}
	endpoints, uncovered, err := a.nativeClients(home)
	if err != nil {
		return nil, err
	}
	if len(uncovered) != 0 {
		return nil, &adapter.BusyError{Holders: adapter.Holders{Manual: uncovered, Why: []string{"Antigravity native client has no verifiable control endpoint"}}}
	}
	previousEmail := ""
	for _, endpoint := range endpoints {
		email, err := endpoint.UserEmail(ctx)
		if err != nil {
			return nil, errors.New("antigravity: cannot verify a native client's current account")
		}
		if previousEmail != "" && !strings.EqualFold(previousEmail, email) {
			return nil, errors.New("antigravity: running clients use different accounts; no credentials were changed")
		}
		previousEmail = email
		idle, err := endpoint.Idle(ctx, 30*time.Second)
		if err != nil || !idle {
			return nil, errors.New("antigravity: a native session is busy or its state is unknown")
		}
	}
	rollback, err := d.write(home, raw)
	if err != nil {
		return nil, err
	}
	if rollback == nil {
		return nil, errors.New("antigravity: credential transaction did not provide rollback")
	}
	var undoOnce sync.Once
	var undoErr error
	undo := func() error {
		undoOnce.Do(func() {
			undoErr = rollback()
			if undoErr == nil && len(endpoints) != 0 {
				// Forward timeout or a failed account-pointer commit must not
				// cancel recovery of already switched native clients.
				recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer recoveryCancel()
				if syncErr := d.sync(recoveryCtx, endpoints, previousEmail); syncErr != nil {
					undoErr = fmt.Errorf("antigravity: previous client account was not confirmed after rollback: %w", syncErr)
				}
			}
		})
		return undoErr
	}
	if err := d.sync(ctx, endpoints, id.Email); err != nil {
		return nil, errors.Join(fmt.Errorf("antigravity: clients did not confirm the new account: %w", err), undo())
	}
	return undo, nil
}

func (a *Adapter) ManualBlockers(home string) (adapter.Holders, error) {
	endpoints, uncovered, err := a.nativeClients(home)
	if err != nil {
		return adapter.Holders{Manual: uncovered, Why: []string{"Antigravity native control state is unavailable"}}, err
	}
	h := adapter.Holders{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, endpoint := range endpoints {
		idle, err := endpoint.Idle(ctx, 30*time.Second)
		if err != nil || !idle {
			h.Manual = append(h.Manual, adapter.Proc{PID: endpoint.PID, Command: "Antigravity native session"})
			h.Why = append(h.Why, "Antigravity session is busy or its status is unavailable")
		}
	}
	for _, p := range uncovered {
		h.Manual = append(h.Manual, p)
		h.Why = append(h.Why, "Antigravity native client has no verifiable control endpoint")
	}
	return h, nil
}

func (a *Adapter) nativeClients(home string) ([]Endpoint, []adapter.Proc, error) {
	list := a.List
	if list == nil {
		list = (busy.PS{}).List
	}
	procs, err := list()
	if err != nil {
		return nil, nil, errors.New("antigravity: process list unavailable")
	}
	var native []adapter.Proc
	for _, p := range procs {
		if p.PID > 0 && nativeRPCProcess(p.Command) {
			native = append(native, adapter.Proc{PID: p.PID, Command: processName(p.Command)})
		}
	}
	endpoints, err := Discover(home, func() ([]adapter.Proc, error) { return procs, nil })
	if err != nil {
		// Discovery fails closed. Preserve the independently observed native
		// PIDs so status surfaces do not describe an unknown client as idle.
		return nil, native, err
	}
	covered := make(map[int]bool, len(endpoints))
	for _, endpoint := range endpoints {
		covered[endpoint.PID] = true
	}
	var uncovered []adapter.Proc
	for _, p := range native {
		if !covered[p.PID] {
			// Keep only the native program name; command arguments can contain
			// prompts or credentials and do not belong in blockers or diagnostics.
			uncovered = append(uncovered, p)
		}
	}
	return endpoints, uncovered, nil
}

func processName(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	if command[0] == '"' {
		end := strings.IndexByte(command[1:], '"')
		if end < 0 {
			return ""
		}
		command = command[1 : 1+end]
	} else {
		command = strings.Fields(command)[0]
	}
	return strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
}

func (a *Adapter) AutoBlockers(home string) (adapter.Holders, error) { return a.ManualBlockers(home) }
func (a *Adapter) Idle(home string, _ time.Duration) bool {
	h, err := a.ManualBlockers(home)
	return err == nil && !h.ManualBusy()
}
func (a *Adapter) KillCLI(home string) error {
	h, err := a.ManualBlockers(home)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return &adapter.BusyError{Holders: h}
	}
	return nil
}
