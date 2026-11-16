package secutil

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// File stores the wrapping key protected by the current Windows user's DPAPI.
// Unix permission bits do not describe Windows ACLs and are not used here.
type File struct{ Path string }

const dpapiPrefix = "qswitch-dpapi-v1\n"

func (f File) GetOrCreate([]string) ([]byte, error) {
	if strings.TrimSpace(f.Path) == "" {
		return nil, errors.New("secutil: empty key file path")
	}
	if key, err := f.read(); err == nil {
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	defer Zero(key)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	protected, err := protectKey(key, false)
	if err != nil {
		return nil, err
	}
	defer Zero(protected)
	file, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return f.read()
		}
		return nil, err
	}
	_, writeErr := file.WriteString(dpapiPrefix + base64.StdEncoding.EncodeToString(protected) + "\n")
	err = errors.Join(writeErr, file.Close())
	if err != nil {
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
	if !strings.HasPrefix(string(raw), dpapiPrefix) {
		return nil, errors.New("keyfile is not a Windows DPAPI-protected wrapping key")
	}
	protected, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(string(raw), dpapiPrefix)))
	if err != nil {
		return nil, errors.New("keyfile wrap key malformed")
	}
	defer Zero(protected)
	key, err := protectKey(protected, true)
	if err != nil {
		return nil, fmt.Errorf("unprotect wrapping key for current Windows user: %w", err)
	}
	if len(key) != 32 {
		Zero(key)
		return nil, errors.New("keyfile wrap key malformed")
	}
	return key, nil
}

func protectKey(raw []byte, decrypt bool) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty DPAPI input")
	}
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	var err error
	if decrypt {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	bytes := unsafe.Slice(out.Data, int(out.Size))
	result := append([]byte(nil), bytes...)
	Zero(bytes)
	return result, nil
}
