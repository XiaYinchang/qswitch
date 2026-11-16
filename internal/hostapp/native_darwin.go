package hostapp

import (
	"fmt"
	"os/exec"
)

func quitNative(app string) error {
	return exec.Command("osascript", "-e", fmt.Sprintf("tell application %q to quit", app)).Run()
}

func launchNative(app string) error { return exec.Command("open", "-a", app).Run() }
