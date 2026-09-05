package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"qswitch/internal/secutil"
)

func TestPutGetAAD(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	dir := t.TempDir()
	s := &Store{Dir: dir, Keys: &secutil.Memory{}}
	plain := []byte(`{"kind":"x","payload":{"a":1}}`)
	if err := s.Put("codex", "acct-1", plain); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("codex", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("mismatch")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "vault", "codex", "acct-1.enc"))
	if err != nil {
		t.Fatal(err)
	}
	// copy file under other id should fail decrypt
	other := filepath.Join(dir, "vault", "codex", "acct-2.enc")
	if err := os.WriteFile(other, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("codex", "acct-2"); err == nil {
		t.Fatal("expected AAD failure")
	}
}
