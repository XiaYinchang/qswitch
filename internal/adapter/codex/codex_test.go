package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
)

func TestCaptureRestoreBusy(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	auth := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"account_id":   "acc-1",
			"access_token": "tok",
			"id_token":     dummyJWT("a@b.c"),
		},
	}
	b, _ := json.Marshal(auth)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), b, 0o600)

	ad := Adapter{List: func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 99, Command: "/opt/homebrew/bin/codex"}}, nil
	}}
	blobs, _, err := ad.Capture(home)
	if err != nil || len(blobs) != 1 || blobs[0].Identity.StableID != "acc-1" {
		t.Fatalf("capture: %#v %v", blobs, err)
	}
	err = ad.Restore(home, blobs[0], adapter.RestoreOpts{})
	if _, ok := err.(*adapter.BusyError); !ok {
		t.Fatalf("want BusyError, got %v", err)
	}
	orig, _ := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))

	ad.List = func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 1, Command: "/Applications/ChatGPT.app/Contents/MacOS/ChatGPT"}}, nil
	}
	other := blobs[0]
	auth["tokens"].(map[string]any)["account_id"] = "acc-2"
	b2, _ := json.Marshal(map[string]any{"kind": "codex.auth.json.v1", "identity": adapter.Identity{StableID: "acc-2"}, "auth_json": json.RawMessage(mustJSON(auth))})
	other.Payload = b2
	other.Identity.StableID = "acc-2"
	if err := ad.Restore(home, other, adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if string(got) == string(orig) {
		t.Fatal("expected file changed while only ChatGPT.app running")
	}
}

func TestCaptureDesktop(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	auth := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"account_id": "acc-1", "access_token": "tok", "id_token": dummyJWT("a@b.c")}}
	b, _ := json.Marshal(auth)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), b, 0o600)
	root := filepath.Join(home, "Library/Application Support/Codex/Default")
	os.MkdirAll(root, 0o700)
	os.WriteFile(filepath.Join(root, "Cookies"), []byte("ck"), 0o600)
	ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	blobs, _, err := ad.Capture(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 || blobs[0].Identity.StableID != "acc-1" {
		t.Fatalf("want one cli blob with desktop zip, got %#v", blobs)
	}
	var env struct {
		DesktopZip []byte `json:"desktop_zip"`
	}
	if json.Unmarshal(blobs[0].Payload, &env) != nil || len(env.DesktopZip) == 0 {
		t.Fatal("missing desktop_zip on cli blob")
	}
	os.WriteFile(filepath.Join(root, "Cookies"), []byte("old"), 0o600)
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "Cookies"))
	if string(got) != "ck" {
		t.Fatalf("cookies %q", got)
	}
}

