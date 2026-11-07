package managed

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// maxJSON bounds control requests and state files. An exit acknowledgement
// carries held messages and a draft: up to 14000 characters, which JSON
// escaping can grow past 64 KiB.
const maxJSON = 256 << 10

type registration struct {
	ID    string `json:"id"`
	PID   int    `json:"pid"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

// StatusInfo describes a runner, including durable recovery information left by
// a runner that died. Arguments and authentication tokens are never published.
type StatusInfo struct {
	ID            string `json:"id"`
	RunnerPID     int    `json:"runner_pid"`
	ChildPID      int    `json:"child_pid"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	Ready         bool   `json:"ready"`
	NeedsContinue bool   `json:"needs_continue"`
	Exited        bool   `json:"exited"`
	Generation    string `json:"generation"`
	Reason        string `json:"reason"`
	Recovery      string `json:"recovery,omitempty"`
	Live          bool   `json:"live"`
}

type participant struct {
	reg    registration
	status StatusInfo
}

type transaction struct {
	Generation string   `json:"generation"`
	IDs        []string `json:"ids"`
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return checkPrivateDir(path)
}

func checkPrivateDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("managed Claude directory must be owned by this user with mode 0700: %s", path)
	}
	return nil
}

func rootDir(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("managed Claude requires a data directory")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	root := filepath.Join(abs, "managed-claude")
	if err := privateDir(root); err != nil {
		return "", err
	}
	return root, nil
}

// markLimitHit records that a managed session just hit the account usage
// limit. It is only a hint: the daemon still confirms exhaustion with the
// official quota endpoint before queueing a switch.
func markLimitHit(dataDir string) error {
	root, err := rootDir(dataDir)
	if err != nil {
		return err
	}
	f, err := privateFile(filepath.Join(root, "limit-hit"), unix.O_CREAT|unix.O_WRONLY)
	if err != nil {
		return err
	}
	return f.Close()
}

// TakeLimitHit reports and clears a pending limit-hit hint.
func TakeLimitHit(dataDir string) bool {
	root, err := rootDir(dataDir)
	if err != nil {
		return false
	}
	return os.Remove(filepath.Join(root, "limit-hit")) == nil
}

func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func privateFile(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err == nil {
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
			err = fmt.Errorf("unsafe managed Claude file: %s", path)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func readJSON(path string, out any) error {
	f, err := privateFile(path, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	return decodeJSON(f, out)
}

func decodeJSON(r io.Reader, out any) error {
	b, err := io.ReadAll(io.LimitReader(r, maxJSON+1))
	if err != nil {
		return err
	}
	if len(b) > maxJSON {
		return errors.New("managed Claude JSON exceeds limit")
	}
	return json.Unmarshal(b, out)
}

func atomicJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > maxJSON {
		return errors.New("managed Claude JSON exceeds limit")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("managed Claude endpoint must be plain HTTP on 127.0.0.1")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid managed Claude endpoint port")
	}
	return nil
}

// ErrRunnerUnavailable marks a runner that did not answer a control request.
// It says nothing about the switch itself, so callers retry instead of
// cancelling it.
var ErrRunnerUnavailable = errors.New("runner did not answer")

var controlClient = &http.Client{
	Timeout:       3 * time.Second,
	Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("managed Claude redirects forbidden") },
}

func request(reg registration, route string, body any, out any) error {
	if err := validateEndpoint(reg.URL); err != nil {
		return err
	}
	method := http.MethodGet
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = strings.NewReader(string(b))
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, reg.URL+route, input)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := controlClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRunnerUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("managed Claude %s: HTTP %d: %s", route, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return decodeJSON(resp.Body, out)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSON))
	return err
}

