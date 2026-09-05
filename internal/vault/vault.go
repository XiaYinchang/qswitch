package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"qswitch/internal/secutil"
)

var magic = []byte("qsw1")

type Store struct {
	Dir  string
	Keys secutil.KeyProvider
	Bins []string
}

func (s *Store) key() ([]byte, error) {
	if s.Keys == nil {
		return nil, errors.New("vault: no key provider")
	}
	return s.Keys.GetOrCreate(s.Bins)
}

func (s *Store) path(tool, id string) string {
	return filepath.Join(s.Dir, "vault", tool, id+".enc")
}

func aad(tool, id string) []byte {
	return []byte("qsw1|" + tool + "|" + id)
}

func (s *Store) Put(tool, id string, plaintext []byte) error {
	if tool == "" || id == "" {
		return errors.New("vault: empty identity")
	}
	if strings.Contains(id, "/") || strings.Contains(id, "..") {
		return errors.New("vault: bad id")
	}
	k, err := s.key()
	if err != nil {
		return err
	}
	defer secutil.Zero(k)
	block, err := aes.NewCipher(k)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad(tool, id))
	out := make([]byte, 0, 4+len(nonce)+len(ct))
	out = append(out, magic...)
	out = append(out, nonce...)
	out = append(out, ct...)
	dir := filepath.Dir(s.path(tool, id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".enc-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(tool, id))
}

func (s *Store) Get(tool, id string) ([]byte, error) {
	k, err := s.key()
	if err != nil {
		return nil, err
	}
	defer secutil.Zero(k)
	raw, err := os.ReadFile(s.path(tool, id))
	if err != nil {
		return nil, err
	}
	if len(raw) < 4+12+16 || string(raw[:4]) != "qsw1" {
		return nil, errors.New("vault: bad blob")
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := raw[4 : 4+gcm.NonceSize()]
	ct := raw[4+gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, aad(tool, id))
	if err != nil {
		return nil, fmt.Errorf("vault: decrypt: %w", err)
	}
	return pt, nil
}

func (s *Store) Delete(tool, id string) error {
	err := os.Remove(s.path(tool, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) List(tool string) ([]string, error) {
	dir := filepath.Join(s.Dir, "vault", tool)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".enc") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".enc"))
	}
	return ids, nil
}

// Envelope is stored as plaintext JSON inside the ciphertext. Callers must not log it.
type Envelope struct {
	Kind     string          `json:"kind"`
	Identity json.RawMessage `json:"identity"`
	Payload  json.RawMessage `json:"payload"`
}

func Marshal(kind string, identity, payload any) ([]byte, error) {
	id, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	pl, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Kind: kind, Identity: id, Payload: pl})
}
