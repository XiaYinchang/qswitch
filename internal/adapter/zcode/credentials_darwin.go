package zcode

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Read the existing item only. In particular, do not invoke the desktop's
// key-store initialization, which can create keys and migrate its files.
func readOSCredential(service, account string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w")
	cmd.WaitDelay = time.Second
	raw, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ErrOSCredentialPending
	}
	if err != nil {
		return "", ErrOSCredential
	}
	// security appends a single newline; do not normalize the secret itself.
	return strings.TrimSuffix(string(raw), "\n"), nil
}
