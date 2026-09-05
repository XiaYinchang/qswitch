package snapshot

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func Pack(root string, rels []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	added := 0
	for _, rel := range rels {
		rel = filepath.Clean(rel)
		if rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		full := filepath.Join(root, rel)
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.IsDir() {
			err = filepath.Walk(full, func(path string, fi os.FileInfo, err error) error {
				if err != nil || fi == nil || fi.IsDir() {
					return nil
				}
				relPath, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				return writeFile(zw, path, relPath)
			})
			if err != nil {
				zw.Close()
				return nil, err
			}
			added++
			continue
		}
		if err := writeFile(zw, full, rel); err != nil {
			zw.Close()
			return nil, err
		}
		added++
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if added == 0 {
		return nil, fmt.Errorf("snapshot: nothing to pack")
	}
	return buf.Bytes(), nil
}

func writeFile(zw *zip.Writer, path, rel string) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	w, err := zw.Create(filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func Unpack(root string, data []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			continue
		}
		dest := filepath.Join(root, name)
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(dest, 0o700)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
