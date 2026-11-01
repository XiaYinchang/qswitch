package managed

import (
	"errors"
	"os"
	"path/filepath"
)

// savedSession reports whether Claude Code may have saved the session's
// transcript, projects/<project>/<session>.jsonl in its config directory.
// Nothing is saved before the session records its first message, and --resume
// then fails with "No conversation found". The project name is derived from
// the starting directory, so every project is searched. Any doubt counts as
// saved: --resume is kept, and its failure leaves a recovery record.
func savedSession(session string) bool {
	if !sessionUUID(session) {
		return true // --session-id only accepts UUIDs
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	// validateEnvironment rejects CLAUDE_CONFIG_DIR, so this is Claude's config directory.
	root := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return false
	} else if err != nil {
		return true
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, entry.Name(), session+".jsonl")); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// sessionUUID reports whether id has the 8-4-4-4-12 hexadecimal UUID form.
func sessionUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
