//go:build !darwin && !windows

package hostapp

import (
	"os/exec"
	"syscall"
)

func detachDesktop(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
