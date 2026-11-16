//go:build !windows

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func disableEcho(f *os.File) (func(), error) {
	fd := int(f.Fd())
	state, err := unix.IoctlGetTermios(fd, termiosGet)
	if err != nil {
		return nil, err
	}
	quiet := *state
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, termiosSet, &quiet); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, termiosSet, state) }, nil
}
