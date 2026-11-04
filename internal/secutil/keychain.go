package secutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	Service = "qswitch.vault"
	Account = "wrap-key-v1"

	// security returns the low eight bits of the Keychain OSStatus.
	securityItemNotFound = 44 // errSecItemNotFound (-25300)
	securityDuplicate    = 45 // errSecDuplicateItem (-25299)

	// DefaultSecurityTimeout bounds one security call. With the login keychain
	// locked, security waits for its password dialog; this leaves time to
	// answer it without letting an unanswered dialog hold qswitch forever.
	DefaultSecurityTimeout = 2 * time.Minute
	// DefaultRetryAfter is how long Cached repeats a failed lookup's error
	// instead of asking the Keychain again.
	DefaultRetryAfter = 10 * time.Minute
)

// ErrSecurityTimeout reports a security call that did not finish in time,
// usually because the login keychain is locked and nobody answered its dialog.
var ErrSecurityTimeout = errors.New("security did not finish in time; the login keychain may be locked")

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

// Cached hands out the key its Provider returned the first time, so a
// long-running qswitchd reads the Keychain once instead of on every vault
// access. One lookup runs at a time. A failed lookup's error is returned again
// for RetryAfter without asking the Provider: when the login keychain is
// locked, every security call opens another password dialog.
type Cached struct {
	Provider   KeyProvider
	RetryAfter time.Duration    // default DefaultRetryAfter
	Now        func() time.Time // default time.Now

	mu       sync.Mutex
	key      []byte
	err      error
	failedAt time.Time
}

func (c *Cached) GetOrCreate(trustedBins []string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.key) > 0 {
		return append([]byte(nil), c.key...), nil
	}
	if c.err != nil {
		retry := c.RetryAfter
		if retry <= 0 {
			retry = DefaultRetryAfter
		}
		if until := c.failedAt.Add(retry); c.now().Before(until) {
			return nil, fmt.Errorf("%w (not asking the Keychain again until %s)", c.err, until.Format("15:04:05"))
		}
	}
	k, err := c.Provider.GetOrCreate(trustedBins)
	if err != nil {
		c.err, c.failedAt = err, c.now()
		return nil, err
	}
	c.err = nil
	c.key = append([]byte(nil), k...)
	return k, nil
}

func (c *Cached) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type Darwin struct {
	Security string        // default /usr/bin/security
	Timeout  time.Duration // per security call, default DefaultSecurityTimeout
}

func (d Darwin) bin() string {
	if d.Security != "" {
		return d.Security
	}
	return "/usr/bin/security"
}

func (d Darwin) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return DefaultSecurityTimeout
}

// security runs one security command under the call deadline and returns its
// stdout when wantOutput is set. A command killed at the deadline reports
// ErrSecurityTimeout instead of the signal that ended it.
func (d Darwin) security(wantOutput bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stderr = io.Discard
	// Do not wait on pipes held open by anything the killed command left behind.
	cmd.WaitDelay = time.Second
	var out []byte
	var err error
	if wantOutput {
		out, err = cmd.Output()
	} else {
		err = cmd.Run()
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		Zero(out)
		return nil, fmt.Errorf("%w (waited %s)", ErrSecurityTimeout, d.timeout())
	}
	return out, err
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
	if _, err := d.security(false, args...); err != nil && !securityExit(err, securityDuplicate) {
		return nil, fmt.Errorf("keychain create: %w", err)
	}
	// Another process may have created the key after our initial lookup.
	return d.find()
}

func (d Darwin) find() ([]byte, error) {
	out, err := d.security(true, "find-generic-password", "-s", Service, "-a", Account, "-w")
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
