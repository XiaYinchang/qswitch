package busy

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"qswitch/internal/adapter"
)

func platformProcesses() ([]adapter.Proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const script = "$ErrorActionPreference = 'Stop'; [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_Process -ErrorAction Stop | Select-Object ProcessId,CommandLine,ExecutablePath,Name)"
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list Windows processes: %w", err)
	}
	return parseWindowsProcesses(raw)
}

func parseWindowsProcesses(raw []byte) ([]adapter.Proc, error) {
	var rows []struct {
		PID            int    `json:"ProcessId"`
		CommandLine    string `json:"CommandLine"`
		ExecutablePath string `json:"ExecutablePath"`
		Name           string `json:"Name"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	result := make([]adapter.Proc, 0, len(rows))
	for _, row := range rows {
		if row.PID <= 0 {
			continue
		}
		command := strings.TrimSpace(row.CommandLine)
		if command == "" {
			command = strings.TrimSpace(row.ExecutablePath)
			if strings.Contains(command, " ") {
				command = `"` + command + `"`
			}
		}
		if command == "" {
			command = strings.TrimSpace(row.Name)
		}
		if command == "" {
			continue
		}
		result = append(result, adapter.Proc{PID: row.PID, Command: command})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("list Windows processes: snapshot has no valid processes")
	}
	return result, nil
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}

// taskkill without /F requests a graceful close and refuses processes that need
// force. Never broaden this to an image-name match or a process-tree kill.
func Terminate(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid process ID")
	}
	cmd := exec.Command("taskkill.exe", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}
