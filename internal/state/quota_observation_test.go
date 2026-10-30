package state

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLatestQuotaUsesTimeThenInsertionOrder(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.LatestQuota("codex", "a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing observation error=%v", err)
	}
	if _, err := db.LatestConfirmedQuota("codex", "a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing confirmed observation error=%v", err)
	}
	now := time.Unix(1791000000, 0)
	for _, p := range []struct {
		tool, id, class, source string
		at                      time.Time
	}{
		{"codex", "a", "ok", "http", now},
		{"codex", "a", "unknown", "refresh", now},
		{"codex", "a", "soft", "http", now.Add(-time.Second)},
		{"codex", "b", "ok", "http", now.Add(time.Second)},
		{"grok", "a", "ok", "http", now.Add(time.Second)},
	} {
		if err := db.LogQuota(p.tool, p.id, p.class, p.source, 21, p.at); err != nil {
			t.Fatal(err)
		}
	}
	p, err := db.LatestQuota("codex", "a")
	if err != nil || p.Class != "unknown" || p.Source != "refresh" || p.Ts != now.Unix() || p.UsedPct != 21 {
		t.Fatalf("latest=%+v error=%v", p, err)
	}
	points, err := db.RecentQuota("codex", "a", 3)
	if err != nil || len(points) != 3 || points[0].Class != "soft" || points[1].Class != "ok" || points[2].Class != "unknown" {
		t.Fatalf("recent ordering=%+v error=%v", points, err)
	}
	if err := db.LogQuota("codex", "a", "expired", "http", 0, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	p, err = db.LatestConfirmedQuota("codex", "a")
	if err != nil || p.Class != "ok" || p.Source != "http" || p.Ts != now.Unix() || p.UsedPct != 21 {
		t.Fatalf("failed observations replaced confirmed record: %+v, error=%v", p, err)
	}
	var id, parent, unused int
	var detail string
	err = db.sql.QueryRow(`EXPLAIN QUERY PLAN SELECT ts,class,source,used_pct FROM quota_log WHERE tool=? AND stable_id=? ORDER BY ts DESC,rowid DESC LIMIT 1`, "codex", "a").Scan(&id, &parent, &unused, &detail)
	if err != nil || !strings.Contains(detail, "quota_log_account_ts") {
		t.Fatalf("latest observation must use the account index: %s, error=%v", detail, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LatestQuota("codex", "a"); err == nil {
		t.Fatal("database errors must not look like an absent observation")
	}
	if _, err := db.LatestConfirmedQuota("codex", "a"); err == nil {
		t.Fatal("database errors must not look like an absent confirmed observation")
	}
}

func TestPruneQuotaLogKeepsWhatReadersNeed(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Unix(1793000000, 0)
	cutoff := now.Add(-30 * 24 * time.Hour)
	old := cutoff.Add(-time.Hour)
	for _, id := range []string{"busy", "parked"} {
		if err := db.UpsertAccount(Account{Tool: "claude", StableID: id, Email: id + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	// A deleted account: only old rows, and no account row any more.
	for i := range 20 {
		if err := db.LogQuota("claude", "deleted", "ok", "http", 10, old.Add(-time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// A busy account: 20 old observations and 20 recent ones.
	for i := range 20 {
		if err := db.LogQuota("claude", "busy", "ok", "http", float64(i), old.Add(-time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := db.LogQuota("claude", "busy", "ok", "http", float64(i), now.Add(-time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// A parked account last observed long ago: a confirmed result followed by
	// 20 failed attempts, all older than the cutoff.
	if err := db.LogQuota("claude", "parked", "exhausted", "http", 100, old.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if err := db.LogQuota("claude", "parked", "unknown", "http_throttled", 0, old.Add(-time.Duration(20-i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.PruneQuotaLog(cutoff); err != nil {
		t.Fatal(err)
	}
	count := func(id string) (n int) {
		if err := db.sql.QueryRow(`SELECT count(*) FROM quota_log WHERE stable_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("busy"); n != 20 {
		t.Fatalf("busy account kept %d rows, want only the 20 recent ones", n)
	}
	if points, err := db.RecentQuota("claude", "parked", 16); err != nil || len(points) != 16 {
		t.Fatalf("parked account lost recent history: %d points, err=%v", len(points), err)
	}
	if c, err := db.LatestConfirmedQuota("claude", "parked"); err != nil || c.Class != "exhausted" {
		t.Fatalf("parked account lost its confirmed observation: %+v err=%v", c, err)
	}
	if n := count("parked"); n != 17 {
		t.Fatalf("parked account kept %d rows, want 16 recent plus 1 confirmed", n)
	}
	if n := count("deleted"); n != 0 {
		t.Fatalf("deleted account kept %d old rows", n)
	}
}
