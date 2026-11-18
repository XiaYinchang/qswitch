package antigravity

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/busy"
)

const maxRPCResponseBytes = 8 << 20

// Endpoint is discovered only from a private native daemon record whose PID
// belongs to a running Antigravity executable. CSRF must never be logged.
type Endpoint struct {
	PID               int    `json:"pid"`
	HTTPSPort         int    `json:"httpsPort"`
	CSRF              string `json:"-"`
	CloudCodeEndpoint string `json:"-"` // Only populated from qswitch's private CLI launch record.
}

// String keeps fmt-based diagnostics from accidentally rendering the CSRF.
func (e Endpoint) String() string {
	return fmt.Sprintf("Antigravity endpoint pid=%d port=%d", e.PID, e.HTTPSPort)
}
func (e Endpoint) GoString() string { return e.String() }

func Discover(home string, list func() ([]adapter.Proc, error)) ([]Endpoint, error) {
	return DiscoverManaged(home, "", list)
}

// DiscoverManaged also reads the private records created by RunCLI. Native
// desktop discovery remains authoritative for independently launched clients.
func DiscoverManaged(home, dataDir string, list func() ([]adapter.Proc, error)) ([]Endpoint, error) {
	if list == nil {
		list = (busy.PS{}).List
	}
	processes, err := list()
	if err != nil {
		return nil, errors.New("antigravity discovery: process list unavailable")
	}
	active := make(map[int]bool)
	for _, process := range processes {
		if process.PID > 0 && nativeRPCProcess(process.Command) {
			active[process.PID] = true
		}
	}
	var endpoints []Endpoint
	seen := make(map[string]int)
	directories := []string{filepath.Join(home, ".gemini", "antigravity", "daemon"), filepath.Join(home, ".gemini", "antigravity-cli", "daemon")}
	if dataDir != "" {
		directories = append(directories, cliRecordDir(dataDir))
	}
	for _, dir := range directories {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, errors.New("antigravity discovery: daemon directory unavailable")
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			raw, err := readDaemonRecord(path)
			if err != nil {
				return nil, err
			}
			var record struct {
				PID               int    `json:"pid"`
				HTTPSPort         int    `json:"httpsPort"`
				CSRF              string `json:"csrfToken"`
				CloudCodeEndpoint string `json:"cloudCodeEndpoint"`
			}
			if json.Unmarshal(raw, &record) != nil {
				return nil, errors.New("antigravity discovery: invalid daemon metadata")
			}
			if !active[record.PID] {
				continue
			} // Old daemon records are not live endpoints.
			if record.HTTPSPort < 1 || record.HTTPSPort > 65535 || strings.TrimSpace(record.CSRF) == "" {
				return nil, errors.New("antigravity discovery: incomplete daemon metadata")
			}
			cloudEndpoint := ""
			if dataDir != "" && dir == cliRecordDir(dataDir) && record.CloudCodeEndpoint != "" {
				cloudEndpoint, err = allowedCloudEndpoint(record.CloudCodeEndpoint)
				if err != nil {
					return nil, errors.New("antigravity discovery: invalid managed CLI quota endpoint")
				}
			}
			key := strconv.Itoa(record.PID) + ":" + strconv.Itoa(record.HTTPSPort)
			if index, found := seen[key]; found {
				prior := &endpoints[index]
				if prior.CSRF != record.CSRF || prior.CloudCodeEndpoint != "" && cloudEndpoint != "" && prior.CloudCodeEndpoint != cloudEndpoint {
					return nil, errors.New("antigravity discovery: conflicting daemon metadata")
				}
				if cloudEndpoint != "" {
					prior.CloudCodeEndpoint = cloudEndpoint
				}
				continue
			}
			seen[key] = len(endpoints)
			endpoints = append(endpoints, Endpoint{PID: record.PID, HTTPSPort: record.HTTPSPort, CSRF: record.CSRF, CloudCodeEndpoint: cloudEndpoint})
		}
	}
	if len(active) != 0 && len(endpoints) == 0 {
		return nil, errors.New("antigravity discovery: active native backend has no verified daemon endpoint; start CLI sessions with qswitch run antigravity")
	}
	return endpoints, nil
}

