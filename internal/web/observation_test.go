package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"qswitch/internal/app"
	"qswitch/internal/state"
)

func observationApp(t *testing.T) *app.App {
	t.Helper()
	a := lifecycleApp(t)
	const cursorID = "cursor-observation-fixture"
	if err := a.State.UpsertAccount(state.Account{Tool: "cursor", StableID: cursorID, Email: "cursor@example.test", LastQuotaClass: "ok", LastUsedPct: 25}); err != nil {
		t.Fatal(err)
	}
	payload := `{"kind":"cursor.auth.v1","identity":{"tool":"cursor","stable_id":"cursor-observation-fixture","email":"cursor@example.test"},"desktop":{"cursorAuth/accessToken":"fixture-cursor-access"}}`
	if err := a.Vault.Put("cursor", cursorID, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetQuotaDetail("cursor", cursorID, `[{"id":"auto","used_pct":25},{"id":"api","used_pct":20},{"id":"bot","used_pct":15}]`); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetPointer(state.Pointer{Tool: "cursor", StableID: cursorID}); err != nil {
		t.Fatal(err)
	}
	if err := a.State.LogQuota("cursor", cursorID, "ok", "http", 25, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.State.UpsertAccount(state.Account{Tool: "devin", StableID: "never-observed-fixture", DisplayName: "从未查到用量", LastQuotaClass: "unknown"}); err != nil {
		t.Fatal(err)
	}
	// Initial snapshots are valid. All subsequent probes go through the real
	// application/API path with synthetic failures and no external transport.
	a.HTTP.Client = &http.Client{Transport: fixtureTransport(func(req *http.Request) (*http.Response, error) {
		status := 0
		switch req.URL.Host {
		case "chatgpt.com":
			status = http.StatusServiceUnavailable
		case "api2.cursor.sh":
			status = http.StatusUnauthorized
		default:
			return nil, errors.New("fixture rejects unexpected upstream")
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"fixture failure"}`)), Request: req}, nil
	})}
	return a
}

func TestProbeFailureHTTPReportsRetainedQuota(t *testing.T) {
	a := observationApp(t)
	h := New(a, "127.0.0.1:7432").Handler()
	for _, tc := range []struct{ tool, id, class string }{
		{"codex", "demo-a", "unknown"}, {"cursor", "cursor-observation-fixture", "expired"},
	} {
		raw, _ := json.Marshal(map[string]string{"tool": tc.tool, "id": tc.id})
		req := httptest.NewRequest(http.MethodPost, "/api/probe", strings.NewReader(string(raw)))
		req.Host = "127.0.0.1:7432"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var result struct {
			Class   string `json:"class"`
			Updated bool   `json:"updated"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Class != tc.class || result.Updated {
			t.Fatalf("failed probe advertised an update: status=%d result=%+v err=%v", w.Code, result, err)
		}
	}
	var codex, cursor, bot app.AccountView
	for _, tool := range a.Overview().Tools {
		for _, ac := range tool.Accounts {
			if tool.Tool == "codex" && ac.StableID == "demo-a" {
				codex = ac
			}
			if tool.Tool == "cursor" {
				cursor = ac
			}
			if tool.Tool == "grokbot" {
				bot = ac
			}
		}
	}
	if codex.Class != "ok" || codex.UsedPct != 25 || !codex.QuotaStale || codex.LastProbeClass != "unknown" || len(codex.Buckets) != 2 {
		t.Fatalf("failed HTTP lost/overstated old Codex quota: %+v", codex)
	}
	if cursor.Class != "expired" || !cursor.QuotaStale || cursor.LastProbeClass != "expired" || len(cursor.Buckets) != 2 || cursor.Buckets[0].UsedPct != 25 {
		t.Fatalf("expired Cursor quota not marked historical: %+v", cursor)
	}
	if bot.Class != "expired" || !bot.QuotaStale || bot.RemainingPct != nil {
		t.Fatalf("derived Bot lost expired auth state: %+v", bot)
	}
}

// Run only when explicitly requested; every credential and upstream is synthetic.
func TestObservationBrowserFixture(t *testing.T) {
	addr := os.Getenv("QSWITCH_OBSERVATION_BROWSER_ADDR")
	if addr == "" {
		t.Skip("browser fixture not requested")
	}
	a := observationApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	t.Logf("observation browser fixture http://%s: Codex 查询 -> unknown with historical buckets; Cursor 查询 -> expired including Grok Bot; Devin has no quota history", addr)
	if err := New(a, addr).Run(ctx); err != nil {
		t.Fatal(err)
	}
}
