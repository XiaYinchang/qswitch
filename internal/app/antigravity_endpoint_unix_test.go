//go:build !windows

package app

import (
	"os"
	"testing"
)

func antigravityFixturePrivateFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
