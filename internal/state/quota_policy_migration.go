package state

import (
	"context"
	"fmt"
)

// Version 1 retires quota evidence created before exact exhaustion thresholds
// and complete-window validation. The log and display values remain historical;
// only their authority to select or leave an account is withdrawn.
const quotaPolicyVersion = 1

func (d *DB) migrateQuotaPolicy() error {
	ctx := context.Background()
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("quota policy migration connection: %w", err)
	}
	defer conn.Close()
	// Reserve the writer before reading user_version. Concurrent CLI/daemon
	// opens must not both read an old version and invalidate a new observation.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin quota policy migration: %w", err)
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read quota policy version: %w", err)
	}
	if version < quotaPolicyVersion {
		if _, err := conn.ExecContext(ctx, `
UPDATE accounts
SET last_quota_class='unknown',
    cooling_until=0,
    last_http_at=CASE WHEN IFNULL(http_backoff_until,0)>0 THEN last_http_at ELSE 0 END
WHERE tool IN ('codex','cursor','devin','kimi')
  AND last_quota_class IN ('ok','soft','exhausted')`); err != nil {
			return fmt.Errorf("invalidate legacy quota evidence: %w", err)
		}
		// Keep last_probed_at as a local-log replay boundary. A retained HTTP
		// failure deadline also retains last_http_at, which determines retry
		// progression; normal scheduling retries it after that deadline.
		if _, err := conn.ExecContext(ctx, "PRAGMA user_version=1"); err != nil {
			return fmt.Errorf("write quota policy version: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit quota policy migration: %w", err)
	}
	return nil
}
