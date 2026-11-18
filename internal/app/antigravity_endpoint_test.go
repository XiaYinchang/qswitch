package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

func antigravityNativeEndpointFixture(t *testing.T, a *App, endpoint string) {
	t.Helper()
	path := filepath.Join(a.UserHome, ".gemini", "antigravity", "daemon", "synthetic_endpoint.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pid": 43101, "httpsPort": 43102, "csrfToken": "synthetic-private-record"})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	antigravityFixturePrivateFile(t, path)
	a.ListProcs = func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 43101, Command: "language_server --cloud_code_endpoint=" + endpoint}}, nil
	}
}

func TestAntigravityProbeUsesVerifiedRuntimeBackendAndPersistsBothGroups(t *testing.T) {
	for _, endpoint := range []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"} {
		t.Run(endpoint, func(t *testing.T) {
			a, now := antigravityPipelineFixture(t)
			antigravityNativeEndpointFixture(t, a, endpoint)
			before := state.Pointer{Tool: "antigravity", StableID: antigravityPipelineID, Gen: 3}
			if err := a.State.SetPointer(before); err != nil {
				t.Fatal(err)
			}
			a.Cfg.General.AutoSwitch, a.Cfg.Antigravity.Enabled = true, true
			calls := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != endpoint+"/v1internal:retrieveUserQuotaSummary" {
					t.Fatal("query ignored verified runtime origin")
				}
				body := antigravityPipelineBody(t, now, [4]float64{1, 1, .008, .501}, -1, -1)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
			})}
			result := a.ProbeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID)
			if result.Class != quota.OK || !QuotaResultUpdated(result) || calls != 1 {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
			account, err := a.State.GetAccount("antigravity", antigravityPipelineID)
			if err != nil {
				t.Fatal(err)
			}
			buckets := quota.DecodeBuckets(account.QuotaDetail)
			if len(buckets) != 4 || math.Abs(buckets[2].UsedPct-99.2) > 1e-8 || math.Abs(buckets[3].UsedPct-49.9) > 1e-8 || buckets[0].UsedPct != 0 || account.LastQuotaClass != "ok" {
				t.Fatal("third-party values were lost or Gemini availability ignored")
			}
			after, err := a.State.GetPointer("antigravity")
			if err != nil || after != before {
				t.Fatal("observation changed current identity")
			}
			// Native clients may close after the endpoint has been verified. The
			// private remembered channel supports parked-account observations.
			a.ListProcs = func() ([]adapter.Proc, error) { return nil, nil }
			if result := a.ProbeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID); result.Class != quota.OK || calls != 2 {
				t.Fatal("verified offline backend was not reused")
			}
		})
	}
}

func TestAntigravityProbeUnknownEndpointDoesNotSendCredentials(t *testing.T) {
	for _, mode := range []string{"never observed", "unverified native", "missing flag", "invalid host", "process list failed"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := antigravityPipelineFixture(t)
			switch mode {
			case "never observed":
				a.ListProcs = func() ([]adapter.Proc, error) { return nil, nil }
			case "unverified native":
				a.ListProcs = func() ([]adapter.Proc, error) {
					return []adapter.Proc{{PID: 43103, Command: "language_server --cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com"}}, nil
				}
			case "missing flag":
				a.ListProcs = func() ([]adapter.Proc, error) {
					return []adapter.Proc{{PID: 43101, Command: "language_server --hub"}}, nil
				}
			case "invalid host":
				antigravityNativeEndpointFixture(t, a, "https://evil.invalid")
			case "process list failed":
				a.ListProcs = func() ([]adapter.Proc, error) { return nil, errors.New("synthetic process failure") }
			}
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unconfirmed channel sent credentials")
				return nil, nil
			})}
			result := a.ProbeAccount(context.Background(), adapter.Antigravity, antigravityPipelineID)
			if result.Class != quota.Unknown || result.Source != "endpoint_unknown" || QuotaResultUpdated(result) {
				t.Fatalf("unknown channel=%+v", result)
			}
			latest, err := a.State.LatestQuota("antigravity", antigravityPipelineID)
			if err != nil || latest.Source != "endpoint_unknown" || latest.Class != "unknown" {
				t.Fatal("unknown channel was not visible in probe status")
			}
		})
	}
}
