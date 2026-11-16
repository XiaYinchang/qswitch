//go:build !windows

package antigravity

import (
	"os"
	"reflect"
)

func privateDaemonOwner(_ string, info os.FileInfo) bool {
	if info.Mode().Perm()&0077 != 0 {
		return false
	}
	// Stat_t differs by OS. Unknown ownership representations fail closed.
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	uid := value.FieldByName("Uid")
	return uid.IsValid() && uid.CanUint() && uid.Uint() == uint64(os.Getuid())
}

func createPrivateMetadataFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}
