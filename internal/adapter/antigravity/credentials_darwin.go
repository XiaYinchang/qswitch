package antigravity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type darwinCredentials struct{}

func nativeCredentialBackend() credentialBackend { return darwinCredentials{} }

func credentialProcessExecutable(pid int) string {
	if pid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (darwinCredentials) Read() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", nativeService, "-a", nativeAccount, "-w")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return nil, ErrCredentialNotFound
		}
		return nil, errors.New("antigravity: Keychain read failed or requires unlocking")
	}
	s := strings.TrimSpace(string(out))
	if strings.HasPrefix(s, "go-keyring-base64:") {
		value, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "go-keyring-base64:"))
		if err != nil {
			return nil, errors.New("invalid native Keychain credential encoding")
		}
		return value, nil
	}
	if strings.HasPrefix(s, "go-keyring-encoded:") {
		value, err := hex.DecodeString(strings.TrimPrefix(s, "go-keyring-encoded:"))
		if err != nil {
			return nil, errors.New("invalid native Keychain credential encoding")
		}
		return value, nil
	}
	return []byte(s), nil
}

func (d darwinCredentials) Write(raw []byte) error {
	// security's interactive input avoids exposing bearer secrets in argv.
	// This is the same encoding and stdin interface used by native go-keyring.
	encoded := "go-keyring-base64:" + base64.StdEncoding.EncodeToString(raw)
	command := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", nativeService, nativeAccount, encoded)
	if len(command) > 4096 {
		return errors.New("native macOS Keychain credential exceeds its command limit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(command)
	if err := cmd.Run(); err != nil {
		return errors.New("antigravity: Keychain write failed or requires unlocking")
	}
	// security -i can exit successfully after an individual command failed.
	got, err := d.Read()
	if err != nil || !bytes.Equal(got, raw) {
		return errors.New("antigravity: Keychain write readback failed")
	}
	return nil
}

func (darwinCredentials) Delete() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "/usr/bin/security", "delete-generic-password", "-s", nativeService, "-a", nativeAccount).Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 44 {
		return nil
	}
	if err != nil {
		return errors.New("antigravity: Keychain rollback deletion failed")
	}
	return nil
}
