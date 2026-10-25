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
	"qswitch/internal/adapter/claude"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
)

// Claude OAuth tokens carry no durable account identifier. Enroll only after
// /api/oauth/profile confirms the identity, and reject credentials rotated
// during that request.
func (a *App) enrichClaudeCapture(blobs []adapter.Blob, warnings []adapter.Warning) ([]adapter.Blob, []adapter.Warning, error) {
	if a.HTTP.Client == nil && secutil.InTest() {
		return nil, warnings, errors.New("claude: profile HTTP client is required in tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	for i, b := range blobs {
		c, err := claude.CredsFromBlob(b)
		if err != nil {
			return nil, warnings, err
		}
		raw, status, err := a.HTTP.ClaudeProfile(ctx, c.AccessToken)
		if err != nil || status != 200 {
			return nil, warnings, errors.New("claude: unable to verify account identity; sign in with Claude Code and retry")
		}
		uuid, _, _, _ := quota.ClaudeIdentity(raw)
		if uuid == "" {
			return nil, warnings, errors.New("claude: profile missing account uuid")
		}
		ident := claude.EnrichIdentity(b.Identity, raw)
		live, err := claude.ReadLiveCreds(a.UserHome)
		if err != nil || !sameClaudeTokens(c, live) {
			return nil, warnings, errors.New("claude: credentials changed while checking account; retry capture")
		}
		blobs[i], err = claude.WriteIdentity(b, ident)
		if err != nil {
			return nil, warnings, err
		}
	}
	return blobs, warnings, nil
}

func sameClaudeTokens(a, b claude.Creds) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken
}

func (a *App) keepAliveClaudeAccounts(ctx context.Context) {
	accounts, err := a.State.ListAccounts(string(adapter.Claude))
	if err != nil {
		return
	}
	for _, account := range accounts {
		if account.RefreshBackoffUntil > a.now().Unix() {
			continue
		}
		lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "claude.lock"))
		if err != nil {
			return
		}
		// Reload inside the tool lock; each refresh token may be used only once.
		b, err := a.loadBlob(adapter.Claude, account.StableID)
		if err == nil {
			c, readErr := claude.CredsFromBlob(b)
			current, _ := a.State.GetAccount("claude", account.StableID)
			if readErr == nil && c.RefreshToken != "" && (c.NeedsRefresh(a.now()) || current.LastQuotaClass == string(quota.Expired)) {
				_, _, _ = a.keepAliveClaudeLocked(ctx, b, current.LastQuotaClass == string(quota.Expired), false)
				lock.Close()
				return
			}
		}
		lock.Close()
	}
}

// Caller holds the Claude tool lock. A running CLI owns live renewal; with no
// CLI running, qswitch renews a verified live account itself. Claude Code has
// no official refresh lockfile, so every write is gated on a manual-blocker
// check and a re-read of the live file.
func (a *App) keepAliveClaudeLocked(ctx context.Context, b adapter.Blob, force, bypassBackoff bool) (adapter.Blob, string, error) {
	c, err := claude.CredsFromBlob(b)
	if err != nil {
		return b, "", err
	}
	live, liveErr := claude.ReadLiveCreds(a.UserHome)
	if liveErr == nil && sameClaudeTokens(c, live) {
		return a.keepAliveLiveClaudeLocked(ctx, b, live, force, bypassBackoff)
	}
	// Reading quota with a fresh stored token cannot rotate live credentials.
	if !force && !c.NeedsRefresh(a.now()) {
		return b, c.AccessToken, nil
	}
	if liveErr == nil {
		// The parked identity differs from live; the live CLI owns its own
		// renewal, so only the parked snapshot is refreshed here.
	} else if !os.IsNotExist(liveErr) {
		return b, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, errors.New("claude: cannot read live credentials"))
	}
	if c.RefreshToken == "" {
		return b, c.AccessToken, nil
	}
	if !bypassBackoff {
		ac, err := a.State.GetAccount("claude", b.Identity.StableID)
		if err != nil {
			return b, "", err
		}
		if ac.RefreshBackoffUntil > a.now().Unix() {
			return b, "", errors.New("claude: refresh is in backoff")
		}
	}
	if a.HTTP.Client == nil && secutil.InTest() {
		return b, "", errors.New("claude: refresh HTTP client is required in tests")
	}
	// A login or rotation during the checks may have moved this parked account
	// into the live slot. Re-read immediately before spending its refresh token.
	currentLive, currentErr := claude.ReadLiveCreds(a.UserHome)
	if (liveErr == nil && (currentErr != nil || !sameClaudeTokens(live, currentLive))) ||
		(os.IsNotExist(liveErr) && !os.IsNotExist(currentErr)) {
		return b, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, errors.New("claude: live credentials changed before refresh"))
	}
	tok, err := a.HTTP.ClaudeRefresh(ctx, c.RefreshToken)
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, tok.Permanent, err)
	}
	updated, err := claude.ApplyRefresh(b, tok, a.now())
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, err)
	}
	if err := a.saveBlob(updated); err != nil {
		return updated, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, err)
	}
	if err := a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, nil); err != nil {
		return updated, "", err
	}
	return updated, tok.AccessToken, nil
}

