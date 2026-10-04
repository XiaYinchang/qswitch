package secutil

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fake runs from a temporary directory and never invokes macOS security.
// It records operation names only; passwords never enter the test log.
func fakeSecurity(t *testing.T, firstOutput string, firstExit, addExit int, afterOutput *string) (Darwin, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"first-output": firstOutput,
		"first-exit":   strconv.Itoa(firstExit),
		"add-exit":     strconv.Itoa(addExit),
		"security": `#!/bin/sh
set -eu
dir=$(dirname "$0")
printf '%s\n' "$1" >> "$dir/calls"
case "$1" in
find-generic-password)
  if [ ! -e "$dir/find-called" ]; then
    : > "$dir/find-called"
    cat "$dir/first-output"
    exit "$(cat "$dir/first-exit")"
  fi
  if [ -e "$dir/after-output" ]; then
    cat "$dir/after-output"
  else
    cat "$dir/created-key"
  fi
  ;;
add-generic-password)
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -U) printf '%s\n' update-flag >> "$dir/calls" ;;
      -w) shift; printf '%s' "$1" > "$dir/created-key" ;;
    esac
    shift
  done
  status=$(cat "$dir/add-exit")
  if [ "$status" -ne 0 ]; then
    printf '%s' private-command-output >&2
  fi
  exit "$status"
  ;;
*) exit 2 ;;
esac
`,
	}
	if afterOutput != nil {
		files["after-output"] = *afterOutput
	}
	for name, data := range files {
		mode := os.FileMode(0o600)
		if name == "security" {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	return Darwin{Security: filepath.Join(dir, "security")}, dir
}

func assertSecurityCalls(t *testing.T, dir string, want ...string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != strings.Join(want, "\n") {
		t.Errorf("security operations = %q, want %q", got, want)
	}
}

func TestDarwinExistingKey(t *testing.T) {
	encoded := strings.Repeat("ab", 32)
	d, dir := fakeSecurity(t, encoded+"\n", 0, 0, nil)
	got, err := d.GetOrCreate(nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString(encoded)
	if !bytes.Equal(got, want) {
		t.Fatal("existing key changed")
	}
	assertSecurityCalls(t, dir, "find-generic-password")
}

func TestDarwinFindFailureDoesNotCreate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		exit   int
	}{
		{name: "interaction denied", exit: 36},
		{name: "authentication failed", exit: 51},
		{name: "other command error", exit: 1},
		{name: "empty"},
		{name: "invalid hex", output: "invalid"},
		{name: "wrong length", output: "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, dir := fakeSecurity(t, tc.output, tc.exit, 0, nil)
			_, err := d.GetOrCreate(nil)
			if err == nil {
				t.Error("find failure must be returned")
			} else if tc.exit != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.exit {
					t.Error("original find exit status was not preserved")
				}
				if !strings.Contains(err.Error(), "keychain find") {
					t.Error("find failure lacks keychain context")
				}
			}
			assertSecurityCalls(t, dir, "find-generic-password")
		})
	}
}

func TestDarwinMissingCreatesKey(t *testing.T) {
	d, dir := fakeSecurity(t, "", 44, 0, nil)
	got, err := d.GetOrCreate([]string{"/tmp/trusted-qswitch", ""})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "created-key"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(string(raw))
	if err != nil || len(want) != 32 || !bytes.Equal(got, want) {
		t.Fatal("created key was not read back correctly")
	}
	assertSecurityCalls(t, dir, "find-generic-password", "add-generic-password", "find-generic-password")
}

func TestDarwinConcurrentCreationUsesExistingKey(t *testing.T) {
	winner := strings.Repeat("cd", 32)
	d, dir := fakeSecurity(t, "", 44, 45, &winner)
	got, err := d.GetOrCreate(nil)
	if err != nil {
		t.Error("concurrent creator's key was not read back")
	} else if want, _ := hex.DecodeString(winner); !bytes.Equal(got, want) {
		t.Error("concurrent creator's key changed")
	}
	assertSecurityCalls(t, dir, "find-generic-password", "add-generic-password", "find-generic-password")
}

func TestDarwinCreateFailureDoesNotExposeOutput(t *testing.T) {
	d, dir := fakeSecurity(t, "", 44, 51, nil)
	_, err := d.GetOrCreate(nil)
	if err == nil {
		t.Fatal("create failure must be returned")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 51 {
		t.Error("original create exit status was not preserved")
	}
	if !strings.Contains(err.Error(), "keychain create") {
		t.Error("create failure lacks keychain context")
	}
	if strings.Contains(err.Error(), "private-command-output") {
		t.Error("create failure exposed command output")
	}
	assertSecurityCalls(t, dir, "find-generic-password", "add-generic-password")
}
