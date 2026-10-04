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
	return Restore(root, data, nil)
}
