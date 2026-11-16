package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/kimi"
	"qswitch/internal/quota"
)

func renewedKimi(t *testing.T, a *App, b adapter.Blob) adapter.Blob {
	t.Helper()
	updated, err := kimi.ApplyRefresh(b, quota.KimiTokens{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}, a.now())
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestKimiRecoveryAfterRestart(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{true: "live", false: "parked"}[live], func(t *testing.T) {
			a, b, now := parkedKimiAccount(t)
			path := ""
			if live {
				writeKimiLive(t, a, "old", now.Add(-time.Minute).Unix())
				path = a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0]
			}
			r, err := a.beginKimiRecovery(b, path)
			if err != nil {
				t.Fatal(err)
			}
			updated := renewedKimi(t, a, b)
			r.Updated = &updated
			if err := a.putKimiRecovery(r); err != nil {
				t.Fatal(err)
			}
			ciphertext, err := os.ReadFile(filepath.Join(a.DataDir, "vault", kimiRecoveryVault, r.key+".enc"))
			if err != nil || strings.Contains(string(ciphertext), "new-refresh") {
				t.Fatal("recovery not encrypted")
			}
			a = restartClaudeRecovery(t, a)
			if err := a.recoverKimiLocked(&b); err != nil {
				t.Fatal(err)
			}
			saved, err := a.loadBlob(adapter.Kimi, b.Identity.StableID)
			if err != nil {
				t.Fatal(err)
			}
			c, err := kimi.CredsFromBlob(saved)
			if err != nil || c.RefreshToken != "new-refresh" {
				t.Fatal("vault did not recover")
			}
			c, err = kimi.ReadLiveCreds(a.UserHome)
			if err != nil {
				t.Fatal(err)
			}
			if live && c.RefreshToken != "new-refresh" {
				t.Fatal("live did not recover")
			}
			if !live && c.AccessToken != "live" {
				t.Fatal("parked recovery changed live")
			}
			records, err := a.kimiRecoveries()
			if err != nil || len(records) != 0 {
				t.Fatal("journal not completed")
			}
		})
	}
}

func TestKimiUnknownExchangeNeverRetriesConsumedToken(t *testing.T) {
	a, b, _ := parkedKimiAccount(t)
	hits := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/coding/v1/me" {
			return kimiResponse(req, 200, `{"user_id":"different-live"}`), nil
		}
		hits++
		return nil, errors.New("response lost after exchange")
	})}
	if _, _, err := a.keepAliveKimiLocked(context.Background(), b, true, true); err == nil {
		t.Fatal("lost response succeeded")
	}
	a = restartClaudeRecovery(t, a)
	if _, _, err := a.keepAliveKimiLocked(context.Background(), b, true, true); err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatal("missing exchange fence", err)
	}
	if hits != 1 {
		t.Fatal("consumed refresh token retried")
	}
	if err := a.saveKimiBlob(b); err == nil {
		t.Fatal("consumed capture entered vault")
	}
	// A newly verified login for this account can replace the uncertain lineage.
	fresh := renewedKimi(t, a, b)
	if err := a.saveKimiBlob(fresh); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverKimiLocked(&fresh); err != nil {
		t.Fatal(err)
	}
}

func TestKimiRecoveryPreservesExternalLoginAndLogout(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(map[bool]string{true: "logout", false: "new-login"}[logout], func(t *testing.T) {
			a, b, now := parkedKimiAccount(t)
			path := a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0]
			writeKimiLive(t, a, "old", now.Add(-time.Minute).Unix())
			r, err := a.beginKimiRecovery(b, path)
			if err != nil {
				t.Fatal(err)
			}
			updated := renewedKimi(t, a, b)
			r.Updated = &updated
			if err := a.putKimiRecovery(r); err != nil {
				t.Fatal(err)
			}
			if logout {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeKimiLive(t, a, "external-login", now.Add(time.Hour).Unix())
			}
			if err := a.recoverKimiLocked(&b); err != nil {
				t.Fatal(err)
			}
			if logout {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("logout undone")
				}
			} else {
				c, err := kimi.ReadLiveCreds(a.UserHome)
				if err != nil || c.AccessToken != "external-login" {
					t.Fatal("external login overwritten")
				}
			}
		})
	}
}

func TestKimiRecoveryWaitsForNativeLock(t *testing.T) {
	a, b, now := parkedKimiAccount(t)
	writeKimiLive(t, a, "old", now.Add(-time.Minute).Unix())
	r, err := a.beginKimiRecovery(b, a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0])
	if err != nil {
		t.Fatal(err)
	}
	updated := renewedKimi(t, a, b)
	r.Updated = &updated
	if err := a.putKimiRecovery(r); err != nil {
		t.Fatal(err)
	}
	lock, err := kimi.AcquireAuthRefreshLock(context.Background(), a.UserHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.recoverKimiLocked(&b); err == nil {
		t.Fatal("peer native lock bypassed")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverKimiLocked(&b); err != nil {
		t.Fatal(err)
	}
}

func TestKimiSameAccountRotatedLiveTokenReplacesParked(t *testing.T) {
	a, b, now := parkedKimiAccount(t)
	writeKimiLive(t, a, "native-renewed", now.Add(time.Hour).Unix())
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/coding/v1/me" {
			t.Fatal("spent a stale refresh token")
		}
		return kimiResponse(req, 200, `{"user_id":"user-old"}`), nil
	})}
	_, token, err := a.keepAliveKimiLocked(context.Background(), b, true, true)
	if err != nil || token != "native-renewed" {
		t.Fatal("live rotation not adopted", err)
	}
	saved, err := a.loadBlob(adapter.Kimi, b.Identity.StableID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := kimi.CredsFromBlob(saved)
	if err != nil || c.AccessToken != token {
		t.Fatal("new live credentials not persisted")
	}
}
