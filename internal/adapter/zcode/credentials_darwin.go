package zcode

import (
	"context"
	"os"
	"time"
)

// Reuse the installed ZCode CLI's Node runtime and native keyring module.
// Never fall back to security: a different executable has different Keychain
// authorization and can repeatedly ask the user for access.
func readOSCredential(service, account string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrNativeRuntimeUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return readNodeCredential(ctx, nativeRuntimeOptions{home: home, path: os.Getenv("PATH"), node: os.Getenv("ZCODE_NODE"), env: os.Environ()}, service, account, runNativeNode)
}
