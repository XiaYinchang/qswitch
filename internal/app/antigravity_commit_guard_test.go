package app

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
	"qswitch/internal/config"
	"qswitch/internal/state"
)

// Models time and native changes during the adapter's target verification and
// idle RPCs. The real Antigravity adapter's guard ordering is tested separately.
type commitBoundaryAdapter struct {
	*antigravityRotationAdapter
	afterCapture func()
	beforeWrite  func()
	revision     string
	guardCalls   int
}

func (f *commitBoundaryAdapter) Fingerprint(string) ([32]byte, error) {
	return sha256.Sum256([]byte(f.live + ":" + f.revision)), nil
}

func (f *commitBoundaryAdapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	blobs, warnings, err := f.antigravityRotationAdapter.Capture(home)
	if f.afterCapture != nil {
		f.afterCapture()
	}
	return blobs, warnings, err
}

func (f *commitBoundaryAdapter) Restore(home string, blob adapter.Blob, opts adapter.RestoreOpts) error {
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	if opts.ExpectedCurrentEmail != "" && opts.ExpectedCurrentEmail != f.live+"@example.test" {
		return fmt.Errorf("%w: synthetic native source identity changed", adapter.ErrRestorePrecondition)
	}
	if opts.BeforeWrite != nil {
		f.guardCalls++
		if err := opts.BeforeWrite(); err != nil {
			return err
		}
	}
	return f.antigravityRotationAdapter.Restore(home, blob, opts)
}

