package grok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
)

func TestPIDFileNotFlockAndBusy(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".grok"), 0o700)
	auth := map[string]any{
		"https://auth.x.ai::abc": map[string]any{
			"principal_id": "pid-1",
			"email":        "g@x.com",
			"key":          "k",
		},
	}
	b, _ := json.Marshal(auth)
	os.WriteFile(filepath.Join(home, ".grok", "auth.json"), b, 0o600)
	os.WriteFile(filepath.Join(home, ".grok", "auth.json.lock"), []byte("999999:1"), 0o644) // dead pid
	os.WriteFile(filepath.Join(home, ".grok", "active_sessions.json"), []byte(`[{"session_id":"s","pid":1,"cwd":"/tmp"}]`), 0o644)

	ad := Adapter{List: func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 42, Command: "/Users/x/.local/bin/grok --session-id s"}}, nil
	}}
	h, err := ad.ManualBlockers(home)
	if err != nil {
		t.Fatal(err)
	}
	if !h.ManualBusy() {
		t.Fatal("expected grok cli busy")
	}
	blobs, _, err := ad.Capture(home)
	if err != nil || blobs[0].Identity.Email != "g@x.com" {
		t.Fatalf("%v %#v", err, blobs)
	}
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{}); err == nil {
		t.Fatal("expected busy")
	}
	ad.List = func() ([]adapter.Proc, error) { return nil, nil }
	// lock pid 999999 is dead; sessions pid 1 may be alive (kernel pid 1) — treat as maybe busy
	// write empty sessions so only dead pidfile remains
	os.WriteFile(filepath.Join(home, ".grok", "active_sessions.json"), []byte(`[]`), 0o644)
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureAttachesGrokBotDesktop(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".grok"), 0o700)
	auth := map[string]any{"https://auth.x.ai::abc": map[string]any{"principal_id": "pid-1", "email": "g@x.com", "key": "k"}}
	b, _ := json.Marshal(auth)
	os.WriteFile(filepath.Join(home, ".grok", "auth.json"), b, 0o600)
	root := filepath.Join(home, "Library", "Application Support", "Grok Bot")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "Cookies"), []byte("ck-1"), 0o600)
	ad := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	blobs, _, err := ad.Capture(home)
	if err != nil || len(blobs) == 0 {
		t.Fatalf("%v %#v", err, blobs)
	}
	var env struct {
		DesktopZip []byte `json:"desktop_zip"`
	}
	if json.Unmarshal(blobs[0].Payload, &env) != nil || len(env.DesktopZip) == 0 {
		t.Fatal("expected desktop_zip on grok cli blob")
	}
	os.WriteFile(filepath.Join(root, "Cookies"), []byte("other"), 0o600)
	if err := ad.Restore(home, blobs[0], adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "Cookies"))
	if string(got) != "ck-1" {
		t.Fatalf("cookies %q", got)
	}
}

func TestNeedsRefreshAndApply(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	slot := "https://auth.x.ai::abc"
	auth := map[string]any{
		slot: map[string]any{
			"principal_id":   "pid-1",
			"user_id":        "pid-1",
			"email":          "g@x.com",
			"key":            "old-key",
			"refresh_token":  "rt",
			"expires_at":     "2020-01-01T00:00:00Z",
			"oidc_issuer":    "https://auth.x.ai",
			"oidc_client_id": "abc",
		},
	}
	raw, _ := json.Marshal(auth)
	b, _ := json.Marshal(map[string]any{"kind": "grok.auth.json.v1", "auth_json": json.RawMessage(raw)})
	blob := adapter.Blob{Payload: b, Identity: adapter.Identity{StableID: "pid-1"}}
	af, err := ReadAuth(blob)
	if err != nil || !af.NeedsRefresh(time.Now()) || af.ClientID != "abc" {
		t.Fatalf("need refresh %+v %v", af, err)
	}
	out, err := ApplyRefresh(blob, quota.GrokTokens{AccessToken: "new-k", RefreshToken: "rt2", ExpiresIn: 3600}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadAuth(out)
	if err != nil || got.Key != "new-k" || got.Refresh != "rt2" || got.NeedsRefresh(time.Now()) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestBlobFromTokens(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	h := "eyJhbGciOiJub25lIn0"
	payload := []byte(`{"principal_id":"pid-9","sub":"pid-9","email":"n@x.com","client_id":"abc"}`)
	tok := quota.GrokTokens{AccessToken: h + "." + b64u(payload) + ".x", RefreshToken: "rt", ExpiresIn: 3600}
	blob, err := BlobFromTokens(tok, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	af, err := ReadAuth(blob)
	if err != nil || af.PrincipalID != "pid-9" || af.Email != "n@x.com" || af.Refresh != "rt" || af.Key == "" {
		t.Fatalf("%+v %v", af, err)
	}
	if blob.Identity.StableID != "pid-9" {
		t.Fatalf("id %+v", blob.Identity)
	}
}

func b64u(raw []byte) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, (len(raw)+2)/3*4)
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
		default:
			n = uint32(raw[i]) << 16
			out = append(out, tbl[n>>18], tbl[(n>>12)&63])
		}
	}
	return string(out)
}

func TestMergeLiveTokensSameUser(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	slot := "https://auth.x.ai::abc"
	stored := map[string]any{
		slot: map[string]any{
			"principal_id": "pid-1", "user_id": "pid-1", "key": "old", "refresh_token": "rt-old",
			"expires_at": "2020-01-01T00:00:00Z", "oidc_issuer": "https://auth.x.ai", "oidc_client_id": "abc",
		},
	}
	raw, _ := json.Marshal(stored)
	b, _ := json.Marshal(map[string]any{"kind": "grok.auth.json.v1", "auth_json": json.RawMessage(raw)})
	blob := adapter.Blob{Payload: b, Identity: adapter.Identity{StableID: "pid-1"}}
	live, _ := json.Marshal(map[string]any{
		slot: map[string]any{
			"principal_id": "pid-1", "user_id": "pid-1", "key": "live-k", "refresh_token": "rt-live",
			"expires_at":  time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339Nano),
			"oidc_issuer": "https://auth.x.ai", "oidc_client_id": "abc",
		},
	})
	out := MergeLiveTokens(blob, live)
	got, err := ReadAuth(out)
	if err != nil || got.Key != "live-k" || got.Refresh != "rt-live" {
		t.Fatalf("%+v %v", got, err)
	}
}
