package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/livefile"
)

const claudeRecoveryVault = "claude-recovery"

// Recovery records are encrypted separately from account snapshots. In
// particular, a captured metadata UUID is never an account key here. Old token
// pairs, not expiry timestamps, identify credentials consumed by an exchange.
// A record without Updated fences an exchange whose outcome is unknown.
type claudeRecovery struct {
	Version   int            `json:"version"`
	Old       []claude.Creds `json:"old"`
	Updated   *adapter.Blob  `json:"updated,omitempty"`
	AccountID string         `json:"account_id,omitempty"`
	LivePath  string         `json:"live_path,omitempty"`
	key       string
}

func (r claudeRecovery) consumed(c claude.Creds) bool {
	for _, old := range r.Old {
		if sameClaudeTokens(old, c) {
			return true
		}
	}
	return false
}

func (a *App) claudeRecoveries() ([]claudeRecovery, error) {
	ids, err := a.Vault.List(claudeRecoveryVault)
	if err != nil {
		return nil, fmt.Errorf("claude: read credential recovery: %w", err)
	}
	var records []claudeRecovery
	for _, id := range ids {
		raw, err := a.Vault.Get(claudeRecoveryVault, id)
		if err != nil {
			return nil, fmt.Errorf("claude: read credential recovery: %w", err)
		}
		var r claudeRecovery
		if json.Unmarshal(raw, &r) != nil || r.Version != 1 || len(r.Old) == 0 {
			return nil, errors.New("claude: invalid credential recovery; restore the encrypted recovery file before retrying")
		}
		for _, old := range r.Old {
			if old.AccessToken == "" || old.RefreshToken == "" {
				return nil, errors.New("claude: invalid credential recovery token pair")
			}
		}
		if r.Updated != nil {
			if _, err := claude.CredsFromBlob(*r.Updated); err != nil || r.Updated.Tool != adapter.Claude || r.Updated.Identity.StableID != r.AccountID {
				return nil, errors.New("claude: invalid recovered credential snapshot")
			}
		}
		r.key = id
		records = append(records, r)
	}
	return records, nil
}

func (a *App) putClaudeRecovery(r claudeRecovery) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := a.Vault.Put(claudeRecoveryVault, r.key, raw); err != nil {
		return fmt.Errorf("claude: preserve credential recovery: %w", err)
	}
	// Vault.Put syncs the ciphertext; also make its directory entry durable
	// before an exchange or any subsequent profile/writeback operation.
	return a.syncClaudeRecoveryDir()
}

