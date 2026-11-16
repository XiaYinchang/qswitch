package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/livefile"
)

type adapterHarness struct {
	home    string
	backend *memoryCredentialBackend
	procs   []adapter.Proc
	adapter *Adapter
}

func newAdapterHarness(t *testing.T) *adapterHarness {
	t.Helper()
	h := &adapterHarness{home: t.TempDir(), backend: &memoryCredentialBackend{raw: syntheticCredential("old")}}
	h.adapter = &Adapter{List: func() ([]adapter.Proc, error) { return append([]adapter.Proc(nil), h.procs...), nil }}
	h.adapter.deps = &adapterDependencies{
		read: func(home string) ([]Candidate, error) { return readCredentials(home, h.backend) },
		validate: func(_ context.Context, raw []byte) ([]byte, adapter.Identity, error) {
			c, err := ParseCredential(raw)
			if err != nil {
				return nil, adapter.Identity{}, err
			}
			if c.IDTokenEmail == "invalid@example.invalid" {
				return nil, adapter.Identity{}, errors.New("synthetic invalid login")
			}
			return raw, adapter.Identity{Tool: adapter.Antigravity, StableID: "subject:" + c.IDTokenEmail, Email: c.IDTokenEmail}, nil
		},
		write: func(home string, raw []byte) (func() error, error) {
			var undo func() error
			err := writeCredentialTransaction(home, raw, h.backend, livefile.AtomicWrite, &undo)
			return undo, err
		},
	}
	return h
}

func (h *adapterHarness) blob(t *testing.T, label string) adapter.Blob {
	t.Helper()
	raw, id, err := h.adapter.deps.validate(context.Background(), syntheticCredential(label))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBlob(raw, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConsumerSnapshotsRejectUnsupportedAndMissingAuthentication(t *testing.T) {
	for _, method := range []string{"gcp", "api_key", "adc", ""} {
		t.Run("method="+method, func(t *testing.T) {
			h := newAdapterHarness(t)
			blob := h.blob(t, "target")
			var credential map[string]any
			if err := json.Unmarshal(syntheticCredential("target"), &credential); err != nil {
				t.Fatal(err)
			}
			if method == "" {
				delete(credential, "auth_method")
			} else {
				credential["auth_method"] = method
			}
			raw, err := json.Marshal(credential)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewBlob(raw, blob.Identity); err == nil {
				t.Fatal("non-consumer credential entered the account vault")
			}
			// A preexisting or externally constructed payload must be rejected
			// before quota refresh or native-account switching can use it.
			blob.Payload, err = json.Marshal(payload{Kind: "antigravity.oauth.v1", Identity: blob.Identity, Credentials: raw})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CredentialsFromBlob(blob); err == nil {
				t.Fatal("non-consumer snapshot exposed credentials to a caller")
			}
			if _, err := h.adapter.IdentityOf(blob); err == nil {
				t.Fatal("non-consumer snapshot exposed a rotatable identity")
			}
			h.adapter.deps.validate = func(context.Context, []byte) ([]byte, adapter.Identity, error) {
				t.Fatal("unsupported snapshot reached online identity validation")
				return nil, adapter.Identity{}, nil
			}
			if _, err := h.adapter.RestoreWithRollback(h.home, blob, adapter.RestoreOpts{}); err == nil {
				t.Fatal("non-consumer snapshot was restored")
			}
			if h.backend.writes != 0 {
				t.Fatal("unsupported authentication changed native credentials")
			}
		})
	}
}

func TestNewBlobRequiresMatchingConsumerIdentityAndPreservesRawCredential(t *testing.T) {
	raw := syntheticCredential("target")
	id := adapter.Identity{Tool: adapter.Antigravity, StableID: "synthetic-subject", Email: "TARGET@example.invalid"}
	blob, err := NewBlob(raw, id)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := CredentialsFromBlob(blob)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatal("consumer snapshot did not preserve native metadata")
	}
	id.Email = "other@example.invalid"
	if _, err := NewBlob(raw, id); err == nil {
		t.Fatal("credential and snapshot identities differ")
	}
}

type adapterNativeClient struct {
	mu       sync.Mutex
	email    string
	activity string
	methods  []string
}

func (h *adapterHarness) nativeClient(t *testing.T, pid int, email, activity string) *adapterNativeClient {
	t.Helper()
	c := &adapterNativeClient{email: email, activity: activity}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Codeium-Csrf-Token") != "synthetic-csrf" {
			http.Error(w, "missing CSRF", http.StatusForbidden)
			return
		}
		method := strings.TrimPrefix(req.URL.Path, "/exa.language_server_pb.LanguageServerService/")
		c.mu.Lock()
		defer c.mu.Unlock()
		c.methods = append(c.methods, method)
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "GetUserStatus":
			_ = json.NewEncoder(w).Encode(map[string]any{"userStatus": map[string]string{"email": c.email}})
		case "GetAllCascadeTrajectories":
			fmt.Fprint(w, c.activity)
		case "GetAuthStatus":
			raw, err := h.backend.Read()
			if err != nil {
				http.Error(w, "synthetic credential failure", 500)
				return
			}
			cred, err := ParseCredential(raw)
			if err != nil {
				http.Error(w, "synthetic credential failure", 500)
				return
			}
			c.email = cred.IDTokenEmail
			fmt.Fprint(w, `{}`)
		case "RetrieveUserQuotaSummary":
			fmt.Fprint(w, `{}`)
		default:
			http.Error(w, "unexpected mutation", 400)
		}
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	dir := filepath.Join(h.home, ".gemini", "antigravity", "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(map[string]any{"pid": pid, "httpsPort": port, "csrfToken": "synthetic-csrf"})
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("native_%d.json", pid)), record, 0o600); err != nil {
		t.Fatal(err)
	}
	h.procs = append(h.procs, adapter.Proc{PID: pid, Command: "/synthetic/Antigravity/language_server"})
	return c
}

