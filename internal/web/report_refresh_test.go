package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
	"qswitch/internal/config"
)

func refreshReportPublisher(t *testing.T, a *app.App, manualACK <-chan struct{}) <-chan app.Report {
	t.Helper()
	reports := make(chan app.Report, 8)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report app.Report
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		reports <- report
		if count.Add(1) > 1 && manualACK != nil {
			select {
			case <-manualACK:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"instance_id": report.Overview.Instance.ID, "observed_at": report.ObservedAt})
	}))
	a.Cfg.Reporting = config.Reporting{URL: server.URL, Token: strings.Repeat("synthetic-report-", 3)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.RunReporter(ctx) }()
	t.Cleanup(func() { cancel(); <-done; server.Close() })
	awaitRefreshReport(t, reports)
	return reports
}

func awaitRefreshReport(t *testing.T, reports <-chan app.Report) app.Report {
	t.Helper()
	select {
	case report := <-reports:
		return report
	case <-time.After(4 * time.Second):
		t.Fatal("report was not received")
		return app.Report{}
	}
}

func refreshRequest(a *app.App, path, body string) *http.Request {
	r := local(http.MethodPost, path)
	r.Header.Set(instanceHeader, a.Instance().ID)
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

func TestManualReportRefreshQueriesOnlySelectedToolBeforeAcknowledgement(t *testing.T) {
	a := subscriptionApp(t)
	var calls atomic.Int32
	a.HTTP.Client = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host+r.URL.Path != "api.kimi.com/coding/v1/usages" {
			t.Error("refresh queried another scope")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"usages":{"limit_5h":{"used_ratio":0.65},"limit_7d":{"used_ratio":0.50},"limit_month_total":{"used_ratio":0.10}}}`)), Request: r}, nil
	})}
	before, err := a.State.GetPointer("kimi")
	if err != nil {
		t.Fatal(err)
	}
	ack := make(chan struct{})
	defer close(ack)
	reports := refreshReportPublisher(t, a, ack)
	if calls.Load() != 0 {
		t.Fatal("periodic reporting queried a provider")
	}
	h := New(a, "127.0.0.1:7432").Handler()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- do(h, refreshRequest(a, "/api/report", `{"tool":"kimi"}`)) }()
	report := awaitRefreshReport(t, reports)
	if calls.Load() != 2 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
	for _, tool := range report.Overview.Tools {
		if tool.Tool != "kimi" {
			continue
		}
		for _, ac := range tool.Accounts {
			if ac.UsedPct != 65 {
				t.Fatalf("reported retained value instead of fresh quota: %v", ac.UsedPct)
			}
		}
	}
	select {
	case <-response:
		t.Fatal("refresh returned before control-plane ACK")
	default:
	}
	ack <- struct{}{}
	var w *httptest.ResponseRecorder
	select {
	case w = <-response:
	case <-time.After(3 * time.Second):
		t.Fatal("ACK did not release refresh response")
	}
	var result struct {
		Instance   app.InstanceInfo        `json:"instance"`
		Reported   bool                    `json:"reported"`
		ObservedAt int64                   `json:"observed_at"`
		Quota      app.QuotaRefreshSummary `json:"quota"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Reported || result.Instance.ID != a.Instance().ID || result.ObservedAt != report.ObservedAt || result.Quota != (app.QuotaRefreshSummary{Tool: adapter.Kimi, Attempted: 2, Updated: 2}) {
		t.Fatalf("refresh response status=%d result=%+v err=%v", w.Code, result, err)
	}
	after, err := a.State.GetPointer("kimi")
	if err != nil || after != before {
		t.Fatal("explicit quota refresh changed account")
	}
}

func TestManualReportRefreshPublishesPartialFailuresAndSkipsBackoff(t *testing.T) {
	a := subscriptionApp(t)
	confirmed, err := a.State.LatestConfirmedQuota("kimi", "kimi-b")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	a.HTTP.Client = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		status, body := 200, `{"usages":{"limit_5h":{"used_ratio":0.55},"limit_7d":{"used_ratio":0.50},"limit_month_total":{"used_ratio":0.10}}}`
		if strings.HasSuffix(r.Header.Get("Authorization"), "kimi-b") {
			status, body = 503, `{}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	reports := refreshReportPublisher(t, a, nil)
	h := New(a, "127.0.0.1:7432").Handler()
	for i, want := range []app.QuotaRefreshSummary{{Tool: adapter.Kimi, Attempted: 2, Updated: 1, Failed: 1}, {Tool: adapter.Kimi, Attempted: 1, Updated: 1, Skipped: 1}} {
		w := do(h, refreshRequest(a, "/api/report", `{"tool":"kimi"}`))
		var result struct {
			Reported bool                    `json:"reported"`
			Quota    app.QuotaRefreshSummary `json:"quota"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Reported || result.Quota != want {
			t.Fatalf("step %d status=%d result=%+v err=%v", i, w.Code, result, err)
		}
		report := awaitRefreshReport(t, reports)
		for _, tool := range report.Overview.Tools {
			if tool.Tool != "kimi" {
				continue
			}
			for _, ac := range tool.Accounts {
				if ac.StableID == "kimi-b" && (ac.LastQuotaAt != confirmed.Ts || ac.UsedPct != 40 || !ac.QuotaStale) {
					t.Fatal("failure hid historical quota or made it appear new")
				}
			}
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("backoff was retried: provider calls=%d", calls.Load())
	}
}

func TestManualReportEmptyBodyAndEmptyToolNeverQueryProviders(t *testing.T) {
	a := setupApp(t)
	if err := a.SetManagedTools([]adapter.Tool{adapter.ZCode}); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: fixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Error("report-only operation queried provider")
		return nil, errors.New("unexpected request")
	})}
	reports := refreshReportPublisher(t, a, nil)
	h := New(a, "127.0.0.1:7432").Handler()
	for _, body := range []string{"", `{}`, `{"tool":"zcode"}`} {
		w := do(h, refreshRequest(a, "/api/report", body))
		var result struct {
			Reported bool                     `json:"reported"`
			Quota    *app.QuotaRefreshSummary `json:"quota"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Reported {
			t.Fatalf("body=%q status=%d err=%v", body, w.Code, err)
		}
		if body == `{"tool":"zcode"}` {
			if result.Quota == nil || *result.Quota != (app.QuotaRefreshSummary{Tool: adapter.ZCode}) {
				t.Fatal("empty read-only tool did not return zero summary")
			}
		} else if result.Quota != nil {
			t.Fatal("report-only response invented a quota refresh")
		}
		awaitRefreshReport(t, reports)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{{`{"tool":"codex"}`, 403}, {`{"tool":"grokbot"}`, 400}, {`{"tool":"missing"}`, 400}, {`{"tool":`, 400}} {
		if w := do(h, refreshRequest(a, "/api/report", tc.body)); w.Code != tc.status {
			t.Fatalf("invalid/scope request=%q status=%d", tc.body, w.Code)
		}
	}
}

func TestSingleAccountProbeDoesNotMarkRetainedBackoffQuotaUpdated(t *testing.T) {
	a := setupApp(t)
	now := time.Now()
	if err := a.State.UpdateQuota("codex", "acc-a", "ok", 15, now.Add(time.Hour).Unix(), now.Unix(), now.Unix(), now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota("codex", "acc-a", "unknown", "http_throttled", 0, now); err != nil {
		t.Fatal(err)
	}
	a.HTTP.Client = &http.Client{Transport: fixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Error("backoff should not query provider")
		return nil, errors.New("unexpected request")
	})}
	w := do(New(a, "127.0.0.1:7432").Handler(), refreshRequest(a, "/api/probe", `{"tool":"codex","id":"acc-a"}`))
	var result struct {
		Updated bool   `json:"updated"`
		Class   string `json:"class"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Updated || result.Class != "ok" || result.Source != "backoff" {
		t.Fatalf("status=%d result=%+v err=%v", w.Code, result, err)
	}
}
