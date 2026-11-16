package antigravity

import (
	"os/exec"
	"time"
)

func configureCLICommand(cmd *exec.Cmd) {
	// Keep the parent's console so the native CLI receives Ctrl+C directly.
	// CommandContext terminates only this owned child when its context ends.
	cmd.WaitDelay = 5 * time.Second
}