func (c *adapterNativeClient) activeEmail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.email
}

const syntheticIdleActivity = `{"trajectorySummaries":{"synthetic-conversation":{"status":"CASCADE_RUN_STATUS_IDLE"}}}`

func TestAdapterCaptureValidatesDeduplicatesAndPrefersNativeIdentity(t *testing.T) {
	h := newAdapterHarness(t)
	paths := CredentialPaths(h.home)
	// Different storage bytes for one Google subject must not create two accounts.
	duplicate := bytes.Replace(syntheticCredential("old"), []byte(`"native-old"`), []byte(`"native-another-writer"`), 1)
	putSyntheticCredential(t, paths[0], duplicate)
	putSyntheticCredential(t, paths[1], syntheticCredential("active"))
	putSyntheticCredential(t, paths[2], syntheticCredential("invalid"))
	h.nativeClient(t, 101, "ACTIVE@example.invalid", syntheticIdleActivity)
	blobs, warnings, err := h.adapter.Capture(h.home)
	if err != nil || len(blobs) != 2 || len(warnings) != 1 {
		t.Fatalf("capture count=%d warnings=%d err=%v", len(blobs), len(warnings), err)
	}
	if blobs[0].Identity.StableID != "subject:active@example.invalid" || blobs[1].Identity.StableID != "subject:old@example.invalid" {
		t.Fatal("stale keyring or duplicate fallback displaced verified native identity")
	}
	if h.backend.writes != 0 {
		t.Fatal("capture changed native credentials")
	}
}

func TestAdapterCaptureDoesNotInventLiveAccountFromFallbacks(t *testing.T) {
	t.Run("ambiguous fallbacks", func(t *testing.T) {
		h := newAdapterHarness(t)
		h.backend.raw = nil
		paths := CredentialPaths(h.home)
		putSyntheticCredential(t, paths[0], syntheticCredential("first"))
		putSyntheticCredential(t, paths[1], syntheticCredential("second"))
		if blobs, _, err := h.adapter.Capture(h.home); err == nil || len(blobs) != 0 {
			t.Fatal("ambiguous fallback became live")
		}
	})
	t.Run("failed keyring", func(t *testing.T) {
		h := newAdapterHarness(t)
		h.backend.raw = syntheticCredential("invalid")
		putSyntheticCredential(t, CredentialPaths(h.home)[0], syntheticCredential("fallback"))
		if blobs, _, err := h.adapter.Capture(h.home); err == nil || len(blobs) != 0 {
			t.Fatal("failed primary credential was silently replaced by fallback")
		}
	})
	t.Run("verified primary", func(t *testing.T) {
		h := newAdapterHarness(t)
		h.adapter.deps.read = func(string) ([]Candidate, error) {
			return []Candidate{{Source: "fallback", Raw: syntheticCredential("fallback")}, {Source: "keyring:gemini/antigravity", Raw: syntheticCredential("old")}}, nil
		}
		blobs, _, err := h.adapter.Capture(h.home)
		if err != nil || len(blobs) != 2 || blobs[0].Identity.Email != "old@example.invalid" {
			t.Fatalf("verified primary not first: count=%d err=%v", len(blobs), err)
		}
	})
	t.Run("unknown native identity", func(t *testing.T) {
		h := newAdapterHarness(t)
		h.nativeClient(t, 101, "unrecorded@example.invalid", syntheticIdleActivity)
		if blobs, _, err := h.adapter.Capture(h.home); err == nil || len(blobs) != 0 {
			t.Fatal("fallback impersonated an unrecorded active account")
		}
	})
}

