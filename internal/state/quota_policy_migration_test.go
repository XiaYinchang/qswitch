package state

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func legacyQuotaFixture(t *testing.T) (string, map[string]Account) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Account{}
	for _, tool := range []string{"codex", "cursor", "devin", "kimi", "antigravity", "claude", "grok", "zcode"} {
		for _, class := range []string{"ok", "soft", "exhausted", "unknown", "expired"} {
			ac := Account{Tool: tool, StableID: class, Email: "synthetic@example.test", Phone: "synthetic-phone", DisplayName: "Fixture", PlanHint: "Fixture Plan", CoolingUntil: 9000, LastQuotaClass: class, LastUsedPct: 99.5, LastResetsAt: 9000, LastProbedAt: 8000, LastHTTPAt: 8000, LastCapturedAt: 7000, VaultGen: 5}
			// Backoff can coexist with retained successful quota. Preserve both
			// its deadlines and its previous attempt time for retry progression.
			if class == "soft" || class == "expired" {
				ac.HTTPBackoffUntil = 10000
				ac.RefreshBackoffUntil = 11000
			}
			if err := db.UpsertAccount(ac); err != nil {
				t.Fatal(err)
			}
			ac.QuotaDetail = `[{"id":"legacy-window","used_pct":99.5,"resets_at":9000}]`
			if err := db.SetQuotaDetail(tool, class, ac.QuotaDetail); err != nil {
				t.Fatal(err)
			}
			if err := db.LogQuota(tool, class, class, "http", ac.LastUsedPct, time.Unix(ac.LastProbedAt, 0)); err != nil {
				t.Fatal(err)
			}
			want[tool+"/"+class] = ac
		}
		if err := db.SetPointer(Pointer{Tool: tool, StableID: "exhausted", Gen: 7}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetPending(Pending{Tool: tool, FromID: "exhausted", ToID: "ok", QueuedAt: 8100, Reason: "exhausted"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.sql.Exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, want
}

func TestQuotaPolicyMigrationPreservesHistoryAndFailureBackoff(t *testing.T) {
	path, want := legacyQuotaFixture(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for key, original := range want {
		expected := original
		if (original.Tool == "codex" || original.Tool == "cursor" || original.Tool == "devin" || original.Tool == "kimi") && (original.LastQuotaClass == "ok" || original.LastQuotaClass == "soft" || original.LastQuotaClass == "exhausted") {
			expected.LastQuotaClass = "unknown"
			expected.CoolingUntil = 0
			if original.HTTPBackoffUntil == 0 {
				expected.LastHTTPAt = 0
			}
		}
		got, err := db.GetAccount(original.Tool, original.StableID)
		if err != nil || got != expected {
			t.Fatalf("%s migration mismatch: got=%+v expected=%+v err=%v", key, got, expected, err)
		}
		last, err := db.LatestQuota(original.Tool, original.StableID)
		if err != nil || last.Class != original.LastQuotaClass || last.UsedPct != original.LastUsedPct || last.Ts != original.LastProbedAt {
			t.Fatalf("history changed for %s: %+v %v", key, last, err)
		}
		p, err := db.GetPointer(original.Tool)
		if err != nil || p.StableID != "exhausted" || p.Gen != 7 {
			t.Fatalf("pointer changed: %+v %v", p, err)
		}
		pending, err := db.GetPending(original.Tool)
		if err != nil || pending.FromID != "exhausted" || pending.ToID != "ok" {
			t.Fatalf("pending operation changed: %+v %v", pending, err)
		}
	}
	var count, version int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM quota_log").Scan(&count); err != nil || count != len(want) {
		t.Fatalf("history count %d %v", count, err)
	}
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != quotaPolicyVersion {
		t.Fatalf("migration version %d %v", version, err)
	}
}

func TestQuotaPolicyMigrationRunsOnlyOnceAcrossConcurrentOpen(t *testing.T) {
	path, _ := legacyQuotaFixture(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQuota("codex", "ok", "ok", 12, 9000, 8200, 8200, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.LogQuota("codex", "ok", "ok", "http", 12, time.Unix(8200, 0)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetQuotaDetail("codex", "ok", `[{"id":"5h","used_pct":12},{"id":"7d","used_pct":10}]`); err != nil {
		t.Fatal(err)
	}
	want, err := db.GetAccount("codex", "ok")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			reopened, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer reopened.Close()
			got, err := reopened.GetAccount("codex", "ok")
			if err == nil && got != want {
				err = sql.ErrNoRows
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("reopen invalidated fresh observation: %v", err)
		}
	}
}

func TestQuotaPolicyMigrationRollsBackVersionAndData(t *testing.T) {
	path, _ := legacyQuotaFixture(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_quota_migration BEFORE UPDATE ON accounts BEGIN SELECT RAISE(ABORT,'synthetic migration failure'); END`); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(path); err == nil {
		db.Close()
		t.Fatal("migration failure was ignored")
	}
	var version int
	var class string
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatalf("failed migration advanced version: %d %v", version, err)
	}
	if err := raw.QueryRow("SELECT last_quota_class FROM accounts WHERE tool='codex' AND stable_id='exhausted'").Scan(&class); err != nil || class != "exhausted" {
		t.Fatalf("failed migration partially changed data: %s %v", class, err)
	}
	if _, err := raw.Exec("DROP TRIGGER reject_quota_migration"); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ac, err := db.GetAccount("codex", "exhausted")
	if err != nil || ac.LastQuotaClass != "unknown" {
		t.Fatalf("retry did not migrate: %+v %v", ac, err)
	}
}
