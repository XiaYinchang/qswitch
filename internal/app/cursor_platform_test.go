package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/cursoradp"
	"qswitch/internal/clock"
	"qswitch/internal/livefile"
	"qswitch/internal/state"
)

func writeCursorCLIOnly(t *testing.T, home, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"auth.json":       map[string]string{"accessToken": "fixture-token-" + id, "refreshToken": "fixture-refresh-" + id},
		"cli-config.json": map[string]any{"authInfo": map[string]string{"authId": id, "email": id + "@example.test"}},
	} {
		raw, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(home, ".cursor", name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func cursorCLIApp(t *testing.T, platform string) *App {
	t.Helper()
	a, _ := setup(t)
	list := func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 42, Command: "/Applications/Cursor.app/Contents/MacOS/Cursor"}}, nil
	}
	a.ListProcs = list
	a.Adapters[adapter.Cursor] = cursoradp.Adapter{List: list, Platform: platform}
	a.Host.QuitFn = func(string) error { t.Fatal("CLI-only switch attempted to quit desktop"); return nil }
	a.Host.LaunchFn = func(string) error { t.Fatal("CLI-only switch attempted to launch desktop"); return nil }
	for _, id := range []string{"account-a", "account-b"} {
		writeCursorCLIOnly(t, a.UserHome, id)
		blobs, _, err := a.Capture(adapter.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(blobs) != 1 || blobs[0].Incomplete {
			t.Fatalf("valid CLI token was considered incomplete: %+v", blobs)
		}
	}
	return a
}

func TestCursorNonDesktopManualAndAutomaticSwitch(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		for _, automatic := range []bool{false, true} {
			name := platform + "/manual"
			if automatic {
				name = platform + "/automatic"
			}
			t.Run(name, func(t *testing.T) {
				a := cursorCLIApp(t, platform)
				now := time.Now().Truncate(time.Second)
				a.Clock = clock.Fixed{T: now}
				for _, entry := range []struct {
					id, class string
					pct       float64
				}{{"account-a", "ok", 12}, {"account-b", "exhausted", 100}} {
					if err := a.State.UpdateQuota("cursor", entry.id, entry.class, entry.pct, now.Add(time.Hour).Unix(), now.Unix(), now.Unix(), 0); err != nil {
						t.Fatal(err)
					}
					if err := a.State.LogQuota("cursor", entry.id, entry.class, "http", entry.pct, now); err != nil {
						t.Fatal(err)
					}
				}
				legacy, err := a.State.GetAccount("cursor", "account-a")
				if err != nil {
					t.Fatal(err)
				}
				legacy.Incomplete = true
				if err := a.State.UpsertAccount(legacy); err != nil {
					t.Fatal(err)
				}
				if automatic {
					a.considerSwitch(adapter.Cursor)
				} else if err := a.Switch(adapter.Cursor, "account-a", adapter.RestoreOpts{}, false); err != nil {
					t.Fatal(err)
				}
				p, err := a.State.GetPointer("cursor")
				if err != nil || p.StableID != "account-a" || p.Desync {
					t.Fatalf("switch did not select CLI account: %+v %v", p, err)
				}
				live, err := os.ReadFile(filepath.Join(a.UserHome, ".cursor", "auth.json"))
				if err != nil || !bytes.Contains(live, []byte("fixture-token-account-a")) {
					t.Fatal("CLI credential was not switched")
				}
				if _, err := os.Stat(filepath.Join(a.UserHome, "Library")); !os.IsNotExist(err) {
					t.Fatal("switch created macOS desktop storage")
				}
				if a.Capabilities(adapter.Cursor)["desktop_sync"].Available {
					t.Fatal("reported unsupported desktop sync")
				}
			})
		}
	}
}

