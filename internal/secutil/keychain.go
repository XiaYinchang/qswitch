package secutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	Service = "qswitch.vault"
	Account = "wrap-key-v1"

	// security returns the low eight bits of the Keychain OSStatus.
	securityItemNotFound = 44 // errSecItemNotFound (-25300)
	securityDuplicate    = 45 // errSecDuplicateItem (-25299)
)

type KeyProvider interface {
	GetOrCreate(trustedBins []string) ([]byte, error)
}

type Memory struct {
	mu  sync.Mutex
	Key []byte
}

func (m *Memory) GetOrCreate([]string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Key) == 32 {
		return append([]byte(nil), m.Key...), nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	m.Key = k
	return append([]byte(nil), k...), nil
}

type Darwin struct {
	Security string // default /usr/bin/security
}

func (d Darwin) bin() string {
	if d.Security != "" {
		return d.Security
	}
	return "/usr/bin/security"
}

func (d Darwin) GetOrCreate(trustedBins []string) ([]byte, error) {
	k, err := d.find()
	if err == nil {
		return k, nil
	}
	if !securityExit(err, securityItemNotFound) {
		return nil, err
	}
	k = make([]byte, 32)
	defer Zero(k)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("keychain generate: %w", err)
	}
	// Never update an existing key: all saved vault entries depend on it.
	args := []string{"add-generic-password", "-s", Service, "-a", Account, "-w", hex.EncodeToString(k)}
	for _, t := range trustedBins {
		if t != "" {
			args = append(args, "-T", t)
		}
	}
	cmd := exec.Command(d.bin(), args...)
	if err := cmd.Run(); err != nil && !securityExit(err, securityDuplicate) {
		return nil, fmt.Errorf("keychain create: %w", err)
	}
	// Another process may have created the key after our initial lookup.
	return d.find()
}

func (d Darwin) find() ([]byte, error) {
	cmd := exec.Command(d.bin(), "find-generic-password", "-s", Service, "-a", Account, "-w")
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	defer Zero(out)
	if err != nil {
		return nil, fmt.Errorf("keychain find: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, errors.New("keychain empty")
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		Zero(b)
		return nil, errors.New("keychain wrap key malformed")
	}
	return b, nil
}

func securityExit(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func InTest() bool {
	if os.Getenv("QSWITCH_IN_TEST") == "1" {
		return true
	}
	return strings.HasSuffix(os.Args[0], ".test") || strings.Contains(os.Args[0], "/_test/")
}
