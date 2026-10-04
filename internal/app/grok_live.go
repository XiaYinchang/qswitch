package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/grok"
	"qswitch/internal/livefile"
	"qswitch/internal/secutil"
)

// The caller holds qswitch's tool lock. A live CLI owns its renewal. With no
// CLI, share the official auth lock across disk re-read, exchange and writeback.
func (a *App) keepAliveLiveGrokLocked(ctx context.Context, b adapter.Blob, live grok.AuthFile, force, bypassBackoff bool) (adapter.Blob, string, error) {
	if !live.Refreshable() || (!force && !live.NeedsRefresh(a.now())) {
		return b, live.Key, nil
	}
	blocked, err := a.liveGrokRefreshBlocked(false)
	if err != nil {
		return b, "", err
	}
	if blocked {
		return b, live.Key, nil
	}
	if !bypassBackoff {
		ac, err := a.State.GetAccount("grok", b.Identity.StableID)
		if err != nil {
			return b, "", err
		}
		if ac.RefreshBackoffUntil > a.now().Unix() {
			return b, "", errors.New("grok: refresh is in backoff")
		}
	}
	lock, err := grok.AcquireAuthRefreshLock(a.UserHome)
	if err != nil {
		return b, "", err
	}
	defer lock.Close()
	path := filepath.Join(a.UserHome, ".grok", "auth.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, "", err
	}
	current, err := grok.ReadAuthForIdentity(raw, b.Identity.StableID)
	stored, storedErr := grok.ReadAuth(b)
	if err != nil || storedErr != nil || !grok.SameOIDCUser(stored, current) {
		return b, "", errors.New("grok: live identity changed before refresh")
	}
	// A sibling may have renewed while qswitch was waiting on its own lock.
	if current.Key != live.Key || current.Refresh != live.Refresh || !current.ExpiresAt.Equal(live.ExpiresAt) {
		force = false
	}
	if !current.Refreshable() || (!force && !current.NeedsRefresh(a.now())) {
		return b, current.Key, nil
	}
	blocked, err = a.liveGrokRefreshBlocked(true)
	if err != nil {
		return b, "", err
	}
	if blocked {
		return b, current.Key, nil
	}
	if a.HTTP.Client == nil && secutil.InTest() {
		return b, "", errors.New("grok: refresh HTTP client is required in tests")
	}
	// Preserve every live slot and unknown field, along with the vault's attached
	// desktop snapshot. Token renewal never restores or restarts the desktop.
	var env map[string]json.RawMessage
	if err := json.Unmarshal(b.Payload, &env); err != nil {
		return b, "", err
	}
	env["auth_json"] = json.RawMessage(raw)
	b.Payload, err = json.Marshal(env)
	if err != nil {
		return b, "", err
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := a.HTTP.GrokRefresh(refreshCtx, current.Refresh, current.ClientID)
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Grok, b.Identity.StableID, tok.Permanent, err)
	}
	nb, err := grok.ApplyRefresh(b, tok, a.now())
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Grok, b.Identity.StableID, false, err)
	}
	// Persist rotated tokens even if an uncooperative writer changed the file;
	// they must not be lost or overwritten with the spent refresh token.
	if err := a.saveBlob(nb); err != nil {
		return nb, "", a.recordRefreshResult(adapter.Grok, b.Identity.StableID, false, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) || !lock.OwnsPath() {
		return nb, "", a.recordRefreshResult(adapter.Grok, b.Identity.StableID, false, errors.New("grok: live credentials or auth lock changed during refresh"))
	}
	var refreshed struct {
		AuthJSON json.RawMessage `json:"auth_json"`
	}
	if err := json.Unmarshal(nb.Payload, &refreshed); err != nil {
		return nb, "", err
	}
	if err := livefile.AtomicWrite(path, refreshed.AuthJSON, 0600); err != nil {
		return nb, "", a.recordRefreshResult(adapter.Grok, b.Identity.StableID, false, err)
	}
	a.rememberFP(adapter.Grok)
	if err := a.recordRefreshResult(adapter.Grok, b.Identity.StableID, false, nil); err != nil {
		return nb, "", err
	}
	return nb, tok.AccessToken, nil
}

func (a *App) liveGrokRefreshBlocked(ownsAuthLock bool) (bool, error) {
	h, err := a.Adapters[adapter.Grok].ManualBlockers(a.UserHome)
	if err != nil {
		return true, err
	}
	for _, p := range h.Manual {
		if ownsAuthLock && p.PID == os.Getpid() && p.Command == "grok-pidfile" {
			continue
		} // Our held auth lock.
		return true, nil
	}
	return false, nil
}
