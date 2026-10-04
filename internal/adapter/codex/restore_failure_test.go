package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/snapshot"
)

func TestRestoreFailurePreservesCredentials(t *testing.T) {
	for _, failure := range []string{"bad zip", "desktop write failure", "CLI write failure", "ps failure", "desktop busy", "invalid root"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("QSWITCH_IN_TEST", "1")
			home := t.TempDir()
			root := filepath.Join(home, snapshot.ChatGPTRelRoot)
			cookies := filepath.Join(root, "Cookies")
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(authPath(home), []byte(`{"auth_mode":"chatgpt","tokens":{"account_id":"saved","access_token":"saved-token"}}`))
			write(cookies, []byte("saved-desktop-session"))
			ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
			blobs, _, err := ad.Capture(home)
			if err != nil || len(blobs) == 0 {
				t.Fatalf("capture: %v, blobs=%d", err, len(blobs))
			}
			blob := blobs[0]
			liveAuth := []byte(`{"auth_mode":"chatgpt","tokens":{"account_id":"live","access_token":"live-token"}}`)
			write(authPath(home), liveAuth)
			write(cookies, []byte("live-desktop-session"))
			var env map[string]json.RawMessage
			if err := json.Unmarshal(blob.Payload, &env); err != nil {
				t.Fatal(err)
			}
			psErr := errors.New("process list failed")
			switch failure {
			case "bad zip":
				env["desktop_zip"], _ = json.Marshal([]byte("broken archive"))
			case "invalid root":
				env["desktop_rel_root"], _ = json.Marshal("../outside")
			case "desktop write failure", "CLI write failure":
				blocked := root
				if failure == "CLI write failure" {
					blocked = filepath.Dir(authPath(home))
				}
				if err := os.Chmod(blocked, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
				if f, err := os.CreateTemp(blocked, "permission-check"); err == nil {
					f.Close()
					os.Remove(f.Name())
					t.Skip("filesystem permits writes despite directory permissions")
				}
			case "ps failure":
				calls := 0
				ad.List = func() ([]adapter.Proc, error) {
					calls++
					if calls == 1 {
						return nil, nil
					}
					return nil, psErr
				}
			case "desktop busy":
				ad.List = func() ([]adapter.Proc, error) {
					return []adapter.Proc{{PID: 42, Command: "/Applications/ChatGPT.app/Contents/MacOS/ChatGPT"}}, nil
				}
			}
			blob.Payload, err = json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			err = ad.Restore(home, blob, adapter.RestoreOpts{})
			if err == nil {
				t.Fatal("restore must fail")
			}
			if failure == "ps failure" && !errors.Is(err, psErr) {
				t.Errorf("lost process-list error: %v", err)
			}
			if failure == "desktop busy" {
				var busy *adapter.BusyError
				if !errors.As(err, &busy) {
					t.Errorf("expected BusyError: %v", err)
				}
			}
			if got, err := os.ReadFile(authPath(home)); err != nil || !bytes.Equal(got, liveAuth) {
				t.Errorf("failed restore changed CLI credentials: %v", err)
			}
			if got, err := os.ReadFile(cookies); err != nil || string(got) != "live-desktop-session" {
				t.Errorf("failed restore changed desktop credentials: %v", err)
			}
		})
	}
}
