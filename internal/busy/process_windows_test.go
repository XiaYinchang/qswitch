package busy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestWindowsProcessSnapshot(t *testing.T) {
	raw := []byte(`[{"ProcessId":42,"CommandLine":"\"C:\\Apps\\Antigravity.exe\" --reuse-window","ExecutablePath":"C:\\Apps\\Antigravity.exe","Name":"Antigravity.exe"},{"ProcessId":43,"CommandLine":null,"ExecutablePath":"C:\\Apps\\idle.exe","Name":"idle.exe"}]`)
	rows, err := parseWindowsProcesses(raw)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].PID != 42 || rows[0].Command != `"C:\Apps\Antigravity.exe" --reuse-window` {
		t.Fatalf("command lost: %+v", rows[0])
	}
	if rows[1].Command != `C:\Apps\idle.exe` {
		t.Fatalf("fallback lost: %+v", rows[1])
	}
}

func TestWindowsProcessSnapshotRejectsEmptyOrInvalidResults(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":              `[]`,
		"null":               `null`,
		"null row":           `[null]`,
		"missing fields":     `[{}]`,
		"invalid pids":       `[{"ProcessId":0,"Name":"idle.exe"},{"ProcessId":-1,"CommandLine":"agent.exe"}]`,
		"missing command":    `[{"ProcessId":42}]`,
		"blank command":      `[{"ProcessId":42,"CommandLine":" ","ExecutablePath":" ","Name":" "}]`,
		"non-array response": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := parseWindowsProcesses([]byte(raw))
			if err == nil || len(rows) != 0 {
				t.Fatalf("invalid snapshot accepted as idle: rows=%v err=%v", rows, err)
			}
		})
	}
}

func TestWindowsProcessSnapshotRetainsValidFallbacks(t *testing.T) {
	raw := []byte(`[{"ProcessId":0},{"ProcessId":41},{"ProcessId":42,"ExecutablePath":" C:\\Program Files\\Agent\\agent.exe "},{"ProcessId":43,"Name":" idle.exe "}]`)
	rows, err := parseWindowsProcesses(raw)
	if err != nil || len(rows) != 2 {
		t.Fatalf("valid fallback processes lost: rows=%v err=%v", rows, err)
	}
	if rows[0].PID != 42 || rows[0].Command != `"C:\Program Files\Agent\agent.exe"` || rows[1].PID != 43 || rows[1].Command != "idle.exe" {
		t.Fatalf("fallback identity changed: %v", rows)
	}
}

func TestWindowsProcessListIncludesCaller(t *testing.T) {
	processes, err := (PS{}).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if process.PID == os.Getpid() {
			return
		}
	}
	t.Fatal("native process snapshot omitted caller")
}

func TestWindowsProcessListRetriesOnceAfterColdStartTimeout(t *testing.T) {
	attempts := 0
	rows, err := listWindowsProcesses(func(timeout time.Duration) ([]byte, error) {
		if timeout != windowsProcessQueryTimeout {
			t.Fatalf("unexpected timeout %v", timeout)
		}
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("%w: exit status 1", context.DeadlineExceeded)
		}
		return []byte(`[{"ProcessId":42,"CommandLine":"\"C:\\Apps\\Antigravity.exe\"","Name":"Antigravity.exe"}]`), nil
	})
	if err != nil || attempts != 2 || len(rows) != 1 || rows[0].PID != 42 {
		t.Fatalf("attempts=%d rows=%v err=%v", attempts, rows, err)
	}

	nonTimeoutAttempts := 0
	_, err = listWindowsProcesses(func(time.Duration) ([]byte, error) {
		nonTimeoutAttempts++
		return nil, errors.New("cim provider unavailable")
	})
	if err == nil || nonTimeoutAttempts != 1 {
		t.Fatalf("non-timeout error retried: attempts=%d err=%v", nonTimeoutAttempts, err)
	}
}

func TestWindowsAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("caller was not alive")
	}
	if Alive(-1) {
		t.Fatal("invalid pid was alive")
	}
}
