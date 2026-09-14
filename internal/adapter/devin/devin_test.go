package devin

import (
	"os"
	"path/filepath"
	"testing"

	"qswitch/internal/adapter"
)

func TestCaptureRestore(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "devin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := "windsurf_api_key = \"devin-session-token$abc\"\napi_server_url = \"https://server.codeium.com\"\n"
	if err := os.WriteFile(filepath.Join(dir, "credentials.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	blobs, _, err := ad.Capture(home)
	if err != nil || len(blobs) != 1 || blobs[0].Identity.StableID == "" {
		t.Fatalf("%v %#v", err, blobs)
	}
	os.Remove(filepath.Join(dir, "credentials.toml"))
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "credentials.toml"))
	if string(got) != toml {
		t.Fatalf("got %q", got)
	}
}

func TestParseCreds(t *testing.T) {
	c, err := ParseCreds([]byte("windsurf_api_key = \"k\"\napi_server_url = \"https://server.codeium.com\"\n"))
	if err != nil || c.APIKey != "k" || c.Server != "https://server.codeium.com" {
		t.Fatalf("%+v %v", c, err)
	}
}
