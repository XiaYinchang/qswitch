package antigravity

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Native go-keyring stores a generic credential under service + ":" + user.
// The layout below is Win32 CREDENTIALW; CredentialBlob is raw UTF-8 JSON.
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

var (
	credentialDLL = windows.NewLazySystemDLL("advapi32.dll")
	credRead      = credentialDLL.NewProc("CredReadW")
	credWrite     = credentialDLL.NewProc("CredWriteW")
	credDelete    = credentialDLL.NewProc("CredDeleteW")
	credFree      = credentialDLL.NewProc("CredFree")
)

type windowsCredentials struct{}

func nativeCredentialBackend() credentialBackend { return windowsCredentials{} }

func credentialProcessExecutable(pid int) string {
	if pid <= 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

func credentialTarget() *uint16 {
	p, _ := windows.UTF16PtrFromString(nativeService + ":" + nativeAccount)
	return p
}

func (windowsCredentials) Read() ([]byte, error) {
	var ptr *windowsCredential
	name := credentialTarget()
	r, _, err := credRead.Call(uintptr(unsafe.Pointer(name)), 1, 0, uintptr(unsafe.Pointer(&ptr)))
	runtime.KeepAlive(name)
	if r == 0 {
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			return nil, ErrCredentialNotFound
		}
		return nil, errors.New("antigravity: Windows Credential Manager read failed")
	}
	defer credFree.Call(uintptr(unsafe.Pointer(ptr)))
	if ptr == nil || ptr.CredentialBlobSize > maxCredentialBytes {
		return nil, errors.New("invalid native Windows credential")
	}
	if ptr.CredentialBlobSize == 0 {
		return nil, ErrCredentialNotFound
	}
	if ptr.CredentialBlob == nil {
		return nil, errors.New("invalid native Windows credential")
	}
	return append([]byte(nil), unsafe.Slice(ptr.CredentialBlob, int(ptr.CredentialBlobSize))...), nil
}

func (windowsCredentials) Write(raw []byte) error {
	if len(raw) == 0 || len(raw) > 2560 {
		return errors.New("native Windows credential exceeds its 2560-byte limit")
	}
	user, _ := windows.UTF16PtrFromString(nativeAccount)
	c := windowsCredential{Type: 1, TargetName: credentialTarget(), CredentialBlobSize: uint32(len(raw)), CredentialBlob: &raw[0], Persist: 2, UserName: user}
	r, _, _ := credWrite.Call(uintptr(unsafe.Pointer(&c)), 0)
	runtime.KeepAlive(c)
	runtime.KeepAlive(raw)
	if r == 0 {
		return errors.New("antigravity: Windows Credential Manager write failed")
	}
	return nil
}

func (windowsCredentials) Delete() error {
	name := credentialTarget()
	r, _, err := credDelete.Call(uintptr(unsafe.Pointer(name)), 1, 0)
	runtime.KeepAlive(name)
	if r == 0 && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		return errors.New("antigravity: Windows Credential Manager rollback deletion failed")
	}
	return nil
}
