package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
)

const reportFixtureID = "11111111111111111111111111111111"

func reportFixture(t *testing.T) (*App, Report) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	a := &App{DataDir: t.TempDir(), Clock: clock.Fixed{T: now}}
	if err := a.SetEnvironments([]Environment{{ID: reportFixtureID, Name: "Synthetic node"}}); err != nil {
		t.Fatal(err)
	}
	report := Report{ObservedAt: now.Unix(), Overview: Overview{
		Instance: InstanceInfo{ID: reportFixtureID, Name: "Synthetic node", Version: "test", OS: "linux", ManagedTools: []adapter.Tool{adapter.Codex}},
		Tools:    []ToolView{{Tool: "codex", Accounts: []AccountView{{Tool: "codex", StableID: "synthetic-account", Email: "fixture@example.test"}}}},
	}}
	return a, report
}

func TestReportPersistsAndExpiresWithoutDeletingDisplayData(t *testing.T) {
	a, report := reportFixture(t)
	missing, err := a.ReportSnapshot(reportFixtureID)
	if err != nil || !missing.Stale || missing.Overview != nil {
		t.Fatalf("missing report = %+v, %v", missing, err)
	}
	if err := a.AcceptReport(reportFixtureID, report); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a.reportPath(reportFixtureID))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report is not private: %v, %v", info, err)
	}
	// A new App reads the persisted snapshot without any adapter or state DB.
	b := &App{DataDir: a.DataDir, Clock: clock.Fixed{T: time.Unix(report.ObservedAt+59, 0)}}
	fresh, err := b.ReportSnapshot(reportFixtureID)
	if err != nil || fresh.Stale || fresh.ObservedAt != report.ObservedAt || fresh.ReceivedAt != report.ObservedAt || fresh.Overview.Instance.ID != reportFixtureID {
		t.Fatalf("reopened report = %+v, %v", fresh, err)
	}
	b.Clock = clock.Fixed{T: time.Unix(report.ObservedAt+60, 0)}
	stale, err := b.ReportSnapshot(reportFixtureID)
	if err != nil || !stale.Stale || stale.Overview == nil || stale.ObservedAt != fresh.ObservedAt || stale.ReceivedAt != fresh.ReceivedAt {
		t.Fatalf("expiration discarded or refreshed historical data: %+v, %v", stale, err)
	}
}

func TestReportRejectsWrongIdentityTimeAndOrderWithoutRefreshingReceipt(t *testing.T) {
	a, report := reportFixture(t)
	if err := a.AcceptReport(reportFixtureID, report); err != nil {
		t.Fatal(err)
	}
	a.Clock = clock.Fixed{T: time.Unix(report.ObservedAt+10, 0)}
	for _, tc := range []struct {
		name string
		at   int64
		id   string
	}{
		{"duplicate", report.ObservedAt, reportFixtureID},
		{"out of order", report.ObservedAt - 1, reportFixtureID},
		{"old", report.ObservedAt - 50, reportFixtureID},
		{"future", report.ObservedAt + 16, reportFixtureID},
		{"wrong body identity", report.ObservedAt + 1, "22222222222222222222222222222222"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := report
			candidate.ObservedAt = tc.at
			candidate.Overview.Instance.ID = tc.id
			if err := a.AcceptReport(reportFixtureID, candidate); !errors.Is(err, ErrReportRejected) {
				t.Fatalf("invalid observation accepted: %v", err)
			}
			got, err := a.ReportSnapshot(reportFixtureID)
			if err != nil || got.ObservedAt != report.ObservedAt || got.ReceivedAt != report.ObservedAt {
				t.Fatalf("rejected report refreshed persisted receipt: %+v %v", got, err)
			}
		})
	}
	report.ObservedAt += 15 // Five seconds ahead is tolerated for clock skew.
	if err := a.AcceptReport(reportFixtureID, report); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ReportSnapshot(reportFixtureID); err != nil || got.Stale {
		t.Fatalf("tolerated clock skew was marked stale: %+v %v", got, err)
	}
	if err := a.SetEnvironments(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReportSnapshot(reportFixtureID); !errors.Is(err, ErrReportInstance) {
		t.Fatalf("removed instance retained directory access: %v", err)
	}
}

