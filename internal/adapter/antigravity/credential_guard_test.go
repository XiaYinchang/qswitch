package antigravity

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/livefile"
)

type interceptedCredentialBackend struct {
	credentialBackend
	readHook func()
}

func (b *interceptedCredentialBackend) Read() ([]byte, error) {
	if b.readHook != nil {
		b.readHook()
	}
	return b.credentialBackend.Read()
}

func TestRestoreFinalGuardRunsAfterCredentialPreparation(t *testing.T) {
	// Read 3 is the final guard's own native fingerprint read. Deadline and
	// configuration checks must also happen after that potentially slow read.
	for _, phase := range []int{1, 2, 3} {
		for _, change := range []string{"source-reset", "target-reset", "target-ttl", "auto-disabled", "native-login"} {
			// A change after the snapshot is already rejected by its comparison.
			// The missed login was the one first observed by the snapshot itself.
			if phase == 2 && change == "native-login" {
				continue
			}
			t.Run(fmt.Sprintf("read-%d/%s", phase, change), func(t *testing.T) {
				h := newAdapterHarness(t)
				intercepted := &interceptedCredentialBackend{credentialBackend: h.backend}
				h.adapter.deps.read = func(home string) ([]Candidate, error) { return readCredentials(home, intercepted) }
				inTransaction, reads := false, 0
				h.adapter.deps.write = func(home string, raw []byte, beforeWrite func() error) (func() error, error) {
					inTransaction = true
					var undo func() error
					err := writeCredentialTransaction(home, raw, intercepted, livefile.AtomicWrite, &undo, beforeWrite)
					return undo, err
				}
				sourceFP, err := h.adapter.Fingerprint(h.home)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
				sourceReset, targetReset, targetTTL := now.Add(24*time.Hour), now.Add(24*time.Hour), now.Add(24*time.Hour)
				switch change {
				case "source-reset":
					sourceReset = now.Add(time.Hour)
				case "target-reset":
					targetReset = now.Add(time.Hour)
				case "target-ttl":
					targetTTL = now.Add(time.Hour)
				}
				enabled := true
				intercepted.readHook = func() {
					if !inTransaction {
						return
					}
					reads++
					if reads != phase {
						return
					}
					switch change {
					case "source-reset":
						now = sourceReset
					case "target-reset":
						now = targetReset
					case "target-ttl":
						now = targetTTL
					case "auto-disabled":
						enabled = false
					case "native-login":
						h.backend.raw = syntheticCredential("external-login")
					}
				}
				guardCalls := 0
				guard := func() error {
					guardCalls++
					current, err := h.adapter.Fingerprint(h.home)
					if err != nil || current != sourceFP || !enabled || !now.Before(sourceReset) || !now.Before(targetReset) || !now.Before(targetTTL) {
						return errors.New("synthetic automatic admission expired")
					}
					return nil
				}
				err = h.adapter.Restore(h.home, h.blob(t, "target"), adapter.RestoreOpts{ExpectedCurrentEmail: "old@example.invalid", BeforeWrite: guard})
				if !errors.Is(err, adapter.ErrRestorePrecondition) || h.backend.writes != 0 || guardCalls != 1 {
					t.Fatalf("credential preparation bypassed final admission: err=%v writes=%d guards=%d reads=%d", err, h.backend.writes, guardCalls, reads)
				}
				if change == "native-login" && !bytes.Equal(h.backend.raw, syntheticCredential("external-login")) {
					t.Fatal("new native login was overwritten")
				}
			})
		}
	}
}

func TestCredentialGuardAlsoProtectsFileOnlyFallback(t *testing.T) {
	home := t.TempDir()
	path := CredentialPaths(home)[0]
	old := syntheticCredential("old")
	putSyntheticCredential(t, path, old)
	backend := &memoryCredentialBackend{readErr: ErrKeyringUnavailable}
	writes := 0
	write := func(path string, data []byte, mode os.FileMode) error {
		writes++
		return livefile.AtomicWrite(path, data, mode)
	}
	var undo func() error
	err := writeCredentialTransaction(home, syntheticCredential("target"), backend, write, &undo, func() error {
		return errors.New("synthetic final source window expired")
	})
	if !errors.Is(err, adapter.ErrRestorePrecondition) || writes != 0 || undo != nil {
		t.Fatalf("file-only transaction bypassed admission: err=%v writes=%d", err, writes)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatal("guard rejection changed the native fallback")
	}
}
