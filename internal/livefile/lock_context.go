package livefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

var ErrLocked = errors.New("file lock is already held")

// TryAcquire never waits for another holder. The lock is shared with Acquire.
func TryAcquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// AcquireContext bounds waiting without leaving a blocking lock goroutine.
func AcquireContext(ctx context.Context, path string) (*Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := TryAcquire(path)
		if !errors.Is(err, ErrLocked) {
			if err == nil && ctx.Err() != nil {
				_ = lock.Close()
				return nil, ctx.Err()
			}
			return lock, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
