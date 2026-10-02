package app

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/state"
)

func TestTryApplyRevalidatesExhaustedPending(t *testing.T) {
	for _, tc := range []struct {
		name        string
		class       string
		from        string
		reason      string
		disableAuto bool
		disableTool bool
		wantSwitch  bool
	}{
		{name: "recovered", class: "ok", from: "acc-b", reason: "exhausted"},
		{name: "current changed", class: "exhausted", from: "acc-a", reason: "exhausted"},
		{name: "auto disabled", class: "exhausted", from: "acc-b", reason: "exhausted", disableAuto: true},
		{name: "tool disabled", class: "exhausted", from: "acc-b", reason: "exhausted", disableTool: true},
		{name: "unknown", class: "unknown", from: "acc-b", reason: "exhausted"},
		{name: "valid exhausted", class: "exhausted", from: "acc-b", reason: "exhausted", wantSwitch: true},
		{name: "manual reason", class: "unknown", from: "acc-a", reason: "manual", disableAuto: true, disableTool: true, wantSwitch: true},
		{name: "empty reason", class: "ok", from: "acc-b", disableAuto: true, wantSwitch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := setup(t)
			if _, _, err := a.Capture(adapter.Codex); err != nil {
				t.Fatal(err)
			}
			writeCodex(t, a.UserHome, "acc-b", "b@x.com")
			if _, _, err := a.Capture(adapter.Codex); err != nil {
				t.Fatal(err)
			}
			a.Cfg.General.AutoSwitch = !tc.disableAuto
			a.Cfg.Codex.Enabled = !tc.disableTool
			if err := a.State.UpdateQuotaSnapshot("codex", tc.from, tc.class, 100, 0, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			if err := a.State.SetPending(state.Pending{Tool: "codex", FromID: tc.from, ToID: "acc-a", Reason: tc.reason, QueuedAt: time.Now().Unix()}); err != nil {
				t.Fatal(err)
			}
			if err := a.TryApply(adapter.Codex, false); err != nil {
				t.Fatal(err)
			}
			wantID := "acc-b"
			if tc.wantSwitch {
				wantID = "acc-a"
			}
			p, err := a.State.GetPointer("codex")
			if err != nil || p.StableID != wantID {
				t.Fatalf("want current %s, got %+v err=%v", wantID, p, err)
			}
			raw, err := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json"))
			if err != nil || !jsonContains(raw, wantID) {
				t.Fatalf("live credentials should still match %s: err=%v", wantID, err)
			}
			pending, err := a.State.GetPending("codex")
			if err != nil || pending.ToID != "" {
				t.Fatalf("pending should be cleared: %+v err=%v", pending, err)
			}
		})
	}
}

func TestTryApplyPreservesPendingOnAccountReadFailure(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetPending(state.Pending{Tool: "codex", FromID: "acc-b", ToID: "acc-a", Reason: "exhausted", QueuedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := a.State.DeleteAccount("codex", "acc-b"); err != nil {
		t.Fatal(err)
	}
	if err := a.TryApply(adapter.Codex, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("source-account read failure must be returned: %v", err)
	}
	p, err := a.State.GetPointer("codex")
	if err != nil || p.StableID != "acc-b" {
		t.Fatalf("read failure must not change current account: %+v err=%v", p, err)
	}
	pend, err := a.State.GetPending("codex")
	if err != nil || pend.ToID != "acc-a" {
		t.Fatalf("read failure must preserve pending: %+v err=%v", pend, err)
	}
}