// Caller holds the Claude tool lock and has matched the blob's credentials to
// the live file. Refreshes the live account and writes the rotated tokens to
// both the live file and the vault.
func (a *App) keepAliveLiveClaudeLocked(ctx context.Context, b adapter.Blob, live claude.Creds, force, bypassBackoff bool) (adapter.Blob, string, error) {
	if !force && !live.NeedsRefresh(a.now()) {
		return b, live.AccessToken, nil
	}
	h, err := a.Adapters[adapter.Claude].ManualBlockers(a.UserHome)
	if err != nil {
		return b, "", err
	}
	if h.ManualBusy() {
		// A running CLI refreshes itself.
		return b, live.AccessToken, nil
	}
	if !bypassBackoff {
		ac, err := a.State.GetAccount("claude", b.Identity.StableID)
		if err != nil {
			return b, "", err
		}
		if ac.RefreshBackoffUntil > a.now().Unix() {
			return b, "", errors.New("claude: refresh is in backoff")
		}
	}
	path := a.Adapters[adapter.Claude].LivePaths(a.UserHome)[0]
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, "", err
	}
	current, err := claude.ParseCreds(raw)
	if err != nil {
		return b, "", err
	}
	if !sameClaudeTokens(live, current) {
		return b, "", errors.New("claude: live credentials changed before refresh")
	}
	if !force && !current.NeedsRefresh(a.now()) {
		return b, current.AccessToken, nil
	}
	h, err = a.Adapters[adapter.Claude].ManualBlockers(a.UserHome)
	if err != nil {
		return b, "", err
	}
	if h.ManualBusy() {
		return b, current.AccessToken, nil
	}
	if a.HTTP.Client == nil && secutil.InTest() {
		return b, "", errors.New("claude: refresh HTTP client is required in tests")
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
	if err != nil || !bytes.Equal(raw, beforeExchange) {
		return b, "", errors.New("claude: live credentials changed before exchange")
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := a.HTTP.ClaudeRefresh(refreshCtx, current.RefreshToken)
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, tok.Permanent, err)
	}
	updated, err := claude.ApplyRefresh(b, tok, a.now())
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, err)
	}
	// Retain the rotated refresh token even if an unrelated writer changed the
	// live slot. A consumed token must never replace this newer vault snapshot.
	if err := a.saveBlob(updated); err != nil {
		return updated, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		return updated, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, errors.New("claude: live credentials changed during refresh"))
	}
	var refreshed struct {
		Credentials string `json:"credentials_json"`
	}
	if err := json.Unmarshal(updated.Payload, &refreshed); err != nil {
		return updated, "", err
	}
	if err := livefile.AtomicWrite(path, []byte(refreshed.Credentials), 0o600); err != nil {
		return updated, "", a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, err)
	}
	a.rememberFP(adapter.Claude)
	if err := a.recordRefreshResult(adapter.Claude, b.Identity.StableID, false, nil); err != nil {
		return updated, "", err
	}
	return updated, tok.AccessToken, nil
}