func TestAdapterMixedNativeAccountsAreNotAlignedImplicitly(t *testing.T) {
	h := newAdapterHarness(t)
	h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
	h.nativeClient(t, 102, "other@example.invalid", syntheticIdleActivity)
	if _, _, err := h.adapter.Capture(h.home); err == nil {
		t.Fatal("mixed client identities were captured as one live account")
	}
	if err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{}); err == nil {
		t.Fatal("mixed client identities were silently aligned")
	}
	if h.backend.writes != 0 {
		t.Fatal("mixed identities changed storage")
	}
}

func TestAdapterUncontrolledNativeCLIBlocksEverySwitchPath(t *testing.T) {
	for _, command := range []string{`/synthetic/bin/agy --prompt synthetic-private-prompt`, `"C:\Program Files\Antigravity\agy.exe" --prompt synthetic-private-prompt`} {
		h := newAdapterHarness(t)
		h.procs = []adapter.Proc{{PID: 101, Command: command}}
		if _, _, err := h.adapter.Capture(h.home); err == nil {
			t.Fatal("uncontrolled native identity was assumed known")
		}
		if err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{}); err == nil {
			t.Fatal("Restore bypassed missing native control endpoint")
		}
		holders, err := h.adapter.ManualBlockers(h.home)
		if len(holders.Manual) != 1 || holders.Manual[0].PID != 101 || strings.Contains(holders.Manual[0].Command, "private") {
			t.Fatalf("uncontrolled CLI was not a redacted blocker: holders=%+v err=%v", holders, err)
		}
		if h.adapter.Idle(h.home, 0) || h.adapter.KillCLI(h.home) == nil || h.backend.writes != 0 {
			t.Fatal("uncontrolled CLI switch protection was bypassed")
		}
	}
}

func TestAdapterRestoreRequiresVerifiedTargetAndIdleClients(t *testing.T) {
	for _, name := range []string{"busy", "unknown activity", "outer identity", "verified identity", "cli only"} {
		t.Run(name, func(t *testing.T) {
			h := newAdapterHarness(t)
			activity := syntheticIdleActivity
			if name == "busy" {
				activity = `{"trajectorySummaries":{"synthetic-conversation":{"status":"CASCADE_RUN_STATUS_RUNNING"}}}`
			}
			if name == "unknown activity" {
				activity = `{"unexpected":true}`
			}
			h.nativeClient(t, 101, "old@example.invalid", activity)
			blob := h.blob(t, "target")
			opts := adapter.RestoreOpts{}
			if name == "outer identity" {
				blob.Identity.StableID = "different-subject"
			}
			if name == "verified identity" {
				validate := h.adapter.deps.validate
				h.adapter.deps.validate = func(ctx context.Context, raw []byte) ([]byte, adapter.Identity, error) {
					raw, id, err := validate(ctx, raw)
					id.Email = "different@example.invalid"
					return raw, id, err
				}
			}
			if name == "cli only" {
				opts.CLIOnly = true
			}
			if err := h.adapter.Restore(h.home, blob, opts); err == nil {
				t.Fatal("unsafe target or activity accepted")
			}
			if h.backend.writes != 0 {
				t.Fatal("rejected switch wrote credentials")
			}
		})
	}
}

func TestAdapterRestoreWritesAndVerifiesEveryNativeClient(t *testing.T) {
	h := newAdapterHarness(t)
	putSyntheticCredential(t, CredentialPaths(h.home)[0], syntheticCredential("old"))
	a := h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
	b := h.nativeClient(t, 102, "old@example.invalid", syntheticIdleActivity)
	if err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
	cred, err := ParseCredential(h.backend.raw)
	if err != nil || cred.IDTokenEmail != "target@example.invalid" || !strings.HasPrefix(cred.WriterID, "qswitch-") {
		t.Fatal("native credential was not committed")
	}
	for _, client := range []*adapterNativeClient{a, b} {
		if client.activeEmail() != "target@example.invalid" {
			t.Fatal("a native client kept the old identity")
		}
	}
	file, _ := os.ReadFile(CredentialPaths(h.home)[0])
	if !bytes.Equal(file, h.backend.raw) {
		t.Fatal("existing fallback did not follow shared credential")
	}
}

