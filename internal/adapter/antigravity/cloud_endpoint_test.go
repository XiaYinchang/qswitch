package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
)

func TestCloudEndpointParsesNativeArguments(t *testing.T) {
	for _, command := range []string{
		"/synthetic/language_server --cloud_code_endpoint https://daily-cloudcode-pa.googleapis.com --hub",
		"/synthetic/agy --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com",
		`"C:\Program Files\Antigravity\language_server.exe" --cloud_code_endpoint="https://daily-cloudcode-pa.googleapis.com" --hub`,
		`"C:\Program Files\Antigravity\language_server.exe" --cloud_code_endpoint 'https://daily-cloudcode-pa.googleapis.com/'`,
	} {
		home, data := t.TempDir(), t.TempDir()
		writeRPCTestRecord(t, home, "antigravity", "endpoint.json", 101, 41001)
		list := func() ([]adapter.Proc, error) {
			return []adapter.Proc{{PID: 101, Command: command}, {PID: 102, Command: "/bin/sh -c language_server --cloud_code_endpoint=https://evil.invalid"}}, nil
		}
		got, err := CloudCodeEndpoint(home, data, list)
		if err != nil || got != "https://daily-cloudcode-pa.googleapis.com" {
			t.Fatalf("native endpoint=%q err=%v", got, err)
		}
		path := filepath.Join(data, cloudEndpointCacheName)
		stat, err := os.Lstat(path)
		if err != nil || !privateDaemonOwner(path, stat) {
			t.Fatal("cache was not private to current user")
		}
		raw, err := os.ReadFile(path)
		var observed cloudEndpointObservation
		if err != nil || json.Unmarshal(raw, &observed) != nil || observed.Endpoint != got || observed.Source != "native_process" || observed.ObservedAt <= 0 {
			t.Fatal("cache lost provenance")
		}
		if bytes.Contains(raw, []byte("csrf")) || bytes.Contains(raw, []byte("synthetic")) {
			t.Fatal("cache retained process/daemon credentials")
		}
		// A new caller with no native processes reads only the persisted proof.
		offline, err := CloudCodeEndpoint(home, data, func() ([]adapter.Proc, error) { return nil, nil })
		if err != nil || offline != got {
			t.Fatalf("offline endpoint=%q err=%v", offline, err)
		}
	}
}

func TestCloudEndpointRejectsAmbiguousNativeStateBeforeCachedFallback(t *testing.T) {
	for _, mode := range []string{"missing flag", "empty flag", "empty equals", "unknown origin", "port", "path", "query", "duplicate flag", "unclosed quote", "conflicting processes", "unverified process", "process list error", "public record"} {
		t.Run(mode, func(t *testing.T) {
			home, data := t.TempDir(), t.TempDir()
			record := writeRPCTestRecord(t, home, "antigravity", "first.json", 101, 41001)
			writeRPCTestRecord(t, home, "antigravity", "second.json", 102, 41002)
			procs := []adapter.Proc{{PID: 101, Command: "language_server --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com"}}
			list := func() ([]adapter.Proc, error) { return procs, nil }
			if _, err := CloudCodeEndpoint(home, data, list); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(data, cloudEndpointCacheName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing flag":
				procs[0].Command = "language_server --hub"
			case "empty flag":
				procs[0].Command = "language_server --cloud_code_endpoint"
			case "empty equals":
				procs[0].Command = "language_server --cloud_code_endpoint="
			case "unknown origin":
				procs[0].Command = "language_server --cloud_code_endpoint=https://evil.invalid"
			case "port":
				procs[0].Command = "language_server --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com:443"
			case "path":
				procs[0].Command = "language_server --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com/quota"
			case "query":
				procs[0].Command = "language_server --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com?synthetic=secret"
			case "duplicate flag":
				procs[0].Command += " --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com"
			case "unclosed quote":
				procs[0].Command = "language_server --cloud_code_endpoint=\"https://daily-cloudcode-pa.googleapis.com"
			case "conflicting processes":
				procs = append(procs, adapter.Proc{PID: 102, Command: "language_server --cloud_code_endpoint=https://cloudcode-pa.googleapis.com"})
			case "unverified process":
				procs = append(procs, adapter.Proc{PID: 103, Command: "language_server --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com"})
			case "process list error":
				list = func() ([]adapter.Proc, error) { return nil, errors.New("synthetic secret process failure") }
			case "public record":
				rpcTestMetadataACL(t, record, true)
			}
			got, err := CloudCodeEndpoint(home, data, list)
			if err == nil || got != "" {
				t.Fatalf("ambiguous runtime borrowed cached endpoint: %q %v", got, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("discovery error exposed process contents")
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected runtime replaced verified cache")
			}
		})
	}
}

func TestCloudEndpointAcceptsConsistentExplicitProductionOverride(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	for i, name := range []string{"first.json", "second.json"} {
		writeRPCTestRecord(t, home, "antigravity", name, 101+i, 41001+i)
	}
	endpoint, err := CloudCodeEndpoint(home, data, func() ([]adapter.Proc, error) {
		return []adapter.Proc{
			{PID: 101, Command: "language_server --cloud_code_endpoint=https://cloudcode-pa.googleapis.com"},
			{PID: 102, Command: "language_server --cloud_code_endpoint https://cloudcode-pa.googleapis.com/"},
		}, nil
	})
	if err != nil || endpoint != "https://cloudcode-pa.googleapis.com" {
		t.Fatalf("consistent override=%q %v", endpoint, err)
	}
}

func TestCloudEndpointNoRuntimeRequiresPrivateValidObservation(t *testing.T) {
	for _, mode := range []string{"missing", "bad json", "bad origin", "bad source", "future", "public", "directory"} {
		t.Run(mode, func(t *testing.T) {
			home, data := t.TempDir(), t.TempDir()
			path := filepath.Join(data, cloudEndpointCacheName)
			observed := cloudEndpointObservation{Endpoint: "https://daily-cloudcode-pa.googleapis.com", ObservedAt: time.Now().Unix(), Source: "native_process"}
			switch mode {
			case "bad origin":
				observed.Endpoint = "https://evil.invalid"
			case "bad source":
				observed.Source = "guessed_default"
			case "future":
				observed.ObservedAt = time.Now().Add(time.Hour).Unix()
			}
			if mode != "missing" && mode != "directory" {
				if err := writeCloudEndpointObservation(path, observed); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "bad json":
				if err := os.WriteFile(path, []byte(`{"endpoint":`), 0600); err != nil {
					t.Fatal(err)
				}
			case "public":
				rpcTestMetadataACL(t, path, true)
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := CloudCodeEndpoint(home, data, func() ([]adapter.Proc, error) { return nil, nil }); err == nil || got != "" {
				t.Fatalf("unproved channel accepted: %q %v", got, err)
			}
		})
	}
}

func writeManagedCloudEndpointRecord(t *testing.T, data string, pid, port int, endpoint string) string {
	t.Helper()
	dir := cliRecordDir(data)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "synthetic-cli.json")
	if err := writeCLIRecord(path, Endpoint{PID: pid, HTTPSPort: port, CSRF: "synthetic-csrf-value", CloudCodeEndpoint: endpoint}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCloudEndpointManagedCLIRecordCanMergeNativeDaemonMetadata(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	const wanted = "https://cloudcode-pa.googleapis.com"
	writeRPCTestRecord(t, home, "antigravity-cli", "native.json", 101, 41001)
	writeManagedCloudEndpointRecord(t, data, 101, 41001, wanted)
	list := func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 101, Command: "/synthetic/agy --model synthetic-model"}}, nil
	}
	endpoint, err := CloudCodeEndpoint(home, data, list)
	if err != nil || endpoint != wanted {
		t.Fatalf("native metadata obscured managed launch context: %q %v", endpoint, err)
	}
	endpoints, err := DiscoverManaged(home, data, list)
	if err != nil || len(endpoints) != 1 || endpoints[0].CloudCodeEndpoint != wanted {
		t.Fatalf("duplicate native record was not merged: count=%d err=%v", len(endpoints), err)
	}
	offline, err := CloudCodeEndpoint(home, data, func() ([]adapter.Proc, error) { return nil, nil })
	if err != nil || offline != wanted {
		t.Fatal("verified CLI launch context was not persisted")
	}
}