func TestConcurrentReportsCannotRollBackNewerSnapshot(t *testing.T) {
	a, report := reportFixture(t)
	a.Clock = clock.Fixed{T: time.Unix(report.ObservedAt+10, 0)}
	var wg sync.WaitGroup
	for _, offset := range []int64{1, 2} {
		wg.Go(func() {
			candidate := report
			candidate.ObservedAt += offset
			if err := a.AcceptReport(reportFixtureID, candidate); err != nil && !errors.Is(err, ErrReportRejected) {
				t.Errorf("accept report: %v", err)
			}
		})
	}
	wg.Wait()
	got, err := a.ReportSnapshot(reportFixtureID)
	if err != nil || got.ObservedAt != report.ObservedAt+2 {
		t.Fatalf("concurrent report moved backwards: %+v %v", got, err)
	}
}

func TestReportingTokensRequireIndependentPrivateCredentials(t *testing.T) {
	a, _ := reportFixture(t)
	a.Cfg.Web.Token = strings.Repeat("a", 32)
	path := filepath.Join(a.DataDir, "reporting-tokens.json")
	for _, tc := range []struct {
		name   string
		tokens map[string]string
		mode   os.FileMode
		ok     bool
	}{
		{"valid", map[string]string{reportFixtureID: strings.Repeat("b", 32)}, 0o600, true},
		{"admin token", map[string]string{reportFixtureID: strings.Repeat("a", 32)}, 0o600, false},
		{"short token", map[string]string{reportFixtureID: "too-short"}, 0o600, false},
		{"shared token", map[string]string{reportFixtureID: strings.Repeat("b", 32), "22222222222222222222222222222222": strings.Repeat("b", 32)}, 0o600, false},
		{"public file", map[string]string{reportFixtureID: strings.Repeat("b", 32)}, 0o644, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.tokens)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			token, err := a.ReportingToken(reportFixtureID)
			if (err == nil) != tc.ok || tc.ok && token != tc.tokens[reportFixtureID] {
				t.Fatalf("credential validity = %v, want %v", err == nil, tc.ok)
			}
		})
	}
}

func TestInvalidStoredReportIsUnavailable(t *testing.T) {
	a, report := reportFixture(t)
	if err := a.AcceptReport(reportFixtureID, report); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.reportPath(reportFixtureID), []byte(`{"overview":{"instance":{"id":"wrong"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := a.ReportSnapshot(reportFixtureID)
	if err == nil || !got.Stale || got.Overview != nil {
		t.Fatalf("corrupt report remained available: %+v %v", got, err)
	}
}

func TestFreshReportRepairsOnlyCorruptContent(t *testing.T) {
	for _, content := range []string{`broken JSON`, `{"overview":{"instance":{"id":"wrong"}}}`, `{"overview":{"instance":{"id":"11111111111111111111111111111111"}},"observed_at":0,"received_at":0}`} {
		t.Run(content, func(t *testing.T) {
			a, report := reportFixture(t)
			if err := a.AcceptReport(reportFixtureID, report); err != nil {
				t.Fatal(err)
			}
			path := a.reportPath(reportFixtureID)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if snapshot, err := a.ReportSnapshot(reportFixtureID); !errors.Is(err, errReportCorrupt) || snapshot.Overview != nil || !snapshot.Stale {
				t.Fatal("corrupt data was exposed")
			}
			a.Clock = clock.Fixed{T: time.Unix(report.ObservedAt+1, 0)}
			report.ObservedAt++
			if err := a.AcceptReport(reportFixtureID, report); err != nil {
				t.Fatalf("fresh authenticated report did not repair corrupt cache: %v", err)
			}
			got, err := a.ReportSnapshot(reportFixtureID)
			if err != nil || got.Stale || got.Overview == nil || got.ObservedAt != report.ObservedAt || got.ReceivedAt != report.ObservedAt {
				t.Fatalf("repaired report = %+v, %v", got, err)
			}
		})
	}
	for _, invalid := range []string{"permissions", "directory", "symlink"} {
		t.Run(invalid, func(t *testing.T) {
			a, report := reportFixture(t)
			if err := a.AcceptReport(reportFixtureID, report); err != nil {
				t.Fatal(err)
			}
			path := a.reportPath(reportFixtureID)
			switch invalid {
			case "permissions":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(a.DataDir, "missing-report"), path); err != nil {
					t.Fatal(err)
				}
			}
			a.Clock = clock.Fixed{T: time.Unix(report.ObservedAt+1, 0)}
			report.ObservedAt++
			if err := a.AcceptReport(reportFixtureID, report); err == nil || errors.Is(err, errReportCorrupt) {
				t.Fatal("unsafe report path was overwritten")
			}
			info, err := os.Lstat(path)
			if err != nil || (invalid == "permissions" && info.Mode().Perm() != 0o644) || (invalid == "directory" && !info.IsDir()) || (invalid == "symlink" && info.Mode()&os.ModeSymlink == 0) {
				t.Fatal("rejected replacement changed the unsafe path")
			}
		})
	}
}
