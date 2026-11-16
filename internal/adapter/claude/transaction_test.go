package claude

import (
	"os"
	"qswitch/internal/adapter"
	"testing"
)

func TestRestoreUndoIncludesAccountMetadata(t *testing.T) {
	for _, exists := range []bool{false, true} {
		home := t.TempDir()
		path := writeCreds(t, home, validOAuth())
		meta := claudeJSONPath(home)
		if err := os.WriteFile(meta, []byte(`{"oauthAccount":{"accountUuid":"target","emailAddress":"target@example.test"}}`), 0600); err != nil {
			t.Fatal(err)
		}
		ad := Adapter{DataDir: t.TempDir(), List: func() ([]adapter.Proc, error) { return nil, nil }}
		blobs, _, err := ad.Capture(home)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, meta} {
			if exists {
				if err := os.WriteFile(p, []byte(`{"preserve":"exact old bytes"}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
		}
		undo, err := ad.RestoreWithRollback(home, blobs[0], adapter.RestoreOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := undo(); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, meta} {
			raw, err := os.ReadFile(p)
			if exists {
				if err != nil || string(raw) != `{"preserve":"exact old bytes"}` {
					t.Fatal("old native account pair lost")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("old absence lost")
			}
		}
	}
}
