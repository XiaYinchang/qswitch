package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/kimi"
	"qswitch/internal/livefile"
	"qswitch/internal/secutil"
)

// The caller holds the tool lock and has established the live identity from
// matching captured credentials or /me. Never infer identity from the pointer.
func (a *App) keepAliveLiveKimiLocked(ctx context.Context, b adapter.Blob, live kimi.Creds, force, bypassBackoff bool) (adapter.Blob, string, error) {
	if !force && !live.NeedsRefresh(a.now()) {
		return b, live.AccessToken, nil
	}
	h, err := a.Adapters[adapter.Kimi].ManualBlockers(a.UserHome)
	if err != nil {
		return b, "", err
	}
	if h.ManualBusy() {
		return b, live.AccessToken, nil
	}
	if !bypassBackoff {
		ac, err := a.State.GetAccount("kimi", b.Identity.StableID)
		if err != nil {
			return b, "", err
		}
		if ac.RefreshBackoffUntil > a.now().Unix() {
			return b, "", errors.New("kimi: refresh is in backoff")
		}
	}
	lock, err := kimi.AcquireAuthRefreshLock(ctx, a.UserHome)
	if err != nil {
		return b, "", err
	}
	defer lock.Close()
	path := a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0]
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, "", err
	}
	current, err := kimi.ParseCreds(raw)
	if err != nil {
		return b, "", err
	}
	// Opaque Kimi tokens require identity verification if another process changed
	// them before we acquired the auth lock. Leave that verification to the next
	// observation rather than spend an unverified refresh token.
	if !sameKimiTokens(live, current) {
		return b, "", errors.New("kimi: live credentials changed before refresh")
	}
	if !force && !current.NeedsRefresh(a.now()) {
		return b, current.AccessToken, nil
	}
	h, err = a.Adapters[adapter.Kimi].ManualBlockers(a.UserHome)
	if err != nil {
		return b, "", err
	}
	if h.ManualBusy() {
		return b, current.AccessToken, nil
	}
	if a.HTTP.Client == nil && secutil.InTest() {
		return b, "", errors.New("kimi: refresh HTTP client is required in tests")
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(b.Payload, &env); err != nil {
		return b, "", err
	}
	env["credentials_json"], err = json.Marshal(string(raw))
	if err != nil {
		return b, "", err
	}
	b.Payload, err = json.Marshal(env)
	if err != nil {
		return b, "", err
	}
	beforeExchange, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, beforeExchange) || !lock.OwnsPath() || lock.Context().Err() != nil {
		return b, "", errors.New("kimi: live credentials or auth lock changed before exchange")
	}
	refreshCtx, cancel := context.WithTimeout(lock.Context(), 15*time.Second)
	defer cancel()
	tok, err := a.HTTP.KimiRefresh(refreshCtx, current.RefreshToken)
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, tok.Permanent, err)
	}
	updated, err := kimi.ApplyRefresh(b, tok, a.now())
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, err)
	}
	// Retain the rotated refresh token even if an unrelated writer changed the
	// live slot. A consumed token must never replace this newer vault snapshot.
	if err := a.saveBlob(updated); err != nil {
		return updated, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) || !lock.OwnsPath() || lock.Context().Err() != nil {
		return updated, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, errors.New("kimi: live credentials or auth lock changed during refresh"))
	}
	var refreshed struct {
		Credentials string `json:"credentials_json"`
	}
	if err := json.Unmarshal(updated.Payload, &refreshed); err != nil {
		return updated, "", err
	}
	if err := livefile.AtomicWrite(path, []byte(refreshed.Credentials), 0600); err != nil {
		return updated, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, err)
	}
	a.rememberFP(adapter.Kimi)
	if err := a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, nil); err != nil {
		return updated, "", err
	}
	return updated, tok.AccessToken, nil
}
