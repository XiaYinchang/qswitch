package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
	"qswitch/internal/notify"
	"qswitch/internal/secutil"
	"qswitch/internal/state"
)

func setup(t *testing.T) (*App, *notify.Log) {
	t.Helper()
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	data := filepath.Join(home, ".qswitch")
	n := &notify.Log{}
	a, err := Open(home, data, &secutil.Memory{}, nil, n, func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	writeCodex(t, home, "acc-a", "a@x.com")
	return a, n
}

func writeCodex(t *testing.T, home, id, email string) {
	t.Helper()
	auth := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"account_id": id, "access_token": "t-" + id, "id_token": jwt(email), "refresh_token": "r",
	}}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func jwt(email string) string {
	h := "eyJhbGciOiJub25lIn0"
	payload := []byte(`{"email":"` + email + `"}`)
	return h + "." + b64u(payload) + ".x"
}

func b64u(raw []byte) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, (len(raw)+2)/3*4)
	for i := 0; i < len(raw); i += 3 {
		var n uint32
		remain := len(raw) - i
		switch {
		case remain >= 3:
			n = uint32(raw[i])<<16 | uint32(raw[i+1])<<8 | uint32(raw[i+2])
			out = append(out, tbl[n>>18], tbl[(n>>12)&63], tbl[(n>>6)&63], tbl[n&63])
		case remain == 2:
			n = uint32(raw[i])<<16 | uint32(raw[i+1])<<8
			out = append(out, tbl[n>>18], tbl[(n>>12)&63], tbl[(n>>6)&63])
		default:
			n = uint32(raw[i]) << 16
			out = append(out, tbl[n>>18], tbl[(n>>12)&63])
		}
	}
	return string(out)
}

