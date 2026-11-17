package app

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
		if err := a.Switch(adapter.ZCode, "available", opts, true); err == nil || !strings.Contains(err.Error(), "仅支持订阅监控") {
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

// Build the desktop envelope independently from the adapter so this regression
// checks the wire format as well as application recovery. Every value is synthetic.
func encryptZCodeV2EnvFixture(t *testing.T, secret, name, value string) string {
	t.Helper()
	key := sha256.Sum256([]byte(secret))
	fingerprint := sha256.Sum256(key[:])
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonceDigest := sha256.Sum256([]byte("synthetic-zcode-recovery:" + name))
	nonce := nonceDigest[:12]
	sealed := gcm.Seal(nil, nonce, []byte(value), nil)
	data, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	b64 := base64.RawURLEncoding.EncodeToString
	return "enc:v2:env:" + hex.EncodeToString(fingerprint[:12]) + ":" + b64(nonce) + "." + b64(tag) + "." + b64(data)
}

func TestZCodeV2SecretRecoveryPreservesExistingSubscription(t *testing.T) {
	const secret = "synthetic-managed-zcode-secret"
	const wrongSecret = "synthetic-wrong-managed-secret"
	const id = "bigmodel:zcode-fixture"
	t.Setenv("ZCODE_CREDENTIAL_SECRET_FILE", "")
	t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
	a, _ := setup(t)
	now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("desktop capture recovery must not contact a provider")
		return nil, nil
	})}
	files := writeZCodeSubscriptionFixture(t, a.UserHome)
	credentialPath := filepath.Join(a.UserHome, ".zcode", "v2", "credentials.json")
	var store map[string]string
	if err := json.Unmarshal(files[credentialPath], &store); err != nil {
		t.Fatal(err)
	}
	for name, value := range store {
		store[name] = encryptZCodeV2EnvFixture(t, secret, name, value)
	}
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	files[credentialPath] = raw
	defer assertZCodeFilesUnchanged(t, files)
	if blobs, _, err := a.Capture(adapter.ZCode); err != nil || len(blobs) != 1 || blobs[0].Identity.StableID != id {
		t.Fatalf("initial v2 capture did not identify the synthetic account: count=%d err=%v", len(blobs), err)
	}
	if err := a.Ingest(adapter.ZCode); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := a.Adapters[adapter.ZCode].Fingerprint(a.UserHome)
	if err != nil || !a.sameLastFP(adapter.ZCode, fingerprint) {
		t.Fatal("initial ingest did not remember the desktop fingerprint")
	}
	if err := a.State.UpdateQuotaSnapshot("zcode", id, "ok", 37, now.Add(time.Hour).Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail("zcode", id, quota.EncodeBuckets([]quota.Bucket{{ID: "5h", UsedPct: 37, ResetsAt: now.Add(time.Hour).Unix()}})); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetPlanHint("zcode", id, "Max"); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota("zcode", id, "ok", "synthetic-recovery", 37, now); err != nil {
		t.Fatal(err)
	}
	account, err := a.State.GetAccount("zcode", id)
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := a.State.GetPointer("zcode")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := a.loadBlob(adapter.ZCode, id)
	if err != nil {
		t.Fatal(err)
	}
	vaultPath := filepath.Join(a.DataDir, "vault", "zcode", id+".enc")
	vaultBefore, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	checkPreserved := func(wantFailure bool) {
		t.Helper()
		accounts, err := a.State.ListAccounts("zcode")
		if err != nil || len(accounts) != 1 || accounts[0] != account {
			t.Fatalf("capture changed or duplicated the saved subscription: count=%d err=%v", len(accounts), err)
		}
		if current, err := a.State.GetPointer("zcode"); err != nil || current != pointer {
			t.Fatal("capture changed the current subscription pointer")
		}
		currentFP, err := a.Adapters[adapter.ZCode].Fingerprint(a.UserHome)
		if err != nil || currentFP != fingerprint || !a.sameLastFP(adapter.ZCode, fingerprint) {
			t.Fatal("recovery must use the exact same desktop credential fingerprint")
		}
		assertZCodeFilesUnchanged(t, files)
		for _, tool := range a.Overview().Tools {
			if tool.Tool != "zcode" {
				continue
			}
			if (tool.CaptureError != "") != wantFailure || tool.LiveID != id || len(tool.Accounts) != 1 || !tool.Accounts[0].Live || tool.Accounts[0].UsedPct != 37 {
				t.Fatal("overview lost the account/quota or misreported capture recovery")
			}
			if wantFailure {
				vaultNow, err := os.ReadFile(vaultPath)
				if err != nil || !bytes.Equal(vaultNow, vaultBefore) {
					t.Fatal("failed capture changed the stored vault credential")
				}
			}
			return
		}
		t.Fatal("saved ZCode subscription disappeared from overview")
	}
	t.Setenv("ZCODE_CREDENTIAL_SECRET", wrongSecret)
	if blobs, _, err := a.Capture(adapter.ZCode); err == nil || len(blobs) != 0 {
		t.Fatal("wrong managed secret must fail capture without returning credentials")
	}
	checkPreserved(true)
	if err := a.Ingest(adapter.ZCode); err == nil {
		t.Fatal("capture_error must force ingest to retry even with an unchanged fingerprint")
	}
	checkPreserved(true)
	for _, sensitive := range []string{secret, wrongSecret, "fixture-desktop-plan-key", "enc:v2:"} {
		if strings.Contains(a.captureStatus(adapter.ZCode), sensitive) {
			t.Fatal("capture error exposed a secret or encrypted source value")
		}
	}
	t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
	if err := a.Ingest(adapter.ZCode); err != nil {
		t.Fatalf("correcting the managed secret did not recover unchanged desktop credentials: %v", err)
	}
	checkPreserved(false)
	recovered, err := a.loadBlob(adapter.ZCode, id)
	if err != nil || !bytes.Equal(recovered.Payload, blob.Payload) {
		t.Fatal("recovery selected a different credential or account")
	}
}

