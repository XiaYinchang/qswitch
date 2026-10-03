package busy

import (
	"os"
	"path/filepath"
	"time"
)

// FilesIdle requires readable activity evidence before allowing a running CLI
// to be terminated. Missing files and traversal failures cannot prove idleness.
func FilesIdle(root string, grace time.Duration, now time.Time) bool {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return false
	}
	var newest time.Time
	err = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return err == nil && !newest.IsZero() && now.Sub(newest) >= grace
}
