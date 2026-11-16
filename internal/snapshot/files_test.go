package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"qswitch/internal/livefile"
)

func TestWriteFilesRollsBackPartialCommit(t *testing.T) {
	for _, existed := range []bool{false, true} {
		dir := t.TempDir()
		first, second := filepath.Join(dir, "auth"), filepath.Join(dir, "metadata")
		if existed {
			if err := os.WriteFile(first, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		failure := errors.New("synthetic second write failure")
		_, err := writeFiles([]FileWrite{{first, []byte("new"), 0600}, {second, []byte("new-meta"), 0600}}, func(path string, data []byte, mode os.FileMode) error {
			if path == second {
				return failure
			}
			return livefile.AtomicWrite(path, data, mode)
		})
		if !errors.Is(err, failure) {
			t.Fatalf("lost write failure: %v", err)
		}
		raw, err := os.ReadFile(first)
		if existed {
			if err != nil || string(raw) != "old" {
				t.Fatal("partial transaction was not undone")
			}
		} else if !os.IsNotExist(err) {
			t.Fatal("rollback did not restore absence")
		}
	}
}

func TestCheckpointRejectsNonregularAndReportsUndoFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	undo, err := CheckpointFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckpointFiles(path); err == nil {
		t.Fatal("directory accepted as credentials")
	}
	if err := undo(); err == nil {
		t.Fatal("rollback failure was discarded")
	}
}
