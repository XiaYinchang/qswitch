package grok

import (
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// AuthRefreshLock uses the official client's flock and PID:timestamp heartbeat.
// Never unlink the lock: a new inode would allow two owners to spend one token.
type AuthRefreshLock struct {
	f    *os.File
	path string
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func AcquireAuthRefreshLock(home string) (*AuthRefreshLock, error) {
	path := lockPath(home)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("grok: open auth lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("grok: auth refresh lock unavailable: %w", err)
	}
	l := &AuthRefreshLock{f: f, path: path, stop: make(chan struct{}), done: make(chan struct{})}
	if !l.OwnsPath() {
		f.Close()
		return nil, fmt.Errorf("grok: auth lock replaced")
	}
	if err := l.stamp(); err != nil {
		f.Close()
		return nil, err
	}
	go func() {
		defer close(l.done)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-tick.C:
				if !l.OwnsPath() {
					return
				}
				if err := l.stamp(); err != nil {
					return
				}
			}
		}
	}()
	return l, nil
}
func (l *AuthRefreshLock) stamp() error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.WriteAt([]byte(fmt.Sprintf("%d:%d", os.Getpid(), time.Now().Unix())), 0); err != nil {
		return err
	}
	return l.f.Sync()
}
func (l *AuthRefreshLock) OwnsPath() bool {
	opened, err := l.f.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(l.path)
	return err == nil && os.SameFile(opened, current)
}
func (l *AuthRefreshLock) Close() error {
	var err error
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		// qswitch stays alive after refresh. Clear its holder stamp while locked so
		// the switcher's PID-file check cannot mistake the daemon for a Grok CLI.
		if l.OwnsPath() {
			err = l.f.Truncate(0)
			if err == nil {
				err = l.f.Sync()
			}
		}
		_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
		if e := l.f.Close(); err == nil {
			err = e
		}
	})
	return err
}