func TestAdapterRestoreSyncFailureRestoresExactSlotsAndNativeIdentity(t *testing.T) {
	h := newAdapterHarness(t)
	paths := CredentialPaths(h.home)
	key, first, second := append([]byte(nil), h.backend.raw...), syntheticCredential("fallback-first"), syntheticCredential("fallback-second")
	putSyntheticCredential(t, paths[0], first)
	putSyntheticCredential(t, paths[1], second)
	a := h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
	b := h.nativeClient(t, 102, "old@example.invalid", syntheticIdleActivity)
	failure := errors.New("synthetic post-write sync failure")
	syncCalls := 0
	h.adapter.deps.sync = func(ctx context.Context, endpoints []Endpoint, email string) error {
		syncCalls++
		if syncCalls == 1 {
			if err := endpoints[0].Call(ctx, "GetAuthStatus", struct{}{}, nil); err != nil {
				return err
			}
			if a.activeEmail() != "target@example.invalid" {
				t.Fatal("failure did not exercise a partially switched client")
			}
			return failure
		}
		return SyncEndpoints(ctx, endpoints, email)
	}
	if err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{}); !errors.Is(err, failure) {
		t.Fatalf("sync failure lost: %v", err)
	}
	if syncCalls != 2 || a.activeEmail() != "old@example.invalid" || b.activeEmail() != "old@example.invalid" {
		t.Fatal("storage rollback did not restore native client identity")
	}
	if !bytes.Equal(h.backend.raw, key) {
		t.Fatal("keyring rollback was not exact")
	}
	for i, want := range [][]byte{first, second} {
		got, _ := os.ReadFile(paths[i])
		if !bytes.Equal(got, want) {
			t.Fatal("distinct fallback slot was overwritten during rollback")
		}
	}
}

func TestAdapterRestoreReportsRollbackFailureWithoutOverwritingExternalLogin(t *testing.T) {
	h := newAdapterHarness(t)
	h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
	external := syntheticCredential("external")
	calls := 0
	h.adapter.deps.sync = func(context.Context, []Endpoint, string) error {
		calls++
		h.backend.raw = append([]byte(nil), external...)
		return errors.New("synthetic sync failure")
	}
	err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{})
	if err == nil || !strings.Contains(err.Error(), "rollback refused") || !bytes.Equal(h.backend.raw, external) || calls != 1 {
		t.Fatalf("external login was overwritten or rollback error lost: calls=%d err=%v", calls, err)
	}
}

func TestAdapterRestoreReportsFailedNativeRecovery(t *testing.T) {
	h := newAdapterHarness(t)
	h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
	old := append([]byte(nil), h.backend.raw...)
	calls := 0
	h.adapter.deps.sync = func(context.Context, []Endpoint, string) error {
		calls++
		return errors.New("synthetic persistent RPC failure")
	}
	err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{})
	if err == nil || !strings.Contains(err.Error(), "previous client account was not confirmed") || !bytes.Equal(h.backend.raw, old) || calls != 2 {
		t.Fatalf("partial recovery was reported as complete: calls=%d err=%v", calls, err)
	}
}

func TestAdapterRestoreWithRollbackSurvivesLaterPointerCommitFailure(t *testing.T) {
	h := newAdapterHarness(t)
	old := append([]byte(nil), h.backend.raw...)
	path := CredentialPaths(h.home)[0]
	fallback := syntheticCredential("distinct-fallback")
	putSyntheticCredential(t, path, fallback)
	native := h.nativeClient(t, 101, "old@example.invalid", syntheticIdleActivity)
	undo, err := h.adapter.RestoreWithRollback(h.home, h.blob(t, "target"), adapter.RestoreOpts{})
	if err != nil || undo == nil || native.activeEmail() != "target@example.invalid" {
		t.Fatalf("switch did not return a reversible commit: %v", err)
	}
	// qswitch may fail to persist its account pointer after native sync has
	// succeeded. Recovery must work even though Restore's context is closed.
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(h.backend.raw, old) || !bytes.Equal(got, fallback) || native.activeEmail() != "old@example.invalid" {
		t.Fatal("late rollback did not restore both exact storage and active identity")
	}
	writes := h.backend.writes
	if err := undo(); err != nil || h.backend.writes != writes {
		t.Fatal("successful undo was not idempotent")
	}
}

func TestAdapterProcessListFailureCannotAuthorizeRestore(t *testing.T) {
	h := newAdapterHarness(t)
	h.adapter.List = func() ([]adapter.Proc, error) { return nil, errors.New("synthetic process failure") }
	if err := h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{}); err == nil || h.backend.writes != 0 {
		t.Fatal("unknown process state authorized a write")
	}
}
