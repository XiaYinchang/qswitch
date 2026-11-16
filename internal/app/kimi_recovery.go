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
	"qswitch/internal/adapter/kimi"
	"qswitch/internal/livefile"
)

const kimiRecoveryVault = "kimi-recovery"

// Recovery records are encrypted separately from account snapshots. In
// particular, only a profile-verified identity may become an account key here. Old token
// pairs, not expiry timestamps, identify credentials consumed by an exchange.
// A record without Updated fences an exchange whose outcome is unknown.
type kimiRecovery struct {
	Version   int           `json:"version"`
	Old       []kimi.Creds  `json:"old"`
	Updated   *adapter.Blob `json:"updated,omitempty"`
	AccountID string        `json:"account_id,omitempty"`
	LivePath  string        `json:"live_path,omitempty"`
	key       string
}

func (r kimiRecovery) consumed(c kimi.Creds) bool {
	for _, old := range r.Old {
		if sameKimiTokens(old, c) {
			return true
		}
	}
	return false
}

func (a *App) kimiRecoveries() ([]kimiRecovery, error) {
	ids, err := a.Vault.List(kimiRecoveryVault)
	if err != nil {
		return nil, fmt.Errorf("kimi: read credential recovery: %w", err)
	}
	var records []kimiRecovery
	for _, id := range ids {
		raw, err := a.Vault.Get(kimiRecoveryVault, id)
		if err != nil {
			return nil, fmt.Errorf("kimi: read credential recovery: %w", err)
		}
		var r kimiRecovery
		if json.Unmarshal(raw, &r) != nil || r.Version != 1 || len(r.Old) == 0 || r.AccountID == "" {
			return nil, errors.New("kimi: invalid credential recovery; restore the encrypted recovery file before retrying")
		}
		for _, old := range r.Old {
			if old.AccessToken == "" || old.RefreshToken == "" {
				return nil, errors.New("kimi: invalid credential recovery token pair")
			}
		}
		if r.Updated != nil {
			if _, err := kimi.CredsFromBlob(*r.Updated); err != nil || r.Updated.Tool != adapter.Kimi || r.Updated.Identity.StableID != r.AccountID {
				return nil, errors.New("kimi: invalid recovered credential snapshot")
			}
		}
		r.key = id
		records = append(records, r)
	}
	return records, nil
}

func (a *App) putKimiRecovery(r kimiRecovery) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := a.Vault.Put(kimiRecoveryVault, r.key, raw); err != nil {
		return fmt.Errorf("kimi: preserve credential recovery: %w", err)
	}
	// Vault.Put syncs the ciphertext; also make its directory entry durable
	// before an exchange or any subsequent profile/writeback operation.
	return a.syncKimiRecoveryDir()
}

