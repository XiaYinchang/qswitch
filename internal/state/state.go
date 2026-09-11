package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	sq, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sq.SetMaxOpenConns(1)
	d := &DB{sql: sq}
	if err := d.migrate(); err != nil {
		sq.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	_, err := d.sql.Exec(`
CREATE TABLE IF NOT EXISTS accounts (
  tool TEXT NOT NULL,
  stable_id TEXT NOT NULL,
  email TEXT,
  plan_hint TEXT,
  incomplete INTEGER NOT NULL DEFAULT 0,
  stale_cli INTEGER NOT NULL DEFAULT 0,
  cooling_until INTEGER,
  last_quota_class TEXT,
  last_used_pct REAL,
  last_resets_at INTEGER,
  last_probed_at INTEGER,
  last_http_at INTEGER,
  http_backoff_until INTEGER,
  last_captured_at INTEGER,
  vault_gen INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tool, stable_id)
);
CREATE TABLE IF NOT EXISTS live_pointer (
  tool TEXT PRIMARY KEY,
  stable_id TEXT,
  gen INTEGER NOT NULL DEFAULT 0,
  desync INTEGER NOT NULL DEFAULT 0,
  last_apply_at INTEGER,
  last_apply_to TEXT,
  writeback_retries INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS pending_switch (
  tool TEXT PRIMARY KEY,
  from_id TEXT,
  to_id TEXT,
  queued_at INTEGER NOT NULL,
  reason TEXT,
  blocked_by_pid TEXT
);
CREATE TABLE IF NOT EXISTS self_writes (
  path TEXT PRIMARY KEY,
  gen INTEGER,
  sha256 BLOB,
  ignore_until_ns INTEGER
);
CREATE TABLE IF NOT EXISTS quota_log (
  ts INTEGER NOT NULL,
  tool TEXT,
  stable_id TEXT,
  class TEXT,
  used_pct REAL,
  source TEXT
);
CREATE INDEX IF NOT EXISTS quota_log_ts ON quota_log(ts);
`)
	if err != nil {
		return err
	}
	_, _ = d.sql.Exec(`ALTER TABLE accounts ADD COLUMN quota_detail TEXT`)
	return nil
}

type Account struct {
	Tool, StableID, Email, PlanHint string
	Incomplete, StaleCLI            bool
	CoolingUntil                    int64
	LastQuotaClass                  string
	LastUsedPct                     float64
	LastResetsAt                    int64
	LastProbedAt                    int64
	LastHTTPAt                      int64
	HTTPBackoffUntil                int64
	LastCapturedAt                  int64
	VaultGen                        int64
	QuotaDetail                     string
}

func (d *DB) UpsertAccount(a Account) error {
	inc, st := 0, 0
	if a.Incomplete {
		inc = 1
	}
	if a.StaleCLI {
		st = 1
	}
	_, err := d.sql.Exec(`INSERT INTO accounts(tool,stable_id,email,plan_hint,incomplete,stale_cli,cooling_until,last_quota_class,last_used_pct,last_resets_at,last_probed_at,last_http_at,http_backoff_until,last_captured_at,vault_gen)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(tool,stable_id) DO UPDATE SET
 email=excluded.email,
 plan_hint=CASE WHEN excluded.plan_hint='' THEN accounts.plan_hint ELSE excluded.plan_hint END,
 incomplete=excluded.incomplete, stale_cli=excluded.stale_cli,
 last_captured_at=excluded.last_captured_at, vault_gen=excluded.vault_gen`,
		a.Tool, a.StableID, a.Email, a.PlanHint, inc, st, a.CoolingUntil, a.LastQuotaClass, a.LastUsedPct, a.LastResetsAt, a.LastProbedAt, a.LastHTTPAt, a.HTTPBackoffUntil, a.LastCapturedAt, a.VaultGen)
	return err
}

