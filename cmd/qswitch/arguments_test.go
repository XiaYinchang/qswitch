package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvalidToolDoesNotOpenApplication(t *testing.T) {
	for _, args := range [][]string{{"capture", "--tool"}, {"probe", "--tool="}, {"status", "--tool", "typo"}, {"doctor", "--tool", "codex", "--tool", "grok"}, {"init", "--tool", "codex"}, {"switch", "typo", "account"}, {"probe", "--account", "account"}} {
		t.Run(args[0]+args[len(args)-1], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "must-not-exist")
			t.Setenv("QSWITCH_DIR", dir)
			if code := run(args); code != 2 {
				t.Fatalf("exit=%d", code)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("opened app before rejecting arguments")
			}
		})
	}
}

func TestWebPasswordDoesNotOpenAccountStorage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QSWITCH_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("invalid database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance-id"), []byte("invalid identity"), 0600); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(t.TempDir(), "password")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString(strings.Repeat("synthetic", 3) + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = previous }()
	if code := run([]string{"web", "set-password", "fixture", "--stdin"}); code != 0 {
		t.Fatalf("metadata setup touched account storage, exit=%d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "web-auth.json")); err != nil {
		t.Fatal(err)
	}
}

func TestNativeToolArgumentsRemainNative(t *testing.T) {
	tool, err := validateArguments([]string{"run", "antigravity", "--tool", "native-value"})
	if err != nil || tool != "antigravity" {
		t.Fatal("parsed native argument as qswitch scope")
	}
}

func TestCaptureFailureReturnsNonzero(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("QSWITCH_DIR", filepath.Join(home, "data"))
	if code := run([]string{"capture", "--tool", "codex"}); code != 1 {
		t.Fatalf("capture failure exit=%d", code)
	}
}
