package app

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
	"qswitch/internal/config"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

// A pre-upgrade classifier could confirm exhaustion at 99.5%, or mark a
// response with a missing window OK. Changing the classifier does not change
// those already-persisted observations; opening the upgraded DB must revoke
// their authority without deleting their historical usage.
func TestUpgradeRevokesLegacyAutomaticQuotaEvidence(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Codex, adapter.Cursor, adapter.Devin, adapter.Kimi} {
		for _, class := range []string{"ok", "exhausted"} {
			t.Run(string(tool)+"/"+class, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "state.db")
				db, err := state.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().Truncate(time.Second)
				pct := 12.0
				if class == "exhausted" {
					pct = 99.5
				}
				ac := state.Account{Tool: string(tool), StableID: "legacy-fixture", LastQuotaClass: class, LastUsedPct: pct, LastResetsAt: now.Add(time.Hour).Unix(), LastProbedAt: now.Unix(), LastHTTPAt: now.Unix(), CoolingUntil: now.Add(time.Hour).Unix()}
				if err := db.UpsertAccount(ac); err != nil {
					t.Fatal(err)
				}
				if err := db.SetQuotaDetail(ac.Tool, ac.StableID, `[{"id":"one-old-window","used_pct":12}]`); err != nil {
					t.Fatal(err)
				}
				if err := db.LogQuota(ac.Tool, ac.StableID, class, "http", pct, now); err != nil {
					t.Fatal(err)
				}
				a := &App{State: db, Cfg: config.Default(), Clock: clock.Fixed{T: now}}
				before, fresh, err := a.cachedAutomaticQuota(tool, ac)
				if err != nil || !fresh || string(before.Class) != class {
					t.Fatalf("fixture did not reproduce legacy authorization: result=%+v fresh=%v err=%v", before, fresh, err)
				}
				if class == "exhausted" {
					if yes, err := a.knownExhausted(tool, ac); err != nil || !yes {
						t.Fatalf("fixture source not authorized: %v %v", yes, err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				// This data represents a database written before the policy migration.
				legacy, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = legacy.Exec("PRAGMA user_version=0")
				closeErr := legacy.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("legacy marker: %v %v", err, closeErr)
				}
				db, err = state.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				a.State = db
				ac, err = db.GetAccount(ac.Tool, ac.StableID)
				if err != nil {
					t.Fatal(err)
				}
				after, fresh, err := a.cachedAutomaticQuota(tool, ac)
				if err != nil || fresh || after.Class != quota.Unknown {
					t.Fatalf("legacy observation still authorizes a target after upgrade: result=%+v fresh=%v err=%v", after, fresh, err)
				}
				if yes, err := a.knownExhausted(tool, ac); err != nil || yes {
					t.Fatalf("legacy observation still authorizes leaving source: %v %v", yes, err)
				}
				if ac.LastUsedPct != pct || ac.QuotaDetail == "" || ac.CoolingUntil != 0 || ac.LastHTTPAt != 0 {
					t.Fatalf("migration damaged history or delayed recheck: %+v", ac)
				}
			})
		}
	}
}
