//go:build !windows

package antigravity

import (
	"os"
	"testing"
)

func rpcTestMetadataACL(t *testing.T, path string, public bool) {
	t.Helper()
	mode := os.FileMode(0o600)
	if public {
		mode = 0o644
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
