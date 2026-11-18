package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
)

func TestMain(m *testing.M) {
	if os.Getenv("QSWITCH_SYNTHETIC_AGY_CHILD") == "1" {
		os.Exit(syntheticCLIChild())
	}
	os.Exit(m.Run())
}

// This subprocess implements only the startup contract, using synthetic RPC
// data and a fresh TLS listener. It never loads any native credentials.
func syntheticCLIChild() int {
	if os.Getenv("CLOUD_CODE_URL") != os.Getenv("QSWITCH_SYNTHETIC_CLOUD_CODE_URL") {
		return 82
	}
	var csrf, logPath string
	var forwarded []string
	for _, arg := range os.Args[1:] {
		switch {
		case strings.HasPrefix(arg, "--csrf_token="):
			csrf = strings.TrimPrefix(arg, "--csrf_token=")
		case strings.HasPrefix(arg, "--log-file="):
			logPath = strings.TrimPrefix(arg, "--log-file=")
		default:
			forwarded = append(forwarded, arg)
		}
	}
	if len(csrf) != 64 || logPath == "" {
		return 80
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Codeium-Csrf-Token") != csrf {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 81
	}
	// A number unrelated to the process PID mirrors the native logging format.
	_, _ = fmt.Fprintf(log, "I0101 00:00:00.000000 17 server.go:1] Language server listening on random port at %s for HTTPS (gRPC)\n", port)
	_ = log.Close()
	_ = json.NewEncoder(os.Stdout).Encode(forwarded)
	_ = bufio.NewScanner(os.Stdin).Scan()
	code, _ := strconv.Atoi(os.Getenv("QSWITCH_SYNTHETIC_AGY_EXIT"))
	return code
}

func TestLauncherRejectsManagedAndAlternateIdentityArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--csrf_token=do-not-echo"}, {"--model", "synthetic-model", "-csrf_token", "do-not-echo"},
		{"--log-file=/private/test"}, {"--app_data_dir=other"}, {"--gemini_dir=other"},
		{"--settings", `{"customModels":[]}`}, {"--model", "synthetic-model", "--settings={}"},
		{"--hub"}, {"--hub-port=41001"}, {"--remote-control"}, {"--remote-agent"},
		{"--use_host_auth"}, {"--host_bridge_url=http://localhost"}, {"--read_host_bridge_token_from_stdin"},
		{"--cloud_code_endpoint=https://cloudcode-pa.googleapis.com"}, {"--cloud-code-endpoint", "https://cloudcode-pa.googleapis.com"},
		{"synthetic request", "--cloud_code_endpoint=https://cloudcode-pa.googleapis.com"},
		{"acp"}, {"remote-control", "start"}, {"--model", "synthetic-model", "acp"},
	} {
		if err := validateCLIArgs(args); err == nil || strings.Contains(err.Error(), "do-not-echo") {
			t.Fatalf("unsafe args accepted or echoed: %v", err)
		}
	}
	if err := validateCLIArgs([]string{"--model", "synthetic-model", "--conversation", "synthetic-conversation", "--print", "synthetic request", "--effort=max"}); err != nil {
		t.Fatalf("normal native arguments rejected: %v", err)
	}
	for _, args := range [][]string{{"--", "--hub"}, {"--print", "--hub"}, {"--prompt-interactive=--hub"}, {"--print", "--cloud_code_endpoint=synthetic prompt"}} {
		if err := validateCLIArgs(args); err != nil {
			t.Fatalf("native prompt text mistaken for a control flag: %v", err)
		}
	}
}

func TestLauncherRejectsUnsupportedCloudEndpointBeforeStartingChild(t *testing.T) {
	for _, endpoint := range []string{"https://private.invalid/secret", "https://daily-cloudcode-pa.googleapis.com?secret=synthetic", "https://cloudcode-pa.googleapis.com:443", " "} {
		t.Run(strconv.Itoa(len(endpoint)), func(t *testing.T) {
			t.Setenv("CLOUD_CODE_URL", endpoint)
			data := t.TempDir()
			err := runCLI(context.Background(), data, "must-not-start", nil, nil, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "CLOUD_CODE_URL") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe endpoint was started or exposed: %v", err)
			}
			entries, err := os.ReadDir(data)
			if err != nil || len(entries) != 0 {
				t.Fatal("invalid endpoint created startup metadata")
			}
		})
	}
}

func TestCLICloudEndpointMatchesInheritedEnvironmentSemantics(t *testing.T) {
	const daily = "https://daily-cloudcode-pa.googleapis.com"
	const production = "https://cloudcode-pa.googleapis.com"
	mixedCase := daily
	if runtime.GOOS == "windows" {
		mixedCase = production
	}
	for _, test := range []struct {
		name string
		env  []string
		want string
	}{
		{"absent", nil, daily},
		{"explicit empty", []string{"CLOUD_CODE_URL="}, daily},
		{"override", []string{"CLOUD_CODE_URL=" + production}, production},
		{"mixed case", []string{"Cloud_Code_Url=" + production}, mixedCase},
		{"last wins", []string{"CLOUD_CODE_URL=" + daily, "CLOUD_CODE_URL=" + production}, production},
		{"mixed case last wins on Windows", []string{"CLOUD_CODE_URL=" + daily, "Cloud_Code_Url=" + production}, mixedCase},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := cliCloudCodeEndpoint(test.env)
			if err != nil || got != test.want {
				t.Fatalf("environment/record mismatch: got=%q want=%q err=%v", got, test.want, err)
			}
		})
	}
}

