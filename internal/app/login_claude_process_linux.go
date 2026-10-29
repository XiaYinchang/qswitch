package app

import (
	"os/exec"
	"runtime"
	"syscall"
)

func ownClaudeLoginThread() func() {
	runtime.LockOSThread()
	return runtime.UnlockOSThread
}

func protectClaudeLoginChild(cmd *exec.Cmd) {
	// Keep this goroutine's OS thread locked through Start AND Wait. Linux
	// sends Pdeathsig when the creating thread exits, not just the process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
