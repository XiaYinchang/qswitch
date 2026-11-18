package antigravity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func cliRecordDir(dataDir string) string { return filepath.Join(dataDir, "antigravity-cli") }

// The official CLI's daemon initialization selects daily Cloud Code; its
// CodeAssist client gives CLOUD_CODE_URL precedence. This is separate from
// CLIAuthProvider's OAuth endpoint, which must not be changed here.
func cliCloudCodeEndpoint(environment []string) (string, error) {
	endpoint := ""
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && (key == "CLOUD_CODE_URL" || runtime.GOOS == "windows" && strings.EqualFold(key, "CLOUD_CODE_URL")) {
			endpoint = value
		}
	}
	if endpoint == "" {
		endpoint = "https://daily-cloudcode-pa.googleapis.com"
	}
	if endpoint, err := allowedCloudEndpoint(endpoint); err == nil {
		return endpoint, nil
	}
	return "", errors.New("qswitch run antigravity: CLOUD_CODE_URL 必须使用受支持的官方额度后端")
}

// RunCLI starts the official CLI in the current terminal. It adds only native
// RPC authentication and a private startup log; prompts and terminal input go
// directly to agy. Existing unmanaged CLI processes are never taken over.
func RunCLI(ctx context.Context, dataDir, binary string, args []string) error {
	return runCLI(ctx, dataDir, binary, args, os.Stdin, os.Stdout, os.Stderr)
}

func validateCLIArgs(args []string) error {
	positional := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break // The rest is native positional input, not flags.
		}
		if !strings.HasPrefix(arg, "-") {
			// The official CLI recognizes these maintenance subcommands.
			if !positional {
				switch arg {
				case "acp", "agent", "agents", "changelog", "help", "install", "mcp", "mic-serve", "models", "plugin", "plugins", "remote-control", "update":
					return errors.New("run Antigravity maintenance subcommands directly with agy; qswitch run antigravity starts a local session")
				}
			}
			positional = true
			continue // Native flags after positional input still need validation.
		}
		name := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
		switch name {
		case "csrf_token", "log-file", "app_data_dir", "gemini_dir", "hub", "hub-port", "remote-agent", "remote-control", "remote-control-name", "host_bridge_url", "read_host_bridge_token_from_stdin", "use_host_auth", "bg-updater", "settings", "cloud_code_endpoint", "cloud-code-endpoint":
			return fmt.Errorf("qswitch run antigravity does not support --%s; it manages the local shared consumer account", name)
		case "print", "p", "prompt", "print-timeout", "output-format", "input-format", "json-schema", "model", "effort", "agent", "conversation", "prompt-interactive", "i", "mode", "add-dir", "project", "release_base_url":
			// These exact native flags take a value, which can itself start
			// with '--' (notably a print prompt). Do not interpret that value.
			if !strings.Contains(arg, "=") {
				i++
			}
		}
	}
	return nil
}