func TestBlobFromTokens(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	tok := quota.CodexTokens{
		AccessToken:  jwtExp("a@x.com", "u1", "ws-1", "plus", time.Now().Add(time.Hour).Unix()),
		IDToken:      jwtExp("a@x.com", "u1", "ws-1", "plus", time.Now().Add(time.Hour).Unix()),
		RefreshToken: "rt",
	}
	blob, err := BlobFromTokens(tok, time.Now())
	if err != nil || blob.Identity.StableID != "ws-1" || blob.Identity.Email != "a@x.com" {
		t.Fatalf("%+v %v", blob.Identity, err)
	}
	got, err := ReadAuth(blob)
	if err != nil || got.Refresh != "rt" || got.AccountID != "ws-1" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestNeedsRefreshAndApply(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	past := jwtExp("a@x.com", "u1", "ws", "plus", 1)
	auth := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"account_id": "ws", "access_token": past, "id_token": past, "refresh_token": "rt",
	}, "last_refresh": "2020-01-01T00:00:00Z"}
	b, _ := json.Marshal(map[string]any{"kind": "codex.auth.json.v1", "auth_json": auth})
	blob := adapter.Blob{Payload: b}
	af, err := ReadAuth(blob)
	if err != nil || !af.NeedsRefresh(time.Now()) {
		t.Fatalf("need refresh %+v %v", af, err)
	}
	out, err := ApplyRefresh(blob, quota.CodexTokens{AccessToken: "new-a", RefreshToken: "new-r"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadAuth(out)
	if err != nil || got.Access != "new-a" || got.Refresh != "new-r" {
		t.Fatalf("%+v %v", got, err)
	}
}

func jwtExp(email, user, account, plan string, exp int64) string {
	h := "eyJhbGciOiJub25lIn0"
	p := b64(`{"email":"` + email + `","exp":` + strconv.FormatInt(exp, 10) + `,"https://api.openai.com/auth":{"chatgpt_account_id":"` + account + `","chatgpt_plan_type":"` + plan + `","chatgpt_user_id":"` + user + `"}}`)
	return h + "." + p + ".x"
}

func TestWorkspaceIdentityNotEmail(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	writeAuth := func(account, email, plan string) {
		t.Helper()
		auth := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
			"account_id": account, "access_token": "t", "id_token": jwtPlan(email, account, plan),
		}}
		b, _ := json.Marshal(auth)
		if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAuth("ws-personal", "a@x.com", "plus")
	a, _, err := ad.Capture(home)
	if err != nil || len(a) != 1 {
		t.Fatalf("%v %#v", err, a)
	}
	writeAuth("ws-team-1", "a@x.com", "team")
	b, _, err := ad.Capture(home)
	if err != nil || len(b) != 1 {
		t.Fatalf("%v %#v", err, b)
	}
	if a[0].Identity.StableID == b[0].Identity.StableID {
		t.Fatal("personal and team workspaces must be distinct stable ids")
	}
	if a[0].Identity.Email != "a@x.com" || b[0].Identity.Email != "a@x.com" {
		t.Fatalf("email %q %q", a[0].Identity.Email, b[0].Identity.Email)
	}
	if a[0].Identity.PlanHint != "plus" || b[0].Identity.PlanHint != "team" {
		t.Fatalf("plan %q %q", a[0].Identity.PlanHint, b[0].Identity.PlanHint)
	}
}

func jwtPlan(email, account, plan string) string {
	h := "eyJhbGciOiJub25lIn0"
	p := b64(`{"email":"` + email + `","https://api.openai.com/auth":{"chatgpt_account_id":"` + account + `","chatgpt_plan_type":"` + plan + `"}}`)
	return h + "." + p + ".x"
}

func TestFingerprintTracksCookies(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"account_id":"a"}}`), 0o600)
	ad := Adapter{}
	a, err := ad.Fingerprint(home)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "Library/Application Support/Codex/Default")
	os.MkdirAll(root, 0o700)
	os.WriteFile(filepath.Join(root, "Cookies"), []byte("session-a"), 0o600)
	b, err := ad.Fingerprint(home)
	if err != nil || a == b {
		t.Fatalf("cookie change should change fingerprint a=%x b=%x err=%v", a, b, err)
	}
}

func dummyJWT(email string) string {
	// header.payload.sig — payload is {"email":"..."}
	h := "eyJhbGciOiJub25lIn0"
	p := b64(`{"email":"` + email + `"}`)
	return h + "." + p + ".x"
}

func b64(s string) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	raw := []byte(s)
	n := (len(raw) + 2) / 3 * 4
	out := make([]byte, 0, n)
	for i := 0; i < len(raw); i += 3 {
		var n uint32
		remain := len(raw) - i
		switch {
		case remain >= 3:
			n = uint32(raw[i])<<16 | uint32(raw[i+1])<<8 | uint32(raw[i+2])
			out = append(out, tbl[n>>18], tbl[(n>>12)&63], tbl[(n>>6)&63], tbl[n&63])
		case remain == 2:
			n = uint32(raw[i])<<16 | uint32(raw[i+1])<<8
			out = append(out, tbl[n>>18], tbl[(n>>12)&63], tbl[(n>>6)&63])
		case remain == 1:
			n = uint32(raw[i]) << 16
			out = append(out, tbl[n>>18], tbl[(n>>12)&63])
		}
	}
	return string(out)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
