package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"qswitch/internal/adapter"
	"testing"
)

func TestRestoreFailurePreservesCredentials(t *testing.T) {
	for _, failure := range []string{"malformed", "identity mismatch", "empty credential", "ps failure", "CLI busy"} {
		t.Run(failure, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("QSWITCH_IN_TEST", "1")
			raw := []byte(`{"slot":{"principal_id":"saved","key":"saved-token"}}`)
			if err := os.MkdirAll(filepath.Dir(authPath(home)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(authPath(home), raw, 0600); err != nil {
				t.Fatal(err)
			}
			ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
			blobs, _, err := ad.Capture(home)
			if err != nil {
				t.Fatal(err)
			}
			blob := blobs[0]
			switch failure {
			case "malformed":
				var env map[string]any
				_ = json.Unmarshal(blob.Payload, &env)
				env["auth_json"] = "invalid"
				blob.Payload, _ = json.Marshal(env)
			case "identity mismatch":
				blob.Identity.StableID = "other"
			case "empty credential":
				var env map[string]any
				_ = json.Unmarshal(blob.Payload, &env)
				env["auth_json"] = json.RawMessage(`{"slot":{"principal_id":"saved"}}`)
				blob.Payload, _ = json.Marshal(env)

			case "ps failure":
				ad.List = func() ([]adapter.Proc, error) { return nil, errors.New("synthetic process failure") }
			case "CLI busy":
				ad.List = func() ([]adapter.Proc, error) { return []adapter.Proc{{PID: 42, Command: "grok"}}, nil }
			}
			if _, err := ad.RestoreWithRollback(home, blob, adapter.RestoreOpts{}); err == nil {
				t.Fatal("restore must fail")
			}
			if got, _ := os.ReadFile(authPath(home)); !bytes.Equal(got, raw) {
				t.Fatal("failed restore modified credentials")
			}
		})
	}
}

func TestRestoreUndoPreservesContentsModeAndAbsence(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[exists], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("QSWITCH_IN_TEST", "1")
			raw := []byte(`{"slot":{"principal_id":"saved","key":"saved-token"}}`)
			if err := os.MkdirAll(filepath.Dir(authPath(home)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(authPath(home), raw, 0600); err != nil {
				t.Fatal(err)
			}
			ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
			blobs, _, err := ad.Capture(home)
			if err != nil {
				t.Fatal(err)
			}
			old := []byte("previous exact bytes")
			if exists {
				if err := os.WriteFile(authPath(home), old, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(authPath(home), 0640); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(authPath(home)); err != nil {
					t.Fatal(err)
				}
			}
			undo, err := ad.RestoreWithRollback(home, blobs[0], adapter.RestoreOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(authPath(home)); !bytes.Equal(got, raw) {
				t.Fatal("target not committed")
			}
			if err := undo(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(authPath(home))
			if !exists {
				if !os.IsNotExist(err) {
					t.Fatal("undo did not restore absence")
				}
			} else {
				if err != nil || !bytes.Equal(got, old) {
					t.Fatal("undo lost old bytes")
				}
				st, _ := os.Stat(authPath(home))
				if st.Mode().Perm() != 0640 {
					t.Fatal("undo lost mode")
				}
			}
		})
	}
}
