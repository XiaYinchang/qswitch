package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClaudeLoginLifecycleHelper(t *testing.T) {
	if os.Getenv("QSWITCH_LOGIN_LIFECYCLE_HELPER") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	a := &App{DataDir: os.Getenv("QSWITCH_LOGIN_TEST_DATA")}
	_, err := a.loginClaudeOfficial(ctx, os.Getenv("QSWITCH_LOGIN_TEST_BIN"), nil)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected interrupted enrollment: %v, context %v", err, ctx.Err())
	}
}

func TestClaudeLoginChildLifecycle(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			binDir := filepath.Join(root, "bin")
			for _, dir := range []string{dataDir, binDir} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cat, err := exec.LookPath("cat")
			if err != nil {
				t.Fatal(err)
			}
			sleep, err := exec.LookPath("sleep")
			if err != nil {
				t.Fatal(err)
			}
			claudePID := filepath.Join(root, "claude.pid")
			catPID := filepath.Join(root, "cat.pid")
			dirFile := filepath.Join(root, "login.dir")
			bin := filepath.Join(binDir, "claude")
			for path, script := range map[string]string{
				bin:                          "#!/bin/sh\nprintf '%s' \"$CLAUDE_CONFIG_DIR\" > \"$QSWITCH_LOGIN_TEST_DIR_FILE\"\nprintf '%s' \"$$\" > \"$QSWITCH_LOGIN_TEST_CLAUDE_PID\"\nexec \"$QSWITCH_LOGIN_TEST_SLEEP\" 60\n",
				filepath.Join(binDir, "cat"): "#!/bin/sh\nprintf '%s' \"$$\" > \"$QSWITCH_LOGIN_TEST_CAT_PID\"\nexec \"$QSWITCH_LOGIN_TEST_CAT\"\n",
			} {
				if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer writer.Close()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			parent := exec.Command(executable, "-test.run=^TestClaudeLoginLifecycleHelper$")
			parent.Env = append(os.Environ(),
				"QSWITCH_LOGIN_LIFECYCLE_HELPER=1",
				"QSWITCH_LOGIN_TEST_DATA="+dataDir,
				"QSWITCH_LOGIN_TEST_BIN="+bin,
				"QSWITCH_LOGIN_TEST_DIR_FILE="+dirFile,
				"QSWITCH_LOGIN_TEST_CLAUDE_PID="+claudePID,
				"QSWITCH_LOGIN_TEST_CAT_PID="+catPID,
				"QSWITCH_LOGIN_TEST_CAT="+cat,
				"QSWITCH_LOGIN_TEST_SLEEP="+sleep,
				"PATH="+binDir+":"+os.Getenv("PATH"),
			)
			parent.Stdin = input
			parent.Stdout = os.Stdout
			parent.Stderr = os.Stderr
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			defer parent.Process.Kill()
			waited := make(chan error, 1)
			go func() { waited <- parent.Wait() }()
			pids := []int{waitClaudeLoginPID(t, claudePID), waitClaudeLoginPID(t, catPID)}
			dirBytes, err := os.ReadFile(dirFile)
			if err != nil {
				t.Fatal(err)
			}
			dir := string(dirBytes)
			// Recovery during an ongoing login must preserve both processes and
			// their private directory, including after a child execs.
			next, err := newClaudeLoginSession(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			next.Close()
			for _, pid := range pids {
				if !claudeLoginPIDRunning(pid) {
					t.Fatalf("child %d died before parent cancellation", pid)
				}
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("active enrollment reclaimed: %v", err)
			}
			if err := parent.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waited:
				if sig != syscall.SIGKILL && err != nil {
					t.Fatalf("helper failed graceful cancellation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("parent did not stop")
			}
			deadline := time.Now().Add(5 * time.Second)
			for _, pid := range pids {
				for claudeLoginPIDRunning(pid) && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if claudeLoginPIDRunning(pid) {
					t.Errorf("owned child %d survived parent %s", pid, sig)
				}
			}
			if sig == syscall.SIGKILL {
				if _, err := os.Stat(dir); err != nil {
					t.Fatalf("expected crash residue before recovery: %v", err)
				}
				next, err := newClaudeLoginSession(dataDir)
				if err != nil {
					t.Fatal(err)
				}
				next.Close()
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("enrollment directory remains after cleanup: %v", err)
			}
		})
	}
}

func waitClaudeLoginPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(raw))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child did not publish PID: %s", path)
	return 0
}

func claudeLoginPIDRunning(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The comm field may contain spaces or parentheses. A zombie has exited
	// and released its descriptors, even when container PID 1 has not reaped it.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(raw[end+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}