func (d *DB) GetAccount(tool, id string) (Account, error) {
	var a Account
	var inc, st int
	err := d.sql.QueryRow(`SELECT tool,stable_id,email,plan_hint,incomplete,stale_cli,IFNULL(cooling_until,0),IFNULL(last_quota_class,''),IFNULL(last_used_pct,0),IFNULL(last_resets_at,0),IFNULL(last_probed_at,0),IFNULL(last_http_at,0),IFNULL(http_backoff_until,0),IFNULL(last_captured_at,0),vault_gen,IFNULL(quota_detail,'') FROM accounts WHERE tool=? AND stable_id=?`, tool, id).
		Scan(&a.Tool, &a.StableID, &a.Email, &a.PlanHint, &inc, &st, &a.CoolingUntil, &a.LastQuotaClass, &a.LastUsedPct, &a.LastResetsAt, &a.LastProbedAt, &a.LastHTTPAt, &a.HTTPBackoffUntil, &a.LastCapturedAt, &a.VaultGen, &a.QuotaDetail)
	a.Incomplete = inc == 1
	a.StaleCLI = st == 1
	return a, err
}

func (d *DB) ListAccounts(tool string) ([]Account, error) {
	q := `SELECT tool,stable_id,email,plan_hint,incomplete,stale_cli,IFNULL(cooling_until,0),IFNULL(last_quota_class,''),IFNULL(last_used_pct,0),IFNULL(last_resets_at,0),IFNULL(last_probed_at,0),IFNULL(last_http_at,0),IFNULL(http_backoff_until,0),IFNULL(last_captured_at,0),vault_gen,IFNULL(quota_detail,'') FROM accounts`
	var args []any
	if tool != "" {
		q += ` WHERE tool=?`
		args = append(args, tool)
	}
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var inc, st int
		if err := rows.Scan(&a.Tool, &a.StableID, &a.Email, &a.PlanHint, &inc, &st, &a.CoolingUntil, &a.LastQuotaClass, &a.LastUsedPct, &a.LastResetsAt, &a.LastProbedAt, &a.LastHTTPAt, &a.HTTPBackoffUntil, &a.LastCapturedAt, &a.VaultGen, &a.QuotaDetail); err != nil {
			return nil, err
		}
		a.Incomplete = inc == 1
		a.StaleCLI = st == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) DeleteAccount(tool, id string) error {
	_, err := d.sql.Exec(`DELETE FROM accounts WHERE tool=? AND stable_id=?`, tool, id)
	return err
}

func (d *DB) UpdateQuota(tool, id, class string, pct float64, resets, probed, httpAt, backoff int64) error {
	_, err := d.sql.Exec(`UPDATE accounts SET last_quota_class=?, last_used_pct=?, last_resets_at=?, last_probed_at=?, last_http_at=?, http_backoff_until=? WHERE tool=? AND stable_id=?`,
		class, pct, resets, probed, httpAt, backoff, tool, id)
	return err
}

func (d *DB) SetQuotaDetail(tool, id, detail string) error {
	_, err := d.sql.Exec(`UPDATE accounts SET quota_detail=? WHERE tool=? AND stable_id=?`, detail, tool, id)
	return err
}

func (d *DB) SetPlanHint(tool, id, plan string) error {
	plan = strings.TrimSpace(plan)
	if plan == "" || plan == "-" {
		return nil
	}
	_, err := d.sql.Exec(`UPDATE accounts SET plan_hint=? WHERE tool=? AND stable_id=?`, plan, tool, id)
	return err
}

func (d *DB) UpdateQuotaSnapshot(tool, id, class string, pct float64, resets, probed int64) error {
	_, err := d.sql.Exec(`UPDATE accounts SET last_quota_class=?, last_used_pct=?, last_resets_at=?, last_probed_at=? WHERE tool=? AND stable_id=?`,
		class, pct, resets, probed, tool, id)
	return err
}

type QuotaPoint struct {
	Ts      int64
	UsedPct float64
	Class   string
}

