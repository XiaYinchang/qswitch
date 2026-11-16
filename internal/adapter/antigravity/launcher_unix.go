//go:build !windows

package antigravity

import (
	"os/exec"
	"syscall"
	"time"
)

func configureCLICommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
}
