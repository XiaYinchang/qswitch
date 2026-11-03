package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
	"qswitch/internal/config"
	"qswitch/internal/state"
)

// claudeUsageByAccount answers the rotation fixture's usage requests with
// each account's body and counts the requests per account.
func claudeUsageByAccount(a *App, bodies map[string]string) map[string]int {
	requests := map[string]int{}
	accounts := map[string]string{"Bearer old-access": "source", "Bearer target-access": "target"}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"source"}}`), nil
		}
		id := accounts[req.Header.Get("Authorization")]
		requests[id]++
		return claudeRecoveryResponse(req, 200, bodies[id]), nil
	})}
	return requests
}

func claudeUsageBody(fiveHour float64, reset time.Time) string {
	return fmt.Sprintf(`{"five_hour":{"utilization":%g,"resets_at":%q},"seven_day":{"utilization":20}}`, fiveHour, reset.UTC().Format(time.RFC3339))
}

// On novix twelve refreshes of every account within seven minutes asked about
// all four accounts each time, three of them known to be exhausted for hours,
// until the provider throttled every account.
func TestRefreshAllSkipsKnownExhaustedAndRecentAccounts(t *testing.T) {
	a, _ := claudeRotationFixture(t, 30*time.Minute)
	a.Cfg.General.AutoSwitch = false
	start := a.now()
	usage := claudeUsageByAccount(a, map[string]string{
		"source": claudeUsageBody(100, start.Add(30*time.Minute)),
		"target": claudeUsageBody(10, start.Add(4*time.Hour)),
	})
	ctx := context.Background()
	refreshAll := func(at time.Duration, source, target int) {
		t.Helper()
		a.Clock = clock.Fixed{T: start.Add(at)}
		a.ProbeHTTP(ctx, adapter.Claude, true)
		a.ProbeRecovered(ctx, adapter.Claude, true)
		if usage["source"] != source || usage["target"] != target {
			t.Fatalf("refresh at +%s: requests source=%d target=%d; want %d and %d", at, usage["source"], usage["target"], source, target)
		}
	}
	refreshAll(2*time.Minute, 0, 1)                // the source is known to be exhausted until its reset
	refreshAll(2*time.Minute+30*time.Second, 0, 1) // the target was asked 30 s ago
	refreshAll(3*time.Minute+time.Second, 0, 2)
	// Refreshing the one account still asks.
	if res := a.ProbeAccount(ctx, adapter.Claude, "source"); res.Source != "http" || usage["source"] != 1 {
		t.Fatalf("single refresh: source=%s requests=%d", res.Source, usage["source"])
	}
	refreshAll(31*time.Minute, 2, 3) // the reset passed
}

// A parked account found exhausted two minutes before its reset waited out
// the five-minute interval from that request; it is asked again once the
// reset passes, by the reset check or by the scheduled one.
func TestParkedAccountRecheckedAfterReset(t *testing.T) {
	for name, check := range map[string]func(*App){
		"reset check":     func(a *App) { a.ProbeReset(context.Background(), adapter.Claude) },
		"scheduled check": func(a *App) { a.ProbeRecovered(context.Background(), adapter.Claude, false) },
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := claudeRotationFixture(t, 4*time.Hour)
			asked := a.now().Add(-time.Minute)
			reset := asked.Add(2 * time.Minute)
			if err := a.State.UpdateQuota("claude", "target", "exhausted", 100, reset.Unix(), asked.Unix(), asked.Unix(), 0); err != nil {
				t.Fatal(err)
			}
			if err := a.State.LogQuota("claude", "target", "exhausted", "http", 100, asked); err != nil {
				t.Fatal(err)
			}
			a.setCooling("claude", "target", reset.Unix())
			usage := claudeUsageByAccount(a, map[string]string{
				"source": claudeUsageBody(100, a.now().Add(4*time.Hour)),
				"target": claudeUsageBody(3, reset.Add(5*time.Hour)),
			})
			a.Clock = clock.Fixed{T: reset.Add(config.ResetGrace - time.Second)}
			check(a)
			if usage["target"] != 0 {
				t.Fatalf("asked %d times within the grace after the reset", usage["target"])
			}
			a.Clock = clock.Fixed{T: reset.Add(config.ResetGrace)}
			check(a)
			if p, _ := a.State.GetPointer("claude"); usage["target"] != 1 || usage["source"] != 0 || p.StableID != "target" {
				t.Fatalf("requests source=%d target=%d pointer=%s; want one request to the target and the switch", usage["source"], usage["target"], p.StableID)
			}
		})
	}
}

// The current account's own reset is checked the same way, once; a request
// that fails is not repeated on every tick.
func TestCurrentAccountRecheckedAfterReset(t *testing.T) {
	t.Run("recovered", func(t *testing.T) {
		a, _ := claudeRotationFixture(t, 2*time.Minute)
		reset := a.now().Add(2 * time.Minute)
		usage := claudeUsageByAccount(a, map[string]string{"source": claudeUsageBody(3, reset.Add(5*time.Hour))})
		for _, at := range []time.Duration{config.ResetGrace - time.Second, config.ResetGrace, config.ResetGrace + 5*time.Second} {
			a.Clock = clock.Fixed{T: reset.Add(at)}
			a.ProbeReset(context.Background(), adapter.Claude)
			if at < config.ResetGrace && usage["source"] != 0 {
				t.Fatalf("asked %d times within the grace after the reset", usage["source"])
			}
		}
		if ac, _ := a.State.GetAccount("claude", "source"); usage["source"] != 1 || ac.LastQuotaClass != "ok" {
			t.Fatalf("requests=%d class=%s; want one request finding the account recovered", usage["source"], ac.LastQuotaClass)
		}
	})
	t.Run("throttled", func(t *testing.T) {
		a, usage := claudeRotationFixture(t, 2*time.Minute)
		reset := a.now().Add(2 * time.Minute)
		for _, at := range []time.Duration{config.ResetGrace, config.ResetGrace + 5*time.Second, config.ResetGrace + 10*time.Second} {
			a.Clock = clock.Fixed{T: reset.Add(at)}
			a.ProbeReset(context.Background(), adapter.Claude)
		}
		if *usage != 1 {
			t.Fatalf("requests=%d; want the throttled request made once", *usage)
		}
	})
}

// The page marked a confirmed exhaustion unconfirmed half an hour after the
// request, though the daemon rightly does not ask again before the reset.
func TestOverviewKeepsConfirmedExhaustionCurrent(t *testing.T) {
	a, _ := setup(t)
	now := time.Unix(1790000000, 0)
	reset := now.Add(3 * time.Hour)
	ac := state.Account{Tool: "devin", StableID: "fixture", LastQuotaClass: "exhausted", LastUsedPct: 100, LastResetsAt: reset.Unix()}
	if err := a.State.UpsertAccount(ac); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota(ac.Tool, ac.StableID, "exhausted", "http", 100, now); err != nil {
		t.Fatal(err)
	}
	view := func(at time.Time) AccountView {
		t.Helper()
		a.Clock = clock.Fixed{T: at}
		for _, tool := range a.Overview().Tools {
			if tool.Tool == ac.Tool && len(tool.Accounts) == 1 {
				return tool.Accounts[0]
			}
		}
		t.Fatal("fixture missing from overview")
		return AccountView{}
	}
	if v := view(now.Add(2 * time.Hour)); v.QuotaStale {
		t.Fatal("a confirmed exhaustion was marked unconfirmed before its reset")
	}
	// A later failed request does not withdraw it.
	if err := a.State.LogQuota(ac.Tool, ac.StableID, "unknown", "http_throttled", 0, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if v := view(now.Add(2 * time.Hour)); v.QuotaStale || v.LastProbeClass != "unknown" {
		t.Fatalf("after a failed request: stale=%v last probe=%s", v.QuotaStale, v.LastProbeClass)
	}
	if v := view(reset.Add(time.Second)); !v.QuotaStale {
		t.Fatal("an exhaustion past its reset was shown as current")
	}
}
