package managed

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// A runner keeps the qswitch binary it started from. When that file is
// replaced, the runner waits until its session is safely idle, exits Claude
// through the same acknowledged /exit used for account rotation, and execs
// the new binary in place. The process ID, runner ID, lease and control
// listener carry over, so the daemon keeps seeing the same participant.

// upgradeGeneration marks an upgrade exit. It is not a daemon generation:
// the daemon never prepares or releases it.
const upgradeGeneration = "self-upgrade"

// handoffEnv carries runner state to the replacement binary.
const handoffEnv = "QSWITCH_RUNNER_HANDOFF"

// upgradeGrace bounds how long an acknowledged upgrade exit may take.
const upgradeGrace = 30 * time.Second

type fileID struct{ dev, ino uint64 }

func identify(path string) (fileID, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fileID{}, err
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// replacedBinary reports whether the runner's own executable was replaced by
// a different, usable file. Install scripts rename a new file over the old
// path, which changes the inode.
func (r *runner) replacedBinary() bool {
	if r.self == "" {
		return false
	}
	id, err := identify(r.self)
	if err != nil || id == r.selfID {
		return false
	}
	return unix.Access(r.self, unix.X_OK) == nil
}

// upgradeDue decides whether to ask the plugin for an upgrade exit. Only a
// ready, fresh, unstarted session qualifies: no daemon generation, no exit
// already requested, and no pending continuation that the upgrade would lose.
func (r *runner) upgradeDueLocked() bool {
	if r.upgrading || r.stopping || r.status.Recovery != "" || r.status.Exited {
		return false
	}
	if r.status.Generation != "" || r.exitRequested || r.resumePending {
		return false
	}
	if !r.freshLocked() {
		return false
	}
	return r.replacedBinary()
}

type handoff struct {
	Reg           registration `json:"reg"`
	Dir           string       `json:"dir"`
	ListenerFD    int          `json:"listener_fd"`
	LeaseFD       int          `json:"lease_fd"`
	Options       Options      `json:"options"`
	SessionID     string       `json:"session_id"`
	CWD           string       `json:"cwd"`
	NeedsContinue bool         `json:"needs_continue"`
}

// execUpgrade replaces this process with the new qswitch binary. On success
// it does not return. Inherited descriptors keep the runner's identity.
func execUpgrade(self string, h handoff, listener, lease *os.File) error {
	for _, f := range []*os.File{listener, lease} {
		if _, err := unix.FcntlInt(f.Fd(), unix.F_SETFD, 0); err != nil {
			return fmt.Errorf("keep descriptor across exec: %w", err)
		}
	}
	h.ListenerFD = int(listener.Fd())
	h.LeaseFD = int(lease.Fd())
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	env := []string{}
	for _, e := range os.Environ() {
		if len(e) < len(handoffEnv)+1 || e[:len(handoffEnv)+1] != handoffEnv+"=" {
			env = append(env, e)
		}
	}
	env = append(env, handoffEnv+"="+string(b))
	args := []string{self, "run", "claude"}
	return syscall.Exec(self, args, env)
}

// takeHandoff reads and clears the handoff left by a previous binary. The
// environment variable is removed so a child Claude never inherits it.
func takeHandoff() (*handoff, error) {
	raw, ok := os.LookupEnv(handoffEnv)
	if !ok {
		return nil, nil
	}
	_ = os.Unsetenv(handoffEnv)
	var h handoff
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return nil, fmt.Errorf("invalid runner handoff: %w", err)
	}
	if !validID(h.Reg.ID) || !validID(h.Reg.Token) || h.ListenerFD < 3 || h.LeaseFD < 3 {
		return nil, errors.New("invalid runner handoff")
	}
	if h.Reg.PID != os.Getpid() {
		return nil, errors.New("runner handoff from another process")
	}
	if filepath.Base(h.Dir) != h.Reg.ID {
		return nil, errors.New("runner handoff directory mismatch")
	}
	return &h, nil
}

func inheritedFile(fd int, name string) (*os.File, error) {
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		return nil, fmt.Errorf("inherited %s descriptor %s: %w", name, strconv.Itoa(fd), err)
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), name), nil
}

// listenerFile returns a descriptor for the control listener. An inherited
// listener already has one; a fresh TCP listener is duplicated from its
// socket. The listener keeps serving until exec replaces the process.
func listenerFile(l net.Listener, inherited *os.File) (*os.File, error) {
	if inherited != nil {
		return inherited, nil
	}
	tcp, ok := l.(*net.TCPListener)
	if !ok {
		return nil, errors.New("control listener is not TCP")
	}
	return tcp.File()
}
