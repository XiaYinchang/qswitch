package grok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"qswitch/internal/adapter"
	"testing"
	"time"
)

func TestRestoreRejectsLegacyMultipleIdentitySnapshot(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(authPath(home)), 0700); err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"live":{"principal_id":"live","key":"live-synthetic"}}`)
	if err := os.WriteFile(authPath(home), old, 0600); err != nil {
		t.Fatal(err)
	}
	identity := adapter.Identity{Tool: adapter.Grok, StableID: "target"}
	raw := json.RawMessage(`{"target-slot":{"principal_id":"target","key":"target-synthetic"},"other-slot":{"principal_id":"other","key":"other-synthetic"}}`)
	payload, err := json.Marshal(map[string]any{"kind": "grok.auth.json.v1", "identity": identity, "auth_json": raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAuthForIdentity(raw, "target"); err != nil {
		t.Fatal(err)
	}
	ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	if _, err := ad.RestoreWithRollback(home, adapter.Blob{Tool: adapter.Grok, Identity: identity, Payload: payload}, adapter.RestoreOpts{}); err == nil {
		t.Fatal("ambiguous legacy credentials restored")
	}
	if got, _ := os.ReadFile(authPath(home)); string(got) != string(old) {
		t.Fatal("rejected snapshot modified the live account")
	}
}

func TestNoActivityEvidenceCannotAuthorizeCLIExit(t *testing.T) {
	ad := Adapter{List: func() ([]adapter.Proc, error) { return []adapter.Proc{{PID: 42, Command: `C:\Tools\grok.exe`}}, nil }}
	if ad.Idle(t.TempDir(), time.Second) {
		t.Fatal("missing session directory was treated as idle")
	}
}

func TestOAuthIssuerRequiresExactHost(t *testing.T) {
	if (AuthFile{Issuer: "https://auth.x.ai.attacker.test", Refresh: "synthetic"}).Refreshable() {
		t.Fatal("issuer prefix mistaken for official issuer")
	}
}

func TestRequestedIdentityNeverFallsBack(t *testing.T) {
	raw := []byte(`{"slot":{"principal_id":"another","key":"synthetic"}}`)
	if _, err := ReadAuthForIdentity(raw, "missing"); err == nil {
		t.Fatal("requested identity fell back to another account")
	}
}

func TestMultipleIdentityCaptureIsAmbiguous(t *testing.T) {
	if _, err := identityFromRaw([]byte(`{"a":{"principal_id":"one","key":"a"},"b":{"principal_id":"two","key":"b"}}`)); err == nil {
		t.Fatal("ambiguous native identity accepted")
	}
	raw := []byte(`{"z":{"principal_id":"same","key":"z"},"a":{"principal_id":"same","key":"a"}}`)
	for range 20 {
		a, err := parseAuthJSON(raw, "")
		if err != nil || a.Slot != "a" {
			t.Fatal("same-account slot selection is not deterministic")
		}
	}
}
