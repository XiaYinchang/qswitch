package hostapp

import (
	"fmt"
	"os/exec"
	"time"

	"qswitch/internal/secutil"
)

// Controller quits and launches macOS GUI apps. Tests override QuitFn/LaunchFn.
type Controller struct {
	QuitFn   func(app string) error
	LaunchFn func(app string) error
	Sleep    func(time.Duration)
	Timeout  time.Duration
}

func Default() Controller {
	return Controller{Timeout: 20 * time.Second}
}

func (c Controller) Quit(app string) error {
	if c.QuitFn != nil {
		return c.QuitFn(app)
	}
	if secutil.InTest() {
		return nil
	}
	return exec.Command("osascript", "-e", fmt.Sprintf("tell application %q to quit", app)).Run()
}

func (c Controller) Launch(app string) error {
	if c.LaunchFn != nil {
		return c.LaunchFn(app)
	}
	if secutil.InTest() {
		return nil
	}
	return exec.Command("open", "-a", app).Run()
}

func (c Controller) WaitUntil(pred func() bool) error {
	sleep := c.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if pred() {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timeout waiting for app")
		}
		sleep(200 * time.Millisecond)
	}
}
