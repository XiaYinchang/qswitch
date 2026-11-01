package managed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// verifiedTerminal accepts a tmux pane or a Rex block. Both keep running on
// their server while no client is attached, so the runner can restart Claude
// into the same terminal after a rotation. A tmux pane inside a Rex terminal
// is checked as tmux; Rex is tried when the tmux check fails, because a Rex
// server started from tmux passes its tmux variables on to every terminal.
func verifiedTerminal(pane, tmux string, rex rexTerminal) error {
	switch {
	case pane != "":
		err := verifiedTMUXPane(pane, tmux)
		if err != nil && rex.block != "" && verifiedRexBlock(rex) == nil {
			return nil
		}
		return err
	case rex.block != "":
		return verifiedRexBlock(rex)
	}
	return errors.New("automatic rotation requires a verified tmux pane or Rex terminal")
}

// checkInput requires the runner's standard input to be the terminal at path.
func checkInput(path, what string) error {
	stdin, err := os.Stdin.Stat()
	if err != nil {
		return err
	}
	tty, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot inspect %s terminal", what)
	}
	inputStat, inputOK := stdin.Sys().(*syscall.Stat_t)
	ttyStat, ttyOK := tty.Sys().(*syscall.Stat_t)
	if !inputOK || !ttyOK || stdin.Mode()&os.ModeCharDevice == 0 || tty.Mode()&os.ModeCharDevice == 0 || inputStat.Rdev != ttyStat.Rdev {
		return fmt.Errorf("runner input is not the %s terminal", what)
	}
	return nil
}

// checkAncestor requires pid to be this runner or one of its ancestors.
func checkAncestor(ctx context.Context, ancestor int, what string) error {
	pid := os.Getpid()
	for depth := 0; pid != ancestor; depth++ {
		if pid <= 1 || depth >= 128 {
			return fmt.Errorf("%s process is not an ancestor of this runner", what)
		}
		out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return fmt.Errorf("cannot verify %s process ancestry", what)
		}
		parent, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil || parent == pid {
			return fmt.Errorf("invalid %s process ancestry", what)
		}
		pid = parent
	}
	return nil
}
