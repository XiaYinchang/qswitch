package kimi

import (
	"os"
	"qswitch/internal/adapter"
	"testing"
)

func TestRestoreUndoPreservesCredentialsAndAbsence(t *testing.T) {
	for _, exists := range []bool{false, true} {
		home, ad := fixture(t)
		path := credentialsPath(home)
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
