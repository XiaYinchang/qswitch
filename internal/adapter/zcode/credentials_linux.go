package zcode

import (
	"context"
	"time"

	"github.com/godbus/dbus/v5"
)

const secretService = "org.freedesktop.secrets"
const secretServicePath dbus.ObjectPath = "/org/freedesktop/secrets"

type secretServiceCall func(path dbus.ObjectPath, method string, args ...any) ([]any, error)

type secretServiceSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func readOSCredential(service, account string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := dbus.SessionBusPrivateNoAutoStartup(dbus.WithContext(ctx))
	if err != nil {
		return "", ErrOSCredential
	}
	defer conn.Close()
	if conn.Auth(nil) != nil || conn.Hello() != nil {
		return "", ErrOSCredential
	}
	return readSecretServiceCredential(service, account, func(path dbus.ObjectPath, method string, args ...any) ([]any, error) {
		call := conn.Object(secretService, path).CallWithContext(ctx, method, 0, args...)
		return call.Body, call.Err
	})
}

func readSecretServiceCredential(service, account string, call secretServiceCall) (string, error) {
	// Search the whole service, matching the desktop's Rust keyring backend.
	// Searching only the login collection or accepting the first match could
	// silently choose a different key when multiple collections are present.
	body, err := call(secretServicePath, "org.freedesktop.Secret.Service.SearchItems", map[string]string{"service": service, "username": account})
	if err != nil {
		return "", ErrOSCredential
	}
	var unlocked, locked []dbus.ObjectPath
	if dbus.Store(body, &unlocked, &locked) != nil {
		return "", ErrOSCredential
	}
	items := map[dbus.ObjectPath]bool{}
	for _, path := range unlocked {
		items[path] = true
	}
	for _, path := range locked {
		items[path] = false
	}
	if len(items) > 1 {
		return "", ErrOSCredentialAmbiguous
	}
	var item dbus.ObjectPath
	for path, available := range items {
		if !available || !path.IsValid() || path == "/" {
			return "", ErrOSCredential
		}
		item = path
	}
	if item == "" {
		return "", ErrOSCredential
	}
	body, err = call(secretServicePath, "org.freedesktop.Secret.Service.OpenSession", "plain", dbus.MakeVariant(""))
	if err != nil {
		return "", ErrOSCredential
	}
	var output dbus.Variant
	var session dbus.ObjectPath
	if dbus.Store(body, &output, &session) != nil || !session.IsValid() || session == "/" {
		return "", ErrOSCredential
	}
	defer call(session, "org.freedesktop.Secret.Session.Close")
	body, err = call(item, "org.freedesktop.Secret.Item.GetSecret", session)
	if err != nil {
		return "", ErrOSCredential
	}
	var secret secretServiceSecret
	if dbus.Store(body, &secret) != nil || secret.Session != session || len(secret.Parameters) != 0 {
		return "", ErrOSCredential
	}
	return string(secret.Value), nil
}
