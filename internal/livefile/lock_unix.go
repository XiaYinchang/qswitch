//go:build !windows

package livefile

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lockFile(f *os.File) error   { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }
func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

func tryLockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrLocked
	}
	return err
}