func TestCursorNonDesktopFingerprintIgnoresForeignDatabase(t *testing.T) {
	a := cursorCLIApp(t, "linux")
	path := filepath.Join(a.UserHome, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	poison := []byte("this is not a sqlite database")
	if err := os.WriteFile(path, poison, 0600); err != nil {
		t.Fatal(err)
	}
	want, err := cursoradp.HashCLI(a.UserHome)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.liveFingerprint(adapter.Cursor)
	if err != nil || got != want {
		t.Fatalf("app included desktop fingerprint: %v", err)
	}
	if _, _, err := a.Capture(adapter.Cursor); err != nil {
		t.Fatal(err)
	}
	if err := a.Switch(adapter.Cursor, "account-a", adapter.RestoreOpts{}, false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, poison) {
		t.Fatal("foreign desktop database was changed")
	}
	if err := a.State.SetPointer(state.Pointer{Tool: "cursor", StableID: "account-a", Desync: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAuto(adapter.Cursor, true); err != nil {
		t.Fatalf("legacy desktop desync blocked CLI management: %v", err)
	}
}

func TestCursorLegacyIncompleteRecheckPreservesOtherState(t *testing.T) {
	a := cursorCLIApp(t, "linux")
	ac, err := a.State.GetAccount("cursor", "account-a")
	if err != nil {
		t.Fatal(err)
	}
	ac.Incomplete, ac.StaleCLI = true, true
	if err := a.State.UpsertAccount(ac); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpdateQuota("cursor", ac.StableID, "expired", 99.5, 7000, 6000, 6000, 9000); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetRefreshBackoff("cursor", ac.StableID, 9500); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetCooling("cursor", ac.StableID, 8000); err != nil {
		t.Fatal(err)
	}
	before, _ := a.State.GetAccount("cursor", ac.StableID)
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "cursor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.recheckCursorCLILocked(before)
	_ = lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := a.State.GetAccount("cursor", ac.StableID)
	before.Incomplete = false
	if after != before {
		t.Fatalf("credential metadata recheck changed auth/quota/history: before=%+v after=%+v", before, after)
	}
}

func TestCursorLegacyDesktopOnlyRemainsIncomplete(t *testing.T) {
	a := cursorCLIApp(t, "linux")
	ac, err := a.State.GetAccount("cursor", "account-a")
	if err != nil {
		t.Fatal(err)
	}
	ac.Incomplete = true
	if err := a.State.UpsertAccount(ac); err != nil {
		t.Fatal(err)
	}
	identity := adapter.Identity{Tool: adapter.Cursor, StableID: ac.StableID}
	raw, _ := json.Marshal(map[string]any{"kind": "cursor.auth.v1", "identity": identity, "desktop": map[string]string{"cursorAuth/accessToken": "desktop-only", "cursorAuth/stripeMembershipAuthId": ac.StableID}})
	if err := a.Vault.Put("cursor", ac.StableID, raw); err != nil {
		t.Fatal(err)
	}
	if err := a.Switch(adapter.Cursor, ac.StableID, adapter.RestoreOpts{}, false); err == nil {
		t.Fatal("desktop token was promoted to CLI")
	}
	after, _ := a.State.GetAccount("cursor", ac.StableID)
	if !after.Incomplete {
		t.Fatal("invalid CLI snapshot was marked complete")
	}
	p, _ := a.State.GetPointer("cursor")
	if p.StableID != "account-b" {
		t.Fatal("failed validation changed live pointer")
	}
}

func TestCursorNonDesktopProbeUsesOnlyValidatedCLIToken(t *testing.T) {
	a := cursorCLIApp(t, "linux")
	ac, err := a.State.GetAccount("cursor", "account-a")
	if err != nil {
		t.Fatal(err)
	}
	ac.Incomplete = true
	if err := a.State.UpsertAccount(ac); err != nil {
		t.Fatal(err)
	}
	raw, err := a.Vault.Get("cursor", ac.StableID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["desktop"] = map[string]string{"cursorAuth/accessToken": "unrelated-desktop-token"}
	raw, _ = json.Marshal(payload)
	if err := a.Vault.Put("cursor", ac.StableID, raw); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") != "Bearer fixture-token-account-a" {
			t.Fatal("probe used desktop token instead of validated CLI token")
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
	})}
	a.ProbeAccount(context.Background(), adapter.Cursor, ac.StableID)
	after, _ := a.State.GetAccount("cursor", ac.StableID)
	if calls != 1 || after.Incomplete {
		t.Fatalf("explicit probe did not validate old CLI snapshot: calls=%d incomplete=%v", calls, after.Incomplete)
	}
}
