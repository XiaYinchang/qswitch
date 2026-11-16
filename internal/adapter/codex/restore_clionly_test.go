package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"qswitch/internal/adapter"
	"testing"
)

func TestLegacyDesktopAttachmentsNeverRestored(t *testing.T) {
	for _, cliOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "cli"}[cliOnly], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("QSWITCH_IN_TEST", "1")
			if err := os.MkdirAll(filepath.Dir(authPath(home)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(authPath(home), []byte(`{"auth_mode":"chatgpt","tokens":{"account_id":"saved","access_token":"saved-token"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
			blobs, _, err := ad.Capture(home)
			if err != nil || len(blobs) != 1 {
				t.Fatalf("capture: %v", err)
			}
			var env map[string]any
			if err := json.Unmarshal(blobs[0].Payload, &env); err != nil {
				t.Fatal(err)
			}
			if _, ok := env["desktop_zip"]; ok {
				t.Fatal("capture associated an unverified desktop")
			}
			env["desktop_zip"] = []byte("untrusted legacy archive")
			env["desktop_rel_root"] = "../../outside"
			blob := blobs[0]
			blob.Payload, _ = json.Marshal(env)
			cookies := filepath.Join(home, "Library/Application Support/Codex/Default", "Cookies")
			if err := os.MkdirAll(filepath.Dir(cookies), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cookies, []byte("independent desktop"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ad.Restore(home, blob, adapter.RestoreOpts{CLIOnly: cliOnly}); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(cookies); !bytes.Equal(got, []byte("independent desktop")) {
				t.Fatal("modified unrelated desktop")
			}
			blob.Payload = []byte(`{"kind":"desktop.profile.v1","zip":"eA=="}`)
			if err := ad.Restore(home, blob, adapter.RestoreOpts{CLIOnly: cliOnly}); err == nil {
				t.Fatal("unbound desktop-only snapshot accepted")
			}
		})
	}
}