func (d *DB) RecentQuota(tool, id string, n int) ([]QuotaPoint, error) {
	if n <= 0 {
		n = 16
	}
	rows, err := d.sql.Query(`SELECT ts, used_pct, class FROM quota_log WHERE tool=? AND stable_id=? ORDER BY ts DESC LIMIT ?`, tool, id, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []QuotaPoint
	for rows.Next() {
		var p QuotaPoint
		if err := rows.Scan(&p.Ts, &p.UsedPct, &p.Class); err != nil {
			return nil, err
		}
		rev = append(rev, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]QuotaPoint, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out, nil
}

func (d *DB) SetCooling(tool, id string, until int64) error {
	_, err := d.sql.Exec(`UPDATE accounts SET cooling_until=? WHERE tool=? AND stable_id=?`, until, tool, id)
	return err
}

type Pointer struct {
	Tool             string
	StableID         string
	Gen              int64
	Desync           bool
	LastApplyAt      int64
	LastApplyTo      string
	WritebackRetries int
}

func (d *DB) GetPointer(tool string) (Pointer, error) {
	var p Pointer
	var desync int
	err := d.sql.QueryRow(`SELECT tool,IFNULL(stable_id,''),gen,desync,IFNULL(last_apply_at,0),IFNULL(last_apply_to,''),writeback_retries FROM live_pointer WHERE tool=?`, tool).
		Scan(&p.Tool, &p.StableID, &p.Gen, &desync, &p.LastApplyAt, &p.LastApplyTo, &p.WritebackRetries)
	if err == sql.ErrNoRows {
		return Pointer{Tool: tool}, nil
	}
	p.Desync = desync == 1
	return p, err
}

func (d *DB) SetPointer(p Pointer) error {
	ds := 0
	if p.Desync {
		ds = 1
	}
	_, err := d.sql.Exec(`INSERT INTO live_pointer(tool,stable_id,gen,desync,last_apply_at,last_apply_to,writeback_retries)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(tool) DO UPDATE SET stable_id=excluded.stable_id, gen=excluded.gen, desync=excluded.desync,
 last_apply_at=excluded.last_apply_at, last_apply_to=excluded.last_apply_to, writeback_retries=excluded.writeback_retries`,
		p.Tool, p.StableID, p.Gen, ds, p.LastApplyAt, p.LastApplyTo, p.WritebackRetries)
	return err
}

type Pending struct {
	Tool, FromID, ToID, Reason, BlockedByPID string
	QueuedAt                                 int64
}

func (d *DB) SetPending(p Pending) error {
	_, err := d.sql.Exec(`INSERT INTO pending_switch(tool,from_id,to_id,queued_at,reason,blocked_by_pid) VALUES(?,?,?,?,?,?)
ON CONFLICT(tool) DO UPDATE SET from_id=excluded.from_id, to_id=excluded.to_id, queued_at=excluded.queued_at, reason=excluded.reason, blocked_by_pid=excluded.blocked_by_pid`,
		p.Tool, p.FromID, p.ToID, p.QueuedAt, p.Reason, p.BlockedByPID)
	return err
}

func (d *DB) GetPending(tool string) (Pending, error) {
	var p Pending
	err := d.sql.QueryRow(`SELECT tool,from_id,to_id,queued_at,reason,IFNULL(blocked_by_pid,'') FROM pending_switch WHERE tool=?`, tool).
		Scan(&p.Tool, &p.FromID, &p.ToID, &p.QueuedAt, &p.Reason, &p.BlockedByPID)
	if err == sql.ErrNoRows {
		return Pending{}, nil
	}
	return p, err
}

func (d *DB) ClearPending(tool string) error {
	_, err := d.sql.Exec(`DELETE FROM pending_switch WHERE tool=?`, tool)
	return err
}

func (d *DB) RememberSelfWrite(path string, sum [32]byte, ignoreUntil time.Time) error {
	_, err := d.sql.Exec(`INSERT INTO self_writes(path,gen,sha256,ignore_until_ns) VALUES(?,1,?,?)
ON CONFLICT(path) DO UPDATE SET gen=gen+1, sha256=excluded.sha256, ignore_until_ns=excluded.ignore_until_ns`,
		path, sum[:], ignoreUntil.UnixNano())
	return err
}

func (d *DB) IsSelfWrite(path string, sum [32]byte, now time.Time) (bool, error) {
	var stored []byte
	var until int64
	err := d.sql.QueryRow(`SELECT sha256, ignore_until_ns FROM self_writes WHERE path=?`, path).Scan(&stored, &until)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(stored) != 32 {
		return false, nil
	}
	var got [32]byte
	copy(got[:], stored)
	if got != sum {
		return false, nil
	}
	// fingerprint match: treat as self-write even after ignore window
	return true, nil
}

func (d *DB) LogQuota(tool, id, class, source string, pct float64, ts time.Time) error {
	_, err := d.sql.Exec(`INSERT INTO quota_log(ts,tool,stable_id,class,used_pct,source) VALUES(?,?,?,?,?,?)`,
		ts.Unix(), tool, id, class, pct, source)
	return err
}
