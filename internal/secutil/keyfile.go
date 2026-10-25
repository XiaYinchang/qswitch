package secutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File stores the vault wrap key in a 0600 file. It is the fallback for
// platforms without a system keychain (Linux servers). The key file lives
// inside the data dir, which must itself be 0700.
type File struct {
	Path string
}

func (f File) GetOrCreate([]string) ([]byte, error) {
	if strings.TrimSpace(f.Path) == "" {
		return nil, errors.New("secutil: empty key file path")
	}
	if b, err := f.read(); err == nil {
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return nil, err
	}
	k := make([]byte, 32)
	defer Zero(k)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("keyfile generate: %w", err)
	}
	// O_EXCL: a concurrent first-initializing process wins; re-read its key.
	fd, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return f.read()
		}
		return nil, fmt.Errorf("keyfile create: %w", err)
	}
	if _, err := fd.WriteString(hex.EncodeToString(k) + "\n"); err != nil {
		fd.Close()
		return nil, err
	}
	if err := fd.Close(); err != nil {
		return nil, err
	}
	return f.read()
}

func (f File) read() ([]byte, error) {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	defer Zero(raw)
	if info, err := os.Stat(f.Path); err == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("keyfile %s permissions %04o too open; want 0600", f.Path, info.Mode().Perm())
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(b) != 32 {
		Zero(b)
		return nil, errors.New("keyfile wrap key malformed")
	}
	return b, nil
}
