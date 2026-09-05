package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpack(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Local Storage"), 0o700)
	os.WriteFile(filepath.Join(root, "Cookies"), []byte("cookie"), 0o600)
	os.WriteFile(filepath.Join(root, "Local Storage", "x"), []byte("ls"), 0o600)
	z, err := Pack(root, []string{"Cookies", "Local Storage"})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Unpack(out, z); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(out, "Cookies"))
	if string(b) != "cookie" {
		t.Fatalf("got %q", b)
	}
	b, _ = os.ReadFile(filepath.Join(out, "Local Storage", "x"))
	if string(b) != "ls" {
		t.Fatalf("ls %q", b)
	}
}
