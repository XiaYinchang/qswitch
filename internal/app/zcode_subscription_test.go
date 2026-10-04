package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/zcode"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

func writeZCodeSubscriptionFixture(t *testing.T, home string) map[string][]byte {
	t.Helper()
	// The official credential service accepts plaintext values while migrating
	// older installations. Keep the independent CLI key deliberately different.
	store := map[string]string{
		"oauth:active_provider":       "bigmodel",
		"oauth:bigmodel:user_info":    `{"id":"zcode-fixture","username":"desktop@example.test","displayName":"Desktop","rawProfile":{"email":"desktop@example.test"}}`,
		"oauth:bigmodel:access_token": "unused-oauth-token",
		"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:zcode-fixture:api-key": "fixture-desktop-plan-key",
		"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:other:api-key":         "unused-other-account-key",
	}
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(home, ".zcode", "v2", "credentials.json"): raw,
		filepath.Join(home, ".zcode", "v2", "setting.json"):     []byte(`{"providerFamilyDomain":"bigmodel"}`),
		filepath.Join(home, ".zcode", "v2", "config.json"):      []byte(`{"providers":{"keep":"desktop"}}`),
		filepath.Join(home, ".zcode", "cli", "config.json"):     []byte(`{"provider":{"zai":{"apiKey":"fixture-independent-cli-key"}}}`),
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func assertZCodeFilesUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("monitoring changed %s (read error: %v)", filepath.Base(path), err)
		}
	}
}

