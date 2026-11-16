//go:build !darwin

package hostapp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"qswitch/internal/busy"
)

// Existing providers have macOS-only desktop credentials. The cross-platform
// lifecycle is currently verified for the Antigravity desktop executable.
func nativeName(app string) (string, error) {
	if strings.EqualFold(strings.TrimSuffix(app, ".app"), "Antigravity") {
		return "antigravity", nil
	}
	return "", fmt.Errorf("%s desktop control is not supported on %s", app, runtime.GOOS)
}

func desktopCommand(command, name string) bool {
	command = strings.TrimSpace(command)
	if command == "" || strings.Contains(command, "--type=") {
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
	base := strings.ToLower(filepath.Base(executable))
	return base == name || base == name+".exe"
}

func quitNative(app string) error {
	name, err := nativeName(app)
	if err != nil {
		return err
	}
	processes, err := (busy.PS{}).List()
	if err != nil {
		return err
	}
	var result error
	for _, process := range processes {
		if desktopCommand(process.Command, name) {
			result = errors.Join(result, busy.Terminate(process.PID))
		}
	}
	return result
}

func launchNative(app string) error {
	name, err := nativeName(app)
	if err != nil {
		return err
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		for _, dir := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
			if dir == "" {
				continue
			}
			if dir == os.Getenv("LOCALAPPDATA") {
				dir = filepath.Join(dir, "Programs")
			}
			candidates = append(candidates, filepath.Join(dir, "Antigravity", "Antigravity.exe"))
		}
		candidates = append(candidates, "Antigravity.exe")
	} else {
		candidates = []string{name, "/usr/share/antigravity/antigravity", "/opt/Antigravity/antigravity"}
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		cmd := exec.Command(path)
		detachDesktop(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		// Do not wait for the desktop lifetime or leave a zombie after it exits.
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return fmt.Errorf("antigravity: desktop executable not found")
}
