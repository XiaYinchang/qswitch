package web

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
	"qswitch/internal/clock"
)

const reportNodeToken = "synthetic-node-report-secret-0000000000000001"

func reportReceiver(t *testing.T) (*app.App, *Server, app.Report) {
	t.Helper()
	a := setupApp(t)
	a.Cfg.Web.Token = "synthetic-administration-token-0000000000001"
	a.Clock = clock.Fixed{T: time.Unix(1800000000, 0)}
	if err := a.SetEnvironments([]app.Environment{{ID: a.Instance().ID, Name: "Control"}, {ID: remoteEnvironmentID, Name: "Node"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{remoteEnvironmentID: reportNodeToken, a.Instance().ID: "synthetic-control-report-secret-0000000000001"})
	if err := os.WriteFile(filepath.Join(a.DataDir, "reporting-tokens.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	report := app.Report{ObservedAt: 1800000000, Overview: app.Overview{
		Instance: app.InstanceInfo{ID: remoteEnvironmentID, Name: "Node", Version: "test", OS: "linux", ManagedTools: []adapter.Tool{adapter.Codex}},
		Tools:    []app.ToolView{{Tool: "codex", Accounts: []app.AccountView{{Tool: "codex", StableID: "synthetic-account", Email: "fixture@example.test"}}}},
	}}
	return a, New(a, "127.0.0.1:7432"), report
}

func reportRequest(report app.Report) *http.Request {
	r := proxied(http.MethodPost, "/api/reports/"+remoteEnvironmentID)
	raw, _ := json.Marshal(report)
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(instanceHeader, remoteEnvironmentID)
	r.Header.Set("Authorization", "Bearer "+reportNodeToken)
	return r
}

func TestReportReceiverSeparatesMachineAndManagementAuthentication(t *testing.T) {
	a, s, report := reportReceiver(t)
	if err := SetPassword(a.DataDir, "operator", "synthetic-management-password"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	cookie := sessionCookieOf(t, do(h, withForm(proxied("POST", "/login"), "operator", "synthetic-management-password")))
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"missing token", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"admin token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+a.WebToken()) }, 401},
		{"session cookie", func(r *http.Request) { r.Header.Del("Authorization"); r.AddCookie(cookie) }, 401},
		{"wrong token with cookie", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong"); r.AddCookie(cookie) }, 401},
		{"other node token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer synthetic-control-report-secret-0000000000001")
		}, 401},
		{"missing instance header", func(r *http.Request) { r.Header.Del(instanceHeader) }, 409},
		{"other instance header", func(r *http.Request) { r.Header.Set(instanceHeader, a.Instance().ID) }, 409},
		{"duplicate instance header", func(r *http.Request) { r.Header.Add(instanceHeader, a.Instance().ID) }, 409},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "https://outside.example") }, 403},
		{"rebinding host", func(r *http.Request) { r.Header.Del("X-Forwarded-Host"); r.Host = "outside.example" }, 403},
		{"wrong expected instance", func(r *http.Request) { r.URL.RawQuery = "expected_instance=" + a.Instance().ID }, 409},
		{"wrong body instance", func(r *http.Request) {
			copy := report
			copy.Overview.Instance.ID = a.Instance().ID
			raw, _ := json.Marshal(copy)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
		}, 409},
		{"oversized body", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", app.MaxReportBytes+1)))
		}, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reportRequest(report)
			tc.change(r)
			if w := do(h, r); w.Code != tc.status {
				t.Fatalf("status=%d, want %d", w.Code, tc.status)
			}
			if snapshot, err := a.ReportSnapshot(remoteEnvironmentID); err != nil || snapshot.Overview != nil {
				t.Fatal("rejected request persisted report data")
			}
		})
	}
	for _, path := range []string{"/api/reports/" + remoteEnvironmentID, "/api/overview", "/api/environments", "/api/report"} {
		method := http.MethodGet
		if path == "/api/report" {
			method = http.MethodPost
		}
		r := proxied(method, path)
		r.Header.Set("Authorization", "Bearer "+reportNodeToken)
		r.Header.Set(instanceHeader, a.Instance().ID)
		if w := do(h, r); w.Code != 401 {
			t.Fatalf("report credential bypassed management auth at %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/reports/" + remoteEnvironmentID + "/extra", "/api/reports/invalid"} {
		r := reportRequest(report)
		r.URL.Path = path
		if w := do(h, r); w.Code != 401 {
			t.Fatalf("neighboring route bypassed normal admission: %d", w.Code)
		}
	}
	r := reportRequest(report)
	w := do(h, r)
	var ack struct {
		InstanceID string `json:"instance_id"`
		ObservedAt int64  `json:"observed_at"`
	}
	if json.Unmarshal(w.Body.Bytes(), &ack) != nil || w.Code != 200 || ack.InstanceID != remoteEnvironmentID || ack.ObservedAt != report.ObservedAt {
		t.Fatalf("valid report failed: status=%d", w.Code)
	}
	if w := do(h, reportRequest(report)); w.Code != 409 {
		t.Fatal("duplicate report was accepted")
	}
}

func TestReportReceiverStoresOnlyTypedOverview(t *testing.T) {
	a, s, report := reportReceiver(t)
	raw, _ := json.Marshal(report)
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	payload["access_token"] = "must-not-be-stored"
	payload["overview"].(map[string]any)["credentials"] = map[string]string{"secret": "must-not-be-stored"}
	raw, _ = json.Marshal(payload)
	r := reportRequest(report)
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	if w := do(s.Handler(), r); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	stored, err := os.ReadFile(filepath.Join(a.DataDir, "reports", remoteEnvironmentID+".json"))
	if err != nil || strings.Contains(string(stored), "must-not-be-stored") || strings.Contains(string(stored), "access_token") || strings.Contains(string(stored), "credentials") {
		t.Fatal("unknown fields were persisted")
	}
}

func TestPortalCatalogUsesOnlyStoredReportsAndIsolatesBrokenNode(t *testing.T) {
	a, s, report := reportReceiver(t)
	const brokenID = "22222222222222222222222222222222"
	if err := a.SetEnvironments([]app.Environment{{ID: a.Instance().ID, Name: "Control"}, {ID: remoteEnvironmentID, Name: "Node"}, {ID: brokenID, Name: "Broken"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.AcceptReport(remoteEnvironmentID, report); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.DataDir, "reports", brokenID+".json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Adapters = nil // Any attempt to synchronously collect an overview must fail.
	if err := a.State.Close(); err != nil {
		t.Fatal(err)
	}
	a.State = nil
	for _, age := range []int64{0, 60} {
		a.Clock = clock.Fixed{T: time.Unix(report.ObservedAt+age, 0)}
		r := proxied("GET", "/api/environments")
		r.Header.Set("X-Qswitch-Portal", "1")
		r.Header.Set("Authorization", "Bearer "+a.WebToken())
		w := do(s.Handler(), r)
		var catalog struct {
			ControlPlane bool `json:"control_plane"`
			Environments []struct {
				ID       string             `json:"id"`
				Snapshot app.ReportSnapshot `json:"snapshot"`
			} `json:"environments"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil || !catalog.ControlPlane || len(catalog.Environments) != 3 {
			t.Fatalf("catalog failed: %d", w.Code)
		}
		for _, node := range catalog.Environments {
			if node.ID == remoteEnvironmentID {
				if node.Snapshot.Overview == nil || node.Snapshot.Stale != (age >= 60) || node.Snapshot.ObservedAt != report.ObservedAt || node.Snapshot.ReceivedAt != report.ObservedAt {
					t.Fatalf("wrong stored snapshot: %+v", node.Snapshot)
				}
			} else if !node.Snapshot.Stale || node.Snapshot.Overview != nil {
				t.Fatal("missing/broken node reported available")
			}
		}
	}
}

func TestManualReportingUsesNormalInstanceGuard(t *testing.T) {
	a, s, _ := reportReceiver(t)
	for _, id := range []string{"", remoteEnvironmentID} {
		r := proxied("POST", "/api/report")
		r.Header.Set("Authorization", "Bearer "+a.WebToken())
		if id != "" {
			r.Header.Set(instanceHeader, id)
		}
		w := do(s.Handler(), r)
		want := 409
		if id == "" {
			want = 400
		}
		if w.Code != want {
			t.Fatalf("manual report target guard=%d, want %d", w.Code, want)
		}
	}
	r := proxied("POST", "/api/report")
	r.Header.Set("Authorization", "Bearer "+a.WebToken())
	r.Header.Set(instanceHeader, a.Instance().ID)
	if w := do(s.Handler(), r); w.Code != 503 || strings.Contains(w.Body.String(), reportNodeToken) {
		t.Fatalf("disabled reporting should fail without exposing credentials: status=%d", w.Code)
	}
}
