package snapshot

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func restoreArchive(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, name := range names {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("saved:" + name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRestoreValidatesBeforeWriting(t *testing.T) {
	for _, failure := range []string{"bad CRC", "path escape", "absolute path", "destination symlink", "duplicate path"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			cookies := filepath.Join(root, "Cookies")
			if err := os.WriteFile(cookies, []byte("live"), 0o600); err != nil {
				t.Fatal(err)
			}
			var archive []byte
			switch failure {
			case "bad CRC":
				archive = restoreArchive(t, "Cookies", "other")
				i := bytes.Index(archive, []byte("saved:other"))
				if i < 0 {
					t.Fatal("missing ZIP test payload")
				}
				archive[i] ^= 1
			case "path escape":
				archive = restoreArchive(t, "Cookies", "../outside")
			case "absolute path":
				archive = restoreArchive(t, "Cookies", filepath.Join(t.TempDir(), "outside"))
			case "destination symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
				archive = restoreArchive(t, "Cookies", "linked/outside")
			case "duplicate path":
				archive = restoreArchive(t, "Cookies", "Cookies")
			}
			committed := false
			if err := Restore(root, archive, func() error { committed = true; return nil }); err == nil {
				t.Fatal("invalid snapshot succeeded")
			}
			if got, err := os.ReadFile(cookies); err != nil || string(got) != "live" {
				t.Fatalf("validation failure changed an earlier file: %v", err)
			}
			if committed {
				t.Fatal("invalid snapshot committed CLI credentials")
			}
		})
	}
}

func TestRestoreRollsBackCommitFailure(t *testing.T) {
	root := t.TempDir()
	cookies := filepath.Join(root, "Cookies")
	if err := os.WriteFile(cookies, []byte("live"), 0o640); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(root, "unrelated")
	if err := os.WriteFile(unrelated, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("CLI auth write failed")
	err := Restore(root, restoreArchive(t, "Cookies", "Session Storage/new"), func() error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("lost commit error: %v", err)
	}
	if got, err := os.ReadFile(cookies); err != nil || string(got) != "live" {
		t.Fatalf("existing file not restored: %v", err)
	}
	if info, err := os.Stat(cookies); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("existing permissions not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Session Storage")); !os.IsNotExist(err) {
		t.Fatalf("new file/directory remains: %v", err)
	}
	if got, err := os.ReadFile(unrelated); err != nil || string(got) != "untouched" {
		t.Fatalf("unrelated file changed: %v", err)
	}
}

func TestRestoreRollsBackPartialWrite(t *testing.T) {
	root := t.TempDir()
	cookies := filepath.Join(root, "Cookies")
	if err := os.WriteFile(cookies, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(root, "blocked")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	if f, err := os.CreateTemp(blocked, "permission-check"); err == nil {
		f.Close()
		os.Remove(f.Name())
		t.Skip("filesystem permits writes despite directory permissions")
	}
	committed := false
	err := Restore(root, restoreArchive(t, "Cookies", "blocked/other"), func() error { committed = true; return nil })
	if err == nil || committed {
		t.Fatalf("write failure must prevent commit: %v", err)
	}
	if got, err := os.ReadFile(cookies); err != nil || string(got) != "live" {
		t.Fatalf("earlier file not restored after write failure: %v", err)
	}
}

func TestRestoreReportsRollbackFailure(t *testing.T) {
	root := t.TempDir()
	cookies := filepath.Join(root, "Cookies")
	if err := os.WriteFile(cookies, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("CLI auth write failed")
	err := Restore(root, restoreArchive(t, "Cookies"), func() error {
		if err := os.Remove(cookies); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cookies, 0o700); err != nil {
			t.Fatal(err)
		}
		return failure
	})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("must preserve commit and rollback errors: %v", err)
	}
}
