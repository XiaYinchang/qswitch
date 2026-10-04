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
