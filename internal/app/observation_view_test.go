package app

import (
	"testing"
	"time"

	"qswitch/internal/quota"
	"qswitch/internal/state"
)

func TestOverviewRetainsQuotaAndReportsLatestFailedObservation(t *testing.T) {
	a, _ := setup(t)
	ac := state.Account{Tool: "devin", StableID: "fixture", LastQuotaClass: "ok", LastUsedPct: 20, LastResetsAt: 1800000000}
	if err := a.State.UpsertAccount(ac); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail(ac.Tool, ac.StableID, `[{"id":"weekly","used_pct":20}]`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790000000, 0)
	if err := a.State.LogQuota(ac.Tool, ac.StableID, "unknown", "http", 0, now); err != nil {
		t.Fatal(err)
	}
	view := func() AccountView {
		t.Helper()
		for _, tool := range a.Overview().Tools {
			if tool.Tool == ac.Tool && len(tool.Accounts) == 1 {
				return tool.Accounts[0]
			}
		}
		t.Fatal("fixture missing from overview")
		return AccountView{}
	}
	v := view()
	if v.Class != "ok" || v.UsedPct != 20 || len(v.Buckets) != 1 || v.Buckets[0].UsedPct != 20 || !v.QuotaStale || v.LastProbeClass != "unknown" || v.LastProbeSource != "http" || v.LastProbeAt != now.Unix() {
		t.Fatalf("retained quota was not distinguished from failed observation: %+v", v)
	}
	if err := a.State.LogQuota(ac.Tool, ac.StableID, "ok", "http", 20, now); err != nil {
		t.Fatal(err)
	}
	v = view()
	if v.QuotaStale || v.LastProbeClass != "ok" {
		t.Fatal("later success in the same second did not clear the failed observation")
	}
}

func TestDerivedBotPreservesUnconfirmedParentState(t *testing.T) {
	for _, class := range []string{"expired", "unknown"} {
		t.Run(class, func(t *testing.T) {
			parent := accountView(state.Account{Tool: "cursor", StableID: "fixture", LastQuotaClass: class, QuotaDetail: quota.EncodeBuckets([]quota.Bucket{{ID: "bot", UsedPct: 20}})}, "fixture", 1)
			_, bot := splitCursorBot(ToolView{Tool: "cursor", Accounts: []AccountView{parent}})
			if len(bot.Accounts) != 1 || bot.Accounts[0].Class != class || bot.Accounts[0].RemainingPct != nil || !bot.Accounts[0].QuotaStale {
				t.Fatal("derived Bot converted an unconfirmed parent into usable quota")
			}
		})
	}
}
