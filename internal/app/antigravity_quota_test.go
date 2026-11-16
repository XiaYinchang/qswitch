package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

// This fixture replaces only the native credential side effect. Quota parsing,
// persistent observations, selection, pending validation and commit use their
// normal implementations. Every account and quota response is synthetic.
type antigravityRotationAdapter struct {
	live     string
	blocked  bool
	restores []string
}

func (*antigravityRotationAdapter) Name() adapter.Tool        { return adapter.Antigravity }
func (*antigravityRotationAdapter) LivePaths(string) []string { return nil }
func (f *antigravityRotationAdapter) Fingerprint(string) ([32]byte, error) {
	return sha256.Sum256([]byte(f.live)), nil
}
func (f *antigravityRotationAdapter) Capture(string) ([]adapter.Blob, []adapter.Warning, error) {
	return []adapter.Blob{antigravityRotationBlob(f.live)}, nil, nil
}
func (f *antigravityRotationAdapter) Restore(_ string, blob adapter.Blob, _ adapter.RestoreOpts) error {
	id, err := f.IdentityOf(blob)
	if err != nil {
		return err
	}
	f.live = id.StableID
	f.restores = append(f.restores, id.StableID)
	return nil
}
func (*antigravityRotationAdapter) IdentityOf(blob adapter.Blob) (adapter.Identity, error) {
	var id adapter.Identity
	err := json.Unmarshal(blob.Payload, &id)
	return id, err
}
func (f *antigravityRotationAdapter) ManualBlockers(string) (adapter.Holders, error) {
	if f.blocked {
		return adapter.Holders{Manual: []adapter.Proc{{PID: 4242, Command: "synthetic-antigravity-agent"}}}, nil
	}
	return adapter.Holders{}, nil
}
func (f *antigravityRotationAdapter) AutoBlockers(home string) (adapter.Holders, error) {
	return f.ManualBlockers(home)
}
func (f *antigravityRotationAdapter) Idle(string, time.Duration) bool { return !f.blocked }
func (*antigravityRotationAdapter) KillCLI(string) error              { return nil }

func antigravityRotationBlob(id string) adapter.Blob {
	ident := adapter.Identity{Tool: adapter.Antigravity, StableID: id, Email: id + "@example.test"}
	raw, _ := json.Marshal(ident)
	return adapter.Blob{Tool: adapter.Antigravity, Identity: ident, Payload: raw}
}

