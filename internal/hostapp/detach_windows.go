package hostapp

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

func detachDesktop(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
