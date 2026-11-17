package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/config"
)

func TestReporterPublishesAndManualRefreshMakesNewObservation(t *testing.T) {
	a, _ := setup(t)
	if err := a.SetManagedTools([]adapter.Tool{adapter.ZCode}); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("report-only-fixture-", 3)
	reports := make(chan Report, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/control/api/reports/"+a.Instance().ID || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Qswitch-Instance") != a.Instance().ID || r.Header.Get("Cookie") != "" {
			t.Errorf("report route or authentication boundary failed")
			w.WriteHeader(403)
			return
		}
		var report Report
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
			return
		}
		reports <- report
		json.NewEncoder(w).Encode(map[string]any{"instance_id": report.Overview.Instance.ID, "observed_at": report.ObservedAt})
	}))
	defer server.Close()
	a.Cfg.Reporting = config.Reporting{URL: server.URL + "/control/", Token: token}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.runReporter(ctx, time.Hour) }()
	defer func() { cancel(); <-done }()
	first := awaitReport(t, reports)
	if first.Overview.Instance.ID != a.Instance().ID || len(first.Overview.Tools) != 1 || first.Overview.Tools[0].Tool != "zcode" {
		t.Fatal("report lost node identity or scope")
	}
	refreshCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	observed, err := a.ReportNow(refreshCtx)
	if err != nil {
		t.Fatal(err)
	}
	second := awaitReport(t, reports)
	if observed != second.ObservedAt || second.ObservedAt <= first.ObservedAt {
		t.Fatal("manual refresh reused the previous observation")
	}
}

func TestReporterRecoversUsingFreshPeriodicObservations(t *testing.T) {
	a, _ := setup(t)
	if err := a.SetManagedTools([]adapter.Tool{adapter.ZCode}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []Report
	completed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report Report
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls = append(calls, report)
		n := len(calls)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"instance_id": report.Overview.Instance.ID, "observed_at": report.ObservedAt})
		select {
		case completed <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	a.Cfg.Reporting = config.Reporting{URL: server.URL, Token: strings.Repeat("x", 40)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.runReporter(ctx, 20*time.Millisecond) }()
	defer func() { cancel(); <-done }()
	select {
	case <-completed:
	case <-time.After(4 * time.Second):
		t.Fatal("periodic reporting did not recover")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 || calls[1].ObservedAt <= calls[0].ObservedAt {
		t.Fatal("failed payload was replayed instead of collecting a new snapshot")
	}
}

func TestReportTransportRejectsRedirectsAndWrongAcknowledgements(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer other.Close()
	for _, mode := range []string{"redirect", "wrong-instance", "wrong-time", "html"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				case "wrong-instance":
					json.NewEncoder(w).Encode(map[string]any{"instance_id": "another", "observed_at": 100})
				case "wrong-time":
					json.NewEncoder(w).Encode(map[string]any{"instance_id": "fixture", "observed_at": 99})
				case "html":
					w.Write([]byte("<html>login</html>"))
				}
			}))
			defer server.Close()
			client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			err := sendReport(context.Background(), client, server.URL, strings.Repeat("s", 40), Report{ObservedAt: 100, Overview: Overview{Instance: InstanceInfo{ID: "fixture"}}})
			if err == nil {
				t.Fatal("accepted an unconfirmed report")
			}
		})
	}
	if leaked {
		t.Fatal("report credential followed a redirect")
	}
}

func TestReportEndpointRequiresPrivateTransportAndDedicatedCredential(t *testing.T) {
	for _, raw := range []string{"http://remote.example.test/", "https://name:pass@example.test/", "https://example.test/?secret=value", "https://example.test/#fragment", "file:///tmp/report"} {
		if _, err := reportEndpoint(config.Reporting{URL: raw, Token: strings.Repeat("x", 40)}, "fixture"); err == nil {
			t.Errorf("accepted unsafe report destination %q", raw)
		}
	}
	a, _ := setup(t)
	if _, err := a.ReportNow(context.Background()); err == nil {
		t.Fatal("unconfigured reporting should fail immediately")
	}
}

func awaitReport(t *testing.T, reports <-chan Report) Report {
	t.Helper()
	select {
	case report := <-reports:
		return report
	case <-time.After(3 * time.Second):
		t.Fatal("report was not published")
		return Report{}
	}
}

func TestReporterCancellationAndShutdownReleaseRefreshWaiters(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "cancel-queued"
		if shutdown {
			name = "stop-worker"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := setup(t)
			if err := a.SetManagedTools([]adapter.Tool{adapter.ZCode}); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			release := make(chan struct{})
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				if n == 1 {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				var report Report
				if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"instance_id": report.Overview.Instance.ID, "observed_at": report.ObservedAt})
			}))
			defer server.Close()
			a.Cfg.Reporting = config.Reporting{URL: server.URL, Token: strings.Repeat("r", 40)}
			workerCtx, stopWorker := context.WithCancel(context.Background())
			workerDone := make(chan struct{})
			go func() { defer close(workerDone); a.runReporter(workerCtx, time.Hour) }()
			defer func() { stopWorker(); <-workerDone }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("worker did not publish")
			}
			requestCtx, stopRequest := context.WithCancel(context.Background())
			defer stopRequest()
			result := make(chan error, 1)
			go func() { _, err := a.ReportNow(requestCtx); result <- err }()
			deadline := time.Now().Add(time.Second)
			for {
				a.reportMu.Lock()
				queued := len(a.reportRequests)
				a.reportMu.Unlock()
				if queued == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("refresh was not queued")
				}
				time.Sleep(time.Millisecond)
			}
			if shutdown {
				stopWorker()
			} else {
				stopRequest()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("canceled refresh reported success")
				}
			case <-time.After(time.Second):
				t.Fatal("refresh waiter did not stop")
			}
			close(release)
			if !shutdown {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if _, err := a.ReportNow(ctx); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				n := calls
				mu.Unlock()
				if n != 2 {
					t.Fatalf("canceled queued refresh still published: %d calls", n)
				}
			}
		})
	}
}
