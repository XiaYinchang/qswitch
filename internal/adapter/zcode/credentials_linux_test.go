package zcode

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestSecretServiceUsesUniqueItemAcrossCollectionsAndClosesSession(t *testing.T) {
	item := dbus.ObjectPath("/org/freedesktop/secrets/collection/custom/item1")
	session := dbus.ObjectPath("/org/freedesktop/secrets/session/fixture")
	value := strings.Repeat("a", 43)
	var calls []string
	got, err := readSecretServiceCredential("fixture-service", "fixture-account", func(path dbus.ObjectPath, method string, args ...any) ([]any, error) {
		calls = append(calls, method)
		switch method {
		case "org.freedesktop.Secret.Service.SearchItems":
			if path != secretServicePath || len(args) != 1 || !reflect.DeepEqual(args[0], map[string]string{"service": "fixture-service", "username": "fixture-account"}) {
				t.Fatal("wrong whole-service search identity")
			}
			return []any{[]dbus.ObjectPath{item}, []dbus.ObjectPath{}}, nil
		case "org.freedesktop.Secret.Service.OpenSession":
			if path != secretServicePath || len(args) != 2 || args[0] != "plain" || args[1].(dbus.Variant).Value() != "" {
				t.Fatal("wrong read session")
			}
			return []any{dbus.MakeVariant(""), session}, nil
		case "org.freedesktop.Secret.Item.GetSecret":
			if path != item || !reflect.DeepEqual(args, []any{session}) {
				t.Fatal("read the wrong secret item or session")
			}
			return []any{[]any{session, []byte{}, []byte(value), "text/plain; charset=utf8"}}, nil
		case "org.freedesktop.Secret.Session.Close":
			if path != session || len(args) != 0 {
				t.Fatal("closed the wrong session")
			}
			return nil, nil
		default:
			t.Fatalf("unexpected keyring mutation or call %s", method)
			return nil, nil
		}
	})
	if err != nil || got != value {
		t.Fatalf("secret read failed: %v", err)
	}
	want := []string{"org.freedesktop.Secret.Service.SearchItems", "org.freedesktop.Secret.Service.OpenSession", "org.freedesktop.Secret.Item.GetSecret", "org.freedesktop.Secret.Session.Close"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("keyring calls = %v", calls)
	}
}

func TestSecretServiceFailsClosedBeforeReadingAmbiguousOrLockedItems(t *testing.T) {
	first := dbus.ObjectPath("/org/freedesktop/secrets/collection/first/item1")
	second := dbus.ObjectPath("/org/freedesktop/secrets/collection/second/item2")
	for _, tc := range []struct {
		name             string
		unlocked, locked []dbus.ObjectPath
		want             error
	}{
		{"missing", nil, nil, ErrOSCredential},
		{"locked", nil, []dbus.ObjectPath{first}, ErrOSCredential},
		{"multiple unlocked", []dbus.ObjectPath{first, second}, nil, ErrOSCredentialAmbiguous},
		{"locked and unlocked", []dbus.ObjectPath{first}, []dbus.ObjectPath{second}, ErrOSCredentialAmbiguous},
		{"invalid path", []dbus.ObjectPath{"/"}, nil, ErrOSCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := readSecretServiceCredential("fixture-service", "fixture-account", func(path dbus.ObjectPath, method string, args ...any) ([]any, error) {
				calls++
				if method != "org.freedesktop.Secret.Service.SearchItems" {
					t.Fatal("read or unlocked an ambiguous/missing/locked secret")
				}
				return []any{tc.unlocked, tc.locked}, nil
			})
			if !errors.Is(err, tc.want) || calls != 1 {
				t.Fatalf("read err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSecretServiceReadErrorStillClosesSessionWithoutLeakingBackendError(t *testing.T) {
	item := dbus.ObjectPath("/org/freedesktop/secrets/collection/custom/item1")
	session := dbus.ObjectPath("/org/freedesktop/secrets/session/fixture")
	closed := false
	_, err := readSecretServiceCredential("fixture-service", "fixture-account", func(path dbus.ObjectPath, method string, args ...any) ([]any, error) {
		switch method {
		case "org.freedesktop.Secret.Service.SearchItems":
			return []any{[]dbus.ObjectPath{item}, []dbus.ObjectPath{}}, nil
		case "org.freedesktop.Secret.Service.OpenSession":
			return []any{dbus.MakeVariant(""), session}, nil
		case "org.freedesktop.Secret.Item.GetSecret":
			return nil, errors.New("backend-secret-must-not-escape")
		case "org.freedesktop.Secret.Session.Close":
			closed = true
			return nil, nil
		default:
			t.Fatal("unexpected keyring operation")
			return nil, nil
		}
	})
	if !errors.Is(err, ErrOSCredential) || !closed || strings.Contains(err.Error(), "backend-secret-must-not-escape") {
		t.Fatal("read failure leaked context or retained the session")
	}
}
