package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/cursoradp"
	"qswitch/internal/adapter/devin"
	"qswitch/internal/adapter/kimi"
	"qswitch/internal/quota"
)

func TestKimiSwitchCurrentPreservesRotatedCredentials(t *testing.T) {
	a, _, now := parkedKimiAccount(t)
	writeKimiLive(t, a, "rotated-live", now.Add(time.Hour).Unix())
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return kimiResponse(req, 200, `{"user_id":"user-old"}`), nil
	})}
	if err := a.Switch(adapter.Kimi, "user-old", adapter.RestoreOpts{}, false); err != nil {
		t.Fatal(err)
	}
	live, err := kimi.ReadLiveCreds(a.UserHome)
	if err != nil {
		t.Fatal(err)
	}
	if live.AccessToken != "rotated-live" || live.RefreshToken != "refresh-rotated-live" {
		t.Fatal("switch to current account restored obsolete vault token over newer live token")
	}
	blob, err := a.loadBlob(adapter.Kimi, "user-old")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := kimi.CredsFromBlob(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKimiTokens(saved, live) {
		t.Fatal("live and captured credentials diverged")
	}
}

func TestKimiSwitchCaptureFailurePreservesLiveCredentials(t *testing.T) {
	a, _, now := parkedKimiAccount(t)
	expected := writeKimiLive(t, a, "rotated-live", now.Add(time.Hour).Unix())
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return kimiResponse(req, 503, `{}`), nil })}
	if err := a.Switch(adapter.Kimi, "user-old", adapter.RestoreOpts{}, false); err == nil {
		t.Fatal("switch must stop when existing live identity cannot be captured")
	}
	raw, err := os.ReadFile(a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(expected) {
		t.Fatal("failed capture overwrote live credentials")
	}
}

func TestKimiSwitchAllowsOnlyActuallyMissingLiveCredentials(t *testing.T) {
	for _, missing := range []bool{false, true} {
		a, _, _ := parkedKimiAccount(t)
		p := a.Adapters[adapter.Kimi].LivePaths(a.UserHome)[0]
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if !missing {
			if err := os.Mkdir(p, 0700); err != nil {
				t.Fatal(err)
			}
		}
		err := a.Switch(adapter.Kimi, "user-old", adapter.RestoreOpts{}, false)
		if missing {
			if err != nil {
				t.Fatal(err)
			}
			c, err := kimi.ReadLiveCreds(a.UserHome)
			if err != nil || c.AccessToken != "old" {
				t.Fatal("missing live credential recovery failed")
			}
		} else if err == nil {
			t.Fatal("non-ENOENT read error must refuse switching")
		}
	}
}

func TestKimiFreshParkedProbeIndependentFromLiveProfile(t *testing.T) {
	a, b, now := parkedKimiAccount(t)
	b, err := kimi.ApplyRefresh(b, quota.KimiTokens{AccessToken: "fresh-parked", RefreshToken: "fresh-refresh", ExpiresIn: 3600}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveBlob(b); err != nil {
		t.Fatal(err)
	}
	profileCalls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/coding/v1/usages" {
			if req.Header.Get("Authorization") != "Bearer fresh-parked" {
				t.Fatal("wrong account token")
			}
			return kimiResponse(req, 200, `{"usages":{"limit_5h":{"used_ratio":0.25}}}`), nil
		}
		profileCalls++
		return kimiResponse(req, 401, `{}`), nil
	})}
	r := a.ProbeAccount(context.Background(), adapter.Kimi, "user-old")
	if r.Class != quota.OK || r.UsedPct != 25 || profileCalls != 0 {
		t.Fatalf("fresh parked quota depends on unrelated live profile: class=%s calls=%d", r.Class, profileCalls)
	}
	ac, err := a.State.GetAccount("kimi", "user-old")
	if err != nil || ac.HTTPBackoffUntil != 0 {
		t.Fatal("healthy account incorrectly backed off")
	}
}

func TestCLIIdleRequiresReadableProcessAndActivityEvidence(t *testing.T) {
	for _, tool := range []adapter.Tool{adapter.Devin, adapter.Cursor} {
		for _, kind := range []string{"ps_error", "missing", "empty", "invalid_root", "recent", "old", "no_cli"} {
			t.Run(string(tool)+"/"+kind, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("XDG_DATA_HOME", "")
				name := "devin"
				root := filepath.Join(home, ".local", "share", "devin", "cli")
				if tool == adapter.Cursor {
					name = "cursor-agent"
					root = filepath.Join(home, ".cursor")
				}
				list := func() ([]adapter.Proc, error) {
					if kind == "ps_error" {
						return nil, errors.New("ps unavailable")
					}
					if kind == "no_cli" {
						return nil, nil
					}
					return []adapter.Proc{{PID: 42, Command: "/bin/" + name}}, nil
				}
				var ad adapter.Adapter = devin.Adapter{List: list}
				if tool == adapter.Cursor {
					ad = cursoradp.Adapter{List: list}
				}
				if kind == "empty" || kind == "recent" || kind == "old" {
					if err := os.MkdirAll(root, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "invalid_root" {
					if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(root, []byte("x"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "recent" || kind == "old" {
					p := filepath.Join(root, "session.log")
					if err := os.WriteFile(p, []byte("activity"), 0600); err != nil {
						t.Fatal(err)
					}
					if kind == "old" {
						old := time.Now().Add(-10 * time.Minute)
						if err := os.Chtimes(p, old, old); err != nil {
							t.Fatal(err)
						}
					}
				}
				want := kind == "old" || kind == "no_cli"
				if got := ad.Idle(home, time.Minute); got != want {
					t.Fatalf("Idle=%v want %v", got, want)
				}
			})
		}
	}
}
