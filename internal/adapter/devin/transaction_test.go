package devin

import (
	"os"
	"path/filepath"
	"qswitch/internal/adapter"
	"testing"
)

func TestRestoreUndoPreservesCredentialsAndAbsence(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	for _, exists := range []bool{false, true} {
		home := t.TempDir()
		path := credPath(home)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("windsurf_api_key = \"synthetic-target\"\napi_server_url = \"https://server.codeium.com\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
		blobs, _, err := ad.Capture(home)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			if err := os.WriteFile(path, []byte("old exact bytes"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		undo, err := ad.RestoreWithRollback(home, blobs[0], adapter.RestoreOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := undo(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if exists {
			if err != nil || string(raw) != "old exact bytes" {
				t.Fatal("old credentials lost")
			}
		} else if !os.IsNotExist(err) {
			t.Fatal("absence lost")
		}
	}
}