func (a *App) syncKimiRecoveryDir() error {
	dir, err := os.Open(filepath.Join(a.Vault.Dir, "vault", kimiRecoveryVault))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (a *App) deleteKimiRecovery(r kimiRecovery) error {
	if r.Updated != nil {
		dir, err := os.Open(filepath.Join(a.Vault.Dir, "vault", string(adapter.Kimi)))
		if err != nil {
			return err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return err
		}
	}
	if err := a.Vault.Delete(kimiRecoveryVault, r.key); err != nil {
		return err
	}
	return a.syncKimiRecoveryDir()
}

// Called under the Kimi tool lock, before spending a refresh token. Persist
// the intent first so a lost response or interrupted process cannot reuse it.
func (a *App) beginKimiRecovery(b adapter.Blob, livePath string) (kimiRecovery, error) {
	c, err := kimi.CredsFromBlob(b)
	if err != nil {
		return kimiRecovery{}, err
	}
	records, err := a.kimiRecoveries()
	if err != nil {
		return kimiRecovery{}, err
	}
	for _, r := range records {
		if r.consumed(c) {
			return kimiRecovery{}, kimiRecoveryUncertain()
		}
		if r.Updated == nil {
			continue
		}
		previous, _ := kimi.CredsFromBlob(*r.Updated)
		if sameKimiTokens(previous, c) {
			r.Old = append(r.Old, c)
			r.Updated = nil
			r.AccountID = b.Identity.StableID
			r.LivePath = livePath
			return r, a.putKimiRecovery(r)
		}
	}
	h := sha256.Sum256([]byte(c.AccessToken + "\x00" + c.RefreshToken))
	r := kimiRecovery{Version: 1, Old: []kimi.Creds{c}, AccountID: b.Identity.StableID, LivePath: livePath, key: hex.EncodeToString(h[:])}
	return r, a.putKimiRecovery(r)
}

func kimiRecoveryUncertain() error {
	return errors.New("kimi: previous refresh outcome is unknown; sign in again with qswitch login kimi before reusing this account")
}

// Reconcile only the selected credential lineage. An unrelated external login
// remains untouched, and an uncertain exchange does not block other accounts.
// A nil target means capture is about to read the live file.
func (a *App) recoverKimiLocked(target *adapter.Blob) error {
	records, err := a.kimiRecoveries()
	if err != nil {
		return err
	}
	path := a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0]
	for _, r := range records {
		var selected kimi.Creds
		if target != nil {
			selected, err = kimi.CredsFromBlob(*target)
			if err != nil {
				return err
			}
		} else {
			if r.LivePath != path {
				continue
			}
			selected, err = kimi.ReadLiveCreds(a.UserHome)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("kimi: read live credentials during recovery: %w", err)
			}
		}
		var renewed kimi.Creds
		if r.Updated != nil {
			renewed, _ = kimi.CredsFromBlob(*r.Updated)
		}
		if !r.consumed(selected) && (r.Updated == nil || !sameKimiTokens(selected, renewed)) {
			continue
		}
		if r.Updated == nil {
			return kimiRecoveryUncertain()
		}
		if r.AccountID != "" {
			stored, getErr := a.Vault.Get(string(adapter.Kimi), r.AccountID)
			if getErr != nil && !os.IsNotExist(getErr) {
				return fmt.Errorf("kimi: read account during credential recovery: %w", getErr)
			}
			if getErr == nil {
				current, err := kimi.CredsFromBlob(adapter.Blob{Payload: stored})
				if err != nil {
					return err
				}
				if !r.consumed(current) && !sameKimiTokens(current, renewed) {
					return errors.New("kimi: saved credentials changed during recovery; refusing to overwrite the newer login")
				}
			}
			if err := a.saveBlobUnchecked(*r.Updated); err != nil {
				return err
			}
		}
		if r.LivePath != "" {
			if r.LivePath != path {
				return errors.New("kimi: credential recovery belongs to a different live path; restore KIMI_CODE_HOME before retrying")
			}
			if err := a.writeKimiRecoveryLive(r); err != nil {
				return err
			}
		}
		if target != nil {
			*target = *r.Updated
		}
		// Remove the journal only after vault and live writeback both completed.
		if r.AccountID != "" {
			if err := a.deleteKimiRecovery(r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) writeKimiRecoveryLive(r kimiRecovery) error {
	lock, err := kimi.AcquireAuthRefreshLock(context.Background(), a.UserHome)
	if err != nil {
		return err
	}
	defer lock.Close()
	return a.writeKimiRecoveryLiveLocked(r, lock)
}

func (a *App) writeKimiRecoveryLiveLocked(r kimiRecovery, lock *kimi.AuthRefreshLock) error {
	raw, err := os.ReadFile(r.LivePath)
	if os.IsNotExist(err) {
		return nil
	} // Do not undo a logout.
	if err != nil {
		return err
	}
	current, err := kimi.ParseCreds(raw)
	if err != nil {
		return err
	}
	if !r.consumed(current) {
		return nil
	} // An external login owns the slot.
	h, err := a.Adapters[adapter.Kimi].ManualBlockers(a.UserHome)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return errors.New("kimi: credential writeback is pending; close the running Kimi CLI and retry")
	}
	var refreshed struct {
		Credentials string `json:"credentials_json"`
	}
	if err := json.Unmarshal(r.Updated.Payload, &refreshed); err != nil {
		return err
	}
	// Keep non-OAuth client metadata; only replace fields owned by the exchange.
	var live, next map[string]json.RawMessage
	if err := json.Unmarshal(raw, &live); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(refreshed.Credentials), &next); err != nil {
		return err
	}
	for _, key := range []string{"access_token", "refresh_token", "expires_at", "expires_in", "scope", "token_type"} {
		if value, ok := next[key]; ok {
			live[key] = value
		}
	}
	out, err := json.Marshal(live)
	if err != nil {
		return err
	}
	h, err = a.Adapters[adapter.Kimi].ManualBlockers(a.UserHome)
	if err != nil {
		return err
	}
	if h.ManualBusy() {
		return errors.New("kimi: credential writeback is pending; close the running Kimi CLI and retry")
	}
	after, err := os.ReadFile(r.LivePath)
	if err != nil || !bytes.Equal(raw, after) || !lock.OwnsPath() || lock.Context().Err() != nil {
		return errors.New("kimi: live credentials or auth lock changed during recovery; retry")
	}
	if err := livefile.AtomicWrite(r.LivePath, out, 0600); err != nil {
		return fmt.Errorf("kimi: credential writeback is pending: %w", err)
	}
	return nil
}

// Every account save passes this guard, including isolated enrollment. A new
// verified login can supersede an uncertain exchange without touching live
// credentials. Old pairs remain fenced until a later safe writeback completes.
func (a *App) saveKimiBlob(b adapter.Blob) error {
	id, err := a.Adapters[adapter.Kimi].IdentityOf(b)
	if err != nil {
		return err
	}
	if id.StableID == "" {
		return errors.New("account identity must be verified before saving")
	}
	b.Identity = id
	records, err := a.kimiRecoveries()
	if err != nil {
		return err
	}
	c, err := kimi.CredsFromBlob(b)
	if err != nil {
		return err
	}
	for _, r := range records {
		var renewed kimi.Creds
		if r.Updated != nil {
			renewed, _ = kimi.CredsFromBlob(*r.Updated)
		}
		matchesUpdated := r.Updated != nil && sameKimiTokens(c, renewed)
		if r.consumed(c) {
			return errors.New("kimi: refusing to save consumed credentials; recover the pending writeback or sign in again")
		}
		if !matchesUpdated && (r.AccountID == "" || r.AccountID != b.Identity.StableID) {
			continue
		}
		if r.AccountID != "" && r.AccountID != b.Identity.StableID {
			return errors.New("kimi: recovered credentials have a different verified account identity")
		}
		if r.Updated != nil && !matchesUpdated {
			r.Old = append(r.Old, renewed)
		}
		r.AccountID = b.Identity.StableID
		r.Updated = &b
		if err := a.putKimiRecovery(r); err != nil {
			return err
		}
	}
	return a.saveBlobUnchecked(b)
}
