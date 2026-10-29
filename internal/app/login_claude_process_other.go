//go:build !linux

package app

import "os/exec"

// Public isolated login rejects non-Linux platforms. These helpers let the
// platform-independent enrollment tests exercise the same command lifecycle.
func ownClaudeLoginThread() func() { return func() {} }

func protectClaudeLoginChild(cmd *exec.Cmd) {}
