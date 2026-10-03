package kimi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
)

const testCredentials = "{\n\"access_token\":\"test-access\",\"refresh_token\":\"test-refresh\",\"expires_at\":4102444800,\"expires_in\":900,\"token_type\":\"Bearer\",\"scope\":\"code\",\"future_field\":\"keep\"}\n"

func fixture(t *testing.T) (string, Adapter) {
	t.Helper()
	t.Setenv("KIMI_CODE_HOME", "")
	home := t.TempDir()
	p := filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(testCredentials), 0600); err != nil {
		t.Fatal(err)
	}
	return home, Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
}

func TestCaptureRestoreCredentialsOnly(t *testing.T) {
	home, a := fixture(t)
	config := filepath.Join(home, ".kimi-code", "config.toml")
	if err := os.WriteFile(config, []byte("hooks = []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	blobs, _, err := a.Capture(home)
	if err != nil || len(blobs) != 1 {
		t.Fatalf("capture %v", err)
	}
	if blobs[0].Identity.StableID != "" {
		t.Fatal("opaque tokens have no verified stable identity")
	}
	p := a.LivePaths(home)[0]
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(home, blobs[0], adapter.RestoreOpts{CLIOnly: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	cfg, _ := os.ReadFile(config)
	if string(b) != testCredentials || string(cfg) != "hooks = []\n" {
		t.Fatal("restore modified unrelated config or raw credentials")
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
}

func TestRestoreBlockedAndInvalidPreservesCredentials(t *testing.T) {
	for _, name := range []string{"busy", "ps_error", "invalid"} {
		t.Run(name, func(t *testing.T) {
			home, a := fixture(t)
			blobs, _, err := a.Capture(home)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "busy":
				a.List = func() ([]adapter.Proc, error) { return []adapter.Proc{{PID: 42, Command: "/somewhere/kimi acp"}}, nil }
			case "ps_error":
				a.List = func() ([]adapter.Proc, error) { return nil, errors.New("ps failed") }
			case "invalid":
				blobs[0].Payload = []byte(`{"kind":"other","credentials_json":"{}"}`)
			}
			if err := a.Restore(home, blobs[0], adapter.RestoreOpts{}); err == nil {
				t.Fatal("expected refusal")
			}
			b, _ := os.ReadFile(a.LivePaths(home)[0])
			if string(b) != testCredentials {
				t.Fatal("changed credentials")
			}
			if name != "invalid" && a.Idle(home, time.Minute) {
				t.Fatal("busy/query failure treated idle")
			}
		})
	}
}

func TestKimiProcessMatching(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"/Users/test/.kimi-code/bin/kimi --acp", true},
		{"kimi", true},
		{"/usr/bin/python3 /Users/test/.local/bin/kimi", true},
		{"python3 -m kimi_cli", true},
		{"node /opt/lib/node_modules/@moonshot-ai/kimi-code/dist/main.mjs", true},
		{"echo kimi", false},
		{"node /work/my-kimi-app/main.mjs", false},
		{"python3 other.py kimi", false},
		{"/usr/bin/rg kimi", false},
	} {
		a := Adapter{List: func() ([]adapter.Proc, error) { return []adapter.Proc{{PID: 12, Command: tc.cmd}}, nil }}
		h, err := a.ManualBlockers("")
		if err != nil || h.ManualBusy() != tc.want {
			t.Fatalf("%s: %+v %v", tc.cmd, h, err)
		}
	}
}

func TestApplyRefreshPreservesIdentityAndUnknownFields(t *testing.T) {
	home, a := fixture(t)
	bs, _, err := a.Capture(home)
	if err != nil {
		t.Fatal(err)
	}
	id := EnrichIdentity(bs[0].Identity, []byte(`{"user_id":"user-1","nickname":"test","user_level_name":"Allegretto"}`))
	blob, err := WriteIdentity(bs[0], id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1900000000, 0)
	updated, err := ApplyRefresh(blob, quota.KimiTokens{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 900}, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := CredsFromBlob(updated)
	if err != nil || c.AccessToken != "new-access" || c.RefreshToken != "new-refresh" || c.ExpiresAt != now.Unix()+900 || c.Scope != "code" || updated.Identity.StableID != "user-1" || !strings.Contains(string(updated.Payload), "future_field") {
		t.Fatalf("refresh invariant failed %v", err)
	}
	if c.NeedsRefresh(now) || !c.NeedsRefresh(now.Add(850*time.Second)) {
		t.Fatal("refresh window")
	}
	if _, err := ApplyRefresh(blob, quota.KimiTokens{AccessToken: "bad", ExpiresIn: 900}, now); err == nil {
		t.Fatal("rotation must include refresh token")
	}
	b, _ := os.ReadFile(a.LivePaths(home)[0])
	if string(b) != testCredentials {
		t.Fatal("ApplyRefresh touched live credentials")
	}
}

func TestKimiRestoreRefusesOfficialRefreshLockAndCustomSlot(t *testing.T) {
	for _, kind := range []string{"lock", "custom_slot", "write_failure"} {
		t.Run(kind, func(t *testing.T) {
			home, a := fixture(t)
			bs, _, err := a.Capture(home)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "lock":
				if err := os.MkdirAll(filepath.Join(home, ".kimi-code", "oauth", "kimi-code.lock"), 0700); err != nil {
					t.Fatal(err)
				}
			case "custom_slot":
				cfg := `[providers."managed:kimi-code".oauth]
storage = "file"
key = "oauth/kimi-code-env-other"
`
				if err := os.WriteFile(filepath.Join(home, ".kimi-code", "config.toml"), []byte(cfg), 0600); err != nil {
					t.Fatal(err)
				}
			case "write_failure":
				if err := os.Remove(a.LivePaths(home)[0]); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(a.LivePaths(home)[0], 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Restore(home, bs[0], adapter.RestoreOpts{}); err == nil {
				t.Fatal("expected restore refusal")
			}
			if kind != "write_failure" {
				b, _ := os.ReadFile(a.LivePaths(home)[0])
				if string(b) != testCredentials {
					t.Fatal("modified credentials")
				}
			}
		})
	}
}

func TestKimiHomeOverride(t *testing.T) {
	home, a := fixture(t)
	other := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", other)
	if got := a.LivePaths(home)[0]; got != filepath.Join(other, "credentials", "kimi-code.json") {
		t.Fatalf("wrong override path")
	}
	if _, err := ReadLiveCreds(home); !os.IsNotExist(err) {
		t.Fatalf("must not read fallback home %v", err)
	}
}
