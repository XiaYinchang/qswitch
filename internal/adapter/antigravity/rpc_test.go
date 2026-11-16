package antigravity

import (
	"context"
	"encoding/json"
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
	"sync/atomic"
	"testing"
	"time"

	"qswitch/internal/adapter"
)

func rpcTestEndpoint(t *testing.T, handler http.HandlerFunc) Endpoint {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return Endpoint{PID: os.Getpid(), HTTPSPort: n, CSRF: "synthetic-csrf-value"}
}

func writeRPCTestRecord(t *testing.T, home, directory, name string, pid, port int) string {
	t.Helper()
	dir := filepath.Join(home, ".gemini", directory, "daemon")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	raw, _ := json.Marshal(map[string]any{"pid": pid, "httpsPort": port, "httpPort": port + 1, "lsVersion": "test-version", "csrfToken": "synthetic-csrf-value"})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// Go's mode bits do not create a private Windows DACL. These fixtures
		// explicitly model native metadata owned and readable by this user.
		rpcTestWindowsACL(t, path, false)
	}
	return path
}

func rpcTestWindowsACL(t *testing.T, path string, public bool) {
	t.Helper()
	const script = `$ErrorActionPreference='Stop'; $sid=[System.Security.Principal.WindowsIdentity]::GetCurrent().User; $acl=[System.Security.AccessControl.FileSecurity]::new(); $acl.SetOwner($sid); $acl.SetAccessRuleProtection($true,$false); $acl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($sid,'FullControl','Allow')); if($env:QSWITCH_TEST_PUBLIC -eq 'true'){$acl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new([System.Security.Principal.SecurityIdentifier]::new('S-1-1-0'),'Read','Allow'))}; Set-Acl -LiteralPath $env:QSWITCH_TEST_RECORD -AclObject $acl`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "QSWITCH_TEST_RECORD="+path, "QSWITCH_TEST_PUBLIC="+strconv.FormatBool(public))
	if err := cmd.Run(); err != nil {
		t.Fatalf("prepare synthetic Windows metadata ACL: %v", err)
	}
}

func TestRPCDiscoverNativeDesktopAndCLIRecords(t *testing.T) {
	home := t.TempDir()
	writeRPCTestRecord(t, home, "antigravity", "hub_41001.json", 101, 41001)
	writeRPCTestRecord(t, home, "antigravity-cli", "hub_41002.json", 102, 41002)
	writeRPCTestRecord(t, home, "antigravity-cli", "hub_stale.json", 103, 41003)
	writeRPCTestRecord(t, home, "antigravity-cli", "hub_wrong_process.json", 104, 41004)
	endpoints, err := Discover(home, func() ([]adapter.Proc, error) {
		return []adapter.Proc{
			{PID: 101, Command: "/Applications/Antigravity.app/Contents/Resources/bin/language_server --hub"},
			{PID: 102, Command: "/synthetic/.local/bin/agy daemon start"},
			{PID: 104, Command: "/bin/sh -c agy"},
		}, nil
	})
	if err != nil || len(endpoints) != 2 {
		t.Fatalf("discover count=%d err=%v", len(endpoints), err)
	}
	if endpoints[0].PID != 101 || endpoints[1].PID != 102 {
		t.Fatalf("wrong native process selection: %v", endpoints)
	}
}

func TestRPCDiscoverRequiresRecordForActiveNativeBackend(t *testing.T) {
	for _, command := range []string{"/bin/agy", `"C:\Program Files\Antigravity\language_server.exe" --hub`} {
		endpoints, err := Discover(t.TempDir(), func() ([]adapter.Proc, error) { return []adapter.Proc{{PID: 101, Command: command}}, nil })
		if err == nil || len(endpoints) != 0 {
			t.Fatalf("active backend with no discovery was accepted: %v %v", endpoints, err)
		}
	}
	endpoints, err := Discover(t.TempDir(), func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil || len(endpoints) != 0 {
		t.Fatalf("no native process should be an empty discovery: %v %v", endpoints, err)
	}
}

func TestRPCDiscoveryRefusesUntrustedFiles(t *testing.T) {
	for _, mode := range []string{"public", "symlink", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			if runtime.GOOS == "windows" && mode == "symlink" {
				t.Skip("unprivileged Windows symlink case")
			}
			home := t.TempDir()
			path := writeRPCTestRecord(t, home, "antigravity", "hub_41001.json", 101, 41001)
			switch mode {
			case "public":
				if runtime.GOOS == "windows" {
					rpcTestWindowsACL(t, path, true)
				} else if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte(`{"csrfToken":"do-not-echo"`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Discover(home, func() ([]adapter.Proc, error) {
				return []adapter.Proc{{PID: 101, Command: "/bin/language_server"}}, nil
			})
			if err == nil {
				t.Fatal("unsafe discovery record accepted")
			}
			if strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("discovery error leaked record contents")
			}
		})
	}
}

func TestRPCCallUsesNativeLoopbackProtocolAndSafeFormatting(t *testing.T) {
	endpoint := rpcTestEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/exa.language_server_pb.LanguageServerService/GetUserStatus" {
			t.Errorf("unexpected method/path: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Connect-Protocol-Version") != "1" || r.Header.Get("X-Codeium-Csrf-Token") != "synthetic-csrf-value" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("native RPC headers missing")
		}
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != "{}" {
			t.Errorf("unexpected body shape: %s", raw)
		}
		_, _ = io.WriteString(w, `{"userStatus":{"email":"synthetic@example.test"}}`)
	})
	email, err := endpoint.UserEmail(context.Background())
	if err != nil || email != "synthetic@example.test" {
		t.Fatalf("email=%q err=%v", email, err)
	}
	raw, _ := json.Marshal(endpoint)
	for _, formatted := range []string{fmt.Sprintf("%v", endpoint), fmt.Sprintf("%+v", endpoint), fmt.Sprintf("%#v", endpoint), string(raw)} {
		if strings.Contains(formatted, endpoint.CSRF) {
			t.Fatal("endpoint formatting leaked CSRF")
		}
	}
}

