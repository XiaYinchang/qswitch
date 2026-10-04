package grok

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/snapshot"
)

func TestRestoreCLIOnlyPreservesDesktop(t *testing.T) {
	for _, desktopOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "attached desktop", true: "desktop only"}[desktopOnly], func(t *testing.T) {
			t.Setenv("QSWITCH_IN_TEST", "1")
			home := t.TempDir()
			cookies := filepath.Join(home, snapshot.GrokBotRelRoot, "Cookies")
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			savedAuth := []byte(`{"https://auth.x.ai::test":{"principal_id":"saved","email":"saved@example.com","key":"saved-token"}}`)
			liveAuth := []byte(`{"https://auth.x.ai::test":{"principal_id":"live","email":"live@example.com","key":"live-token"}}`)
			write(authPath(home), savedAuth)
			write(cookies, []byte("saved-desktop-session"))
			// No desktop or CLI process is running, so only CLIOnly can prevent
			// the attached desktop snapshot from being restored.
			ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
			blobs, _, err := ad.Capture(home)
			if err != nil || len(blobs) == 0 {
				t.Fatalf("capture: %v, blobs=%d", err, len(blobs))
			}
			blob := blobs[0]
			if desktopOnly {
				blob, err = captureDesktop(home)
				if err != nil {
					t.Fatal(err)
				}
			}
			write(authPath(home), liveAuth)
			write(cookies, []byte("live-desktop-session"))

			err = ad.Restore(home, blob, adapter.RestoreOpts{CLIOnly: true})
			wantAuth := savedAuth
			if desktopOnly {
				wantAuth = liveAuth
				if err == nil || !strings.Contains(err.Error(), "desktop-only") {
					t.Errorf("desktop-only snapshot must be rejected: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			gotAuth, err := os.ReadFile(authPath(home))
			if err != nil || !bytes.Equal(gotAuth, wantAuth) {
				t.Errorf("unexpected CLI credentials, read error: %v", err)
			}
			gotCookies, err := os.ReadFile(cookies)
			if err != nil || string(gotCookies) != "live-desktop-session" {
				t.Errorf("CLI-only restore changed desktop cookies, read error: %v", err)
			}
		})
	}
}
