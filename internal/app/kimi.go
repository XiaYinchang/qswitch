package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/kimi"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
)

// Kimi tokens contain no durable account identifier. Enroll only after /me
// confirms the identity, and reject credentials rotated during that request.
func (a *App) enrichKimiCapture(blobs []adapter.Blob, warnings []adapter.Warning) ([]adapter.Blob, []adapter.Warning, error) {
	if a.HTTP.Client == nil && secutil.InTest() {
		return nil, warnings, errors.New("kimi: profile HTTP client is required in tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	for i, b := range blobs {
		c, err := kimi.CredsFromBlob(b)
		if err != nil {
			return nil, warnings, err
		}
		raw, status, err := a.HTTP.KimiProfile(ctx, c.AccessToken)
		if err != nil || status != 200 {
			return nil, warnings, errors.New("kimi: unable to verify account identity; sign in with Kimi Code and retry")
		}
		uid, _, _ := quota.KimiIdentity(raw)
		if uid == "" {
			return nil, warnings, errors.New("kimi: profile missing user_id")
		}
		ident := kimi.EnrichIdentity(b.Identity, raw)
		live, err := kimi.ReadLiveCreds(a.UserHome)
		if err != nil || !sameKimiTokens(c, live) {
			return nil, warnings, errors.New("kimi: credentials changed while checking account; retry capture")
		}
		blobs[i], err = kimi.WriteIdentity(b, ident)
		if err != nil {
			return nil, warnings, err
		}
	}
	return blobs, warnings, nil
}

func sameKimiTokens(a, b kimi.Creds) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken
}

func (a *App) keepAliveKimiAccounts(ctx context.Context) {
	accounts, err := a.State.ListAccounts(string(adapter.Kimi))
	if err != nil {
		return
	}
	p, _ := a.State.GetPointer(string(adapter.Kimi))
	for _, account := range accounts {
		if account.StableID == p.StableID || account.HTTPBackoffUntil > a.now().Unix() {
			continue
		}
		lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "kimi.lock"))
		if err != nil {
			return
		}
		// Reload inside the tool lock; each refresh token may be used only once.
		b, err := a.loadBlob(adapter.Kimi, account.StableID)
		if err == nil {
			c, readErr := kimi.CredsFromBlob(b)
			current, _ := a.State.GetAccount("kimi", account.StableID)
			if readErr == nil && c.RefreshToken != "" && (c.NeedsRefresh(a.now()) || current.LastQuotaClass == string(quota.Expired)) {
				_, _, _ = a.keepAliveKimiLocked(ctx, b, current.LastQuotaClass == string(quota.Expired), false)
				lock.Close()
				return
			}
		}
		lock.Close()
	}
}

// Caller holds the Kimi tool lock. Live refresh belongs to the official CLI.
// A parked account is refreshed only after establishing it differs from live.
func (a *App) keepAliveKimiLocked(ctx context.Context, b adapter.Blob, force, bypassBackoff bool) (adapter.Blob, string, error) {
	c, err := kimi.CredsFromBlob(b)
	if err != nil {
		return b, "", err
	}
	live, liveErr := kimi.ReadLiveCreds(a.UserHome)
	if liveErr == nil && sameKimiTokens(c, live) {
		return b, live.AccessToken, nil
	}
	if liveErr == nil {
		if a.HTTP.Client == nil && secutil.InTest() {
			return b, "", errors.New("kimi: HTTP client is required in tests")
		}
		raw, status, err := a.HTTP.KimiProfile(ctx, live.AccessToken)
		if err != nil || status != 200 {
			return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, errors.New("kimi: cannot establish live account identity"))
		}
		uid, _, _ := quota.KimiIdentity(raw)
		liveID := kimi.EnrichIdentity(adapter.Identity{Tool: adapter.Kimi}, raw).StableID
		if uid == "" || liveID == "" {
			return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, errors.New("kimi: live profile missing identity"))
		}
		if liveID == b.Identity.StableID {
			return b, live.AccessToken, nil
		}
	} else if !os.IsNotExist(liveErr) {
		return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, errors.New("kimi: cannot read live credentials"))
	}
	if !force && !c.NeedsRefresh(a.now()) {
		return b, c.AccessToken, nil
	}
	if c.RefreshToken == "" {
		return b, c.AccessToken, nil
	}
	if !bypassBackoff {
		ac, err := a.State.GetAccount("kimi", b.Identity.StableID)
		if err != nil {
			return b, "", err
		}
		if ac.HTTPBackoffUntil > a.now().Unix() {
			return b, "", errors.New("kimi: refresh is in backoff")
		}
	}
	if a.HTTP.Client == nil && secutil.InTest() {
		return b, "", errors.New("kimi: refresh HTTP client is required in tests")
	}
	// A login or rotation during /me may have moved this parked account into
	// the live slot. Re-read immediately before spending its refresh token.
	currentLive, currentErr := kimi.ReadLiveCreds(a.UserHome)
	if (liveErr == nil && (currentErr != nil || !sameKimiTokens(live, currentLive))) ||
		(os.IsNotExist(liveErr) && !os.IsNotExist(currentErr)) {
		return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, errors.New("kimi: live credentials changed before refresh"))
	}
	tok, err := a.HTTP.KimiRefresh(ctx, c.RefreshToken)
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, tok.Permanent, err)
	}
	updated, err := kimi.ApplyRefresh(b, tok, a.now())
	if err != nil {
		return b, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, err)
	}
	if err := a.saveBlob(updated); err != nil {
		return updated, "", a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, err)
	}
	if err := a.recordRefreshResult(adapter.Kimi, b.Identity.StableID, false, nil); err != nil {
		return updated, "", err
	}
	return updated, tok.AccessToken, nil
}
