package state

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAccountIdentityMigratesExistingQuotaDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE accounts (
 tool TEXT NOT NULL, stable_id TEXT NOT NULL, email TEXT, plan_hint TEXT,
 incomplete INTEGER NOT NULL DEFAULT 0, stale_cli INTEGER NOT NULL DEFAULT 0,
 cooling_until INTEGER, last_quota_class TEXT, last_used_pct REAL,
 last_resets_at INTEGER, last_probed_at INTEGER, last_http_at INTEGER,
 http_backoff_until INTEGER, last_captured_at INTEGER,
 vault_gen INTEGER NOT NULL DEFAULT 0, quota_detail TEXT,
 PRIMARY KEY (tool,stable_id)
);
INSERT INTO accounts VALUES ('kimi','fixture','fixture@example.test','Pro',0,1,110,'soft',95,120,130,140,150,160,7,'[{"id":"weekly","used_pct":95}]');
CREATE TABLE live_pointer (
 tool TEXT PRIMARY KEY, stable_id TEXT, gen INTEGER NOT NULL DEFAULT 0,
 desync INTEGER NOT NULL DEFAULT 0, last_apply_at INTEGER, last_apply_to TEXT,
 writeback_retries INTEGER NOT NULL DEFAULT 0
);
INSERT INTO live_pointer VALUES ('kimi','fixture',3,0,170,'fixture',1);
`)
	closeErr := legacy.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("legacy fixture: exec=%v close=%v", err, closeErr)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GetAccount("kimi", "fixture")
	want := Account{Tool: "kimi", StableID: "fixture", Email: "fixture@example.test", PlanHint: "Pro", StaleCLI: true, CoolingUntil: 110, LastQuotaClass: "soft", LastUsedPct: 95, LastResetsAt: 120, LastProbedAt: 130, LastHTTPAt: 140, HTTPBackoffUntil: 150, LastCapturedAt: 160, VaultGen: 7, QuotaDetail: `[{"id":"weekly","used_pct":95}]`}
	if err != nil || got != want {
		t.Fatalf("migration changed existing account or quota: got=%+v err=%v", got, err)
	}
	p, err := db.GetPointer("kimi")
	if err != nil || p != (Pointer{Tool: "kimi", StableID: "fixture", Gen: 3, LastApplyAt: 170, LastApplyTo: "fixture", WritebackRetries: 1}) {
		t.Fatalf("migration changed pointer: %+v err=%v", p, err)
	}
	var phone, name string
	if err := db.sql.QueryRow(`SELECT IFNULL(phone,''),IFNULL(display_name,'') FROM accounts WHERE tool='kimi' AND stable_id='fixture'`).Scan(&phone, &name); err != nil {
		t.Fatalf("identity columns were not added: %v", err)
	}
	if phone != "" || name != "" {
		t.Fatal("migration invented account labels")
	}
}

func TestAccountIdentityRoundTripPreservesQuotaAndMissingLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	want := Account{Tool: "kimi", StableID: "fixture", Email: "fixture@example.test", PlanHint: "Pro", Phone: "+8613800000000", DisplayName: "Fixture Name", CoolingUntil: 110, LastQuotaClass: "soft", LastUsedPct: 95, LastResetsAt: 120, LastProbedAt: 130, LastHTTPAt: 140, HTTPBackoffUntil: 150, RefreshBackoffUntil: 155, LastCapturedAt: 160, VaultGen: 7}
	if err := db.UpsertAccount(want); err != nil {
		t.Fatal(err)
	}
	want.QuotaDetail = `[{"id":"weekly","used_pct":95}]`
	if err := db.SetQuotaDetail(want.Tool, want.StableID, want.QuotaDetail); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		got, err := db.GetAccount(want.Tool, want.StableID)
		if err != nil || got != want {
			t.Fatalf("account roundtrip mismatch: got=%+v want=%+v err=%v", got, want, err)
		}
		for _, tool := range []string{"kimi", ""} {
			accounts, err := db.ListAccounts(tool)
			if err != nil || len(accounts) != 1 || accounts[0] != want {
				t.Fatalf("account listing mismatch: count=%d err=%v", len(accounts), err)
			}
		}
	}
	check()
	for _, labels := range []struct{ phone, name string }{
		{"+8613900000000", "Updated Name"},
		{"", "Newest Name"},
		{"+8613700000000", ""},
		{"", ""},
	} {
		capture := Account{Tool: want.Tool, StableID: want.StableID, Email: want.Email, Phone: labels.phone, DisplayName: labels.name, LastCapturedAt: want.LastCapturedAt + 1, VaultGen: want.VaultGen + 1}
		if err := db.UpsertAccount(capture); err != nil {
			t.Fatal(err)
		}
		if labels.phone != "" {
			want.Phone = labels.phone
		}
		if labels.name != "" {
			want.DisplayName = labels.name
		}
		want.LastCapturedAt, want.VaultGen = capture.LastCapturedAt, capture.VaultGen
		check()
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	check()
}

func TestAccountIdentityMigrationReportsSchemaFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE VIEW accounts AS SELECT 'kimi' AS tool, 'fixture' AS stable_id`)
	_ = legacy.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err == nil {
		_ = db.Close()
		t.Fatal("unsupported accounts schema was silently accepted")
	}
}

func TestAccountIdentityMigrationKeepsExistingPhoneColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Account{Tool: "kimi", StableID: "fixture", Phone: "+8613800000000", LastQuotaClass: "soft", LastUsedPct: 95}
	if err := db.UpsertAccount(want); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	// Emulate a database where phone already exists but display_name does not.
	if _, err := db.sql.Exec(`ALTER TABLE accounts DROP COLUMN display_name`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GetAccount(want.Tool, want.StableID)
	if err != nil || got != want {
		t.Fatalf("existing phone or quota changed during migration: got=%+v err=%v", got, err)
	}
}
