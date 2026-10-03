package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
)

func TestGrokQuotaSnapshotsReplaceBucketsAndSurviveFailures(t *testing.T) {
	a, _ := setup(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	writeGrokLive(t, a.UserHome, "grok-fixture", "grok@example.test")
	if _, _, err := a.Capture(adapter.Grok); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpdateQuota("grok", "grok-fixture", "expired", 100, now.Unix(), now.Unix(), now.Unix(), now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail("grok", "grok-fixture", `[{"id":"old","used_pct":100}]`); err != nil {
		t.Fatal(err)
	}
	body := `{"config":{"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-27T14:04:34Z","end":"2026-10-04T14:04:34Z"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"prepaidBalance":{"val":0}}}`
	status := 200
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "cli-chat-proxy.grok.com" {
			t.Fatalf("unexpected host %s", req.URL.Host)
		}
		response := body
		if req.URL.Path == "/v1/settings" {
			response = `{"subscription_tier_display":"SuperGrok Heavy"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})}
	check := func(wantID string, pct float64) {
		t.Helper()
		ac, err := a.State.GetAccount("grok", "grok-fixture")
		b := quota.DecodeBuckets(ac.QuotaDetail)
		if err != nil || ac.LastQuotaClass != "ok" || ac.LastUsedPct != pct || len(b) != 1 || b[0].ID != wantID || b[0].UsedPct != pct {
			t.Fatalf("stored class=%s pct=%v buckets=%+v err=%v", ac.LastQuotaClass, ac.LastUsedPct, b, err)
		}
		for _, tool := range a.Overview().Tools {
			if tool.Tool == "grok" {
				if len(tool.Accounts) != 1 || len(tool.Accounts[0].Buckets) != 1 || tool.Accounts[0].Buckets[0].ID != wantID || tool.Accounts[0].RemainingPct == nil || *tool.Accounts[0].RemainingPct != 100-pct {
					t.Fatalf("overview: %+v", tool)
				}
				return
			}
		}
		t.Fatal("Grok section absent")
	}
	res := a.ProbeAccount(context.Background(), adapter.Grok, "grok-fixture")
	if res.Class != quota.OK || res.UsedPct != 0 {
		t.Fatalf("omitted zero: %+v", res)
	}
	check("weekly", 0)
	ac, _ := a.State.GetAccount("grok", "grok-fixture")
	if ac.HTTPBackoffUntil != 0 {
		t.Fatal("successful quota retained backoff")
	}
	body = `{"config":{"creditUsagePercent":25,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-10-01T00:00:00Z","end":"2026-11-01T00:00:00Z"}}}`
	a.ProbeAccount(context.Background(), adapter.Grok, "grok-fixture")
	check("monthly", 25)
	status = 503
	a.ProbeAccount(context.Background(), adapter.Grok, "grok-fixture")
	check("monthly", 25)
	// A later official billing log is another complete snapshot, not a bucket delta.
	logdir := filepath.Join(a.UserHome, ".grok", "logs")
	if err := os.MkdirAll(logdir, 0700); err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf(`{"ts":%q,"msg":"billing: fetched credits config","ctx":{"config":{"creditUsagePercent":1,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-27T14:04:34Z","end":"2026-10-04T14:04:34Z"}}}}`+"\n", now.Add(time.Minute).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(logdir, "unified.jsonl"), []byte(event), 0600); err != nil {
		t.Fatal(err)
	}
	a.Clock = clock.Fixed{T: now.Add(2 * time.Minute)}
	a.ProbeLocal(adapter.Grok)
	check("weekly", 1)
}