func antigravityRotationFixture(t *testing.T) (*App, *antigravityRotationAdapter, time.Time) {
	t.Helper()
	a, _ := setup(t)
	now := time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	a.Cfg.General.AutoSwitch = true
	a.Cfg.Antigravity.Enabled = true
	fake := &antigravityRotationAdapter{live: "source"}
	a.Adapters[adapter.Antigravity] = fake
	for _, id := range []string{"source", "target"} {
		if err := a.saveBlob(antigravityRotationBlob(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.State.SetPointer(state.Pointer{Tool: string(adapter.Antigravity), StableID: "source"}); err != nil {
		t.Fatal(err)
	}
	antigravityRecordObservation(t, a, "target", antigravityHTTPObservation(t, now, [4]float64{0.5, 0.5, 0.5, 0.5}, [4]time.Duration{2 * time.Hour, 6 * time.Hour, 2 * time.Hour, 6 * time.Hour}, -1, http.StatusOK))
	return a, fake, now
}

// missing is a bucket index, or -1 for the complete server contract. This
// fixture uses the actual HTTP parser rather than manufacturing quota classes.
func antigravityHTTPObservation(t *testing.T, now time.Time, remaining [4]float64, resets [4]time.Duration, missing, status int) quota.Result {
	t.Helper()
	var buckets []map[string]any
	ids := []string{"gemini-5h", "gemini-weekly", "3p-5h", "3p-weekly"}
	windows := []string{"5h", "weekly", "5h", "weekly"}
	for i, id := range ids {
		if i == missing {
			continue
		}
		buckets = append(buckets, map[string]any{"bucketId": id, "window": windows[i], "remainingFraction": remaining[i], "resetTime": now.Add(resets[i]).Format(time.RFC3339)})
	}
	body, err := json.Marshal(map[string]any{"groups": []any{map[string]any{"buckets": buckets}}})
	if err != nil {
		t.Fatal(err)
	}
	h := quota.HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != quota.AntigravityQuotaURL {
			t.Fatalf("unexpected quota endpoint %s", req.URL)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
	})}}
	result, err := h.Antigravity(context.Background(), "synthetic-access-token", "synthetic-project")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func antigravityRecordObservation(t *testing.T, a *App, id string, result quota.Result) {
	t.Helper()
	at := a.now()
	if err := a.State.UpdateQuota(string(adapter.Antigravity), id, string(result.Class), result.UsedPct, result.ResetsAt, at.Unix(), at.Unix(), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail(string(adapter.Antigravity), id, quota.EncodeBuckets(result.Buckets)); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota(string(adapter.Antigravity), id, string(result.Class), result.Source, result.UsedPct, at); err != nil {
		t.Fatal(err)
	}
}

func TestAntigravityAutomaticRotationRequiresBothModelGroups(t *testing.T) {
	for _, test := range []struct {
		name       string
		remaining  [4]float64
		missing    int
		status     int
		wantSwitch bool
	}{
		{"gemini exhausted only", [4]float64{0, 0.5, 0.2, 0.5}, -1, 200, false},
		{"third party exhausted only", [4]float64{0.2, 0.5, 0, 0.5}, -1, 200, false},
		{"both short windows exhausted", [4]float64{0, 0.5, 0, 0.5}, -1, 200, true},
		{"both weekly windows exhausted", [4]float64{0.5, 0, 0.5, 0}, -1, 200, true},
		{"99.5 percent is not exhausted", [4]float64{0, 0, 0.005, 0.5}, -1, 200, false},
		{"smallest positive fraction remains usable", [4]float64{0, 0, math.SmallestNonzeroFloat64, 0.5}, -1, 200, false},
		{"missing third party window", [4]float64{}, 3, 200, false},
		{"missing Gemini window", [4]float64{}, 1, 200, false},
		{"rate limited response", [4]float64{}, -1, 429, false},
		{"unavailable response", [4]float64{}, -1, 503, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, fake, now := antigravityRotationFixture(t)
			result := antigravityHTTPObservation(t, now, test.remaining, [4]time.Duration{time.Hour, 7 * 24 * time.Hour, 3 * time.Hour, 7 * 24 * time.Hour}, test.missing, test.status)
			antigravityRecordObservation(t, a, "source", result)
			a.ProbeHTTP(context.Background(), adapter.Antigravity, false)
			pointer, err := a.State.GetPointer(string(adapter.Antigravity))
			if err != nil {
				t.Fatal(err)
			}
			wantID, wantRestores := "source", 0
			if test.wantSwitch {
				wantID, wantRestores = "target", 1
			}
			if pointer.StableID != wantID || fake.live != wantID || len(fake.restores) != wantRestores {
				t.Fatalf("class=%s pointer=%s live=%s writes=%d; want account=%s writes=%d", result.Class, pointer.StableID, fake.live, len(fake.restores), wantID, wantRestores)
			}
		})
	}
}

func TestAntigravityExhaustionCacheExpiresWhenEitherGroupCanRecover(t *testing.T) {
	a, fake, now := antigravityRotationFixture(t)
	fake.blocked = true
	result := antigravityHTTPObservation(t, now, [4]float64{}, [4]time.Duration{time.Hour, 9 * time.Hour, 3 * time.Hour, 4 * time.Hour}, -1, 200)
	antigravityRecordObservation(t, a, "source", result)
	a.ProbeHTTP(context.Background(), adapter.Antigravity, false)
	pending, err := a.State.GetPending(string(adapter.Antigravity))
	if err != nil || pending.ToID != "target" {
		t.Fatalf("switch was not queued: %+v %v", pending, err)
	}
	account, err := a.State.GetAccount(string(adapter.Antigravity), "source")
	if err != nil {
		t.Fatal(err)
	}
	if account.LastResetsAt != now.Add(4*time.Hour).Unix() {
		t.Fatalf("recovery deadline=%d; want the third-party group's last blocking window", account.LastResetsAt)
	}
	for _, test := range []struct {
		elapsed time.Duration
		want    bool
	}{
		{time.Hour + time.Second, true},
		{3*time.Hour + time.Second, true},
		{4 * time.Hour, false},
	} {
		a.Clock = clock.Fixed{T: now.Add(test.elapsed)}
		known, err := a.knownExhausted(adapter.Antigravity, account)
		if err != nil || known != test.want {
			t.Fatalf("elapsed=%s known=%v err=%v; want %v", test.elapsed, known, err, test.want)
		}
	}
	// Passing the recovery boundary revokes the old observation, even while
	// another group's weekly window is still exhausted for five more hours.
	fake.blocked = false
	if err := a.TryApply(adapter.Antigravity, false); err != nil {
		t.Fatal(err)
	}
	pending, err = a.State.GetPending(string(adapter.Antigravity))
	if err != nil || pending.ToID != "" || len(fake.restores) != 0 {
		t.Fatalf("stale exhaustion wrote credentials: pending=%+v writes=%v err=%v", pending, fake.restores, err)
	}
}

func TestAntigravityQueuedRotationCancelsAfterOneGroupRecovers(t *testing.T) {
	a, fake, now := antigravityRotationFixture(t)
	fake.blocked = true
	resets := [4]time.Duration{time.Hour, 7 * 24 * time.Hour, 3 * time.Hour, 7 * 24 * time.Hour}
	antigravityRecordObservation(t, a, "source", antigravityHTTPObservation(t, now, [4]float64{}, resets, -1, 200))
	a.ProbeHTTP(context.Background(), adapter.Antigravity, false)
	pending, _ := a.State.GetPending(string(adapter.Antigravity))
	if pending.ToID != "target" {
		t.Fatal("expected exhausted switch to wait for busy client")
	}
	a.Clock = clock.Fixed{T: now.Add(time.Second)}
	antigravityRecordObservation(t, a, "source", antigravityHTTPObservation(t, now, [4]float64{0, 0, 0.4, 0.5}, resets, -1, 200))
	fake.blocked = false
	if err := a.TryApply(adapter.Antigravity, false); err != nil {
		t.Fatal(err)
	}
	pending, err := a.State.GetPending(string(adapter.Antigravity))
	if err != nil || pending.ToID != "" || fake.live != "source" || len(fake.restores) != 0 {
		t.Fatalf("recovered group did not cancel switch: pending=%+v writes=%v err=%v", pending, fake.restores, err)
	}
}

func TestAntigravityDoesNotUseClaudeEarlyExpiryRotation(t *testing.T) {
	a, fake, now := antigravityRotationFixture(t)
	antigravityRecordObservation(t, a, "source", antigravityHTTPObservation(t, now, [4]float64{0, 0.5, 0.5, 0.5}, [4]time.Duration{time.Hour, 7 * 24 * time.Hour, 3 * time.Hour, 7 * 24 * time.Hour}, -1, 200))
	// Target's longest window expires in six hours, with half remaining;
	// it would satisfy Claude's early-expiry rule while source still works.
	source, err := a.State.GetAccount(string(adapter.Antigravity), "source")
	if err != nil {
		t.Fatal(err)
	}
	if target := a.expiringTarget(adapter.Antigravity, source); target != "target" {
		t.Fatalf("fixture does not qualify for early-expiry selection: %q", target)
	}
	a.considerExpiring(adapter.Antigravity)
	a.ProbeHTTP(context.Background(), adapter.Antigravity, false)
	pending, err := a.State.GetPending(string(adapter.Antigravity))
	if err != nil || pending.ToID != "" || fake.live != "source" || len(fake.restores) != 0 {
		t.Fatalf("Antigravity used Claude early-expiry policy: pending=%+v writes=%v err=%v", pending, fake.restores, err)
	}
}

func TestAntigravityNextAccountKeepsLongestWindowExpiryOrder(t *testing.T) {
	a, _, now := antigravityRotationFixture(t)
	if err := a.saveBlob(antigravityRotationBlob("later")); err != nil {
		t.Fatal(err)
	}
	antigravityRecordObservation(t, a, "later", antigravityHTTPObservation(t, now, [4]float64{0.9, 0.9, 0.9, 0.9}, [4]time.Duration{time.Hour, 24 * time.Hour, time.Hour, 24 * time.Hour}, -1, 200))
	// An almost-used-up target still goes first because its longest window
	// closes at six hours; source recovery deadlines do not affect ordering.
	antigravityRecordObservation(t, a, "target", antigravityHTTPObservation(t, now, [4]float64{0.01, 0.01, 0.01, 0.01}, [4]time.Duration{2 * time.Hour, 6 * time.Hour, 2 * time.Hour, 6 * time.Hour}, -1, 200))
	if got := a.pickNext(adapter.Antigravity, "source"); got != "target" {
		t.Fatalf("picked %q; want target whose longest window expires first", got)
	}
}

type transactionalAntigravityAdapter struct {
	*antigravityRotationAdapter
	afterRestore func()
	undone       bool
}

func (f *transactionalAntigravityAdapter) RestoreWithRollback(home string, blob adapter.Blob, opts adapter.RestoreOpts) (func() error, error) {
	previous := f.live
	if err := f.Restore(home, blob, opts); err != nil {
		return nil, err
	}
	f.afterRestore()
	return func() error { f.live, f.undone = previous, true; return nil }, nil
}

func TestAntigravityPointerFailureRollsBackNativeIdentity(t *testing.T) {
	a, fake, _ := antigravityRotationFixture(t)
	transaction := &transactionalAntigravityAdapter{antigravityRotationAdapter: fake, afterRestore: func() { _ = a.State.Close() }}
	a.Adapters[adapter.Antigravity] = transaction
	err := a.Switch(adapter.Antigravity, "target", adapter.RestoreOpts{}, false)
	if err == nil || !strings.Contains(err.Error(), "database is closed") || !transaction.undone || fake.live != "source" {
		t.Fatalf("pointer failure did not roll back: err=%v undone=%v live=%s", err, transaction.undone, fake.live)
	}
}

func TestAntigravityUserLoginIsNotRevertedAsWriteback(t *testing.T) {
	a, fake, now := antigravityRotationFixture(t)
	if err := a.State.SetPointer(state.Pointer{Tool: "antigravity", StableID: "source", LastApplyAt: now.Unix(), LastApplyTo: "source"}); err != nil {
		t.Fatal(err)
	}
	fake.live = "target"
	if err := a.Ingest(adapter.Antigravity); err != nil {
		t.Fatal(err)
	}
	pointer, err := a.State.GetPointer("antigravity")
	if err != nil || pointer.StableID != "target" || len(fake.restores) != 0 {
		t.Fatalf("user's new login was reverted: pointer=%+v restores=%v err=%v", pointer, fake.restores, err)
	}
}

func TestAntigravityResetAtObservationSecondIsAlreadyStale(t *testing.T) {
	a, _, now := antigravityRotationFixture(t)
	account := state.Account{StableID: "source", LastQuotaClass: "exhausted", LastUsedPct: 100, LastResetsAt: now.Unix()}
	observation := state.QuotaObservation{Ts: now.Unix(), Class: "exhausted", UsedPct: 100}
	if result, fresh := a.observedQuota(adapter.Antigravity, account, observation); fresh || result.Class != quota.Unknown {
		t.Fatalf("reset window remained evidence in its observation second: fresh=%v class=%s", fresh, result.Class)
	}
}
