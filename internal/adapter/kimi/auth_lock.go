package kimi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// AuthRefreshLock shares the official proper-lockfile directory. Its heartbeat
// stays below the client's five-second stale limit. Existing locks are never
// removed; the official client owns recovery of abandoned peer locks.
type AuthRefreshLock struct {
	f      *os.File
	path   string
	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func AcquireAuthRefreshLock(ctx context.Context, home string) (*AuthRefreshLock, error) {
	if err := validateConfig(home); err != nil {
		return nil, err
	}
	target := filepath.Join(dataDir(home), "oauth", "kimi-code")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(target, unix.O_CREAT|unix.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	_ = unix.Close(fd)
	path := target + ".lock"
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, fmt.Errorf("kimi: OAuth refresh lock unavailable: %w", err)
	}
	fd, err = unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	lockCtx, cancel := context.WithCancel(ctx)
	l := &AuthRefreshLock{f: os.NewFile(uintptr(fd), path), path: path, ctx: lockCtx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-l.ctx.Done():
				return
			case <-tick.C:
				tv := unix.NsecToTimeval(time.Now().UnixNano())
				if !l.OwnsPath() || unix.Futimes(int(l.f.Fd()), []unix.Timeval{tv, tv}) != nil {
					l.cancel()
					return
				}
			}
		}
	}()
	return l, nil
}

func (l *AuthRefreshLock) Context() context.Context { return l.ctx }

func (l *AuthRefreshLock) OwnsPath() bool {
	opened, err := l.f.Stat()
	if err != nil {
		return false
	}
	current, err := os.Lstat(l.path)
	return err == nil && current.IsDir() && os.SameFile(opened, current)
}

func (l *AuthRefreshLock) Close() error {
	var err error
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		if l.OwnsPath() {
			err = os.Remove(l.path) // Empty directory only; never remove peer data.
		}
		if e := l.f.Close(); err == nil {
			err = e
		}
		l.cancel()
	})
	return err
}