type zcodeCaptureFailureAdapter struct {
	adapter.Adapter
	captureErr error
	calls      int
}

func (a *zcodeCaptureFailureAdapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	a.calls++
	if a.captureErr != nil {
		return nil, nil, a.captureErr
	}
	return a.Adapter.Capture(home)
}

func TestZCodeKeyringCaptureBackoffAndRecovery(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{{"unavailable", zcode.ErrOSCredential}, {"authorization pending", zcode.ErrOSCredentialPending}} {
		for _, recovery := range []string{"manual during backoff", "background after backoff"} {
			t.Run(failure.name+"/"+recovery, func(t *testing.T) {
				a, _ := setup(t)
				now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
				a.Clock = clock.Fixed{T: now}
				files := writeZCodeSubscriptionFixture(t, a.UserHome)
				defer assertZCodeFilesUnchanged(t, files)
				if err := a.Ingest(adapter.ZCode); err != nil {
					t.Fatal(err)
				}
				pointer, err := a.State.GetPointer("zcode")
				if err != nil || pointer.StableID != "bigmodel:zcode-fixture" {
					t.Fatal("initial ingest did not select the synthetic subscription")
				}
				fp, err := a.Adapters[adapter.ZCode].Fingerprint(a.UserHome)
				if err != nil || !a.sameLastFP(adapter.ZCode, fp) {
					t.Fatal("initial ingest did not remember the desktop fingerprint")
				}
				injected := &zcodeCaptureFailureAdapter{Adapter: a.Adapters[adapter.ZCode], captureErr: fmt.Errorf("synthetic read: %w", failure.err)}
				a.Adapters[adapter.ZCode] = injected
				if _, _, err := a.Capture(adapter.ZCode); !errors.Is(err, failure.err) || injected.calls != 1 {
					t.Fatalf("explicit capture did not expose the simulated keyring failure: %v", err)
				}
				if err := a.Ingest(adapter.ZCode); !errors.Is(err, failure.err) || injected.calls != 2 {
					t.Fatalf("background capture did not retry the unchanged failed fingerprint: %v", err)
				}
				captureError := a.captureStatus(adapter.ZCode)
				if captureError == "" {
					t.Fatal("keyring failure was not retained for the dashboard")
				}
				// The simulated keyring recovers immediately. Background ticks must
				// still respect the one-minute delay and retain the last error.
				injected.captureErr = nil
				for _, elapsed := range []time.Duration{0, 59 * time.Second, time.Minute - time.Nanosecond} {
					a.Clock = clock.Fixed{T: now.Add(elapsed)}
					if err := a.Ingest(adapter.ZCode); err != nil || injected.calls != 2 || a.captureStatus(adapter.ZCode) != captureError {
						t.Fatalf("background retry escaped backoff or cleared the error at %v: calls=%d err=%v", elapsed, injected.calls, err)
					}
				}
				if recovery == "manual during backoff" {
					if blobs, _, err := a.Capture(adapter.ZCode); err != nil || len(blobs) != 1 || injected.calls != 3 {
						t.Fatalf("explicit capture was blocked by background backoff: calls=%d err=%v", injected.calls, err)
					}
				} else {
					a.Clock = clock.Fixed{T: now.Add(time.Minute)}
					if err := a.Ingest(adapter.ZCode); err != nil || injected.calls != 3 {
						t.Fatalf("background did not retry at the one-minute boundary: calls=%d err=%v", injected.calls, err)
					}
				}
				if a.captureStatus(adapter.ZCode) != "" {
					t.Fatal("successful recovery left capture_error visible")
				}
				if accounts, err := a.State.ListAccounts("zcode"); err != nil || len(accounts) != 1 || accounts[0].StableID != pointer.StableID {
					t.Fatalf("recovery duplicated or changed the saved subscription: count=%d err=%v", len(accounts), err)
				}
				if current, err := a.State.GetPointer("zcode"); err != nil || current != pointer {
					t.Fatal("recovery changed the current subscription pointer")
				}
				if currentFP, err := injected.Fingerprint(a.UserHome); err != nil || currentFP != fp || !a.sameLastFP(adapter.ZCode, fp) {
					t.Fatal("recovery did not preserve the exact same desktop fingerprint")
				}
				if err := a.Ingest(adapter.ZCode); err != nil || injected.calls != 3 {
					t.Fatal("a recovered unchanged credential was captured repeatedly")
				}
			})
		}
	}
}

