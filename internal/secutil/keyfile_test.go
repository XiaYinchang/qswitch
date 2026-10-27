package secutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFileProviderCreateAndReread(t *testing.T) {
	dir := t.TempDir()
	f := File{Path: filepath.Join(dir, "sub", "wrap.key")}
	k1, err := f.GetOrCreate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 {
		t.Fatalf("key len %d", len(k1))
	}
	info, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm %04o", info.Mode().Perm())
	}
	k2, err := f.GetOrCreate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("second read must return the same key")
	}
}

func TestFileProviderRejectsOpenPermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wrap.key")
	if err := os.WriteFile(p, []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (File{Path: p}).GetOrCreate(nil); err == nil {
		t.Fatal("world-readable key file must be rejected")
	}
}

func TestFileProviderRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wrap.key")
	if err := os.WriteFile(p, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (File{Path: p}).GetOrCreate(nil); err == nil {
		t.Fatal("malformed key must be rejected")
	}
}
