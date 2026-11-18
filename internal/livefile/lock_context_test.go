package livefile

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireContextCancelsHeldLockAndReleasesHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if next, err := TryAcquire(path); !errors.Is(err, ErrLocked) || next != nil {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("held lock was not excluded: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	started := time.Now()
	if next, err := AcquireContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) || next != nil {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("held lock did not honor cancellation: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("lock wait ignored its deadline")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("cancelled wait retained a lock: %v", err)
	}
	_ = next.Close()
	if next, err := AcquireContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) || next != nil {
		if next != nil {
			_ = next.Close()
		}
		t.Fatal("already cancelled caller acquired an available lock")
	}
}

func TestAcquireContextWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "released.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		next, err := AcquireContext(ctx, path)
		if next != nil {
			_ = next.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("acquired a held lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = first.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
