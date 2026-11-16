package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/antigravity"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
)

func (a *App) setAntigravityCaptureError(err error) {
	a.fpMu.Lock()
	defer a.fpMu.Unlock()
	a.antigravityCaptureError = ""
	if err != nil {
		a.antigravityCaptureError = err.Error()
	}
}

func (a *App) antigravityCaptureStatus() string {
	a.fpMu.Lock()
	defer a.fpMu.Unlock()
	return a.antigravityCaptureError
}

// Google consumer refresh tokens are reusable. Parked accounts are renewed
// into the encrypted vault; native clients own renewal of their live slot.
func (a *App) keepAliveAntigravityLocked(ctx context.Context, blob adapter.Blob, force bool) (adapter.Blob, string, error) {
	tool := adapter.Antigravity
	raw, err := antigravity.CredentialsFromBlob(blob)
	if err != nil {
		return blob, "", err
	}
	c, err := antigravity.ParseCredential(raw)
	if err != nil {
		return blob, "", err
	}
	if !force && !c.Token.Expiry.IsZero() && c.Token.Expiry.After(a.now().Add(5*time.Minute)) {
		return blob, c.Token.AccessToken, nil
	}
	client := antigravity.CredentialsClient{Client: a.HTTP.Client}
	next, err := client.Refresh(ctx, raw)
	if err != nil {
		_ = a.recordRefreshResult(tool, blob.Identity.StableID, errors.Is(err, antigravity.ErrReauthenticationRequired), err)
		return blob, "", err
	}
	id, err := client.Verify(ctx, next)
	if err != nil || id.StableID != blob.Identity.StableID || !strings.EqualFold(id.Email, blob.Identity.Email) {
		if err == nil {
			err = errors.New("antigravity: renewed credential belongs to another account")
		}
		_ = a.recordRefreshResult(tool, blob.Identity.StableID, errors.Is(err, antigravity.ErrReauthenticationRequired), err)
		return blob, "", err
	}
	id.PlanHint = blob.Identity.PlanHint
	nb, err := antigravity.NewBlob(next, id)
	if err != nil {
		return blob, "", err
	}
	if err := a.saveBlob(nb); err != nil {
		return blob, "", err
	}
	_ = a.recordRefreshResult(tool, id.StableID, false, nil)
	c, err = antigravity.ParseCredential(next)
	if err != nil {
		return nb, "", err
	}
	return nb, c.Token.AccessToken, nil
}

func (a *App) keepAliveAntigravityAccounts(ctx context.Context) {
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", string(adapter.Antigravity)+".lock"))
	if err != nil {
		return
	}
	defer lock.Close()
	accounts, err := a.State.ListAccounts(string(adapter.Antigravity))
	if err != nil {
		return
	}
	for _, account := range accounts {
		if account.RefreshBackoffUntil > a.now().Unix() {
			continue
		}
		blob, err := a.loadBlob(adapter.Antigravity, account.StableID)
		if err != nil {
			continue
		}
		raw, err := antigravity.CredentialsFromBlob(blob)
		if err != nil {
			continue
		}
		c, err := antigravity.ParseCredential(raw)
		if err != nil || (!c.Token.Expiry.IsZero() && c.Token.Expiry.After(a.now().Add(5*time.Minute))) {
			continue
		}
		_, _, _ = a.keepAliveAntigravityLocked(ctx, blob, false)
		return // Bound renewal work per daemon tick.
	}
}

func (a *App) probeAntigravityLocked(ctx context.Context, blob adapter.Blob) quota.Result {
	nb, token, err := a.keepAliveAntigravityLocked(ctx, blob, false)
	if err != nil {
		return a.refreshFailureResult(adapter.Antigravity, blob.Identity.StableID)
	}
	r, _ := a.HTTP.Antigravity(ctx, token, "")
	if r.Class == quota.Expired {
		_, renewed, err := a.keepAliveAntigravityLocked(ctx, nb, true)
		if err != nil {
			return a.refreshFailureResult(adapter.Antigravity, blob.Identity.StableID)
		}
		r, _ = a.HTTP.Antigravity(ctx, renewed, "")
	}
	return r
}
