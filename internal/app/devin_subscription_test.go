package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/devin"
	"qswitch/internal/clock"
	"qswitch/internal/quota"
)

func writeDevinLive(t *testing.T, home, token string) string {
	t.Helper()
	path := filepath.Join(home, ".local", "share", "devin", "credentials.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("windsurf_api_key = %q\napi_server_url = \"https://server.codeium.com\"\n", token)
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDevinMaxSnapshotRemovesDailyAndSurvivesHTTPFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	a, _ := setup(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: now}
	status := http.StatusOK
	body := `{"userStatus":{"userId":"devin-fixture","email":"devin@example.test","planStatus":{"planInfo":{"planName":"Max","billingStrategy":"BILLING_STRATEGY_QUOTA","hideDailyQuota":true},"dailyQuotaRemainingPercent":100,"weeklyQuotaRemainingPercent":19,"dailyQuotaResetAtUnix":"1791014400","weeklyQuotaResetAtUnix":"1791100800"}}}`
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "server.codeium.com" || req.URL.Path != "/exa.seat_management_pb.SeatManagementService/GetUserStatus" {
			t.Fatal("unexpected Devin quota endpoint")
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	writeDevinLive(t, a.UserHome, "fixture-token")
	if _, _, err := a.Capture(adapter.Devin); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpdateQuota("devin", "devin-fixture", "exhausted", 100, now.Unix(), now.Unix(), now.Unix(), now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail("devin", "devin-fixture", `[{"id":"daily","used_pct":100},{"id":"weekly","used_pct":90}]`); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		ac, err := a.State.GetAccount("devin", "devin-fixture")
		if err != nil {
			t.Fatal(err)
		}
		b := quota.DecodeBuckets(ac.QuotaDetail)
		if ac.LastQuotaClass != "ok" || ac.LastUsedPct != 81 || ac.LastResetsAt != 1791100800 || len(b) != 1 || b[0].ID != "weekly" || b[0].UsedPct != 81 || b[0].ResetsAt != 1791100800 {
			t.Fatalf("stored class=%s pct=%v reset=%d buckets=%+v", ac.LastQuotaClass, ac.LastUsedPct, ac.LastResetsAt, b)
		}
		for _, tool := range a.Overview().Tools {
			if tool.Tool == "devin" {
				if len(tool.Accounts) != 1 {
					t.Fatalf("account count=%d", len(tool.Accounts))
				}
				view := tool.Accounts[0]
				if view.Plan != "Max" || len(view.Buckets) != 1 || view.Buckets[0].ID != "weekly" || view.RemainingPct == nil || *view.RemainingPct != 19 {
					t.Fatal("overview did not preserve the complete Max weekly snapshot")
				}
				return
			}
		}
		t.Fatal("Devin section absent")
	}
	res := a.ProbeAccount(context.Background(), adapter.Devin, "devin-fixture")
	if res.Class != quota.OK || res.UsedPct != 81 {
		t.Fatalf("quota result class=%s pct=%v", res.Class, res.UsedPct)
	}
	check()
	status = http.StatusServiceUnavailable
	body = `{"error":"unavailable"}`
	res = a.ProbeAccount(context.Background(), adapter.Devin, "devin-fixture")
	if res.Class != quota.Unknown {
		t.Fatalf("failed request class=%s", res.Class)
	}
	check()
}

func TestDevinCLISwitchLeavesIndependentDesktopRunning(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	a, _ := setup(t)
	list := func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 42, Command: "/Applications/Devin.app/Contents/MacOS/Devin"}}, nil
	}
	a.ListProcs = list
	a.Adapters[adapter.Devin] = devin.Adapter{List: list}
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "server.codeium.com" || req.URL.Path != "/exa.seat_management_pb.SeatManagementService/GetUserStatus" {
			t.Fatal("unexpected Devin identity endpoint")
		}
		id := "devin-a"
		if req.Header.Get("Authorization") == "Bearer fixture-b" {
			id = "devin-b"
		}
		body := fmt.Sprintf(`{"userStatus":{"userId":%q,"planStatus":{"planInfo":{"planName":"Max"}}}}`, id)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	path := writeDevinLive(t, a.UserHome, "fixture-a")
	if _, _, err := a.Capture(adapter.Devin); err != nil {
		t.Fatal(err)
	}
	writeDevinLive(t, a.UserHome, "fixture-b")
	if _, _, err := a.Capture(adapter.Devin); err != nil {
		t.Fatal(err)
	}
	quit, launch := 0, 0
	a.Host.QuitFn = func(string) error { quit++; return nil }
	a.Host.LaunchFn = func(string) error { launch++; return nil }
	a.Host.Sleep = func(time.Duration) {}
	a.Host.Timeout = time.Millisecond
	if err := a.Switch(adapter.Devin, "devin-a", adapter.RestoreOpts{}, false); err != nil {
		t.Fatal(err)
	}
	if quit != 0 || launch != 0 {
		t.Fatalf("independent Desktop quit=%d launch=%d", quit, launch)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := devin.ParseCreds(raw)
	if err != nil || creds.APIKey != "fixture-a" {
		t.Fatal("CLI credentials were not switched to the selected account")
	}
	p, err := a.State.GetPointer("devin")
	if err != nil || p.StableID != "devin-a" {
		t.Fatal("CLI pointer was not switched to the selected account")
	}
}
