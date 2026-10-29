package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const claudeLoginLeaseMarker = "qswitch-claude-enrollment-v1\n"

type claudeLoginSession struct {
	dir   string
	lease *os.File
}

// Close runs after both direct children have been reaped. Never explicitly
// unlock the lease: inherited descriptors keep it held if the parent crashes.
func (s *claudeLoginSession) Close() {
	_ = os.RemoveAll(s.dir)
	_ = s.lease.Close()
}

func newClaudeLoginSession(dataDir string) (*claudeLoginSession, error) {
	root, err := filepath.Abs(filepath.Join(dataDir, "claude-enrollments"))
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if !ownedClaudeLoginPath(root, true) {
		return nil, fmt.Errorf("claude: enrollment directory must be an owned private directory: %s", root)
	}
	// Serialize creation and scavenging, so a concurrent login never sees a
	// partially initialized lease. This lock is not inherited by children.
	guard, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	if !ownedClaudeLoginFile(guard) {
		return nil, errors.New("claude: unsafe enrollment directory lock")
	}
	if err := unix.Flock(int(guard.Fd()), unix.LOCK_EX); err != nil {
		return nil, err
	}
	if err := reapClaudeLoginSessions(root); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "session-")
	if err != nil {
		return nil, err
	}
	lease, err := os.OpenFile(filepath.Join(dir, ".lease"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	session := &claudeLoginSession{dir: dir, lease: lease}
	if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		session.Close()
		return nil, err
	}
	if _, err := lease.WriteString(claudeLoginLeaseMarker); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

func ownedClaudeLoginPath(path string, directory bool) bool {
	st, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return ownedClaudeLoginInfo(st, directory)
}

func ownedClaudeLoginFile(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && ownedClaudeLoginInfo(st, false)
}

func ownedClaudeLoginInfo(st os.FileInfo, directory bool) bool {
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return st.IsDir() && st.Mode().Perm() == 0o700
	}
	return st.Mode().IsRegular() && st.Mode().Perm() == 0o600 && owner.Nlink == 1
}

// Only qswitch's private namespace is considered, never a prefix search in
// /tmp. An inherited flock proves a session is still in use without relying
// on recyclable PIDs or inspecting/killing any existing Claude processes.
func reapClaudeLoginSessions(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "session-") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if !ownedClaudeLoginPath(dir, true) {
			continue
		}
		lease, err := os.OpenFile(filepath.Join(dir, ".lease"), os.O_RDWR|unix.O_NOFOLLOW, 0)
		if err != nil {
			if os.IsNotExist(err) {
				// A crash between mkdir and lease creation can only leave an
				// empty directory. Remove never recurses into unmarked data.
				_ = os.Remove(dir)
			}
			continue
		}
		if !ownedClaudeLoginFile(lease) {
			_ = lease.Close()
			continue
		}
		marker := make([]byte, len(claudeLoginLeaseMarker)+1)
		n, _ := lease.ReadAt(marker, 0)
		if string(marker[:n]) != claudeLoginLeaseMarker {
			_ = lease.Close()
			continue
		}
		if err := unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lease.Close()
			continue
		}
		err = os.RemoveAll(dir)
		_ = lease.Close()
		if err != nil {
			return fmt.Errorf("claude: remove abandoned enrollment: %w", err)
		}
	}
	return nil
}
