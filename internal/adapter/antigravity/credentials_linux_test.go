package antigravity

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
)

func TestLinuxNativeErrorsDistinguishMissingLockedAndAbsentBackend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err, want error
	}{
		{"missing", keyring.ErrNotFound, ErrCredentialNotFound},
		{"no_bus_launcher", exec.ErrNotFound, ErrKeyringUnavailable},
		{"service_unknown", dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown", Body: []any{"synthetic-secret"}}, ErrKeyringUnavailable},
		{"no_server", &dbus.Error{Name: "org.freedesktop.DBus.Error.NoServer"}, ErrKeyringUnavailable},
		{"no_socket", &net.OpError{Op: "dial", Net: "unix", Err: os.ErrNotExist}, ErrKeyringUnavailable},
		{"permission_denied", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"synthetic-secret"}}, nil},
		{"cancelled", dbus.Error{Name: "org.freedesktop.Secret.Error.Cancelled"}, nil},
		{"locked_dbus", dbus.Error{Name: "org.freedesktop.Secret.Error.IsLocked"}, nil},
		{"locked", errors.New("locked synthetic-secret"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := linuxCredentials{get: func(service, user string) (string, error) {
				if service != "gemini" || user != "antigravity" {
					t.Fatal("wrong native scope")
				}
				return "", tc.err
			}}
			_, err := k.Read()
			if err == nil {
				t.Fatal("native failure ignored")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatal("incorrect native failure classification")
			}
			if tc.want == nil && (errors.Is(err, ErrCredentialNotFound) || errors.Is(err, ErrKeyringUnavailable)) {
				t.Fatal("locked native item treated as missing")
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("native details leaked")
			}
		})
	}
}

func TestLinuxNativeWriteAndDeleteErrorsAreRedacted(t *testing.T) {
	k := linuxCredentials{
		set: func(service, user, raw string) error {
			if service != "gemini" || user != "antigravity" || raw != "synthetic-credential" {
				t.Fatal("wrong native write")
			}
			return errors.New("synthetic-secret")
		},
		remove: func(string, string) error { return errors.New("synthetic-secret") },
	}
	for _, err := range []error{k.Write([]byte("synthetic-credential")), k.Delete()} {
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("native failure was ignored or leaked")
		}
	}
}