func runCLI(ctx context.Context, dataDir, binary string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := validateCLIArgs(args); err != nil {
		return err
	}
	for _, key := range []string{"ANTIGRAVITY_GEMINI_DIR", "ANTIGRAVITY_APP_DATA_DIR", "ANTIGRAVITY_CONFIG_REL_DIR", "ANTIGRAVITY_CREDENTIAL_SCOPE"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return fmt.Errorf("qswitch run antigravity does not support %s overrides; it manages the default shared consumer account", key)
		}
	}
	if dataDir == "" {
		return errors.New("antigravity CLI requires a qswitch data directory")
	}
	// Record exactly the environment inherited by this child, even if another
	// goroutine changes the launcher's environment before cmd.Start.
	environment := os.Environ()
	cloudEndpoint, err := cliCloudCodeEndpoint(environment)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if binary == "" {
		var err error
		binary, err = exec.LookPath("agy")
		if err != nil {
			return errors.New("antigravity CLI is not installed or agy is not on PATH")
		}
	}
	var nonce [48]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("cannot generate Antigravity CLI RPC authentication")
	}
	csrf := hex.EncodeToString(nonce[:32])
	dir := cliRecordDir(dataDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot create Antigravity CLI metadata directory")
	}
	base := filepath.Join(dir, "run-"+hex.EncodeToString(nonce[32:]))
	logPath, recordPath := base+".log", base+".json"
	logFile, err := createPrivateMetadataFile(logPath)
	if err != nil {
		return errors.New("cannot create private Antigravity CLI log")
	}
	if err := logFile.Close(); err != nil {
		_ = os.Remove(logPath)
		return errors.New("cannot prepare private Antigravity CLI log")
	}
	defer func() {
		for _, path := range []string{recordPath, recordPath + ".pending", logPath} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				_, _ = fmt.Fprintln(stderr, "qswitch: could not remove an Antigravity CLI temporary file")
			}
		}
	}()
	// Prefix the controlled flags and reject duplicates anywhere in user args.
	// Never format cmd.Args or this command's diagnostic output into errors.
	commandArgs := append([]string{"--csrf_token=" + csrf, "--log-file=" + logPath}, args...)
	cmd := exec.CommandContext(ctx, binary, commandArgs...)
	cmd.Env = environment
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	configureCLICommand(cmd)
	// Both processes share the console. agy receives Ctrl+C itself; the parent
	// stays alive to retain its endpoint record while agy handles cancellation.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Start(); err != nil {
		return errors.New("could not start the official Antigravity CLI")
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	reader, err := os.Open(logPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "qswitch: CLI endpoint is unavailable; automatic switching stays blocked")
		return <-exited
	}
	defer reader.Close()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var tail cliLogTail
	port, registeredPort := 0, 0
	warned := false
	for {
		select {
		case err := <-exited:
			return err
		case <-ticker.C:
			next, err := tail.readPort(reader)
			if next > 0 {
				port = next
			}
			if err != nil && !warned {
				_, _ = fmt.Fprintln(stderr, "qswitch: CLI startup metadata could not be read; automatic switching stays blocked")
				warned = true
			}
			if port == 0 || port == registeredPort {
				continue
			}
			endpoint := Endpoint{PID: cmd.Process.Pid, HTTPSPort: port, CSRF: csrf, CloudCodeEndpoint: cloudEndpoint}
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = endpoint.Call(probe, "GetAuthStatus", struct{}{}, nil)
			cancel()
			if err != nil {
				continue // First login can take minutes; never stop the native UI.
			}
			if err := writeCLIRecord(recordPath, endpoint); err != nil {
				if !warned {
					_, _ = fmt.Fprintln(stderr, "qswitch: CLI endpoint could not be registered; automatic switching stays blocked")
					warned = true
				}
				continue
			}
			registeredPort = port
		}
	}
}

var cliHTTPSListener = regexp.MustCompile(`Language server listening on random port at ([0-9]{1,5}) for HTTPS \(gRPC\)`)

type cliLogTail struct{ partial string }

// Read only bounded chunks of the exclusive private startup log. The number
// in the native log prefix is not the OS PID; ownership is cmd.Process.Pid and
// a successful RPC with the random CSRF supplied only to that child.
func (t *cliLogTail) readPort(reader io.Reader) (int, error) {
	var buffer [64 << 10]byte
	n, err := reader.Read(buffer[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, errors.New("cannot read native startup log")
	}
	text := t.partial + string(buffer[:n])
	lines := strings.Split(text, "\n")
	t.partial = lines[len(lines)-1]
	if len(t.partial) > 4096 {
		t.partial = t.partial[len(t.partial)-4096:]
	}
	port := 0
	for _, line := range lines[:len(lines)-1] {
		match := cliHTTPSListener.FindStringSubmatch(line)
		if len(match) == 2 {
			value, _ := strconv.Atoi(match[1])
			if value > 0 && value <= 65535 {
				port = value
			}
		}
	}
	return port, nil
}

func writeCLIRecord(path string, endpoint Endpoint) error {
	file, err := createPrivateMetadataFile(path + ".pending")
	if err != nil {
		return errors.New("cannot create private CLI endpoint")
	}
	defer os.Remove(path + ".pending")
	err = json.NewEncoder(file).Encode(struct {
		PID               int    `json:"pid"`
		HTTPSPort         int    `json:"httpsPort"`
		CSRF              string `json:"csrfToken"`
		CloudCodeEndpoint string `json:"cloudCodeEndpoint,omitempty"`
	}{endpoint.PID, endpoint.HTTPSPort, endpoint.CSRF, endpoint.CloudCodeEndpoint})
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot write private CLI endpoint")
	}
	if err := os.Rename(path+".pending", path); err != nil {
		return errors.New("cannot publish private CLI endpoint")
	}
	return nil
}