func TestZCodeOtherCaptureErrorsRemainImmediatelyRetryable(t *testing.T) {
	for _, failure := range []error{zcode.ErrCredentialKeyMismatch, zcode.ErrCredentialSource, zcode.ErrOSCredentialFormat, errors.New("synthetic capture error")} {
		t.Run(failure.Error(), func(t *testing.T) {
			a, _ := setup(t)
			a.Clock = clock.Fixed{T: time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)}
			files := writeZCodeSubscriptionFixture(t, a.UserHome)
			defer assertZCodeFilesUnchanged(t, files)
			injected := &zcodeCaptureFailureAdapter{Adapter: a.Adapters[adapter.ZCode], captureErr: failure}
			a.Adapters[adapter.ZCode] = injected
			if err := a.Ingest(adapter.ZCode); !errors.Is(err, failure) || injected.calls != 1 || a.captureStatus(adapter.ZCode) == "" {
				t.Fatalf("initial failure was not retained: calls=%d err=%v", injected.calls, err)
			}
			injected.captureErr = nil
			if err := a.Ingest(adapter.ZCode); err != nil || injected.calls != 2 || a.captureStatus(adapter.ZCode) != "" {
				t.Fatalf("non-keyring failure delayed a corrected credential: calls=%d err=%v", injected.calls, err)
			}
		})
	}
}