func TestAntigravityAutomaticCommitRejectsChangedAdmission(t *testing.T) {
	for _, change := range []string{"user-login-before-apply", "user-login-during-capture", "user-login-during-restore", "same-user-token-rotation", "source-reset", "target-reset", "target-observation-expiry", "global-disabled", "tool-disabled", "disk-global-disabled", "disk-tool-disabled", "disk-config-missing", "disk-config-invalid", "source-unknown", "target-expired"} {
		t.Run(change, func(t *testing.T) {
			a, original, now := antigravityRotationFixture(t)
			fake := &commitBoundaryAdapter{antigravityRotationAdapter: original}
			a.Adapters[adapter.Antigravity] = fake
			var logs []string
			a.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			sourceReset, targetReset := 24*time.Hour, 24*time.Hour
			if change == "source-reset" {
				sourceReset = time.Minute
			}
			if change == "target-reset" {
				targetReset = time.Minute
			}
			antigravityRecordObservation(t, a, "source", antigravityHTTPObservation(t, now, [4]float64{}, [4]time.Duration{sourceReset, sourceReset, sourceReset, sourceReset}, -1, 200))
			antigravityRecordObservation(t, a, "target", antigravityHTTPObservation(t, now, [4]float64{.5, .5, .5, .5}, [4]time.Duration{targetReset, targetReset, targetReset, targetReset}, -1, 200))
			pending := state.Pending{Tool: "antigravity", FromID: "source", ToID: "target", Reason: "exhausted", QueuedAt: now.Unix()}
			if err := a.State.SetPending(pending); err != nil {
				t.Fatal(err)
			}
			expectedLive := "source"
			userLogin := func() { fake.live = "user-login" }
			switch change {
			case "user-login-before-apply":
				userLogin()
				expectedLive = "user-login"
			case "user-login-during-capture":
				fake.afterCapture = userLogin
				expectedLive = "user-login"
			case "user-login-during-restore":
				fake.beforeWrite = userLogin
				expectedLive = "user-login"
			case "same-user-token-rotation":
				fake.beforeWrite = func() { fake.revision = "new-native-token" }
			case "source-reset", "target-reset":
				fake.beforeWrite = func() { a.Clock = clock.Fixed{T: now.Add(time.Minute)} }
			case "target-observation-expiry":
				interval := a.quotaInterval("antigravity", "target")
				fake.beforeWrite = func() { a.Clock = clock.Fixed{T: now.Add(interval)} }
			case "global-disabled":
				fake.beforeWrite = func() { a.Cfg.General.AutoSwitch = false }
			case "tool-disabled":
				fake.beforeWrite = func() { a.Cfg.Antigravity.Enabled = false }
			case "disk-global-disabled", "disk-tool-disabled":
				fake.beforeWrite = func() {
					path := filepath.Join(a.DataDir, "config.toml")
					cfg, err := config.Load(path)
					if err != nil {
						t.Fatal(err)
					}
					if change == "disk-global-disabled" {
						cfg.General.AutoSwitch = false
					} else {
						cfg.Antigravity.Enabled = false
					}
					if err := cfg.Save(path); err != nil {
						t.Fatal(err)
					}
				}
			case "disk-config-missing", "disk-config-invalid":
				fake.beforeWrite = func() {
					path := filepath.Join(a.DataDir, "config.toml")
					var err error
					if change == "disk-config-missing" {
						err = os.Remove(path)
					} else {
						err = os.WriteFile(path, []byte("invalid = ["), 0600)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			case "source-unknown", "target-expired":
				fake.beforeWrite = func() {
					id, class := "source", "unknown"
					if change == "target-expired" {
						id, class = "target", "expired"
					}
					if err := a.State.UpdateQuota("antigravity", id, class, 0, 0, now.Unix(), now.Unix(), 0); err != nil {
						t.Fatal(err)
					}
				}
			}
			_ = a.TryApply(adapter.Antigravity, false)
			if len(fake.restores) != 0 || fake.live != expectedLive {
				t.Fatalf("invalid admission wrote native credentials: live=%s writes=%v", fake.live, fake.restores)
			}
			remaining, err := a.State.GetPending("antigravity")
			if err != nil || remaining.ToID != "" {
				t.Fatalf("obsolete automatic switch remained queued: %+v err=%v", remaining, err)
			}
			pointer, err := a.State.GetPointer("antigravity")
			if err != nil || pointer.StableID != "source" {
				t.Fatalf("cancel changed the account pointer: %+v / %v", pointer, err)
			}
			if strings.HasPrefix(change, "disk-") && (!a.Cfg.General.AutoSwitch || !a.Cfg.Antigravity.Enabled) {
				t.Fatal("final persisted-settings read replaced in-memory configuration")
			}
			for _, line := range logs {
				if strings.Contains(line, "switched from") {
					t.Fatal("cancelled admission was recorded as a completed switch")
				}
			}
		})
	}
}

func TestAntigravityAutomaticCommitWithValidAdmission(t *testing.T) {
	a, original, now := antigravityRotationFixture(t)
	fake := &commitBoundaryAdapter{antigravityRotationAdapter: original}
	a.Adapters[adapter.Antigravity] = fake
	antigravityRecordObservation(t, a, "source", antigravityHTTPObservation(t, now, [4]float64{}, [4]time.Duration{time.Hour, time.Hour, time.Hour, time.Hour}, -1, 200))
	if err := a.State.SetPending(state.Pending{Tool: "antigravity", FromID: "source", ToID: "target", Reason: "exhausted", QueuedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := a.TryApply(adapter.Antigravity, false); err != nil {
		t.Fatal(err)
	}
	if fake.live != "target" || len(fake.restores) != 1 || fake.guardCalls != 1 {
		t.Fatalf("valid admission was not committed exactly once: live=%s writes=%v guards=%d", fake.live, fake.restores, fake.guardCalls)
	}
}

func TestAntigravityManualCommitKeepsExplicitIntent(t *testing.T) {
	a, original, now := antigravityRotationFixture(t)
	fake := &commitBoundaryAdapter{antigravityRotationAdapter: original}
	a.Adapters[adapter.Antigravity] = fake
	fake.beforeWrite = func() {
		a.Clock = clock.Fixed{T: now.Add(48 * time.Hour)}
		a.Cfg.General.AutoSwitch = false
		fake.live = "user-login"
	}
	if err := a.Switch(adapter.Antigravity, "target", adapter.RestoreOpts{}, false); err != nil {
		t.Fatal(err)
	}
	if fake.live != "target" || len(fake.restores) != 1 || fake.guardCalls != 0 {
		t.Fatal("manual target was incorrectly subject to automatic admission")
	}
}
