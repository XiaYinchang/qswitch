package zcode

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var credentialDLL = windows.NewLazySystemDLL("advapi32.dll")

// @napi-rs/keyring uses windows-native-keyring-store: its target is
// username.service and its password is UTF-16LE without a terminator.
func readOSCredential(service, account string) (string, error) {
	name, err := windows.UTF16PtrFromString(windowsCredentialTarget(service, account))
	if err != nil {
		return "", ErrOSCredential
	}
	var ptr *windowsCredential
	read := credentialDLL.NewProc("CredReadW")
	free := credentialDLL.NewProc("CredFree")
	result, _, _ := read.Call(uintptr(unsafe.Pointer(name)), 1, 0, uintptr(unsafe.Pointer(&ptr)))
	runtime.KeepAlive(name)
	if result == 0 {
		return "", ErrOSCredential
	}
	defer free.Call(uintptr(unsafe.Pointer(ptr)))
	if ptr == nil || ptr.CredentialBlobSize != 86 || ptr.CredentialBlob == nil {
		return "", ErrOSCredentialFormat
	}
	raw := unsafe.Slice(ptr.CredentialBlob, int(ptr.CredentialBlobSize))
	defer clear(raw)
	return decodeWindowsCredential(raw)
}
