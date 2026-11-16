package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/config"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
)

func TestInstancePersistsAndScopeRejectsBeforeAdapter(t *testing.T) {
	a, _ := setup(t)
	b, err := Open(a.UserHome, a.DataDir, &secutil.Memory{}, nil, nil, func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(a.Instance().ID) != 32 || a.Instance().ID != b.Instance().ID {
		t.Fatal("instance identity changed on reopen")
	}
	if err := a.SetManagedTools([]adapter.Tool{adapter.Antigravity}); err != nil {
		t.Fatal(err)
	}
	a.Adapters[adapter.Codex] = nil // Any accidental adapter access must fail the test.
	if _, _, err := a.Capture(adapter.Codex); err == nil {
		t.Fatal("scope bypass")
	}
	if err := a.Switch(adapter.Codex, "ignored", adapter.RestoreOpts{}, false); err == nil {
		t.Fatal("switch scope bypass")
	}
	ov := a.Overview()
	if len(ov.Tools) != 1 || ov.Tools[0].Tool != "antigravity" || len(ov.Instance.ManagedTools) != 1 {
		t.Fatalf("wrong scope: %+v", ov.Instance)
	}
}

func TestConfigUpdatesMergeIndependentProcessesAndPreserveMaster(t *testing.T) {
	a, _ := setup(t)
	b, err := Open(a.UserHome, a.DataDir, &secutil.Memory{}, nil, nil, func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() { errs <- a.SetAuto(adapter.Codex, false) })
	wg.Go(func() { errs <- b.SetAuto(adapter.Grok, false) })
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(filepath.Join(a.DataDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.Enabled || cfg.Grok.Enabled || !cfg.Cursor.Enabled {
		t.Fatal("cross-process update lost a tool setting")
	}
	if err := a.SetAuto("", false); err != nil {
		t.Fatal(err)
	}
	if err := b.SetAuto(adapter.Codex, true); err != nil {
		t.Fatal(err)
	}
	cfg, _ = config.Load(filepath.Join(a.DataDir, "config.toml"))
	if cfg.General.AutoSwitch || !cfg.Codex.Enabled || cfg.Grok.Enabled {
		t.Fatal("enabling one tool changed master or other tool")
	}
	for _, tv := range b.Overview().Tools {
		if tv.Tool == "codex" && (tv.Auto || tv.AutoReason == "") {
			t.Fatal("reported effective auto despite disabled master")
		}
	}
}

func TestDisabledAutoStillProbesAndDoesNotSwitch(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAuto(adapter.Codex, false); err != nil {
		t.Fatal(err)
	}
	hits := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":100},"secondary_window":{"used_percent":100}}}`)), Request: r}, nil
	})}
	a.ProbeHTTP(context.Background(), adapter.Codex, false)
	ac, _ := a.State.GetAccount("codex", "acc-a")
	pending, _ := a.State.GetPending("codex")
	if hits != 1 || ac.LastQuotaClass != string(quota.Exhausted) || pending.ToID != "" {
		t.Fatalf("auto/probe coupled: hits=%d class=%s pending=%s", hits, ac.LastQuotaClass, pending.ToID)
	}
}

func TestEnvironmentDirectoryContainsOnlyInstanceMetadata(t *testing.T) {
	a, _ := setup(t)
	const id = "11111111111111111111111111111111"
	items := []Environment{{ID: id, Name: "远端测试"}}
	if err := a.SetEnvironments(items); err != nil {
		t.Fatal(err)
	}
	for _, badID := range []string{"", "remote-fixture", "../escape", strings.Repeat("1", 31), strings.Repeat("1", 33), strings.Repeat("g", 32), strings.Repeat("A", 32)} {
		items[0].ID = badID
		if err := a.SetEnvironments(items); err == nil {
			t.Fatalf("accepted invalid instance ID %q", badID)
		}
	}
	got, err := a.Environments()
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatal("invalid write replaced directory")
	}
	raw, err := os.ReadFile(filepath.Join(a.DataDir, "environments.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields []map[string]any
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 || len(fields[0]) != 2 || fields[0]["id"] != id || fields[0]["name"] != "远端测试" {
		t.Fatal("environment directory persisted more than instance metadata")
	}
	st, err := os.Stat(filepath.Join(a.DataDir, "environments.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
		t.Fatal("environment directory is not private")
	}
}

func TestOverviewDoesNotReadEnvironmentDirectory(t *testing.T) {
	a, _ := setup(t)
	if err := os.WriteFile(filepath.Join(a.DataDir, "environments.json"), []byte(`invalid-directory`), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(a.Overview())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["environments"]; ok {
		t.Fatal("business overview contains the global environment directory")
	}
	if _, ok := fields["environments_error"]; ok {
		t.Fatal("global directory errors leaked into the business overview")
	}
	if len(fields["tools"]) == 0 {
		t.Fatal("invalid directory interrupted the business overview")
	}
}

func TestBotUsesIndependentObservation(t *testing.T) {
	now := time.Now().Unix()
	parent := ToolView{Tool: "cursor", Accounts: []AccountView{{Class: "ok", LastProbeAt: now, Buckets: []quota.Bucket{{ID: "bot", Class: quota.Unknown, ObservedAt: now, UsedPct: 12}}}}}
	_, bot := splitCursorBot(parent)
	if bot.Accounts[0].Class != "unknown" || bot.Accounts[0].RemainingPct != nil || !bot.Accounts[0].QuotaStale {
		t.Fatal("parent success promoted failed bot request")
	}
	parent.Accounts[0].Class = "unknown"
	parent.Accounts[0].Buckets[0].Class = quota.OK
	_, bot = splitCursorBot(parent)
	if bot.Accounts[0].Class != "ok" || bot.Accounts[0].LastProbeAt != now {
		t.Fatal("bot observation was replaced with parent state")
	}
	parent.Accounts[0].Buckets[0].ObservedAt = now - 3600
	_, bot = splitCursorBotAt(parent, now, 1800)
	if !bot.Accounts[0].QuotaStale {
		t.Fatal("fresh parent hid stale Bot evidence")
	}
}

func TestPlatformCapabilitiesPreserveProviderBoundaries(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		z := capabilities(adapter.ZCode, goos, nil)
		if z["switch"].Available || z["auto_switch"].Available || !z["probe"].Available {
			t.Fatalf("ZCode boundary on %s", goos)
		}
		a := capabilities(adapter.Antigravity, goos, nil)
		if !a["switch"].Available || !a["desktop_sync"].Available || !a["run"].Available {
			t.Fatalf("Antigravity boundary on %s", goos)
		}
		c := capabilities(adapter.Claude, goos, nil)
		if c["login"].Available != (goos == "linux") || (goos == "windows" && c["run"].Available) {
			t.Fatalf("Claude boundary on %s", goos)
		}
		if goos == "windows" && capabilities(adapter.Kimi, goos, nil)["parked_refresh"].Available {
			t.Fatal("unverified Windows Kimi recovery advertised")
		}
		for _, tool := range []adapter.Tool{adapter.Grok, adapter.Kimi} {
			if goos == "windows" && capabilities(tool, goos, nil)["live_refresh"].Available {
				t.Fatalf("unverified native lock on %s", tool)
			}
		}
	}
}
