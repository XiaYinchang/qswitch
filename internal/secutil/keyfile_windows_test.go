package secutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsWrapKeyDPAPIRoundTrip(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "wrap.key")}
	first, err := f.GetOrCreate(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(first)
	second, err := f.GetOrCreate(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(second)
	if len(first) != 32 || !bytes.Equal(first, second) {
		t.Fatal("wrapping key changed")
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, first) || !bytes.HasPrefix(raw, []byte(dpapiPrefix)) {
		t.Fatal("wrapping key was not DPAPI protected")
	}
}

func TestWindowsMalformedKeyIsNotOverwritten(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "wrap.key")}
	want := []byte("existing-invalid-key")
	if err := os.WriteFile(f.Path, want, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.GetOrCreate(nil); err == nil {
		t.Fatal("accepted malformed key")
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil || !bytes.Equal(raw, want) {
		t.Fatal("existing key was overwritten")
	}
}