func TestRPCCallNeverFollowsRedirectOrLeaksBody(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	endpoint := rpcTestEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(307)
		_, _ = io.WriteString(w, "secret-response-body")
	})
	err := endpoint.Call(context.Background(), "GetUserStatus", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || strings.Contains(err.Error(), "secret-response-body") || destinationCalls.Load() != 0 {
		t.Fatalf("redirect/body handling: err=%v calls=%d", err, destinationCalls.Load())
	}
	if err := endpoint.Call(context.Background(), "../OtherMethod", nil, nil); err == nil {
		t.Fatal("accepted path traversal")
	}
}

func TestRPCActivityRequiresKnownIdleAndNoBackgroundWork(t *testing.T) {
	now := time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
	idle := func(extra map[string]any) map[string]any {
		m := map[string]any{"status": "CASCADE_RUN_STATUS_IDLE"}
		for k, v := range extra {
			m[k] = v
		}
		return map[string]any{"trajectorySummaries": map[string]any{"synthetic-id": m}}
	}
	for _, test := range []struct {
		name  string
		value any
		want  bool
	}{
		{"omitted proto3 empty map", map[string]any{}, true},
		{"explicit empty map", map[string]any{"trajectorySummaries": map[string]any{}}, true},
		{"null response", nil, false},
		{"unknown without map", map[string]any{"unknown": true}, false},
		{"null map", map[string]any{"trajectorySummaries": nil}, false},
		{"idle", idle(nil), true},
		{"running", idle(map[string]any{"status": "CASCADE_RUN_STATUS_RUNNING"}), false},
		{"busy", idle(map[string]any{"status": "CASCADE_RUN_STATUS_BUSY"}), false},
		{"canceling", idle(map[string]any{"status": "CASCADE_RUN_STATUS_CANCELING"}), false},
		{"unknown status", idle(map[string]any{"status": "FUTURE_STATUS"}), false},
		{"not fully idle", idle(map[string]any{"notFullyIdle": true}), false},
		{"active children", idle(map[string]any{"hasActiveChildren": true}), false},
		{"running agents", idle(map[string]any{"hasRunningSubagents": true}), false},
		{"background task", idle(map[string]any{"backgroundTaskCount": 1}), false},
		{"recent input", idle(map[string]any{"lastUserInputTime": now.Add(-10 * time.Second).Format(time.RFC3339)}), false},
		{"future input", idle(map[string]any{"lastUserInputTime": now.Add(time.Second).Format(time.RFC3339)}), false},
		{"old input", idle(map[string]any{"lastUserInputTime": now.Add(-time.Minute).Format(time.RFC3339)}), true},
		{"old proto input", idle(map[string]any{"lastUserInputTime": map[string]any{"seconds": fmt.Sprint(now.Add(-time.Minute).Unix()), "nanos": 0}}), true},
		{"invalid input", idle(map[string]any{"lastUserInputTime": "invalid"}), false},
		{"response error", map[string]any{"error": "private text", "trajectorySummaries": map[string]any{}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(test.value)
			var response map[string]json.RawMessage
			_ = json.Unmarshal(raw, &response)
			got, err := trajectoriesIdle(response, 30*time.Second, now)
			if got != test.want {
				t.Fatalf("idle=%v err=%v; want %v", got, err, test.want)
			}
		})
	}
}

func TestRPCIdleIncludesSubtrajectoriesAndAcceptsNativeEmptyObject(t *testing.T) {
	endpoint := rpcTestEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/GetAllCascadeTrajectories") {
			t.Error("wrong idle method")
		}
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["excludeSubtrajectories"] != false {
			t.Error("subtrajectories were excluded")
		}
		_, _ = io.WriteString(w, "{}")
	})
	idle, err := endpoint.Idle(context.Background(), 30*time.Second)
	if err != nil || !idle {
		t.Fatalf("native empty object idle=%v err=%v", idle, err)
	}
}

func TestRPCSyncWaitsForEveryNativeAccountWithoutSendingMessages(t *testing.T) {
	var refreshes atomic.Int32
	endpoint := rpcTestEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/exa.language_server_pb.LanguageServerService/")
		switch method {
		case "GetAuthStatus":
			_, _ = io.WriteString(w, "{}")
		case "RetrieveUserQuotaSummary":
			var request struct {
				ForceRefresh bool `json:"forceRefresh"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || !request.ForceRefresh {
				t.Error("refresh did not request native sync")
			}
			refreshes.Add(1)
			_, _ = io.WriteString(w, "{}")
		case "GetUserStatus":
			email := "previous@example.test"
			if refreshes.Load() >= 2 {
				email = "target@example.test"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"userStatus": map[string]any{"email": email}})
		default:
			t.Errorf("unexpected RPC mutation %s", method)
			w.WriteHeader(404)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := SyncEndpoints(ctx, []Endpoint{endpoint}, "TARGET@example.test"); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() < 2 {
		t.Fatal("account mismatch was treated as success")
	}
}

func TestRPCSyncMismatchTimesOutWithoutReturningPrivateResponse(t *testing.T) {
	endpoint := rpcTestEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/GetUserStatus") {
			_, _ = io.WriteString(w, `{"userStatus":{"email":"wrong-private@example.test"}}`)
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := SyncEndpoints(ctx, []Endpoint{endpoint}, "target@example.test")
	if err == nil || strings.Contains(err.Error(), "wrong-private") {
		t.Fatalf("sync mismatch error=%v", err)
	}
}