func TestZCodeSubscriptionCaptureVaultProbeOverview(t *testing.T) {
	a, _ := setup(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	before := writeZCodeSubscriptionFixture(t, a.UserHome)
	defer assertZCodeFilesUnchanged(t, before)
	const id = "bigmodel:zcode-fixture"
	status, calls := http.StatusOK, 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.String() != "https://bigmodel.cn/api/monitor/usage/quota/limit" || req.Header.Get("Authorization") != "fixture-desktop-plan-key" || req.Body != nil {
			t.Fatal("unexpected subscription request or credential")
		}
		body := `{"code":200,"success":true,"data":{"level":"max","limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1,"nextResetTime":1791009301952},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":1,"nextResetTime":1791513884984},{"type":"TIME_LIMIT","unit":5,"number":1,"usage":4000,"currentValue":511,"remaining":3489,"percentage":12,"nextResetTime":1791686684993}]}}`
		if status != http.StatusOK {
			body = `{"error":"unavailable"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	blobs, warnings, err := a.Capture(adapter.ZCode)
	if err != nil || len(blobs) != 1 || len(warnings) != 0 || blobs[0].Identity.StableID != id || calls != 0 {
		t.Fatalf("capture count=%d warnings=%d calls=%d error=%v", len(blobs), len(warnings), calls, err)
	}
	encrypted, err := os.ReadFile(filepath.Join(a.DataDir, "vault", "zcode", id+".enc"))
	if err != nil || !bytes.HasPrefix(encrypted, []byte("qsw1")) || bytes.Contains(encrypted, []byte("fixture-desktop-plan-key")) {
		t.Fatal("subscription credential was not persisted as an encrypted vault blob")
	}
	blob, err := a.loadBlob(adapter.ZCode, id)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := zcode.CredsFromBlob(blob)
	if err != nil || creds.Provider != "bigmodel" || creds.APIKey != "fixture-desktop-plan-key" {
		t.Fatal("vault round trip lost the current desktop subscription credential")
	}
	for _, excluded := range []string{"unused-oauth-token", "unused-other-account-key", "fixture-independent-cli-key"} {
		if bytes.Contains(blob.Payload, []byte(excluded)) {
			t.Fatal("subscription vault retained an unrelated credential")
		}
	}
	if err := a.State.UpdateQuota("zcode", id, "expired", 100, now.Unix(), now.Unix(), now.Unix(), now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail("zcode", id, `[{"id":"old","used_pct":100}]`); err != nil {
		t.Fatal(err)
	}
	wantBuckets := []quota.Bucket{{ID: "5h", UsedPct: 1, ResetsAt: 1791009301}, {ID: "weekly", UsedPct: 1, ResetsAt: 1791513884}, {ID: "mcp", UsedPct: 12, ResetsAt: 1791686684}}
	checkSnapshot := func() state.Account {
		t.Helper()
		ac, err := a.State.GetAccount("zcode", id)
		if err != nil || ac.LastQuotaClass != "ok" || ac.LastUsedPct != 1 || ac.LastResetsAt != 1791513884 || !reflect.DeepEqual(quota.DecodeBuckets(ac.QuotaDetail), wantBuckets) {
			t.Fatalf("unexpected stored snapshot: class=%s used=%v reset=%d error=%v", ac.LastQuotaClass, ac.LastUsedPct, ac.LastResetsAt, err)
		}
		for _, tool := range a.Overview().Tools {
			if tool.Tool != "zcode" {
				continue
			}
			if tool.Switchable || tool.Auto || tool.LiveID != id || len(tool.Accounts) != 1 {
				t.Fatal("overview did not identify a single current monitor-only subscription")
			}
			view := tool.Accounts[0]
			if view.Plan != "Max" || !view.Live || view.RemainingPct == nil || *view.RemainingPct != 99 || !reflect.DeepEqual(view.Buckets, wantBuckets) {
				t.Fatal("overview did not preserve the complete subscription snapshot")
			}
			return ac
		}
		t.Fatal("ZCode section absent")
		return ac
	}
	res := a.ProbeAccount(context.Background(), adapter.ZCode, id)
	if res.Class != quota.OK || res.UsedPct != 1 || calls != 1 {
		t.Fatalf("successful quota class=%s pct=%v calls=%d", res.Class, res.UsedPct, calls)
	}
	if ac := checkSnapshot(); ac.HTTPBackoffUntil != 0 {
		t.Fatal("successful subscription response retained backoff")
	}
	status = http.StatusServiceUnavailable
	res = a.ProbeAccount(context.Background(), adapter.ZCode, id)
	if res.Class != quota.Unknown || calls != 2 {
		t.Fatalf("failed quota class=%s calls=%d", res.Class, calls)
	}
	if ac := checkSnapshot(); ac.HTTPBackoffUntil <= now.Unix() {
		t.Fatal("failed subscription request did not back off")
	}
}

func TestZCodeSubscriptionBlocksManualAndAutomaticSwitches(t *testing.T) {
	a, _ := setup(t)
	before := writeZCodeSubscriptionFixture(t, a.UserHome)
	defer assertZCodeFilesUnchanged(t, before)
	const id = "bigmodel:zcode-fixture"
	if _, _, err := a.Capture(adapter.ZCode); err != nil {
		t.Fatal(err)
	}
	a.Cfg.General.AutoSwitch = true
	a.Cfg.ZCode.Enabled = true
	a.Host.QuitFn = func(string) error { t.Fatal("subscription monitoring tried to quit a desktop app"); return nil }
	a.Host.LaunchFn = func(string) error { t.Fatal("subscription monitoring tried to launch a desktop app"); return nil }
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("blocked switching must not probe another account")
		return nil, nil
	})}
	if err := a.State.UpdateQuotaSnapshot("zcode", id, "exhausted", 100, 0, a.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpsertAccount(state.Account{Tool: "zcode", StableID: "available", LastQuotaClass: "ok"}); err != nil {
		t.Fatal(err)
	}
	pointer, err := a.State.GetPointer("zcode")
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []adapter.RestoreOpts{{}, {ForceAlign: true, CLIOnly: true}} {
		if err := a.Switch(adapter.ZCode, "available", opts, true); err == nil || !strings.Contains(err.Error(), "subscription monitoring only") {
			t.Fatalf("public switch should explain its read-only boundary: %v", err)
		}
	}
	a.considerSwitch(adapter.ZCode)
	if pending, err := a.State.GetPending("zcode"); err != nil || pending.ToID != "" {
		t.Fatalf("automatic switching queued ZCode: pending=%v err=%v", pending.ToID != "", err)
	}
	for _, reason := range []string{"exhausted", "manual"} {
		if err := a.State.SetPending(state.Pending{Tool: "zcode", FromID: id, ToID: "available", Reason: reason, QueuedAt: a.now().Unix()}); err != nil {
			t.Fatal(err)
		}
		if err := a.TryApply(adapter.ZCode, true); err != nil {
			t.Fatal(err)
		}
		if pending, err := a.State.GetPending("zcode"); err != nil || pending.ToID != "" {
			t.Fatalf("unsupported pending switch not cleared: err=%v", err)
		}
	}
	if after, err := a.State.GetPointer("zcode"); err != nil || after != pointer {
		t.Fatal("blocked switch changed the current desktop subscription pointer")
	}
}
