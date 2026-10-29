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

const maxJSON = 64 << 10

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
		return err
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
		if err := privateDir(dir); err != nil {
			return nil, nil, err
		}
		lease, err := privateFile(filepath.Join(dir, "lease"), unix.O_RDWR)
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
