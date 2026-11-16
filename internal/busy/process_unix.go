//go:build !windows

package busy

import (
	"os/exec"
	"qswitch/internal/adapter"
	"strconv"
	"syscall"
)

func platformProcesses() ([]adapter.Proc, error) {
	return (PS{Run: func() ([]byte, error) { return exec.Command("ps", "-axo", "pid=,command=").Output() }}).List()
}

func processAlive(pid int) bool { return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil }

// Terminate requests the process to exit, after the caller checks its identity
// and idle state. It does not act on similarly named processes.
func Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
