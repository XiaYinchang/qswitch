package quota

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
)

func TestExactExhaustionAndIncompleteWindows(t *testing.T) {
	for _, pct := range []float64{99.5, 99.999, 100} {
		body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%v,"reset_at":2000000000},"secondary_window":{"used_percent":10,"reset_at":2000100000}}}`, pct)
		r := Classify(KindCodex, http.StatusOK, []byte(body))
		if (r.Class == Exhausted) != (pct == 100) {
			t.Fatalf("Codex %v: %s", pct, r.Class)
		}
		body = fmt.Sprintf(`{"planUsage":{"autoPercentUsed":100,"apiPercentUsed":%v}}`, pct)
		r = Classify(KindCursor, http.StatusOK, []byte(body))
		if (r.Class == Exhausted) != (pct == 100) {
			t.Fatalf("Cursor %v: %s", pct, r.Class)
		}
	}
	for _, tc := range []struct {
		kind Kind
		body string
	}{
		{KindCodex, `{"rate_limit":{"primary_window":{"used_percent":100}}}`},
		{KindCursor, `{"planUsage":{"autoPercentUsed":100}}`},
		{KindCursor, `{"planUsage":{"apiPercentUsed":100}}`},
	} {
		if r := Classify(tc.kind, http.StatusOK, []byte(tc.body)); r.Class != Unknown {
			t.Fatalf("incomplete %s was %s", tc.kind, r.Class)
		}
	}
	r := Classify(KindCodex, http.StatusOK, []byte(`{"rate_limit":{"primary_window":{"used_percent":100,"reset_at":2000000000},"secondary_window":{"used_percent":100,"reset_at":2000100000}}}`))
	if r.Class != Exhausted || r.ResetsAt != 2000100000 {
		t.Fatal("recovery must wait for every blocking window")
	}
}

func TestCursorBotFailureHasIndependentObservation(t *testing.T) {
	status := http.StatusOK
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		code := http.StatusOK
		switch {
		case strings.Contains(req.URL.Path, "GetCurrentPeriodUsage"):
			body = `{"planUsage":{"autoPercentUsed":10,"apiPercentUsed":20}}`
		case strings.Contains(req.URL.Path, "GetSandUsageStatus"):
			code = status
			body = `{"usagePercent":30}`
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	first, err := h.Cursor(context.Background(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	status = http.StatusServiceUnavailable
	second, err := h.Cursor(context.Background(), "synthetic")
	if err != nil || second.Class != OK {
		t.Fatal("Bot failure changed the independent Cursor result")
	}
	merged := MergeBuckets(first.Buckets, second.Buckets)
	for _, b := range merged {
		if b.ID == "bot" {
			if b.Class != Unknown || b.ObservedAt == 0 || b.UsedPct != 0 {
				t.Fatal("Bot inherited the parent's successful freshness or retained known usage")
			}
			return
		}
	}
	t.Fatal("missing explicit Bot unknown observation")
}

func TestDirIdleRequiresReadableEvidence(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for _, path := range []string{root, filepath.Join(root, "missing")} {
		if DirIdle(path, time.Second, now) {
			t.Fatal("absence was treated as idle")
		}
	}
	path := filepath.Join(root, "session")
	if err := os.WriteFile(path, []byte("activity"), 0600); err != nil {
		t.Fatal(err)
	}
	if DirIdle(root, time.Minute, now) {
		t.Fatal("recent activity was treated as idle")
	}
	old := now.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if !DirIdle(root, time.Minute, now) {
		t.Fatal("old readable activity did not permit idle")
	}
}
