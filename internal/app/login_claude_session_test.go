package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestClaudeLoginSessionRecovery(t *testing.T) {
	dataDir := t.TempDir()
	active, err := newClaudeLoginSession(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	secret := filepath.Join(active.dir, ".credentials.json")
	if err := os.WriteFile(secret, []byte("test credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A real inherited descriptor must protect the session after its parent's
	// descriptor closes. PID liveness and merely holding a Go object do not.
	child := exec.Command("sleep", "60")
	child.ExtraFiles = []*os.File{active.lease}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	_ = active.lease.Close()
	next, err := newClaudeLoginSession(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("reclaimed a session whose child still owns its lease: %v", err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	next, err = newClaudeLoginSession(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err := os.Stat(active.dir); !os.IsNotExist(err) {
		t.Fatalf("abandoned credentials were not reclaimed: %v", err)
	}
}

func TestClaudeLoginSessionRecoveryBoundaries(t *testing.T) {
	dataDir := t.TempDir()
	active, err := newClaudeLoginSession(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	root := filepath.Dir(active.dir)
	outside := t.TempDir()
	outsideSecret := filepath.Join(outside, ".credentials.json")
	if err := os.WriteFile(outsideSecret, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session-unmarked", "session-invalid", "session-public", "unrelated"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("unrelated"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "session-unmarked" {
			marker := claudeLoginLeaseMarker
			if name == "session-invalid" {
				marker = "unrecognized lease"
			}
			if err := os.WriteFile(filepath.Join(dir, ".lease"), []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if name == "session-public" {
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "session-symlink")); err != nil {
		t.Fatal(err)
	}
	leaseLink := filepath.Join(root, "session-lease-symlink")
	if err := os.Mkdir(leaseLink, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(leaseLink, ".lease")); err != nil {
		t.Fatal(err)
	}
	next, err := newClaudeLoginSession(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	for _, name := range []string{"session-unmarked", "session-invalid", "session-public", "unrelated", "session-symlink", "session-lease-symlink"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Errorf("removed unowned or unrecognized entry %s: %v", name, err)
		}
	}
	if got, err := os.ReadFile(outsideSecret); err != nil || string(got) != "unrelated" {
		t.Fatal("recovery followed a symlink outside its namespace")
	}
	if _, err := os.Stat(active.dir); err != nil {
		t.Fatal("recovery removed an active session")
	}
}

func TestClaudeLoginRejectsSharedOrSymlinkRoot(t *testing.T) {
	for _, mode := range []string{"shared", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dataDir := t.TempDir()
			root := filepath.Join(dataDir, "claude-enrollments")
			if mode == "symlink" {
				if err := os.Symlink(t.TempDir(), root); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if session, err := newClaudeLoginSession(dataDir); err == nil {
				session.Close()
				t.Fatal("accepted an unsafe enrollment namespace")
			}
		})
	}
}
