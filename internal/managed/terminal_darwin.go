//go:build darwin

package managed

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func terminalRestore() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, fmt.Errorf("managed Claude requires an interactive terminal: %w", err)
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TIOCSETA, state) }, nil
}
