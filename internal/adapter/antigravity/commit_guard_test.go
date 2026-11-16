package antigravity

import (
	"context"
	"errors"
	"testing"

	"qswitch/internal/adapter"
)

func TestRestoreChecksAutomaticGuardAfterNativeValidation(t *testing.T) {
	h := newAdapterHarness(t)
	client := h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
	blob := h.blob(t, "target")
	validated := false
	validate := h.adapter.deps.validate
	h.adapter.deps.validate = func(ctx context.Context, raw []byte) ([]byte, adapter.Identity, error) {
		validated = true
		return validate(ctx, raw)
	}
	stop := errors.New("synthetic quota window expired during native validation")
	guarded := false
	err := h.adapter.Restore(h.home, blob, adapter.RestoreOpts{ExpectedCurrentEmail: "old@example.invalid", BeforeWrite: func() error {
		guarded = true
		client.mu.Lock()
		defer client.mu.Unlock()
		if !validated || len(client.methods) < 2 || client.methods[len(client.methods)-1] != "GetAllCascadeTrajectories" {
			t.Fatal("guard ran before native identity and idle validation completed")
		}
		return stop
	}})
	if !errors.Is(err, stop) || !guarded || h.backend.writes != 0 {
		t.Fatalf("late rejection did not prevent transaction: guarded=%v writes=%d err=%v", guarded, h.backend.writes, err)
	}
}

func TestRestoreRejectsChangedExpectedNativeAccount(t *testing.T) {
	h := newAdapterHarness(t)
	h.nativeClient(t, 101, "user-new-login@example.invalid", syntheticIdleActivity)
	err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{ExpectedCurrentEmail: "old@example.invalid", BeforeWrite: func() error { return nil }})
	if err == nil || h.backend.writes != 0 {
		t.Fatalf("different native account accepted automatic switch: writes=%d err=%v", h.backend.writes, err)
	}
}

func TestRestoreValidAutomaticGuardCommitsOnce(t *testing.T) {
	for _, nativeClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed-clients", true: "active-idle-client"}[nativeClient], func(t *testing.T) {
			h := newAdapterHarness(t)
			if nativeClient {
				h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
			}
			calls := 0
			if err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{ExpectedCurrentEmail: "old@example.invalid", BeforeWrite: func() error {
				calls++
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || h.backend.writes != 1 {
				t.Fatalf("valid native switch was not committed exactly once: guards=%d writes=%d", calls, h.backend.writes)
			}
		})
	}
}
