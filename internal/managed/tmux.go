package managed

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Every check consults the server again: a pane can be replaced between
// prepare and the final exit acknowledgement. Attached clients do not block
// rotation; the plugin's draft and activity checks protect typed input.
func verifiedTMUXPane(pane, tmux string) error {
	if len(pane) < 2 || pane[0] != '%' {
		return errors.New("automatic rotation requires a verified tmux pane")
	}
	if _, err := strconv.ParseUint(pane[1:], 10, 64); err != nil {
		return errors.New("invalid tmux pane identity")
	}
	last := strings.LastIndex(tmux, ",")
	if last < 0 {
		return errors.New("missing tmux server identity")
	}
	previous := strings.LastIndex(tmux[:last], ",")
	if previous <= 0 {
		return errors.New("invalid tmux server identity")
	}
	socket := tmux[:previous]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	query := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", socket}, args...)...).Output()
		if err != nil {
			return "", errors.New("cannot verify tmux pane")
		}
		return strings.TrimSpace(string(out)), nil
	}
	info, err := query("display-message", "-p", "-t", pane, "#{pane_id}\t#{pane_pid}\t#{pane_tty}\t#{pane_dead}")
	if err != nil {
		return err
	}
	fields := strings.Split(info, "\t")
	if len(fields) != 4 || fields[0] != pane || fields[3] != "0" {
		return errors.New("tmux pane is missing or no longer alive")
	}
	panePID, err := strconv.Atoi(fields[1])
	if err != nil || panePID <= 0 {
		return errors.New("invalid tmux pane process")
	}
	if err := checkInput(fields[2], "tmux pane"); err != nil {
		return err
	}
	return checkAncestor(ctx, panePID, "tmux pane")
}
