package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"qswitch/internal/adapter"
	"qswitch/internal/adapter/grok"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

func liveGrokRefreshFixture(t *testing.T) (*App, adapter.Blob, time.Time) {
	t.Helper()
	a, b, now := parkedRefreshAccount(t, adapter.Grok)
	if err := a.Adapters[adapter.Grok].Restore(a.UserHome, b, adapter.RestoreOpts{CLIOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetPointer(state.Pointer{Tool: "grok", StableID: b.Identity.StableID}); err != nil {
		t.Fatal(err)
	}
	return a, b, now
}
func grokRefreshResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}
}
func TestLiveGrokRefreshOnlyWhenOfficialCLIAbsent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		procs   []adapter.Proc
		listErr error
		want    int
	}{
		{name: "no client", want: 1},
		{name: "desktop only", procs: []adapter.Proc{{PID: 40, Command: "/Applications/Grok Bot.app/Contents/MacOS/Grok Bot"}}, want: 1},
		{name: "CLI running", procs: []adapter.Proc{{PID: 41, Command: "/bin/grok --acp"}}},
		{name: "process query failed", listErr: errors.New("ps unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, _ := liveGrokRefreshFixture(t)
			a.Adapters[adapter.Grok] = grok.Adapter{List: func() ([]adapter.Proc, error) { return tc.procs, tc.listErr }}
			old, _ := os.ReadFile(filepath.Join(a.UserHome, ".grok/auth.json"))
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "auth.x.ai" {
					t.Fatal("unexpected upstream")
				}
				f, err := os.OpenFile(filepath.Join(a.UserHome, ".grok/auth.json.lock"), os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
					_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
					t.Fatal("exchange did not hold official flock")
				}

				if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "refresh-old" {
					t.Fatal("not using current live refresh token")
				}
				return grokRefreshResponse(r, 200, `{"access_token":"live-new","refresh_token":"live-refresh-new","expires_in":21600}`), nil
			})}
			_, tok, err := a.keepAliveGrok(context.Background(), b, false)
			if calls != tc.want {
				t.Fatalf("calls=%d want=%d err=%v", calls, tc.want, err)
			}
			raw, _ := os.ReadFile(filepath.Join(a.UserHome, ".grok/auth.json"))
			if tc.want == 1 {
				if err != nil || tok != "live-new" {
					t.Fatalf("refresh failed %v", err)
				}
				live, _ := grok.ReadAuthBytes(raw)
				saved, _ := a.loadBlob(adapter.Grok, b.Identity.StableID)
				stored, _ := grok.ReadAuth(saved)
				if live.Key != "live-new" || live.Refresh != "live-refresh-new" || stored.Key != live.Key || stored.Refresh != live.Refresh {
					t.Fatal("rotated credentials not saved in live and vault")
				}
				p, _ := a.State.GetPointer("grok")
				if p.StableID != b.Identity.StableID {
					t.Fatal("refresh switched identity")
				}
				lockRaw, lockErr := os.ReadFile(filepath.Join(a.UserHome, ".grok/auth.json.lock"))
				if lockErr != nil || len(lockRaw) != 0 {
					t.Fatal("lock cleanup deleted inode or left a live daemon PID")
				}
				st, _ := os.Stat(filepath.Join(a.UserHome, ".grok/auth.json"))
				if st.Mode().Perm() != 0600 {
					t.Fatal("credential file permissions")
				}
			} else if !bytes.Equal(old, raw) {
				t.Fatal("busy/unknown client changed live auth")
			}
		})
	}
}
func TestLiveGrokKeepAliveAndConcurrentRefresh(t *testing.T) {
	a, b, _ := liveGrokRefreshFixture(t)
	var calls atomic.Int32
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return grokRefreshResponse(r, 200, `{"access_token":"live-new","refresh_token":"live-refresh-new","expires_in":21600}`), nil
	})}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, _, err := a.keepAliveGrok(context.Background(), b, true); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a.KeepAlive(context.Background(), adapter.Grok)
	if calls.Load() != 1 {
		t.Fatalf("one-time refresh reused %d times", calls.Load())
	}
	// Proactive keepalive must also cover a current account before any quota call.
	a2, _, _ := liveGrokRefreshFixture(t)
	hits := 0
	a2.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		return grokRefreshResponse(r, 200, `{"access_token":"proactive-new","refresh_token":"proactive-refresh","expires_in":21600}`), nil
	})}
	a2.KeepAlive(context.Background(), adapter.Grok)
	if hits != 1 {
		t.Fatal("current account skipped by proactive keepalive")
	}
}
func TestLiveGrokRefreshUsesLatestDiskAndPreservesOtherSlots(t *testing.T) {
	a, b, _ := liveGrokRefreshFixture(t)
	raw, _ := os.ReadFile(filepath.Join(a.UserHome, ".grok/auth.json"))
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	for _, v := range root {
		m := v.(map[string]any)
		m["refresh_token"] = "newer-disk-refresh"
		m["preserved_setting"] = "yes"
	}
	root["unrelated"] = map[string]any{"key": "unrelated-key", "principal_id": "different-user"}
	raw, _ = json.Marshal(root)
	_ = os.WriteFile(filepath.Join(a.UserHome, ".grok/auth.json"), raw, 0600)
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "newer-disk-refresh" {
			t.Fatal("spent stale vault refresh")
		}
		return grokRefreshResponse(r, 200, `{"access_token":"live-new","refresh_token":"live-refresh-new","expires_in":21600}`), nil
	})}
	if _, _, err := a.keepAliveGrok(context.Background(), b, false); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(a.UserHome, ".grok/auth.json"))
	_ = json.Unmarshal(raw, &root)
	if root["unrelated"].(map[string]any)["key"] != "unrelated-key" {
		t.Fatal("lost unrelated slot")
	}
	for k, v := range root {
		if k != "unrelated" && v.(map[string]any)["preserved_setting"] != "yes" {
			t.Fatal("lost current slot fields")
		}
	}
	saved, err := a.loadBlob(adapter.Grok, b.Identity.StableID)
	if err != nil || saved.Identity != b.Identity {
		t.Fatalf("renewal changed vault identity: %v", err)
	}
	stored, err := grok.ReadAuth(saved)
	if err != nil || stored.Key != "live-new" || stored.Refresh != "live-refresh-new" {
		t.Fatalf("renewed tokens missing from current account vault: %v", err)
	}
}
func TestLiveGrokRefreshRequiresSharedLockAndUnchangedFile(t *testing.T) {
	for _, kind := range []string{"held lock", "live holder PID", "file replaced during refresh", "lock replaced during refresh"} {
		t.Run(kind, func(t *testing.T) {
			a, b, _ := liveGrokRefreshFixture(t)
			path := filepath.Join(a.UserHome, ".grok/auth.json")
			lockpath := path + ".lock"
			old, _ := os.ReadFile(path)
			var f *os.File
			if kind == "held lock" {
				var err error
				f, err = os.OpenFile(lockpath, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
			}
			if kind == "live holder PID" {
				if err := os.WriteFile(lockpath, []byte(fmt.Sprintf("%d:1", os.Getpid())), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			replacement := []byte(`{"changed":"do not overwrite"}`)
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if kind == "file replaced during refresh" {
					_ = os.WriteFile(path, replacement, 0600)
				}
				if kind == "lock replaced during refresh" {
					_ = os.Remove(lockpath)
					_ = os.WriteFile(lockpath, []byte("replacement"), 0600)
				}
				return grokRefreshResponse(r, 200, `{"access_token":"live-new","refresh_token":"live-refresh-new","expires_in":21600}`), nil
			})}
			_, _, err := a.keepAliveGrok(context.Background(), b, false)
			wantCalls := 0
			if strings.Contains(kind, "during refresh") {
				wantCalls = 1
				if err == nil {
					t.Fatal("race must fail closed")
				}
			}
			if calls != wantCalls {
				t.Fatalf("requests=%d want=%d", calls, wantCalls)
			}
			raw, _ := os.ReadFile(path)
			want := old
			if kind == "file replaced during refresh" {
				want = replacement
			}
			if !bytes.Equal(raw, want) {
				t.Fatal("overwrote concurrently changed credential file")
			}
			if kind == "lock replaced during refresh" {
				raw, _ := os.ReadFile(lockpath)
				if string(raw) != "replacement" {
					t.Fatal("cleanup altered replacement lock")
				}
			}
		})
	}
}
func TestLiveGrokQuotaProbeRecoversExpiredAuthentication(t *testing.T) {
	a, b, _ := liveGrokRefreshFixture(t)
	refreshes := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "auth.x.ai" {
			refreshes++
			return grokRefreshResponse(r, 200, `{"access_token":"live-new","refresh_token":"live-refresh-new","expires_in":21600}`), nil
		}
		if r.URL.Path == "/v1/settings" {
			return grokRefreshResponse(r, 200, `{}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer live-new" {
			return grokRefreshResponse(r, 401, `{}`), nil
		}
		return grokRefreshResponse(r, 200, `{"config":{"creditUsagePercent":1}}`), nil
	})}
	res := a.ProbeAccount(context.Background(), adapter.Grok, b.Identity.StableID)
	if refreshes != 1 || res.Class != quota.OK || res.UsedPct != 1 {
		t.Fatalf("refreshes=%d class=%s", refreshes, res.Class)
	}
}

func TestLiveGrokRefreshFailureAndBackoffPreserveCredentials(t *testing.T) {
	for _, status := range []int{500, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a, b, now := liveGrokRefreshFixture(t)
			path := filepath.Join(a.UserHome, ".grok/auth.json")
			old, _ := os.ReadFile(path)
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"error":"temporary_failure"}`
				if status == 400 {
					body = `{"error":"invalid_grant"}`
				}
				return grokRefreshResponse(r, status, body), nil
			})}
			a.KeepAlive(context.Background(), adapter.Grok)
			a.KeepAlive(context.Background(), adapter.Grok)
			raw, _ := os.ReadFile(path)
			if calls != 1 || !bytes.Equal(raw, old) {
				t.Fatal("failed live renewal changed credentials or ignored backoff")
			}
			ac, _ := a.State.GetAccount("grok", b.Identity.StableID)
			want := "ok"
			if status == 400 {
				want = "expired"
			}
			if ac.LastQuotaClass != want || ac.LastUsedPct != 21 || ac.RefreshBackoffUntil != now.Add(a.refreshRetryDelay(status == 400)).Unix() {
				t.Fatal("incorrect failure state")
			}
		})
	}
}
func TestLiveGrokRefreshRequiresReadableIdentity(t *testing.T) {
	for _, kind := range []string{"malformed", "directory"} {
		t.Run(kind, func(t *testing.T) {
			a, b, _ := liveGrokRefreshFixture(t)
			path := filepath.Join(a.UserHome, ".grok/auth.json")
			if kind == "malformed" {
				_ = os.WriteFile(path, []byte("{"), 0600)
			} else {
				_ = os.Remove(path)
				_ = os.Mkdir(path, 0700)
			}
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not reach HTTP") })}
			if _, _, err := a.keepAliveGrok(context.Background(), b, true); err == nil || calls != 0 {
				t.Fatal("ambiguous live credentials must block refresh")
			}
		})
	}
}

func TestLiveGrokRefreshRechecksIdentityUnderOfficialLock(t *testing.T) {
	a, b, _ := liveGrokRefreshFixture(t)
	changed := false
	a.Adapters[adapter.Grok] = grok.Adapter{List: func() ([]adapter.Proc, error) {
		if !changed {
			changed = true
			writeGrokLive(t, a.UserHome, "other-user", "other@example.test")
		}
		return nil, nil
	}}
	calls := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not reach HTTP") })}
	if _, _, err := a.keepAliveGrok(context.Background(), b, false); err == nil || calls != 0 {
		t.Fatal("live identity race consumed a refresh token")
	}
	raw, _ := os.ReadFile(filepath.Join(a.UserHome, ".grok/auth.json"))
	live, _ := grok.ReadAuthBytes(raw)
	if live.PrincipalID != "other-user" {
		t.Fatal("identity race overwritten")
	}
}
