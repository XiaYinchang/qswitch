package secutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	Service = "qswitch.vault"
	Account = "wrap-key-v1"
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
	if k, err := d.find(); err == nil && len(k) == 32 {
		return k, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	args := []string{"add-generic-password", "-U", "-s", Service, "-a", Account, "-w", hex.EncodeToString(k)}
	for _, t := range trustedBins {
		if t != "" {
			args = append(args, "-T", t)
		}
	}
	cmd := exec.Command(d.bin(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("keychain create: %w (%s)", err, redact(string(out)))
	}
	Zero(k)
	got, err := d.find()
	if err != nil {
		return nil, err
	}
	return got, nil
}

func (d Darwin) find() ([]byte, error) {
	cmd := exec.Command(d.bin(), "find-generic-password", "-s", Service, "-a", Account, "-w")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(out))
	Zero(out)
	if s == "" {
		return nil, errors.New("keychain empty")
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("keychain wrap key malformed")
	}
	return b, nil
}

func redact(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
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
