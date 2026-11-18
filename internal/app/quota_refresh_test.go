package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/antigravity"
	"qswitch/internal/clock"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

func TestRefreshToolQuotaUpdatesOnlyRequestedToolWithoutSwitching(t *testing.T) {
	a, now := antigravityPipelineFixture(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	blob, err := a.loadBlob(adapter.Antigravity, antigravityPipelineID)
	if err != nil {
		t.Fatal(err)
	}
	blob.Identity.StableID = "synthetic-second-account"
	raw, err := antigravity.CredentialsFromBlob(blob)
	if err != nil {
		t.Fatal(err)
	}
	blob, err = antigravity.NewBlob(raw, blob.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(blob); err != nil {
		t.Fatal(err)
	}
	ptr := state.Pointer{Tool: "antigravity", StableID: antigravityPipelineID, Gen: 4}
	if err := a.State.SetPointer(ptr); err != nil {
		t.Fatal(err)
	}
	a.Cfg.General.AutoSwitch = true
	a.Cfg.Antigravity.Enabled = true
	remaining := [4]float64{.8, .8, .8, .8}
	calls := antigravityPipelineHTTP(t, a, func() []byte { return antigravityPipelineBody(t, now, remaining, -1, -1) })
	for _, wantClass := range []string{"ok", "exhausted"} {
		summary, err := a.RefreshToolQuota(context.Background(), adapter.Antigravity)
		if err != nil || summary != (QuotaRefreshSummary{Tool: adapter.Antigravity, Attempted: 2, Updated: 2}) {
			t.Fatalf("refresh summary=%+v err=%v", summary, err)
		}
		for _, id := range []string{antigravityPipelineID, blob.Identity.StableID} {
			account, err := a.State.GetAccount("antigravity", id)
			if err != nil || account.LastQuotaClass != wantClass || account.LastHTTPAt != now.Unix() || len(quota.DecodeBuckets(account.QuotaDetail)) != 4 {
				t.Fatalf("fresh provider quota was not persisted: class=%s err=%v", account.LastQuotaClass, err)
			}
		}
		remaining = [4]float64{}
	}
	if *calls != 4 {
		t.Fatalf("provider calls=%d", *calls)
	}
	got, err := a.State.GetPointer("antigravity")
	if err != nil || got != ptr {
		t.Fatalf("observation changed current account: %+v %v", got, err)
	}
	if pending, _ := a.State.GetPending("antigravity"); pending.ToID != "" {
		t.Fatal("observation queued a switch")
	}
	if codex, _ := a.State.GetAccount("codex", "acc-a"); codex.LastHTTPAt != 0 {
		t.Fatal("refresh queried another tool")
	}
}

func TestRefreshToolQuotaFailureAndBackoffAreNotUpdates(t *testing.T) {
	a, now := antigravityPipelineFixture(t)
	if err := a.State.UpdateQuota("antigravity", antigravityPipelineID, "ok", 25, now.Add(time.Hour).Unix(), now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix(), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota("antigravity", antigravityPipelineID, "ok", "http", 25, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})}
	first, err := a.RefreshToolQuota(context.Background(), adapter.Antigravity)
	if err != nil || first != (QuotaRefreshSummary{Tool: adapter.Antigravity, Attempted: 1, Failed: 1}) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	a.Clock = clock.Fixed{T: now.Add(time.Second)}
	second, err := a.RefreshToolQuota(context.Background(), adapter.Antigravity)
	if err != nil || second != (QuotaRefreshSummary{Tool: adapter.Antigravity, Skipped: 1}) || calls != 1 {
		t.Fatalf("backoff=%+v calls=%d err=%v", second, calls, err)
	}
	confirmed, err := a.State.LatestConfirmedQuota("antigravity", antigravityPipelineID)
	if err != nil || confirmed.Ts != now.Add(-time.Hour).Unix() {
		t.Fatal("failed query made confirmed quota appear new")
	}
}

func TestRefreshToolQuotaRejectsConcurrentRefreshAndCancels(t *testing.T) {
	a, _ := antigravityPipelineFixture(t)
	started := make(chan struct{})
	var calls atomic.Int32
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan QuotaRefreshSummary, 1)
	go func() { summary, _ := a.RefreshToolQuota(ctx, adapter.Antigravity); done <- summary }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not begin")
	}
	if _, err := a.RefreshToolQuota(context.Background(), adapter.Antigravity); !errors.Is(err, ErrQuotaRefreshBusy) {
		t.Fatalf("concurrent refresh was not rejected: %v", err)
	}
	cancel()
	select {
	case result := <-done:
		if result.Failed != 1 || result.Updated != 0 || calls.Load() != 1 {
			t.Fatalf("cancelled refresh=%+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled query did not finish")
	}
}

func TestRefreshToolQuotaLockDeadlineAndScope(t *testing.T) {
	a, _ := antigravityPipelineFixture(t)
	if err := a.SetManagedTools([]adapter.Tool{adapter.Antigravity}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RefreshToolQuota(context.Background(), adapter.Codex); err == nil {
		t.Fatal("refreshed outside managed scope")
	}
	if _, err := a.RefreshToolQuota(context.Background(), adapter.Tool("grokbot")); err == nil {
		t.Fatal("derived view bypassed source-tool scope")
	}
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "antigravity.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("query escaped held tool lock")
		return nil, errors.New("unexpected query")
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	summary, err := a.RefreshToolQuota(ctx, adapter.Antigravity)
	if err != nil || summary != (QuotaRefreshSummary{Tool: adapter.Antigravity, Skipped: 1}) {
		t.Fatalf("lock deadline summary=%+v err=%v", summary, err)
	}
}

func TestRefreshToolQuotaEmptyAndReadOnlySubscription(t *testing.T) {
	a, _ := setup(t)
	summary, err := a.RefreshToolQuota(context.Background(), adapter.ZCode)
	if err != nil || summary != (QuotaRefreshSummary{Tool: adapter.ZCode}) {
		t.Fatalf("empty=%+v err=%v", summary, err)
	}
	before := writeZCodeSubscriptionFixture(t, a.UserHome)
	defer assertZCodeFilesUnchanged(t, before)
	if _, _, err := a.Capture(adapter.ZCode); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "bigmodel.cn" {
			t.Error("queried unrelated provider")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":200,"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":35}]}}`)), Request: r}, nil
	})}
	summary, err = a.RefreshToolQuota(context.Background(), adapter.ZCode)
	if err != nil || summary.Updated != 1 || summary.Failed != 0 {
		t.Fatalf("read-only probe=%+v err=%v", summary, err)
	}
	if a.CheckAction(adapter.ZCode, "switch") == nil {
		t.Fatal("refresh enabled a read-only tool's switch")
	}
}

func TestQuotaResultUpdatedRequiresFreshProviderResult(t *testing.T) {
	for _, source := range []string{"", "cached", "backoff", "refresh_backoff", "refresh", "jsonl", "http_throttled"} {
		if QuotaResultUpdated(quota.Result{Class: quota.OK, Source: source}) {
			t.Errorf("cached or non-provider source %q counted as updated", source)
		}
	}
	for _, class := range []quota.Class{quota.Unknown, quota.Expired} {
		if QuotaResultUpdated(quota.Result{Class: class, Source: "http"}) {
			t.Errorf("unsuccessful %s counted as updated", class)
		}
	}
	for _, source := range []string{"http", "http_402", "http_usage_limit"} {
		if !QuotaResultUpdated(quota.Result{Class: quota.Exhausted, Source: source}) {
			t.Errorf("fresh result %s not counted", source)
		}
	}
}
