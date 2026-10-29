package managed

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

//go:embed all:plugin
var pluginFiles embed.FS

type Options struct {
	DataDir   string
	ClaudeBin string
	Args      []string
	// Explicitly classify an adopted transcript. This never sends a prompt on
	// initial startup; continuation is authorized only by a committed release.
	InitialContinuation string
}

type sessionState struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	Ready         bool   `json:"ready"`
	NeedsContinue bool   `json:"needs_continue"`
	Reason        string `json:"reason"`
	Generation    string `json:"generation,omitempty"`
}

type action struct {
	Action     string `json:"action"`
	Generation string `json:"generation,omitempty"`
}

type runner struct {
	mu               sync.Mutex
	status           StatusInfo
	reg              registration
	dir              string
	options          Options
	pane             string
	tmux             string
	lastState        time.Time
	exitRequested    bool
	exitAcknowledged bool
	released         bool
	committed        bool
	stopping         bool
	wake             chan struct{}
	gate             func() error
}

func writeResponse(w http.ResponseWriter, value any) { _ = json.NewEncoder(w).Encode(value) }

func validateEnvironment() error {
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_SIMPLE"} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("managed Claude requires default OAuth configuration; unset %s", key)
		}
	}
	return nil
}

func adoptedSession(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--resume", "-r", "--continue", "-c", "--session-id":
			return true
		}
	}
	return false
}

func extractPlugin(dir string) error {
	return fs.WalkDir(pluginFiles, "plugin", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		dest := filepath.Join(dir, filepath.FromSlash(path))
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		data, err := pluginFiles.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0600)
	})
}

