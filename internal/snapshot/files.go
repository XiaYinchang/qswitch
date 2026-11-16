package snapshot

import (
	"errors"
	"fmt"
	"os"

	"qswitch/internal/livefile"
)

// FileWrite describes a native credential file, never a path from a snapshot.
type FileWrite struct {
	Path string
	Data []byte
	Mode os.FileMode
}

// CheckpointFiles saves exact file contents, permissions and absence for a
// returned-error rollback. It deliberately refuses unreadable and nonregular
// targets before any mutation. Callers retain the undo through pointer commit.
func CheckpointFiles(paths ...string) (func() error, error) {
	var saved []restoreFile
	for _, path := range paths {
		f := restoreFile{path: path}
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("snapshot: nonregular credential target %s", path)
			}
			f.previous, err = os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			f.existed, f.mode = true, info.Mode().Perm()
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		saved = append(saved, f)
	}
	return func() error {
		var result error
		for i := len(saved) - 1; i >= 0; i-- {
			f := saved[i]
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
				result = errors.Join(result, fmt.Errorf("snapshot: rollback %s: %w", f.path, err))
			}
		}
		return result
	}, nil
}

// WriteFiles commits a small group of native files and returns its undo.
func WriteFiles(files ...FileWrite) (func() error, error) {
	return writeFiles(files, livefile.AtomicWrite)
}

func writeFiles(files []FileWrite, write func(string, []byte, os.FileMode) error) (func() error, error) {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	undo, err := CheckpointFiles(paths...)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := write(f.Path, f.Data, f.Mode); err != nil {
			return nil, errors.Join(err, undo())
		}
	}
	return undo, nil
}
