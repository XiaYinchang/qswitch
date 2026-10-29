//go:build linux

package managed

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func terminalRestore() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("managed Claude requires an interactive terminal: %w", err)
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, state) }, nil
}