func TestCaptureSwitch(t *testing.T) {
	a, _ := setup(t)
	blobs, _, err := a.Capture(adapter.Codex)
	if err != nil || len(blobs) != 1 {
		t.Fatalf("%v %#v", err, blobs)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	if err := a.Switch(adapter.Codex, "acc-a", adapter.RestoreOpts{}, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json"))
	if !jsonContains(raw, "acc-a") {
		t.Fatalf("live not acc-a: %s", raw)
	}
}

func TestBusySwitchNoWrite(t *testing.T) {
	a, _ := setup(t)
	a.Capture(adapter.Codex)
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	a.Capture(adapter.Codex)
	a.Adapters[adapter.Codex] = a.Adapters[adapter.Codex] // keep
	// replace list via new open
	n := &notify.Log{}
	busyApp, err := Open(a.UserHome, a.DataDir, a.Vault.Keys, nil, n, func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 7, Command: "/opt/homebrew/bin/codex"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer busyApp.Close()
	before, _ := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json"))
	err = busyApp.Switch(adapter.Codex, "acc-a", adapter.RestoreOpts{}, false)
	if _, ok := err.(*adapter.BusyError); !ok {
		t.Fatalf("want busy, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json"))
	if string(before) != string(after) {
		t.Fatal("file changed under busy")
	}
}

func TestWritebackRepeatDisablesAuto(t *testing.T) {
	a, n := setup(t)
	a.Capture(adapter.Codex)
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	a.Capture(adapter.Codex)
	if err := a.Switch(adapter.Codex, "acc-a", adapter.RestoreOpts{}, false); err != nil {
		t.Fatal(err)
	}
	a.Clock = clock.Fixed{T: time.Now()}
	// simulate old process writing acc-b back
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if err := a.Ingest(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if err := a.Ingest(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	if a.Cfg.Codex.Enabled {
		t.Fatalf("auto should disable after writeback_repeat; notes=%v", n.Msgs)
	}
}

func TestIngestNewAccount(t *testing.T) {
	a, n := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if err := a.Ingest(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	p, err := a.State.GetPointer("codex")
	if err != nil || p.StableID != "acc-b" {
		t.Fatalf("pointer=%+v err=%v", p, err)
	}
	if _, err := a.State.GetAccount("codex", "acc-b"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range n.Msgs {
		if strings.Contains(m, "收录新账号") && strings.Contains(m, "b@x.com") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want 收录 notify, got %v", n.Msgs)
	}
	if err := a.Ingest(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	nNew := 0
	for _, m := range n.Msgs {
		if strings.Contains(m, "收录新账号") {
			nNew++
		}
	}
	if nNew != 1 {
		t.Fatalf("duplicate ingest notify: %v", n.Msgs)
	}
}

func TestProbeLocalCodexUsageLimitQueuesSwitch(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpdateQuotaSnapshot("codex", "acc-a", "ok", 10, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(a.UserHome, ".codex", "sessions")
	if err := os.MkdirAll(sess, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "rollout.jsonl"), []byte(`{"error":{"code":"usage_limit_reached"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ProbeLocal(adapter.Codex)
	p, err := a.State.GetPointer("codex")
	if err != nil || p.StableID != "acc-a" {
		pend, _ := a.State.GetPending("codex")
		t.Fatalf("pointer=%+v pending=%+v err=%v", p, pend, err)
	}
}

func TestSwitchAmbiguousEmail(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "ws-personal", "same@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "ws-team", "same@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	err := a.Switch(adapter.Codex, "same@x.com", adapter.RestoreOpts{}, false)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want ambiguous email, got %v", err)
	}
	if err := a.Switch(adapter.Codex, "ws-personal", adapter.RestoreOpts{}, false); err != nil {
		t.Fatal(err)
	}
	p, _ := a.State.GetPointer("codex")
	if p.StableID != "ws-personal" {
		t.Fatalf("pointer %s", p.StableID)
	}
}

func TestCaptureFollowsLiveIdentity(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	p, err := a.State.GetPointer("codex")
	if err != nil || p.StableID != "acc-b" {
		t.Fatalf("pointer=%+v err=%v", p, err)
	}
}

func TestPickNextProbesUnprobedAccount(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"rate_limit":{"primary_window":{"used_percent":12}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	if err := a.State.UpdateQuotaSnapshot("codex", "acc-b", "exhausted", 100, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	a.considerSwitch(adapter.Codex)
	p, err := a.State.GetPointer("codex")
	if err != nil || p.StableID != "acc-a" {
		t.Fatalf("want acc-a got %+v err=%v", p, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProbeRecoveredAfterCooling(t *testing.T) {
	a, n := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Clock = clock.Fixed{T: now}
	if err := a.State.UpdateQuotaSnapshot("codex", "acc-a", "exhausted", 100, now.Add(-time.Hour).Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetCooling("codex", "acc-a", now.Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"rate_limit":{"primary_window":{"used_percent":8}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	a.ProbeRecovered(context.Background(), adapter.Codex, false)
	ac, err := a.State.GetAccount("codex", "acc-a")
	if err != nil || ac.LastQuotaClass != "ok" {
		t.Fatalf("recovered class=%s err=%v", ac.LastQuotaClass, err)
	}
	if ac.CoolingUntil != 0 {
		t.Fatalf("cooling %d", ac.CoolingUntil)
	}
	found := false
	for _, m := range n.Msgs {
		if strings.Contains(m, "quota recovered") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want recovered notify, got %v", n.Msgs)
	}
}

func TestOverviewHidesGrokDesktopSnapshot(t *testing.T) {
	a, _ := setup(t)
	if err := a.State.UpsertAccount(state.Account{
		Tool: "grok", StableID: "desktop:grokbot", Email: "grok-bot.app", PlanHint: "desktop",
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpsertAccount(state.Account{
		Tool: "grok", StableID: "pid-g", Email: "g@x.com",
	}); err != nil {
		t.Fatal(err)
	}
	ov := a.Overview()
	for _, tool := range ov.Tools {
		if tool.Tool != "grok" {
			continue
		}
		for _, ac := range tool.Accounts {
			if strings.HasPrefix(ac.StableID, "desktop:") {
				t.Fatalf("desktop snapshot still listed: %+v", ac)
			}
		}
	}
}

func TestOverviewSplitsGrokBot(t *testing.T) {
	a, _ := setup(t)
	if err := a.State.UpsertAccount(state.Account{
		Tool: "cursor", StableID: "u1", Email: "c@x.com", PlanHint: "ultra",
		LastQuotaClass: "soft", LastUsedPct: 97.9,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail("cursor", "u1", `[{"id":"auto","used_pct":50},{"id":"api","used_pct":97.9},{"id":"bot","used_pct":12,"resets_at":1789309692}]`); err != nil {
		t.Fatal(err)
	}
	ov := a.Overview()
	var cursor, bot *ToolView
	for i := range ov.Tools {
		switch ov.Tools[i].Tool {
		case "cursor":
			cursor = &ov.Tools[i]
		case "grokbot":
			bot = &ov.Tools[i]
		}
	}
	if cursor == nil || len(cursor.Accounts) != 1 {
		t.Fatalf("cursor %+v", cursor)
	}
	for _, b := range cursor.Accounts[0].Buckets {
		if b.ID == "bot" {
			t.Fatal("bot bucket still on cursor card")
		}
	}
	if bot == nil || len(bot.Accounts) != 1 {
		t.Fatalf("bot %+v", bot)
	}
	ac := bot.Accounts[0]
	if !ac.Derived || ac.UsedPct != 12 || ac.Class != "ok" || ac.Plan != "Grok Bot" || ac.ResetsAt != 1789309692 {
		t.Fatalf("bot account %+v", ac)
	}
	if ac.Tool != "cursor" || ac.StableID != "u1" {
		t.Fatalf("probe target %+v", ac)
	}
}

func TestProbeOKClearsCooling(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Clock = clock.Fixed{T: now}
	if err := a.State.SetCooling("codex", "acc-a", now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"rate_limit":{"primary_window":{"used_percent":50}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	res := a.ProbeAccount(context.Background(), adapter.Codex, "acc-a")
	if res.Class != "ok" {
		t.Fatalf("class %s", res.Class)
	}
	ac, err := a.State.GetAccount("codex", "acc-a")
	if err != nil || ac.CoolingUntil != 0 {
		t.Fatalf("cooling %d err=%v", ac.CoolingUntil, err)
	}
}

func TestOverviewHidesStaleCooling(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Clock = clock.Fixed{T: now}
	past := now.Add(-3 * 24 * time.Hour).Unix()
	if err := a.State.UpdateQuotaSnapshot("codex", "acc-a", "ok", 50, now.Add(24*time.Hour).Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetCooling("codex", "acc-a", past); err != nil {
		t.Fatal(err)
	}
	ov := a.Overview()
	if got := coolingOf(ov, "acc-a"); got != 0 {
		t.Fatalf("ok account still showing cooling %d", got)
	}
	wantReset := now.Add(24 * time.Hour).Unix()
	if got := resetOf(ov, "acc-a"); got != wantReset {
		t.Fatalf("want resets %d got %d", wantReset, got)
	}
	until := now.Add(3 * time.Hour).Unix()
	if err := a.State.UpdateQuotaSnapshot("codex", "acc-a", "exhausted", 100, until, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetCooling("codex", "acc-a", until); err != nil {
		t.Fatal(err)
	}
	if got := coolingOf(a.Overview(), "acc-a"); got != until {
		t.Fatalf("want cooling %d got %d", until, got)
	}
}

func coolingOf(ov Overview, id string) int64 {
	for _, tool := range ov.Tools {
		for _, ac := range tool.Accounts {
			if ac.StableID == id {
				return ac.CoolingUntil
			}
		}
	}
	return -1
}

func resetOf(ov Overview, id string) int64 {
	for _, tool := range ov.Tools {
		for _, ac := range tool.Accounts {
			if ac.StableID == id {
				return ac.ResetsAt
			}
		}
	}
	return -1
}

func TestProbeRecoveredCodexBeforeAdvertisedReset(t *testing.T) {
	a, n := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Clock = clock.Fixed{T: now}
	if err := a.State.UpdateQuotaSnapshot("codex", "acc-a", "exhausted", 100, now.Add(3*time.Hour).Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetCooling("codex", "acc-a", now.Add(3*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"rate_limit":{"primary_window":{"used_percent":4}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	a.ProbeRecovered(context.Background(), adapter.Codex, false)
	ac, err := a.State.GetAccount("codex", "acc-a")
	if err != nil || ac.LastQuotaClass != "ok" {
		t.Fatalf("codex should recheck before advertised reset, class=%s err=%v", ac.LastQuotaClass, err)
	}
	found := false
	for _, m := range n.Msgs {
		if strings.Contains(m, "quota recovered") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want recovered notify, got %v", n.Msgs)
	}
}

func TestProbeRecoveredCodexNotTooSoon(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	writeCodex(t, a.UserHome, "acc-b", "b@x.com")
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Clock = clock.Fixed{T: now}
	if err := a.State.UpdateQuota("codex", "acc-a", "exhausted", 100, now.Add(3*time.Hour).Unix(), now.Unix(), now.Add(-5*time.Minute).Unix(), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetCooling("codex", "acc-a", now.Add(3*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	hit := false
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hit = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":1}}}`)), Header: make(http.Header), Request: req}, nil
	})}
	a.ProbeRecovered(context.Background(), adapter.Codex, false)
	if hit {
		t.Fatal("codex recover must wait ~30m between probes")
	}
}

func TestProbeRecoveredGrokWaitsReset(t *testing.T) {
	a, _ := setup(t)
	now := time.Now()
	a.Clock = clock.Fixed{T: now}
	if err := a.State.UpsertAccount(state.Account{
		Tool: "grok", StableID: "g1", Email: "g@x.com",
		LastQuotaClass: "exhausted", LastUsedPct: 100,
		LastResetsAt: now.Add(time.Hour).Unix(), CoolingUntil: now.Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	hit := false
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hit = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"config":{"creditUsagePercent":1}}`)), Header: make(http.Header), Request: req}, nil
	})}
	a.ProbeRecovered(context.Background(), adapter.Grok, false)
	if hit {
		t.Fatal("grok must wait for advertised reset")
	}
}

func TestProbeLocalGrokIgnoresTUISessionDump(t *testing.T) {
	a, _ := setup(t)
	writeGrokLive(t, a.UserHome, "pid-g", "g@x.com")
	if _, _, err := a.Capture(adapter.Grok); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.UserHome, ".grok", "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	billing := `{"msg":"billing: fetched credits config","ctx":{"config":{"creditUsagePercent":7.0,"currentPeriod":{"end":"2026-09-13T14:04:34Z"}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(a.UserHome, ".grok", "logs", "unified.jsonl"), []byte(billing), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(a.UserHome, ".grok", "sessions", "workspace", "updates.jsonl")
	if err := os.MkdirAll(filepath.Dir(sess), 0o700); err != nil {
		t.Fatal(err)
	}
	dump := `{"method":"session/update","params":{"update":{"content":[{"text":"usage balance exhausted status 402 payment required"}]}}}` + "\n"
	if err := os.WriteFile(sess, []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ProbeLocal(adapter.Grok)
	ac, err := a.State.GetAccount("grok", "pid-g")
	if err != nil || ac.LastQuotaClass != "ok" || ac.LastUsedPct != 7 {
		t.Fatalf("class=%s pct=%v err=%v", ac.LastQuotaClass, ac.LastUsedPct, err)
	}
}

func TestProbeLocalGrokDoesNotOverwriteHTTPWithRequest402(t *testing.T) {
	a, _ := setup(t)
	writeGrokLive(t, a.UserHome, "pid-g", "g@x.com")
	if _, _, err := a.Capture(adapter.Grok); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Clock = clock.Fixed{T: now}
	if err := a.State.UpdateQuota("grok", "pid-g", "ok", 6, now.Add(24*time.Hour).Unix(), now.Unix(), now.Unix(), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.UserHome, ".grok", "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	fail := `{"msg":"shell.turn.inference_failed","ctx":{"message":"API error (status 402 Payment Required): Grok Build usage balance exhausted"}}` + "\n"
	if err := os.WriteFile(filepath.Join(a.UserHome, ".grok", "logs", "unified.jsonl"), []byte(fail), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ProbeLocal(adapter.Grok)
	ac, err := a.State.GetAccount("grok", "pid-g")
	if err != nil || ac.LastQuotaClass != "ok" || ac.LastUsedPct != 6 {
		t.Fatalf("class=%s pct=%v err=%v", ac.LastQuotaClass, ac.LastUsedPct, err)
	}
}

func writeGrokLive(t *testing.T, home, id, email string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	auth := []byte(`{"https://auth.x.ai::abc":{"principal_id":"` + id + `","email":"` + email + `","key":"k","refresh_token":"rt"}}`)
	if err := os.WriteFile(filepath.Join(home, ".grok", "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProbeLocalIgnoresTPM429(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(a.UserHome, ".codex", "sessions")
	if err := os.MkdirAll(sess, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "rollout.jsonl"), []byte(`{"error":{"code":"rate_limit_exceeded","status":429}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ProbeLocal(adapter.Codex)
	pend, _ := a.State.GetPending("codex")
	if pend.ToID != "" {
		t.Fatalf("tpm 429 should not queue switch: %+v", pend)
	}
}

func TestInitWritesStandardPlist(t *testing.T) {
	a, _ := setup(t)
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(a.DataDir, "com.qswitch.plist"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "com.qswitch") || !strings.Contains(s, "QSWITCH_DIR") {
		t.Fatalf("plist %s", s)
	}
	if strings.Contains(s, "xiayinchang") || strings.Contains(s, "/Users/bytedance") {
		t.Fatalf("personal path leaked: %s", s)
	}
}

func TestIsolatedCodexEnvDropsParentHome(t *testing.T) {
	got := isolatedCodexEnv([]string{"PATH=/bin", "CODEX_HOME=/old", "FOO=bar"}, "/tmp/new")
	joined := strings.Join(got, ",")
	if strings.Contains(joined, "CODEX_HOME=/old") || !strings.Contains(joined, "CODEX_HOME=/tmp/new") {
		t.Fatalf("%v", got)
	}
}

func TestIsolatedGrokEnvDropsParentHome(t *testing.T) {
	got := isolatedGrokEnv([]string{"PATH=/bin", "GROK_HOME=/old", "FOO=bar"}, "/tmp/new")
	joined := strings.Join(got, ",")
	if strings.Contains(joined, "GROK_HOME=/old") || !strings.Contains(joined, "GROK_HOME=/tmp/new") {
		t.Fatalf("%v", got)
	}
}

func TestLoginCodexEnrollsWithoutTouchingLive(t *testing.T) {
	a, n := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(a.UserHome, ".codex", "auth.json")
	before, _ := os.ReadFile(live)
	idTok := jwtWS("b@x.com", "u-b", "ws-b", "plus")
	polls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "deviceauth/usercode"):
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"device_auth_id":"did","user_code":"ABCD","interval":1}`)), Header: make(http.Header), Request: req}, nil
		case strings.Contains(req.URL.Path, "deviceauth/token"):
			polls++
			if polls < 2 {
				return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: req}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"authorization_code":"ac","code_verifier":"cv"}`)), Header: make(http.Header), Request: req}, nil
		case req.URL.Path == "/oauth/token":
			body := `{"access_token":"` + idTok + `","id_token":"` + idTok + `","refresh_token":"rt-b"}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		default:
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: req}, nil
		}
	})}
	id, err := a.LoginCodex(context.Background(), func(string, string) {})
	if err != nil || id.StableID != "ws-b" || id.Email != "b@x.com" {
		t.Fatalf("%+v %v", id, err)
	}
	after, _ := os.ReadFile(live)
	if !bytes.Equal(before, after) {
		t.Fatal("login mutated live auth.json")
	}
	p, _ := a.State.GetPointer("codex")
	if p.StableID != "acc-a" {
		t.Fatalf("pointer moved %s", p.StableID)
	}
	if _, err := a.State.GetAccount("codex", "ws-b"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range n.Msgs {
		if strings.Contains(m, "收录新账号") && strings.Contains(m, "b@x.com") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notify %v", n.Msgs)
	}
}

func TestLoginGrokEnrollsWithoutTouchingLive(t *testing.T) {
	a, n := setup(t)
	os.MkdirAll(filepath.Join(a.UserHome, ".grok"), 0o700)
	liveAuth := []byte(`{"https://auth.x.ai::abc":{"principal_id":"pid-live","email":"live@x.com","key":"k","refresh_token":"rt"}}`)
	live := filepath.Join(a.UserHome, ".grok", "auth.json")
	if err := os.WriteFile(live, liveAuth, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Capture(adapter.Grok); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(live)
	idTok := grokJWT("new@x.com", "pid-new")
	polls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/oauth2/device/code":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"device_code":"dc","user_code":"AB","verification_uri":"https://accounts.x.ai/oauth2/device","expires_in":900,"interval":1}`)), Header: make(http.Header), Request: req}, nil
		case "/oauth2/token":
			polls++
			if polls < 2 {
				return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"authorization_pending"}`)), Header: make(http.Header), Request: req}, nil
			}
			body := `{"access_token":"` + idTok + `","refresh_token":"rt-new","expires_in":3600}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		case "/oauth2/userinfo":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"email":"new@x.com"}`)), Header: make(http.Header), Request: req}, nil
		default:
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: req}, nil
		}
	})}
	id, err := a.LoginGrok(context.Background(), func(string, string) {})
	if err != nil || id.StableID != "pid-new" || id.Email != "new@x.com" {
		t.Fatalf("%+v %v", id, err)
	}
	after, _ := os.ReadFile(live)
	if !bytes.Equal(before, after) {
		t.Fatal("login mutated live auth.json")
	}
	p, _ := a.State.GetPointer("grok")
	if p.StableID != "pid-live" {
		t.Fatalf("pointer moved %s", p.StableID)
	}
	if _, err := a.State.GetAccount("grok", "pid-new"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range n.Msgs {
		if strings.Contains(m, "收录新账号") && strings.Contains(m, "new@x.com") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notify %v", n.Msgs)
	}
}

func grokJWT(email, pid string) string {
	h := "eyJhbGciOiJub25lIn0"
	payload := []byte(`{"principal_id":"` + pid + `","sub":"` + pid + `","email":"` + email + `","client_id":"abc"}`)
	return h + "." + b64u(payload) + ".x"
}

func jwtWS(email, user, account, plan string) string {
	h := "eyJhbGciOiJub25lIn0"
	payload := []byte(`{"email":"` + email + `","https://api.openai.com/auth":{"chatgpt_account_id":"` + account + `","chatgpt_plan_type":"` + plan + `","chatgpt_user_id":"` + user + `"}}`)
	return h + "." + b64u(payload) + ".x"
}

func jsonContains(b []byte, s string) bool {
	return len(b) > 0 && string(b) != "" && (string(b) == s || len(s) > 0 && (string(b) != "" && contains(string(b), s)))
}

func contains(a, b string) bool {
	return len(a) >= len(b) && (a == b || len(b) == 0 || (len(a) > 0 && indexOf(a, b) >= 0))
}

func indexOf(a, b string) int {
	for i := 0; i+len(b) <= len(a); i++ {
		if a[i:i+len(b)] == b {
			return i
		}
	}
	return -1
}
