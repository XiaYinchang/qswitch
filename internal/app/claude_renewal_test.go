package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
)

// On novix the live account was marked expired six times in four days: the
// managed sessions sat idle past the expiry of the access token, so the
// scheduled quota request used a rejected token while the running CLI owned
// renewal. The account stayed backed off for two hours even after a session
// renewed the token, and limit hits in that time could not queue a switch.
func TestRenewedLiveClaudeTokenWithdrawsExpiredBackoff(t *testing.T) {
	a := setupClaudeRecovery(t, "", false)
	a.Cfg.Claude.Enabled = true
	start := a.now()
	// Claude Code is running, so qswitch leaves renewal of the live token to it.
	a.Adapters[adapter.Claude] = claude.Adapter{DataDir: a.DataDir, List: func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 4242, Command: "claude"}}, nil
	}}
	usage := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"live-account"}}`), nil
		}
		usage++
		if req.Header.Get("Authorization") != "Bearer new-access" {
			return claudeRecoveryResponse(req, http.StatusUnauthorized, `{}`), nil
		}
		return claudeRecoveryResponse(req, 200, `{"five_hour":{"utilization":40},"seven_day":{"utilization":10}}`), nil
	})}
	if err := a.Ingest(adapter.Claude); err != nil {
		t.Fatal(err)
	}
	a.ProbeHTTP(context.Background(), adapter.Claude, false)
	ac, err := a.State.GetAccount("claude", "live-account")
	if err != nil || quota.Class(ac.LastQuotaClass) != quota.Expired || ac.HTTPBackoffUntil < start.Add(time.Hour).Unix() || usage != 1 {
		t.Fatalf("setup: class=%s backoff until %d requests=%d err=%v", ac.LastQuotaClass, ac.HTTPBackoffUntil, usage, err)
	}
	// A session goes back to work and Claude Code renews the token.
	writeClaudeRecoveryLive(t, a, "new-access", "new-refresh", start.Add(8*time.Hour))
	if err := a.Ingest(adapter.Claude); err != nil {
		t.Fatal(err)
	}
	a.Clock = clock.Fixed{T: start.Add(16 * time.Minute)}
	a.ProbeHTTP(context.Background(), adapter.Claude, false)
	ac, err = a.State.GetAccount("claude", "live-account")
	if err != nil || usage != 2 || quota.Class(ac.LastQuotaClass) != quota.OK || ac.HTTPBackoffUntil != 0 || ac.RefreshBackoffUntil != 0 {
		t.Fatalf("renewed account was not re-checked: class=%s requests=%d backoff=%d/%d err=%v", ac.LastQuotaClass, usage, ac.HTTPBackoffUntil, ac.RefreshBackoffUntil, err)
	}
}

// A capture that still carries an expired token leaves the verdict alone.
func TestExpiredLiveClaudeTokenKeepsExpiredBackoff(t *testing.T) {
	a := setupClaudeRecovery(t, "", false)
	raw, err := json.Marshal(map[string]any{"claudeAiOauth": claude.Creds{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: a.now().Add(-time.Minute).UnixMilli()}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := claudeBlobFromRaw(raw, claude.OAuthAccount{AccountUUID: "live-account"}, a.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(b); err != nil {
		t.Fatal(err)
	}
	until := a.now().Add(2 * time.Hour).Unix()
	if err := a.State.UpdateQuota("claude", "live-account", string(quota.Expired), 0, 0, a.now().Unix(), a.now().Unix(), until); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetRefreshBackoff("claude", "live-account", until); err != nil {
		t.Fatal(err)
	}
	a.clearRenewedClaudeExpiry([]adapter.Blob{b}, "live-account")
	ac, err := a.State.GetAccount("claude", "live-account")
	if err != nil || quota.Class(ac.LastQuotaClass) != quota.Expired || ac.HTTPBackoffUntil != until || ac.RefreshBackoffUntil != until {
		t.Fatalf("an expired token withdrew the verdict: class=%s backoff=%d/%d err=%v", ac.LastQuotaClass, ac.HTTPBackoffUntil, ac.RefreshBackoffUntil, err)
	}
}
