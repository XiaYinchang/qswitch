package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
)

func TestLocalQuotaDoesNotReplayOldEvidence(t *testing.T) {
	for _, switched := range []bool{false, true} {
		t.Run(fmt.Sprintf("switched=%v", switched), func(t *testing.T) {
			a, _ := setup(t)
			if _, _, err := a.Capture(adapter.Codex); err != nil {
				t.Fatal(err)
			}
			a.Cfg.General.AutoSwitch = false
			now := time.Now().Truncate(time.Second)
			a.Clock = clock.Fixed{T: now}
			p, err := a.State.GetPointer("codex")
			if err != nil {
				t.Fatal(err)
			}
			if switched {
				p.LastApplyAt = now.Add(-time.Minute).Unix()
				if err := a.State.SetPointer(p); err != nil {
					t.Fatal(err)
				}
			}
			probed := int64(0)
			if !switched {
				probed = now.Add(-time.Minute).Unix()
			}
			if err := a.State.UpdateQuota("codex", "acc-a", "ok", 10, 0, probed, probed, 0); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(a.UserHome, ".codex", "sessions")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "session.jsonl")
			old := now.Add(-time.Hour).Format(time.RFC3339Nano)
			// A recent unrelated append must not turn yesterday's quota into fresh evidence.
			data := fmt.Sprintf("{\"timestamp\":%q,\"error\":{\"code\":\"usage_limit_reached\"}}\n{\"timestamp\":%q,\"type\":\"unrelated\"}\n", old, now.Format(time.RFC3339Nano))
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			a.ProbeLocal(adapter.Codex)
			ac, err := a.State.GetAccount("codex", "acc-a")
			if err != nil || ac.LastQuotaClass != "ok" || ac.LastUsedPct != 10 {
				t.Fatalf("old sample replaced newer account quota: %+v %v", ac, err)
			}
			fresh := fmt.Sprintf("{\"timestamp\":%q,\"error\":{\"code\":\"usage_limit_reached\"}}\n", now.Format(time.RFC3339Nano))
			if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
				t.Fatal(err)
			}
			a.ProbeLocal(adapter.Codex)
			ac, err = a.State.GetAccount("codex", "acc-a")
			if err != nil || ac.LastQuotaClass != "exhausted" {
				t.Fatalf("new evidence was ignored: %+v %v", ac, err)
			}
		})
	}
}
