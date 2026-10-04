package snapshot

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"qswitch/internal/livefile"
)

type restoreFile struct {
	path     string
	data     []byte
	previous []byte
	mode     os.FileMode
	existed  bool
	dir      bool
}

// Restore validates the entire archive before writing and rolls back only its
// affected files on failure. commit runs last; it must either succeed or leave
// its own target unchanged (for example, an AtomicWrite of CLI credentials).
// This recovers returned errors, not interruption of the process or host.
func Restore(root string, data []byte, commit func() error) (retErr error) {
	files, err := prepareRestore(root, data)
	if err != nil {
		return err
	}
	var applied []restoreFile
	var createdDirs []string
	defer func() {
		if retErr == nil {
			return
		}
		for i := len(applied) - 1; i >= 0; i-- {
			f := applied[i]
			var err error
			if f.existed {
				err = livefile.AtomicWrite(f.path, f.previous, f.mode)
			} else {
				err = os.Remove(f.path)
				if os.IsNotExist(err) {
					err = nil
				}
			}
			if err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("snapshot: rollback %s: %w", f.path, err))
			}
		}
		for i := len(createdDirs) - 1; i >= 0; i-- {
			if err := os.Remove(createdDirs[i]); err != nil && !os.IsNotExist(err) {
				retErr = errors.Join(retErr, fmt.Errorf("snapshot: remove created directory %s: %w", createdDirs[i], err))
			}
		}
	}()
	for _, f := range files {
		dir := filepath.Dir(f.path)
		if f.dir {
			dir = f.path
		}
		if err := mkdirRestore(dir, &createdDirs); err != nil {
			return err
		}
		if f.dir {
			continue
		}
		if err := livefile.AtomicWrite(f.path, f.data, 0o600); err != nil {
			return fmt.Errorf("snapshot: write %s: %w", f.path, err)
		}
		applied = append(applied, f)
	}
	if commit != nil {
		return commit()
	}
	return nil
}

func prepareRestore(root string, data []byte) ([]restoreFile, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	files := make([]restoreFile, 0, len(zr.File))
	for _, entry := range zr.File {
		name := filepath.Clean(entry.Name)
		if name == "." || !filepath.IsLocal(name) {
			return nil, fmt.Errorf("snapshot: unsafe archive path %q", entry.Name)
		}
		if seen[name] {
			return nil, fmt.Errorf("snapshot: duplicate archive path %q", entry.Name)
		}
		seen[name] = true
		mode := entry.Mode()
		if mode&os.ModeType != 0 && !mode.IsDir() {
			return nil, fmt.Errorf("snapshot: non-regular archive entry %q", entry.Name)
		}
		f := restoreFile{path: filepath.Join(root, name), dir: mode.IsDir()}
		if err := checkRestorePath(root, f.path, f.dir); err != nil {
			return nil, err
		}
		if !f.dir {
			rc, err := entry.Open()
			if err != nil {
				return nil, err
			}
			f.data, err = io.ReadAll(rc) // Reading to EOF verifies the ZIP CRC.
			err = errors.Join(err, rc.Close())
			if err != nil {
				return nil, fmt.Errorf("snapshot: read %q: %w", entry.Name, err)
			}
			info, err := os.Lstat(f.path)
			if err == nil {
				f.previous, err = os.ReadFile(f.path)
				if err != nil {
					return nil, err
				}
				f.existed, f.mode = true, info.Mode().Perm()
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
		files = append(files, f)
	}
	return files, nil
}

func checkRestorePath(root, path string, dir bool) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			wantDir := current != path || dir
			if info.Mode()&os.ModeSymlink != 0 || (wantDir && !info.IsDir()) || (!wantDir && !info.Mode().IsRegular()) {
				return fmt.Errorf("snapshot: unsafe destination %s", current)
			}
		}
		if current == root {
			return nil
		}
	}
}

func mkdirRestore(path string, created *[]string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("snapshot: destination parent is not a directory: %s", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := mkdirRestore(filepath.Dir(path), created); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	*created = append(*created, path)
	return nil
}