func scan(root string) ([]participant, []StatusInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var live []participant
	var stale []StatusInfo
	for _, entry := range entries {
		if !validID(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		// A runner removes its directory when it ends after its Claude exited.
		// Skip a directory that is gone or has no lease file left; recreating
		// it would leave a directory that fails every later scan.
		if err := checkPrivateDir(dir); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, nil, err
		}
		lease, err := privateFile(filepath.Join(dir, "lease"), unix.O_RDWR)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		lockErr := unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if lockErr == nil {
			var state StatusInfo
			err := readJSON(filepath.Join(dir, "status.json"), &state)
			if err == nil {
				state.Live = false
				state.Ready = false
				if state.Recovery == "" {
					state.Recovery = "runner stopped; inspect the terminal before manually resuming this session"
				}
				stale = append(stale, state)
			} else if !errors.Is(err, os.ErrNotExist) {
				lease.Close()
				return nil, nil, err
			}
			lease.Close()
			continue
		}
		lease.Close()
		if !errors.Is(lockErr, unix.EWOULDBLOCK) && !errors.Is(lockErr, unix.EAGAIN) {
			return nil, nil, lockErr
		}
		var reg registration
		if err := readJSON(filepath.Join(dir, "registration.json"), &reg); err != nil {
			return nil, nil, err
		}
		if reg.ID != entry.Name() || reg.PID <= 0 || !validID(reg.Token) {
			return nil, nil, errors.New("invalid managed Claude registration")
		}
		var state StatusInfo
		if err := request(reg, "/status", nil, &state); err != nil {
			return nil, nil, fmt.Errorf("runner %s unavailable: %w", reg.ID, err)
		}
		if state.ID != reg.ID || state.RunnerPID != reg.PID {
			return nil, nil, errors.New("managed Claude registration identity changed")
		}
		state.Live = true
		live = append(live, participant{reg: reg, status: state})
	}
	return live, stale, nil
}

// Status includes stale registrations so interrupted rotations remain visible.
func Status(dataDir string) ([]StatusInfo, error) {
	root, err := rootDir(dataDir)
	if err != nil {
		return nil, err
	}
	live, stale, err := scan(root)
	if err != nil {
		return nil, err
	}
	states := make([]StatusInfo, 0, len(live)+len(stale))
	for _, p := range live {
		states = append(states, p.status)
	}
	return append(states, stale...), nil
}

// Resume asks every live session still waiting after a usage-limit failure to
// continue. It is for an account that has quota without a switch: Claude Code
// cancels its own automatic continue when a session restarts during the wait.
// Runners refuse sessions that are busy, preparing, or not interrupted.
func Resume(dataDir string) (int, error) {
	root, err := rootDir(dataDir)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(filepath.Join(root, "pending.json")); err == nil {
		return 0, nil // a switch is in progress; its release continues sessions
	}
	live, _, err := scan(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range live {
		if p.status.Exited || !p.status.Ready || !p.status.NeedsContinue || p.status.Generation != "" {
			continue
		}
		if request(p.reg, "/resume", struct{}{}, nil) == nil {
			n++
		}
	}
	return n, nil
}

// Forget removes the record a runner left behind: for a session whose Claude
// crashed or could not be restarted, which only shows the resume command in
// qswitch sessions, and for a runner killed in the middle of a switch, whose
// record holds every switch until it is known that its Claude is gone.
// prefix names the runner, as qswitch sessions shows it. running reports
// whether a Claude Code process with that pid still runs; a record whose
// child was never seen to exit is kept while it does. A running runner's
// record is never forgotten: it removes its own when it ends.
func Forget(dataDir, prefix string, running func(pid int) bool) (StatusInfo, error) {
	root, err := rootDir(dataDir)
	if err != nil {
		return StatusInfo{}, err
	}
	if prefix == "" {
		return StatusInfo{}, errors.New("forget needs a runner id")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return StatusInfo{}, err
	}
	var matches []string
	for _, entry := range entries {
		if validID(entry.Name()) && strings.HasPrefix(entry.Name(), prefix) {
			matches = append(matches, entry.Name())
		}
	}
	switch len(matches) {
	case 0:
		return StatusInfo{}, fmt.Errorf("no managed Claude runner matches %q", prefix)
	case 1:
	default:
		return StatusInfo{}, fmt.Errorf("%d managed Claude runners match %q; give more of the id", len(matches), prefix)
	}
	dir := filepath.Join(root, matches[0])
	lease, err := privateFile(filepath.Join(dir, "lease"), unix.O_RDWR)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return StatusInfo{}, err
	}
	if err == nil {
		defer lease.Close()
		if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
				return StatusInfo{}, fmt.Errorf("runner %s is still running; it removes its own record when it ends", matches[0][:12])
			}
			return StatusInfo{}, err
		}
	}
	var state StatusInfo
	if err := readJSON(filepath.Join(dir, "status.json"), &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return StatusInfo{}, err
	}
	if !state.Exited && state.ChildPID > 0 && running(state.ChildPID) {
		return StatusInfo{}, fmt.Errorf("Claude process %d of session %s may still be running; stop it before forgetting the record", state.ChildPID, state.SessionID)
	}
	if err := os.RemoveAll(dir); err != nil {
		return StatusInfo{}, err
	}
	state.ID = matches[0]
	return state, nil
}