func (a *App) syncClaudeRecoveryDir() error {
	dir, err := os.Open(filepath.Join(a.Vault.Dir, "vault", claudeRecoveryVault))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (a *App) deleteClaudeRecovery(r claudeRecovery) error {
	if err := a.Vault.Delete(claudeRecoveryVault, r.key); err != nil {
		return err
	}
	return a.syncClaudeRecoveryDir()
}

// Called under the Claude tool lock, before spending a refresh token. Persist
// the intent first so a lost response or interrupted process cannot reuse it.
func (a *App) beginClaudeRecovery(b adapter.Blob, livePath string) (claudeRecovery, error) {
	c, err := claude.CredsFromBlob(b)
	if err != nil {
		return claudeRecovery{}, err
	}
	records, err := a.claudeRecoveries()
	if err != nil {
		return claudeRecovery{}, err
	}
	for _, r := range records {
		if r.consumed(c) {
			return claudeRecovery{}, claudeRecoveryUncertain()
		}
		if r.Updated == nil {
			continue
		}
		previous, _ := claude.CredsFromBlob(*r.Updated)
		if sameClaudeTokens(previous, c) {
			r.Old = append(r.Old, c)
			r.Updated = nil
			r.AccountID = b.Identity.StableID
			r.LivePath = livePath
			return r, a.putClaudeRecovery(r)
		}
	}
	h := sha256.Sum256([]byte(c.AccessToken + "\x00" + c.RefreshToken))
	r := claudeRecovery{Version: 1, Old: []claude.Creds{c}, AccountID: b.Identity.StableID, LivePath: livePath, key: hex.EncodeToString(h[:])}
	return r, a.putClaudeRecovery(r)
}

func claudeRecoveryUncertain() error {
	return errors.New("claude: previous refresh outcome is unknown; sign in again with qswitch login claude before reusing this account")
}

// Reconcile only the selected credential lineage. An unrelated external login
// remains untouched, and an uncertain exchange does not block other accounts.
// A nil target means capture is about to read the live file.
func (a *App) recoverClaudeLocked(target *adapter.Blob) error {
	records, err := a.claudeRecoveries()
	if err != nil {
		return err
	}
	path := a.Adapters[adapter.Claude].LivePaths(a.UserHome)[0]
	for _, r := range records {
		var selected claude.Creds
		if target != nil {
			selected, err = claude.CredsFromBlob(*target)
			if err != nil {
				return err
			}
		} else {
			if r.LivePath != path {
				continue
			}
			selected, err = claude.ReadLiveCreds(a.UserHome)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("claude: read live credentials during recovery: %w", err)
			}
		}
		var renewed claude.Creds
		if r.Updated != nil {
			renewed, _ = claude.CredsFromBlob(*r.Updated)
		}
		if !r.consumed(selected) && (r.Updated == nil || !sameClaudeTokens(selected, renewed)) {
			continue
		}
		if r.Updated == nil {
			return claudeRecoveryUncertain()
		}
		if r.AccountID != "" {
			stored, getErr := a.Vault.Get(string(adapter.Claude), r.AccountID)
			if getErr != nil && !os.IsNotExist(getErr) {
				return fmt.Errorf("claude: read account during credential recovery: %w", getErr)
			}
			if getErr == nil {
				current, err := claude.CredsFromBlob(adapter.Blob{Payload: stored})
				if err != nil {
					return err
				}
				if !r.consumed(current) && !sameClaudeTokens(current, renewed) {
					return errors.New("claude: saved credentials changed during recovery; refusing to overwrite the newer login")
				}
			}
			if err := a.saveBlobUnchecked(*r.Updated); err != nil {
				return err
			}
		}
		if r.LivePath != "" {
			if r.LivePath != path {
				return errors.New("claude: credential recovery belongs to a different live path; restore CLAUDE_CONFIG_DIR before retrying")
			}
			if err := a.writeClaudeRecoveryLive(r); err != nil {
				return err
			}
		}
		if target != nil {
			*target = *r.Updated
		}
		// Unverified credentials stay quarantined even after live writeback;
		// capture will promote them only after a successful profile response.
		if r.AccountID != "" {
			if err := a.deleteClaudeRecovery(r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) writeClaudeRecoveryLive(r claudeRecovery) error {
	raw, err := os.ReadFile(r.LivePath)
	if os.IsNotExist(err) {
		return nil // A logout must not recreate the live login.
	}
	if err != nil {
		return err
	}
	current, err := claude.ParseCreds(raw)
	if err != nil {
		return err
	}
	if !r.consumed(current) {
		return nil // Already written, or a genuine external login owns the slot.
	}
	h, err := a.Adapters[adapter.Claude].ManualBlockers(a.UserHome)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return errors.New("claude: credential writeback is pending; close the running Claude CLI and retry")
	}
	var refreshed struct {
		Credentials string `json:"credentials_json"`
	}
	if err := json.Unmarshal(r.Updated.Payload, &refreshed); err != nil {
		return err
	}
	// Preserve fields changed by the CLI without a token rotation.
	var live, next map[string]json.RawMessage
	if err := json.Unmarshal(raw, &live); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(refreshed.Credentials), &next); err != nil {
		return err
	}
	live["claudeAiOauth"] = next["claudeAiOauth"]
	out, err := json.Marshal(live)
	if err != nil {
		return err
	}
	h, err = a.Adapters[adapter.Claude].ManualBlockers(a.UserHome)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return errors.New("claude: credential writeback is pending; close the running Claude CLI and retry")
	}
	after, err := os.ReadFile(r.LivePath)
	if err != nil || !bytes.Equal(raw, after) {
		return errors.New("claude: live credentials changed during recovery; retry")
	}
	write := livefile.AtomicWrite
	if a.claudeWriteLive != nil {
		write = a.claudeWriteLive
	}
	if err := write(r.LivePath, out, 0o600); err != nil {
		return fmt.Errorf("claude: credential writeback is pending: %w", err)
	}
	return nil
}

// Every account save passes this guard, including isolated enrollment. A new
// verified login can supersede an uncertain exchange without touching live
// credentials. Old pairs remain fenced until a later safe writeback completes.
func (a *App) saveClaudeBlob(b adapter.Blob) error {
	id, err := a.Adapters[adapter.Claude].IdentityOf(b)
	if err != nil {
		return err
	}
	if id.StableID == "" {
		return errors.New("account identity must be verified before saving")
	}
	b.Identity = id
	records, err := a.claudeRecoveries()
	if err != nil {
		return err
	}
	c, err := claude.CredsFromBlob(b)
	if err != nil {
		return err
	}
	for _, r := range records {
		var renewed claude.Creds
		if r.Updated != nil {
			renewed, _ = claude.CredsFromBlob(*r.Updated)
		}
		matchesUpdated := r.Updated != nil && sameClaudeTokens(c, renewed)
		if r.consumed(c) {
			return errors.New("claude: refusing to save consumed credentials; recover the pending writeback or sign in again")
		}
		if !matchesUpdated && (r.AccountID == "" || r.AccountID != b.Identity.StableID) {
			continue
		}
		if r.AccountID != "" && r.AccountID != b.Identity.StableID {
			return errors.New("claude: recovered credentials have a different verified account identity")
		}
		if r.Updated != nil && !matchesUpdated {
			r.Old = append(r.Old, renewed)
		}
		r.AccountID = b.Identity.StableID
		r.Updated = &b
		if err := a.putClaudeRecovery(r); err != nil {
			return err
		}
	}
	return a.saveBlobUnchecked(b)
}

// The HTTP profile rejected the captured access token with 401. Metadata is
// deliberately removed before a refresh can write any durable state.
func (a *App) renewClaudeCapture(ctx context.Context, b adapter.Blob, c claude.Creds) (adapter.Blob, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(b.Payload, &env); err != nil {
		return b, err
	}
	b.Identity = adapter.Identity{Tool: adapter.Claude}
	env["identity"], _ = json.Marshal(b.Identity)
	delete(env, "oauth_account_json")
	var err error
	b.Payload, err = json.Marshal(env)
	if err != nil {
		return b, err
	}
	updated, _, err := a.keepAliveLiveClaudeLocked(ctx, b, c, true, true)
	return updated, err
}