func nativeRPCProcess(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	var executable string
	if command[0] == '"' {
		end := strings.IndexByte(command[1:], '"')
		if end < 0 {
			return false
		}
		executable = command[1 : 1+end]
	} else {
		executable = strings.Fields(command)[0]
	}
	executable = strings.ReplaceAll(executable, `\`, "/")
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(executable)), ".exe")
	switch name {
	case "agy", "language_server", "language_server_macos", "language_server_macos_arm", "language_server_macos_arm64", "language_server_linux_x64", "language_server_linux_arm", "language_server_linux_arm64", "language_server_windows_x64", "language_server_windows_arm64":
		return true
	}
	return false
}

func readDaemonRecord(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("antigravity discovery: cannot inspect daemon file")
	}
	if !before.Mode().IsRegular() || before.Size() > 64<<10 {
		return nil, errors.New("antigravity discovery: unsafe daemon file")
	}
	if !privateDaemonOwner(path, before) {
		return nil, errors.New("antigravity discovery: daemon file is not private to the current user")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("antigravity discovery: cannot open daemon file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("antigravity discovery: daemon file changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return nil, errors.New("antigravity discovery: cannot read daemon metadata")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || !privateDaemonOwner(path, after) {
		return nil, errors.New("antigravity discovery: daemon file changed")
	}
	return raw, nil
}

func (e Endpoint) Call(ctx context.Context, method string, request, response any) error {
	if !rpcMethodName(method) {
		return errors.New("antigravity RPC: invalid method")
	}
	if e.HTTPSPort < 1 || e.HTTPSPort > 65535 || strings.TrimSpace(e.CSRF) == "" {
		return fmt.Errorf("antigravity RPC %s: invalid endpoint", method)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if request == nil {
		request = struct{}{}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("antigravity RPC %s: invalid request", method)
	}
	address := "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(e.HTTPSPort)) + "/exa.language_server_pb.LanguageServerService/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("antigravity RPC %s: invalid request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Codeium-Csrf-Token", e.CSRF)
	transport := &http.Transport{
		Proxy:           nil,
		DialContext:     (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, // Native daemon uses self-signed TLS on fixed loopback only.
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("antigravity RPC %s: %w", method, ctx.Err())
		}
		return fmt.Errorf("antigravity RPC %s: request failed", method)
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		return fmt.Errorf("antigravity RPC %s: HTTP %d", method, result.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(result.Body, maxRPCResponseBytes+1))
	if err != nil || len(raw) > maxRPCResponseBytes {
		return fmt.Errorf("antigravity RPC %s: invalid response size", method)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("antigravity RPC %s: invalid JSON response", method)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		if _, present := object["error"]; present {
			return fmt.Errorf("antigravity RPC %s: error response", method)
		}
		if _, present := object["errors"]; present {
			return fmt.Errorf("antigravity RPC %s: error response", method)
		}
	}
	if response != nil && json.Unmarshal(raw, response) != nil {
		return fmt.Errorf("antigravity RPC %s: invalid response", method)
	}
	return nil
}

func rpcMethodName(method string) bool {
	if method == "" {
		return false
	}
	for _, c := range method {
		if c < 'A' || c > 'Z' {
			if c < 'a' || c > 'z' {
				if c < '0' || c > '9' {
					return false
				}
			}
		}
	}
	return true
}

func (e Endpoint) UserEmail(ctx context.Context) (string, error) {
	var result struct {
		UserStatus struct {
			Email string `json:"email"`
		} `json:"userStatus"`
	}
	if err := e.Call(ctx, "GetUserStatus", struct{}{}, &result); err != nil {
		return "", err
	}
	email := strings.TrimSpace(result.UserStatus.Email)
	if email == "" {
		return "", errors.New("antigravity RPC GetUserStatus: missing account identity")
	}
	return email, nil
}

func (e Endpoint) Idle(ctx context.Context, grace time.Duration) (bool, error) {
	var response map[string]json.RawMessage
	request := struct {
		ExcludeSubtrajectories bool `json:"excludeSubtrajectories"`
	}{false}
	if err := e.Call(ctx, "GetAllCascadeTrajectories", request, &response); err != nil {
		return false, err
	}
	return trajectoriesIdle(response, grace, time.Now())
}

type trajectoryActivity struct {
	Status              string          `json:"status"`
	NotFullyIdle        bool            `json:"notFullyIdle"`
	HasActiveChildren   bool            `json:"hasActiveChildren"`
	HasRunningSubagents bool            `json:"hasRunningSubagents"`
	BackgroundTaskCount int64           `json:"backgroundTaskCount"`
	LastUserInputTime   json.RawMessage `json:"lastUserInputTime"`
}

func trajectoriesIdle(response map[string]json.RawMessage, grace time.Duration, now time.Time) (bool, error) {
	unknown := errors.New("antigravity RPC GetAllCascadeTrajectories: unknown activity state")
	if response == nil {
		return false, unknown
	}
	if len(response) == 0 {
		return true, nil
	} // Verified proto3 default: an empty map is omitted.
	for _, key := range []string{"error", "errors", "code"} {
		if _, ok := response[key]; ok {
			return false, unknown
		}
	}
	raw, exists := response["trajectorySummaries"]
	if !exists {
		return false, unknown
	}
	var summaries map[string]trajectoryActivity
	if json.Unmarshal(raw, &summaries) != nil || summaries == nil {
		return false, unknown
	}
	if grace <= 0 {
		grace = 30 * time.Second
	}
	for _, summary := range summaries {
		if summary.Status != "CASCADE_RUN_STATUS_IDLE" || summary.NotFullyIdle || summary.HasActiveChildren || summary.HasRunningSubagents || summary.BackgroundTaskCount != 0 {
			return false, nil
		}
		if len(summary.LastUserInputTime) != 0 {
			input, err := rpcTimestamp(summary.LastUserInputTime)
			if err != nil {
				return false, unknown
			}
			if !input.IsZero() && now.Sub(input) < grace {
				return false, nil
			}
		}
	}
	return true, nil
}

func rpcTimestamp(raw json.RawMessage) (time.Time, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339Nano, text)
	}
	var stamp struct {
		Seconds json.Number `json:"seconds"`
		Nanos   int64       `json:"nanos"`
	}
	if json.Unmarshal(raw, &stamp) != nil || len(raw) == 0 || raw[0] != '{' || stamp.Seconds == "" {
		return time.Time{}, errors.New("invalid input timestamp")
	}
	seconds, err := stamp.Seconds.Int64()
	if err != nil || stamp.Nanos < 0 || stamp.Nanos >= 1e9 {
		return time.Time{}, errors.New("invalid input timestamp")
	}
	return time.Unix(seconds, stamp.Nanos), nil
}

// SyncEndpoints asks official clients to notice external credential storage
// changes, then verifies their own active identity. It never injects a prompt.
func SyncEndpoints(ctx context.Context, endpoints []Endpoint, expectedEmail string) error {
	if len(endpoints) == 0 {
		return nil
	}
	expectedEmail = strings.TrimSpace(expectedEmail)
	if expectedEmail == "" {
		return errors.New("antigravity sync: missing expected account")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		allMatch := true
		for _, endpoint := range endpoints {
			if err := endpoint.Call(ctx, "GetAuthStatus", struct{}{}, nil); err != nil {
				allMatch = false
				continue
			}
			if err := endpoint.Call(ctx, "RetrieveUserQuotaSummary", struct {
				ForceRefresh bool `json:"forceRefresh"`
			}{true}, nil); err != nil {
				allMatch = false
				continue
			}
			email, err := endpoint.UserEmail(ctx)
			if err != nil || !strings.EqualFold(strings.TrimSpace(email), expectedEmail) {
				allMatch = false
			}
		}
		if allMatch {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("antigravity sync: active account was not confirmed: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
