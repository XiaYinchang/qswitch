package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/antigravity"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
)

func TestAntigravityPipelineRevokedAuthenticationRequiresLogin(t *testing.T) {
	a, now := antigravityPipelineFixture(t)
	blob, err := a.loadBlob(adapter.Antigravity, antigravityPipelineID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(antigravity.Credential{
		AuthMethod: "consumer", IDTokenEmail: blob.Identity.Email,
		Token: &antigravity.OAuthToken{AccessToken: "synthetic-expired-token", Expiry: now.Add(-time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := antigravity.NewBlob(raw, blob.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(expired); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing refresh authorization reached an HTTP endpoint")
		return nil, errors.New("unexpected HTTP call")
	})}
	result := a.probeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID, false)
	account, err := a.State.GetAccount("antigravity", antigravityPipelineID)
	if err != nil || result.Class != quota.Expired || account.LastQuotaClass != "expired" || account.RefreshBackoffUntil <= now.Add(time.Minute).Unix() {
		t.Fatalf("revoked authorization did not require login: result=%s class=%s backoff=%d err=%v", result.Class, account.LastQuotaClass, account.RefreshBackoffUntil, err)
	}
	if err := a.saveBlob(blob); err != nil {
		t.Fatal(err)
	}
	account, err = a.State.GetAccount("antigravity", antigravityPipelineID)
	if err != nil || account.LastQuotaClass != "unknown" || account.RefreshBackoffUntil != 0 {
		t.Fatalf("new login did not recover: class=%s backoff=%d err=%v", account.LastQuotaClass, account.RefreshBackoffUntil, err)
	}
}

const antigravityPipelineID = "synthetic-pipeline-account"
const antigravityPipelineAccess = "synthetic-pipeline-access-token"

// These tests enter through the account probe, including encrypted-vault reads,
// native credential decoding, HTTP request construction and SQLite persistence.
// The fresh synthetic token bypasses OAuth; no native login or restore is used.
func antigravityPipelineFixture(t *testing.T) (*App, time.Time) {
	t.Helper()
	a, _ := setup(t)
	antigravityNativeEndpointFixture(t, a, "https://daily-cloudcode-pa.googleapis.com")
	now := time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	id := adapter.Identity{Tool: adapter.Antigravity, StableID: antigravityPipelineID, Email: "pipeline@example.test"}
	raw, err := json.Marshal(antigravity.Credential{
		AuthMethod: "consumer", IDTokenEmail: id.Email,
		Token: &antigravity.OAuthToken{AccessToken: antigravityPipelineAccess, RefreshToken: "synthetic-pipeline-refresh-token", TokenType: "Bearer", Expiry: now.Add(24 * time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := antigravity.NewBlob(raw, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(blob); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(antigravityPipelineVaultPath(a))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(stored, []byte("qsw1")) || bytes.Contains(stored, []byte(antigravityPipelineAccess)) {
		t.Fatal("fixture must store an encrypted native credential blob")
	}
	return a, now
}

func antigravityPipelineVaultPath(a *App) string {
	return filepath.Join(a.Vault.Dir, "vault", string(adapter.Antigravity), antigravityPipelineID+".enc")
}

func antigravityPipelineBody(t *testing.T, now time.Time, remaining [4]float64, omitWindow, omitFraction int) []byte {
	t.Helper()
	ids := []string{"gemini-5h", "gemini-weekly", "3p-5h", "3p-weekly"}
	windows := []string{"5h", "weekly", "5h", "weekly"}
	var buckets []map[string]any
	for i, id := range ids {
		if i == omitWindow {
			continue
		}
		bucket := map[string]any{"bucketId": id, "window": windows[i], "resetTime": now.Add(time.Duration(i+1) * time.Hour).Format(time.RFC3339)}
		if i != omitFraction {
			bucket["remainingFraction"] = remaining[i]
		}
		buckets = append(buckets, bucket)
	}
	body, err := json.Marshal(map[string]any{"groups": []any{map[string]any{"buckets": buckets}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func antigravityPipelineHTTP(t *testing.T, a *App, response func() []byte) *int {
	t.Helper()
	calls := new(int)
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		*calls++
		if req.Method != http.MethodPost || req.URL.String() != quota.AntigravityQuotaURL {
			t.Fatalf("unexpected native quota request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer "+antigravityPipelineAccess {
			t.Fatal("quota request did not use the access token decrypted from the vault")
		}
		if req.Header.Get("User-Agent") != "antigravity/2.32.1" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json" {
			t.Fatal("quota request omitted native user agent or JSON headers")
		}
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != "{}" {
			t.Fatalf("unexpected consumer quota request body (read error: %v)", err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(response())), Request: req}, nil
	})}
	return calls
}

func TestAntigravityPipelinePersistsBothModelGroups(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining [4]float64
		wantClass quota.Class
	}{
		{"Gemini only exhausted", [4]float64{0, 0.5, 0.5, 0.5}, quota.OK},
		{"third party only exhausted", [4]float64{0.5, 0.5, 0.5, 0}, quota.OK},
		{"both short windows exhausted", [4]float64{0, 0.5, 0, 0.5}, quota.Exhausted},
		{"both groups constrained by different windows", [4]float64{0, 0.5, 0.5, 0}, quota.Exhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, now := antigravityPipelineFixture(t)
			body := antigravityPipelineBody(t, now, tc.remaining, -1, -1)
			calls := antigravityPipelineHTTP(t, a, func() []byte { return body })
			result := a.probeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID, false)
			if result.Class != tc.wantClass || *calls != 1 {
				t.Fatalf("class=%s HTTP calls=%d; want class=%s and one quota call", result.Class, *calls, tc.wantClass)
			}
			account, err := a.State.GetAccount(string(adapter.Antigravity), antigravityPipelineID)
			if err != nil {
				t.Fatal(err)
			}
			if account.LastQuotaClass != string(tc.wantClass) || account.LastHTTPAt != now.Unix() || account.LastProbedAt != now.Unix() || account.LastUsedPct != result.UsedPct || account.LastResetsAt != result.ResetsAt {
				t.Fatal("quota result was not persisted as the current account observation")
			}
			antigravityPipelineAssertBuckets(t, quota.DecodeBuckets(account.QuotaDetail), tc.remaining, now)
			known, err := a.knownExhausted(adapter.Antigravity, account)
			if err != nil || known != (tc.wantClass == quota.Exhausted) {
				t.Fatalf("automatic exhaustion evidence=%v, error=%v; class=%s", known, err, tc.wantClass)
			}
			latest, err := a.State.LatestQuota(string(adapter.Antigravity), antigravityPipelineID)
			if err != nil || latest.Class != string(tc.wantClass) || latest.Source != "http" {
				t.Fatalf("quota history did not record the HTTP observation: %+v, %v", latest, err)
			}
		})
	}
}

func TestAntigravityPipelineUnknownRevokesOldExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		omitWindow, omitFraction int
	}{
		{"missing window", 3, -1},
		{"missing remaining field", -1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, now := antigravityPipelineFixture(t)
			body := antigravityPipelineBody(t, now, [4]float64{}, -1, -1)
			calls := antigravityPipelineHTTP(t, a, func() []byte { return body })
			if first := a.probeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID, false); first.Class != quota.Exhausted {
				t.Fatalf("initial snapshot should exhaust both groups: %s", first.Class)
			}
			a.Clock = clock.Fixed{T: now.Add(time.Second)}
			body = antigravityPipelineBody(t, now, [4]float64{}, tc.omitWindow, tc.omitFraction)
			result := a.probeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID, false)
			if result.Class != quota.Unknown || !result.Authenticated || *calls != 2 {
				t.Fatalf("incomplete observation class=%s authenticated=%v calls=%d", result.Class, result.Authenticated, *calls)
			}
			account, err := a.State.GetAccount(string(adapter.Antigravity), antigravityPipelineID)
			if err != nil {
				t.Fatal(err)
			}
			if account.LastQuotaClass != string(quota.Unknown) || account.LastUsedPct != 0 || account.LastResetsAt != 0 || account.HTTPBackoffUntil <= a.now().Unix() {
				t.Fatal("incomplete response retained old exhaustion or failed to schedule retry")
			}
			if account.QuotaDetail != quota.EncodeBuckets(result.Buckets) {
				t.Fatal("incomplete snapshot was merged with stale quota windows")
			}
			if known, err := a.knownExhausted(adapter.Antigravity, account); err != nil || known {
				t.Fatalf("old snapshot still authorizes automatic switching: known=%v err=%v", known, err)
			}
		})
	}
}

func TestAntigravityPipelineReplacesCompleteSnapshot(t *testing.T) {
	a, now := antigravityPipelineFixture(t)
	body := antigravityPipelineBody(t, now, [4]float64{}, -1, -1)
	calls := antigravityPipelineHTTP(t, a, func() []byte { return body })
	first := a.probeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID, false)
	if first.Class != quota.Exhausted {
		t.Fatal("initial HTTP snapshot must be exhausted")
	}
	// Simulate a window retained by an older installation. A complete current
	// server response must replace the snapshot, including obsolete IDs.
	old := append(first.Buckets, quota.Bucket{ID: "retired_window", UsedPct: 100})
	if err := a.State.SetQuotaDetail(string(adapter.Antigravity), antigravityPipelineID, quota.EncodeBuckets(old)); err != nil {
		t.Fatal(err)
	}
	nextTime := now.Add(time.Minute)
	a.Clock = clock.Fixed{T: nextTime}
	remaining := [4]float64{0.2, 0.4, 0.6, 0.8}
	body = antigravityPipelineBody(t, nextTime, remaining, -1, -1)
	result := a.probeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID, false)
	if result.Class != quota.OK || *calls != 2 {
		t.Fatalf("fresh usable quota class=%s calls=%d", result.Class, *calls)
	}
	account, err := a.State.GetAccount(string(adapter.Antigravity), antigravityPipelineID)
	if err != nil {
		t.Fatal(err)
	}
	antigravityPipelineAssertBuckets(t, quota.DecodeBuckets(account.QuotaDetail), remaining, nextTime)
	if !reflect.DeepEqual(quota.DecodeBuckets(account.QuotaDetail), result.Buckets) || account.CoolingUntil != 0 || account.LastQuotaClass != string(quota.OK) {
		t.Fatal("complete usable response did not replace the old exhaustion snapshot")
	}
}

func antigravityPipelineAssertBuckets(t *testing.T, got []quota.Bucket, remaining [4]float64, now time.Time) {
	t.Helper()
	ids := []string{"gemini_5h", "gemini_weekly", "third_party_5h", "third_party_weekly"}
	if len(got) != len(ids) {
		t.Fatalf("persisted %d windows, want all four current windows", len(got))
	}
	for i, id := range ids {
		if got[i].ID != id || math.Abs(got[i].UsedPct-(1-remaining[i])*100) > 1e-10 || got[i].ResetsAt != now.Add(time.Duration(i+1)*time.Hour).Unix() {
			t.Fatalf("persisted quota window %d has wrong ID, remaining quota or reset: %+v", i, got[i])
		}
	}
}

func TestAntigravityPipelineRequiresDecryptableVault(t *testing.T) {
	a, _ := antigravityPipelineFixture(t)
	calls := antigravityPipelineHTTP(t, a, func() []byte { return []byte(`{}`) })
	path := antigravityPipelineVaultPath(a)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	result := a.probeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID, false)
	if result.Class != quota.Unknown || *calls != 0 {
		t.Fatalf("corrupt encrypted credentials must stop before HTTP: class=%s calls=%d", result.Class, *calls)
	}
}
