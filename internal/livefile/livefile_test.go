package livefile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAtomicWriteReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := AtomicWrite(path, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, []byte("replacement")) {
		t.Fatalf("read replacement: %q, %v", raw, err)
	}
}

func TestLockExcludesConcurrentHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	type result struct {
		lock *Lock
		err  error
	}
	started := make(chan struct{})
	done := make(chan result, 1)
	go func() { close(started); lock, err := Acquire(path); done <- result{lock, err} }()
	<-started
	select {
	case r := <-done:
		if r.lock != nil {
			_ = r.lock.Close()
		}
		t.Fatalf("second handle acquired a held lock: %v", r.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		_ = r.lock.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("second handle did not acquire released lock")
	}
}
