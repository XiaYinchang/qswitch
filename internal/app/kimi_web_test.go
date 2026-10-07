package app

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"qswitch/internal/clock"
)

// writeLDBLog builds a leveldb journal file containing localStorage puts,
// mirroring how the Kimi desktop app appends web session refreshes.
func writeLDBLog(t *testing.T, path string, puts []struct{ key, val string }) {
	t.Helper()
	var batch bytes.Buffer
	batch.Write(make([]byte, 8)) // sequence
	binary.Write(&batch, binary.LittleEndian, uint32(len(puts)))
	for _, p := range puts {
		batch.WriteByte(1)
		batch.Write(ldbVarintBytes(uint64(len(p.key))))
		batch.WriteString(p.key)
		batch.Write(ldbVarintBytes(uint64(len(p.val))))
		batch.WriteString(p.val)
	}
	var file bytes.Buffer
	file.Write(make([]byte, 4)) // crc placeholder
	binary.Write(&file, binary.LittleEndian, uint16(batch.Len()))
	file.WriteByte(1) // Chromium standalone-batch type
	file.Write(batch.Bytes())
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ldbVarintBytes(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func webToken(sub string, exp int64) string {
	claims := `{"iss":"account","exp":` + strconv.FormatInt(exp, 10) + `,"sub":"` + sub + `","device_id":"dev-1","ssid":"sess-1","region":"cn"}`
	return "eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
}

func TestKimiWebSessionToken(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	ldb := filepath.Join(home, "Library", "Application Support", "kimi-desktop", "Local Storage", "leveldb")
	if err := os.MkdirAll(ldb, 0o700); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	expired := webToken("user-1", now.Add(-time.Minute).Unix())
	fresh := webToken("user-1", now.Add(10*time.Minute).Unix())
	newer := webToken("user-2", now.Add(20*time.Minute).Unix())

	// Multiple refresh generations; the newest unexpired must win.
	writeLDBLog(t, filepath.Join(ldb, "000003.log"), []struct{ key, val string }{
		{"_https://www.kimi.com\x00\x01theme", "dark"},
		{"_https://www.kimi.com\x00\x01access_token", "\x01" + expired},
	})
	writeLDBLog(t, filepath.Join(ldb, "000005.log"), []struct{ key, val string }{
		{"_https://www.kimi.com\x00\x01refresh_token", "eyJhbGciOiJI.sig"},
		{"_https://www.kimi.com\x00\x01access_token", "\x01" + fresh},
		{"_https://www.kimi.com\x00\x01access_token", newer},
	})

	a := &App{UserHome: home, Clock: clock.Fixed{T: now}}
	s, host, ok := a.kimiWebSessionToken()
	if !ok || host != "www.kimi.com" || s.Sub != "user-2" || s.DeviceID != "dev-1" || s.SessionID != "sess-1" {
		t.Fatalf("%+v host=%q ok=%v", s, host, ok)
	}

	// Only expired sessions remain: nothing may be reported.
	if err := os.Remove(filepath.Join(ldb, "000005.log")); err != nil {
		t.Fatal(err)
	}
	if s, host, ok = a.kimiWebSessionToken(); ok || host != "" || s.Sub != "" {
		t.Fatalf("expired token must not be reported: %+v %q %v", s, host, ok)
	}

	// The oversea origin is reported with its own host.
	writeLDBLog(t, filepath.Join(ldb, "000007.log"), []struct{ key, val string }{
		{"_https://www.kimi.ai\x00\x01access_token", webToken("user-9", now.Add(15*time.Minute).Unix())},
	})
	if s, host, ok = a.kimiWebSessionToken(); !ok || host != "www.kimi.ai" || s.Sub != "user-9" {
		t.Fatalf("oversea origin: %+v %q %v", s, host, ok)
	}

	// A home without the desktop app stays quiet.
	b := &App{UserHome: filepath.Join(root, "empty"), Clock: clock.Fixed{T: now}}
	if _, _, ok := b.kimiWebSessionToken(); ok {
		t.Fatal("no leveldb must report nothing")
	}
}
