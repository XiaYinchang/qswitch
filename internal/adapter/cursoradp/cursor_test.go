package cursoradp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"qswitch/internal/adapter"
)

func TestDualWriteAndIncomplete(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	vpath := vscdb(home)
	os.MkdirAll(filepath.Dir(vpath), 0o755)
	if err := initMiniDB(vpath); err != nil {
		t.Fatal(err)
	}
	desk := map[string]string{
		"cursorAuth/accessToken":            "d-tok",
		"cursorAuth/refreshToken":           "d-ref",
		"cursorAuth/cachedEmail":            "desk@x.com",
		"cursorAuth/stripeMembershipAuthId": "auth0|desk",
		"cursorAuth/stripeMembershipType":   "ultra",
	}
	if err := writeDesktop(home, desk); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	os.WriteFile(cliAuth(home), []byte(`{"accessToken":"c-tok","refreshToken":"c-ref"}`), 0o600)
	os.WriteFile(cliCfg(home), []byte(`{"authInfo":{"email":"cli@x.com","authId":"google-oauth2|cli"}}`), 0o644)

	ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	blobs, warn, err := ad.Capture(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 2 {
		t.Fatalf("want 2 desync blobs, got %d warn=%v", len(blobs), warn)
	}
	if blobs[0].Incomplete || blobs[1].Incomplete {
		t.Fatalf("both accounts should be saved complete, got inc=%v/%v", blobs[0].Incomplete, blobs[1].Incomplete)
	}
	if blobs[0].Identity.Email != "desk@x.com" || blobs[1].Identity.Email != "cli@x.com" {
		t.Fatalf("emails %s %s", blobs[0].Identity.Email, blobs[1].Identity.Email)
	}
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
	cli, _ := os.ReadFile(cliAuth(home))
	var m map[string]string
	json.Unmarshal(cli, &m)
	if m["accessToken"] != "d-tok" {
		t.Fatalf("aligned CLI token=%s", m["accessToken"])
	}

	ad.List = func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 5, Command: "/Applications/Cursor.app/Contents/MacOS/Cursor"}}, nil
	}
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{}); err == nil {
		t.Fatal("expected busy while Cursor.app running")
	}
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{CLIOnly: true}); err != nil {
		t.Fatal(err)
	}
}

func TestRefuseEmptyToken(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	p := payload{
		Desktop: map[string]string{"cursorAuth/accessToken": "x", "cursorAuth/refreshToken": "y"},
		CLIAuth: map[string]string{"accessToken": ""},
	}
	if err := writeCLI(home, p); err == nil {
		t.Fatal("expected empty token refusal")
	}
}
