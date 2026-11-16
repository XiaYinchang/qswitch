package antigravity

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
)

// Use the same Secret Service implementation as the official application:
// it selects collection/login when available, then aliases/default, and
// distinguishes a missing item from an unsuccessful unlock.
type linuxCredentials struct {
	get    func(string, string) (string, error)
	set    func(string, string, string) error
	remove func(string, string) error
}

func nativeCredentialBackend() credentialBackend {
	return linuxCredentials{keyring.Get, keyring.Set, keyring.Delete}
}

func credentialProcessExecutable(pid int) string {
	if pid <= 0 {
		return ""
	}
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return path
}

func (k linuxCredentials) Read() ([]byte, error) {
	value, err := k.get(nativeService, nativeAccount)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrCredentialNotFound
	}
	if missingSecretService(err) {
		return nil, ErrKeyringUnavailable
	}
	if err != nil {
		return nil, errors.New("antigravity: Secret Service read failed; ensure the session keyring is available and unlocked")
	}
	return []byte(value), nil
}

func missingSecretService(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var value dbus.Error
	var pointer *dbus.Error
	name := ""
	if errors.As(err, &value) {
		name = value.Name
	} else if errors.As(err, &pointer) {
		name = pointer.Name
	}
	switch name {
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner", "org.freedesktop.DBus.Error.NoServer":
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" && (op.Net == "unix" || op.Net == "unixpacket") {
		return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
	}
	// This exact diagnostic is produced by godbus when no session address
	// exists; do not treat general D-Bus or unlock errors as absence.
	return err.Error() == "dbus: couldn't determine address of session bus"
}

func (k linuxCredentials) Write(raw []byte) error {
	if err := k.set(nativeService, nativeAccount, string(raw)); err != nil {
		return errors.New("antigravity: Secret Service write failed; ensure the session keyring is available and unlocked")
	}
	return nil
}

func (k linuxCredentials) Delete() error {
	if err := k.remove(nativeService, nativeAccount); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return errors.New("antigravity: Secret Service rollback deletion failed")
	}
	return nil
}