func acquireClaudeLock(ctx context.Context, dataDir string) (*os.File, error) {
	dir := filepath.Join(dataDir, "locks")
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	file, err := privateFile(filepath.Join(dir, "claude.lock"), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return file, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *runner) persistLocked() error {
	return atomicJSON(filepath.Join(r.dir, "status.json"), r.status)
}

func (r *runner) failLocked(err error) {
	r.status.Ready = false
	r.status.Recovery = err.Error() + "; " + recoveryCommand(r.options.ClaudeBin, r.status.CWD, r.status.SessionID)
}

func (r *runner) freshLocked() bool {
	return r.status.Ready && r.status.SessionID != "" && !r.lastState.IsZero() && time.Since(r.lastState) < 5*time.Second && r.status.Recovery == "" && !r.stopping
}

func (r *runner) snapshotLocked() StatusInfo {
	s := r.status
	if !s.Exited && !r.freshLocked() {
		s.Ready = false
	}
	return s
}

func (r *runner) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *runner) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+r.reg.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if req.URL.Path == "/status" && req.Method == http.MethodGet {
		r.mu.Lock()
		state := r.snapshotLocked()
		r.mu.Unlock()
		writeResponse(w, state)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if req.URL.Path == "/state" || req.URL.Path == "/exiting" {
		var state sessionState
		if err := decodeJSON(req.Body, &state); err != nil {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		r.handleState(w, req.URL.Path, state)
		return
	}
	var ctl control
	if err := decodeJSON(req.Body, &ctl); err != nil || ctl.Generation == "" || len(ctl.Generation) > 1024 {
		http.Error(w, "invalid control", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping || r.status.Recovery != "" {
		http.Error(w, "runner requires manual recovery", http.StatusConflict)
		return
	}
	switch req.URL.Path {
	case "/prepare":
		if r.status.Generation != "" && r.status.Generation != ctl.Generation {
			http.Error(w, "generation mismatch", http.StatusConflict)
			return
		}
		if r.released {
			http.Error(w, "previous generation is restarting", http.StatusConflict)
			return
		}
		if err := r.gate(); err != nil {
			r.status.Ready = false
			r.status.Reason = err.Error()
			writeResponse(w, r.snapshotLocked())
			return
		}
		r.status.Generation = ctl.Generation
	case "/exit":
		if r.status.Generation != ctl.Generation || r.released {
			http.Error(w, "generation mismatch", http.StatusConflict)
			return
		}
		if !r.status.Exited {
			if !r.freshLocked() {
				http.Error(w, "session no longer ready", http.StatusConflict)
				return
			}
			if err := r.gate(); err != nil {
				r.status.Ready = false
				r.status.Reason = err.Error()
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			r.exitRequested = true
		}
	case "/release":
		if r.status.Generation != ctl.Generation {
			http.Error(w, "generation mismatch", http.StatusConflict)
			return
		}
		if ctl.Committed && (!r.status.Exited || !r.exitAcknowledged) {
			http.Error(w, "committed release requires confirmed child exit", http.StatusConflict)
			return
		}
		if r.released && r.committed != ctl.Committed {
			http.Error(w, "release decision changed", http.StatusConflict)
			return
		}
		r.released = true
		r.committed = ctl.Committed
		if !r.exitAcknowledged && !r.status.Exited {
			r.status.Generation = ""
			r.exitRequested = false
			r.released = false
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := r.persistLocked(); err != nil {
		r.failLocked(err)
		http.Error(w, "cannot persist runner state", http.StatusInternalServerError)
		return
	}
	writeResponse(w, r.snapshotLocked())
	// The lifecycle worker may wait for claude.lock, but this handler never
	// waits for that worker: the coordinator itself owns the lock during release.
	r.notify()
}

func (r *runner) handleState(w http.ResponseWriter, route string, state sessionState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping || r.status.Exited || r.status.ChildPID == 0 || r.status.Recovery != "" {
		http.Error(w, "child unavailable", http.StatusConflict)
		return
	}
	if len(state.SessionID) > 256 || len(state.CWD) > 4096 || len(state.Reason) > 4096 || (state.Ready && (state.SessionID == "" || !filepath.IsAbs(state.CWD))) {
		http.Error(w, "invalid session identity", http.StatusBadRequest)
		return
	}
	if route == "/exiting" {
		if !r.exitRequested || r.released || r.exitAcknowledged || state.Generation != r.status.Generation || state.Generation == "" || !state.Ready || !r.freshLocked() || state.SessionID != r.status.SessionID || state.CWD != r.status.CWD {
			http.Error(w, "exit no longer authorized", http.StatusConflict)
			return
		}
		if err := r.gate(); err != nil {
			r.status.Ready = false
			r.status.Reason = err.Error()
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		r.status.NeedsContinue = state.NeedsContinue
		r.exitAcknowledged = true
		if err := r.persistLocked(); err != nil {
			r.exitAcknowledged = false
			r.failLocked(err)
			http.Error(w, "cannot persist exit acknowledgement", http.StatusInternalServerError)
			return
		}
		writeResponse(w, action{Action: "exit", Generation: r.status.Generation})
		return
	}
	if r.exitAcknowledged {
		r.exitAcknowledged = false
		r.exitRequested = false
		if r.released {
			r.status.Generation = ""
			r.released = false
			r.committed = false
		}
	}
	r.status.SessionID = state.SessionID
	r.status.CWD = state.CWD
	r.status.Ready = state.Ready
	r.status.NeedsContinue = state.NeedsContinue
	r.status.Reason = state.Reason
	r.lastState = time.Now()
	response := action{Action: "run"}
	if r.status.Generation != "" {
		response.Generation = r.status.Generation
		response.Action = "prepare"
		if r.exitRequested && r.freshLocked() {
			response.Action = "exit"
		}
	}
	if err := r.persistLocked(); err != nil {
		r.failLocked(err)
		http.Error(w, "cannot persist session state", http.StatusInternalServerError)
		return
	}
	writeResponse(w, response)
}

func (r *runner) restart(ctx context.Context, root string) (*exec.Cmd, error) {
	for {
		lock, err := acquireClaudeLock(ctx, r.options.DataDir)
		if err != nil {
			return nil, err
		}
		var txn transaction
		err = readJSON(filepath.Join(root, "pending.json"), &txn)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			lock.Close()
			return nil, err
		}
		if txn.Generation == "" {
			cmd, startErr := r.startLocked(ctx, true)
			lock.Close()
			return cmd, startErr
		}
		lock.Close()
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *runner) startLocked(ctx context.Context, resume bool) (*exec.Cmd, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil || r.stopping {
		return nil, context.Canceled
	}
	id := ""
	continuationState := r.options.InitialContinuation
	continuePrompt := false
	if resume {
		if !r.status.Exited || !r.released || !r.exitAcknowledged {
			return nil, errors.New("restart was not released")
		}
		// Attachment after exit is still a blocker: never inject input into a
		// terminal somebody has returned to while the account switch was running.
		if err := r.gate(); err != nil {
			return nil, err
		}
		id = r.status.SessionID
		continuePrompt = r.committed && r.status.NeedsContinue
		continuationState = "idle"
		if r.status.NeedsContinue {
			continuationState = "continue"
		}
	}
	args, err := parseArgs(r.options.Args, id, continuePrompt)
	if err != nil {
		return nil, err
	}
	args = append([]string{"--plugin-dir", filepath.Join(r.dir, "plugin")}, args...)
	cmd := exec.Command(r.options.ClaudeBin, args...)
	cmd.Dir = r.status.CWD
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "QSWITCH_CONTROL_URL" && key != "QSWITCH_CONTROL_TOKEN" && key != "QSWITCH_INITIAL_CONTINUATION" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "QSWITCH_CONTROL_URL="+r.reg.URL, "QSWITCH_CONTROL_TOKEN="+r.reg.Token, "QSWITCH_INITIAL_CONTINUATION="+continuationState)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	r.status.ChildPID = cmd.Process.Pid
	r.status.Exited = false
	r.status.Ready = false
	r.status.Generation = ""
	r.status.Reason = "waiting for Claude session safety state"
	r.lastState = time.Time{}
	r.exitRequested, r.exitAcknowledged, r.released, r.committed = false, false, false, false
	if err := r.persistLocked(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	if err := atomicJSON(filepath.Join(r.dir, "registration.json"), r.reg); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return cmd, nil
}

// Run keeps Claude attached to the caller's real terminal. Only a verified
// detached tmux session may participate in unattended account rotation.
func Run(ctx context.Context, options Options) error {
	if options.ClaudeBin == "" {
		options.ClaudeBin = "claude"
	}
	if err := validateEnvironment(); err != nil {
		return err
	}
	if _, err := parseArgs(options.Args, "", false); err != nil {
		return err
	}
	if options.InitialContinuation != "" && options.InitialContinuation != "idle" && options.InitialContinuation != "continue" {
		return errors.New("resume continuation must be idle or continue")
	}
	if adoptedSession(options.Args) && options.InitialContinuation == "" {
		return errors.New("adopting a transcript requires --resume-continuation=idle|continue")
	}
	if options.InitialContinuation == "" {
		options.InitialContinuation = "idle"
	}
	versionCtx, cancelVersion := context.WithTimeout(ctx, 5*time.Second)
	version, err := exec.CommandContext(versionCtx, options.ClaudeBin, "--version").Output()
	cancelVersion()
	if err != nil {
		return fmt.Errorf("read Claude version: %w", err)
	}
	if !supportedVersion(string(version)) {
		return errors.New("managed Claude requires Claude Code 2.1.312 or newer")
	}
	restore, err := terminalRestore()
	if err != nil {
		return err
	}
	defer restore()
	root, err := rootDir(options.DataDir)
	if err != nil {
		return err
	}
	options.DataDir, err = filepath.Abs(options.DataDir)
	if err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	token, err := randomID()
	if err != nil {
		return err
	}
	startupLock, err := acquireClaudeLock(ctx, options.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if startupLock != nil {
			_ = startupLock.Close()
		}
	}()
	var pending transaction
	if err := readJSON(filepath.Join(root, "pending.json"), &pending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if pending.Generation != "" {
		return errors.New("managed Claude rotation is pending; retry after it finishes")
	}
	dir := filepath.Join(root, id)
	if err := privateDir(dir); err != nil {
		return err
	}
	lease, err := privateFile(filepath.Join(dir, "lease"), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	defer lease.Close()
	if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		os.RemoveAll(dir)
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := extractPlugin(dir); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	r := &runner{dir: dir, options: options, pane: os.Getenv("TMUX_PANE"), tmux: os.Getenv("TMUX"), wake: make(chan struct{}, 1)}
	r.reg = registration{ID: id, PID: os.Getpid(), URL: "http://" + listener.Addr().String(), Token: token}
	r.status = StatusInfo{ID: id, RunnerPID: os.Getpid(), CWD: cwd, Live: true, NeedsContinue: options.InitialContinuation == "continue"}
	r.gate = func() error { return unattendedTMUX(r.pane, r.tmux) }
	server := &http.Server{Handler: r, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 4096}
	defer server.Close()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	// Parent and child share the terminal's foreground process group. Catching
	// SIGINT here leaves Ctrl+C delivery to Claude intact and keeps the runner.
	interrupts := make(chan os.Signal, 8)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	runCtx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	cmd, err := r.startLocked(runCtx, false)
	if err != nil {
		return err
	}
	_ = startupLock.Close()
	startupLock = nil
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	childRunning := true
	for {
		select {
		case <-interrupts:
			// Claude receives this signal directly from the shared terminal.
		case err := <-serverErrors:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				cancel()
			}
		case <-runCtx.Done():
			r.mu.Lock()
			r.stopping = true
			r.status.Ready = false
			if r.status.Generation != "" {
				r.failLocked(errors.New("runner stopped during rotation"))
				keep = true
			}
			_ = r.persistLocked()
			r.mu.Unlock()
			if childRunning {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				timer := time.NewTimer(5 * time.Second)
				select {
				case <-childDone:
					timer.Stop()
				case <-timer.C:
					_ = cmd.Process.Kill()
					<-childDone
				}
			}
			restore()
			return runCtx.Err()
		case childErr := <-childDone:
			childRunning = false
			restore()
			r.mu.Lock()
			r.status.Exited = true
			r.status.Ready = false
			managedExit := r.exitAcknowledged && r.status.Generation != "" && childErr == nil
			if !managedExit {
				if r.status.Generation != "" || childErr != nil {
					r.failLocked(errors.New("Claude exited without a confirmed successful rotation exit"))
					keep = true
				}
				persistErr := r.persistLocked()
				r.mu.Unlock()
				if childErr != nil {
					return childErr
				}
				return persistErr
			}
			if err := r.persistLocked(); err != nil {
				r.failLocked(err)
				keep = true
				r.mu.Unlock()
				return err
			}
			r.mu.Unlock()
			r.notify()
		case <-r.wake:
			r.mu.Lock()
			restart := r.status.Exited && r.released && r.exitAcknowledged
			r.mu.Unlock()
			if !restart {
				continue
			}
			cmd, err = r.restart(runCtx, root)
			if err != nil {
				r.mu.Lock()
				r.failLocked(err)
				_ = r.persistLocked()
				r.mu.Unlock()
				keep = true
				return err
			}
			childRunning = true
			go func(child *exec.Cmd) { childDone <- child.Wait() }(cmd)
		}
	}
}