func TestLauncherRejectsNativeDirectoryAndIdentityEnvironmentOverrides(t *testing.T) {
	for _, key := range []string{"ANTIGRAVITY_GEMINI_DIR", "ANTIGRAVITY_APP_DATA_DIR", "ANTIGRAVITY_CONFIG_REL_DIR", "ANTIGRAVITY_CREDENTIAL_SCOPE"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "synthetic-private-value")
			err := runCLI(context.Background(), t.TempDir(), "not-started", nil, nil, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "synthetic-private-value") {
				t.Fatalf("native override was not safely rejected: %v", err)
			}
		})
	}
}

func TestLauncherReadsOnlyBoundedCompleteHTTPSLines(t *testing.T) {
	var tail cliLogTail
	port, err := tail.readPort(strings.NewReader("private log text\nLanguage server listening on random port at 41001 for HTTP\nLanguage server listening on random port at 41002 for HTTPS (g"))
	if err != nil || port != 0 {
		t.Fatalf("partial or HTTP line accepted: %d %v", port, err)
	}
	port, err = tail.readPort(strings.NewReader("RPC)\n"))
	if err != nil || port != 41002 {
		t.Fatalf("split HTTPS line missed: %d %v", port, err)
	}
	port, err = tail.readPort(strings.NewReader("Language server listening on random port at 99999 for HTTPS (gRPC)\n"))
	if err != nil || port != 0 {
		t.Fatal("invalid port accepted")
	}
	long := strings.NewReader(strings.Repeat("x", 128<<10))
	_, _ = tail.readPort(long)
	if long.Len() != 64<<10 || len(tail.partial) > 4096 {
		t.Fatal("startup log read or partial line was unbounded")
	}
}

func TestLauncherRegistersNativeRPCAndCleansUpOnChildExit(t *testing.T) {
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "agy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, raw, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QSWITCH_SYNTHETIC_AGY_CHILD", "1")
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Setenv("QSWITCH_SYNTHETIC_AGY_EXIT", strconv.Itoa(code))
			cloudURL, expected := "", "https://daily-cloudcode-pa.googleapis.com"
			if code != 0 {
				cloudURL, expected = "https://cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"
			}
			t.Setenv("CLOUD_CODE_URL", cloudURL)
			t.Setenv("QSWITCH_SYNTHETIC_CLOUD_CODE_URL", cloudURL)
			dataDir, home := t.TempDir(), t.TempDir()
			input, feed := io.Pipe()
			defer input.Close()
			defer feed.Close()
			var output, diagnostics bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			args := []string{"--model", "synthetic-model", "--conversation", "synthetic-session"}
			go func() { finished <- runCLI(ctx, dataDir, binary, args, input, &output, &diagnostics) }()
			var endpoint Endpoint
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				paths, _ := filepath.Glob(filepath.Join(cliRecordDir(dataDir), "*.json"))
				if len(paths) == 1 {
					body, err := readDaemonRecord(paths[0])
					if err != nil {
						t.Fatal(err)
					}
					var record struct {
						PID               int    `json:"pid"`
						HTTPSPort         int    `json:"httpsPort"`
						CSRF              string `json:"csrfToken"`
						CloudCodeEndpoint string `json:"cloudCodeEndpoint"`
					}
					if err := json.Unmarshal(body, &record); err != nil {
						t.Fatal(err)
					}
					endpoint = Endpoint{PID: record.PID, HTTPSPort: record.HTTPSPort, CSRF: record.CSRF, CloudCodeEndpoint: record.CloudCodeEndpoint}
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if endpoint.PID <= 0 || endpoint.PID == os.Getpid() {
				t.Fatal("native child was not registered")
			}
			if endpoint.CloudCodeEndpoint != expected {
				t.Fatal("managed record lost the child's actual quota endpoint")
			}
			endpoints, err := DiscoverManaged(home, dataDir, func() ([]adapter.Proc, error) {
				return []adapter.Proc{{PID: endpoint.PID, Command: `"` + binary + `"`}}, nil
			})
			if err != nil || len(endpoints) != 1 || endpoints[0] != endpoint {
				t.Fatalf("managed native endpoint not discovered: count=%d err=%v", len(endpoints), err)
			}
			cloud, err := CloudCodeEndpoint(home, dataDir, func() ([]adapter.Proc, error) {
				return []adapter.Proc{{PID: endpoint.PID, Command: `"` + binary + `"`}}, nil
			})
			if err != nil || cloud != expected {
				t.Fatalf("managed CLI without desktop flags lost its quota channel: %q %v", cloud, err)
			}
			if idle, err := endpoint.Idle(ctx, 30*time.Second); err != nil || !idle {
				t.Fatalf("managed native RPC failed: idle=%t err=%v", idle, err)
			}
			_, _ = io.WriteString(feed, "\n")
			_ = feed.Close()
			err = <-finished
			if code == 0 && err != nil {
				t.Fatal(err)
			}
			if code != 0 {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != code {
					t.Fatalf("native exit status not preserved: %v", err)
				}
			}
			var forwarded []string
			if json.Unmarshal(output.Bytes(), &forwarded) != nil || strings.Join(forwarded, "|") != strings.Join(args, "|") {
				t.Fatal("native user arguments were changed")
			}
			if diagnostics.Len() != 0 {
				t.Fatal("unexpected launcher diagnostics")
			}
			files, _ := os.ReadDir(cliRecordDir(dataDir))
			if len(files) != 0 {
				t.Fatal("private CLI metadata remained after child exit")
			}
		})
	}
}