func TestCloudEndpointUncertainCLIRecordsCannotBorrowCachedChannel(t *testing.T) {
	for _, mode := range []string{"old record", "unsupported origin", "public record", "wrong pid", "native metadata only", "conflicting desktop", "conflicting csrf"} {
		t.Run(mode, func(t *testing.T) {
			home, data := t.TempDir(), t.TempDir()
			cache := filepath.Join(data, cloudEndpointCacheName)
			if err := writeCloudEndpointObservation(cache, cloudEndpointObservation{Endpoint: "https://daily-cloudcode-pa.googleapis.com", ObservedAt: time.Now().Unix(), Source: "native_process"}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(cache)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := "https://cloudcode-pa.googleapis.com"
			if mode == "old record" {
				endpoint = ""
			}
			if mode == "unsupported origin" {
				endpoint = "https://private.invalid?secret=synthetic"
			}
			path := writeManagedCloudEndpointRecord(t, data, 101, 41001, endpoint)
			processes := []adapter.Proc{{PID: 101, Command: "/synthetic/agy"}}
			switch mode {
			case "public record":
				rpcTestMetadataACL(t, path, true)
			case "wrong pid":
				processes[0].PID = 102
			case "native metadata only":
				// Only qswitch's own launch records can assert CLI environment.
				native := writeRPCTestRecord(t, home, "antigravity-cli", "native.json", 101, 41001)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(native, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "conflicting desktop":
				writeRPCTestRecord(t, home, "antigravity", "desktop.json", 102, 41002)
				processes = append(processes, adapter.Proc{PID: 102, Command: "/synthetic/language_server --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com"})
			case "conflicting csrf":
				native := writeRPCTestRecord(t, home, "antigravity-cli", "native.json", 101, 41001)
				raw, err := json.Marshal(map[string]any{"pid": 101, "httpsPort": 41001, "csrfToken": "synthetic-different-csrf"})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(native, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := CloudCodeEndpoint(home, data, func() ([]adapter.Proc, error) { return processes, nil })
			if err == nil || got != "" || strings.Contains(err.Error(), "secret") {
				t.Fatalf("uncertain CLI context borrowed cached quota endpoint: %q %v", got, err)
			}
			after, err := os.ReadFile(cache)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("uncertain CLI context overwrote cache")
			}
		})
	}
}
