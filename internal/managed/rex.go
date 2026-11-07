package managed

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// rexTerminal is the Rex block a runner was started in, from the REX_SESSION,
// REX_BLOCK and REX_BIN_DIR variables Rex sets in its terminals.
type rexTerminal struct {
	session, block, binDir string
}

// validRexID accepts IDs such as block:01a192f7-0a46-7866-b86a-cd87b36311d7.
// They are passed as separate arguments, never through a shell.
func validRexID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == ':' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Like a tmux pane, every check asks the server again: the block can close or
// be replaced between prepare and the final exit acknowledgement. The Rex
// server reports the block's own process; the runner must descend from it and
// read from its terminal.
func verifiedRexBlock(t rexTerminal) error {
	if !validRexID(t.session) || !validRexID(t.block) {
		return errors.New("invalid Rex terminal identity")
	}
	// REX_BIN_DIR names the CLI of the server that opened this terminal; other
	// Rex versions on the same host may run their own servers.
	bin := "rex"
	if filepath.IsAbs(t.binDir) {
		bin = filepath.Join(t.binDir, "rex")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Without an explicit block, Rex falls back to the focused one.
	out, err := exec.CommandContext(ctx, bin, "--autostart=false", "--timeout=2s", "block", "call", "--session="+t.session, "--block="+t.block, "process").Output()
	if err != nil {
		return errors.New("cannot verify Rex terminal")
	}
	var info struct {
		Child *struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	if err := decodeJSON(bytes.NewReader(out), &info); err != nil {
		return errors.New("cannot verify Rex terminal")
	}
	if info.Child == nil {
		return errors.New("rex terminal is missing or no longer alive")
	}
	if info.Child.PID <= 1 {
		return errors.New("invalid Rex terminal process")
	}
	out, err = exec.CommandContext(ctx, "ps", "-o", "tty=", "-p", strconv.Itoa(info.Child.PID)).Output()
	if err != nil {
		return errors.New("cannot inspect Rex block terminal")
	}
	tty := strings.TrimSpace(string(out))
	if !validTTYName(tty) {
		return errors.New("cannot inspect Rex block terminal")
	}
	if err := checkInput("/dev/"+tty, "Rex block"); err != nil {
		return err
	}
	return checkAncestor(ctx, info.Child.PID, "Rex block")
}

// validTTYName accepts ps names such as pts/3 and ttys003; ps prints ? or ??
// for a process without a terminal.
func validTTYName(name string) bool {
	if name == "" || name[0] == '/' || strings.Contains(name, "..") {
		return false
	}
	for _, c := range name {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '/') {
			return false
		}
	}
	return true
}
