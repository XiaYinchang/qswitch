package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/clock"
)

func TestCurrentCodexRecoverySchedule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset time.Duration
		wait  time.Duration
	}{
		{name: "before advertised reset", reset: 3 * time.Hour, wait: 30 * time.Minute},
		{name: "after advertised reset", reset: -time.Hour, wait: 10 * time.Minute},
		{name: "without advertised reset", wait: 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := setup(t)
			if _, _, err := a.Capture(adapter.Codex); err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
			var reset int64
			if tc.reset != 0 {
				reset = start.Add(tc.reset).Unix()
			}
			if err := a.State.UpdateQuota("codex", "acc-a", "exhausted", 100, reset, start.Unix(), start.Unix(), 0); err != nil {
				t.Fatal(err)
			}
			if err := a.State.SetCooling("codex", "acc-a", reset); err != nil {
				t.Fatal(err)
			}
			if err := a.State.LogQuota("codex", "acc-a", "exhausted", "http", 100, start); err != nil {
				t.Fatal(err)
			}
			hits := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				hits++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":8}}}`)), Header: make(http.Header), Request: req}, nil
			})}
			a.Clock = clock.Fixed{T: start.Add(tc.wait - time.Minute)}
			a.ProbeHTTP(context.Background(), adapter.Codex, false)
			if hits != 0 {
				t.Fatalf("recovery probed too soon: %d HTTP requests", hits)
			}
			a.Clock = clock.Fixed{T: start.Add(tc.wait)}
			a.ProbeHTTP(context.Background(), adapter.Codex, false)
			if hits != 1 {
				t.Fatalf("due recovery should probe current account: %d HTTP requests", hits)
			}
			ac, err := a.State.GetAccount("codex", "acc-a")
			if err != nil || ac.LastQuotaClass != "ok" || ac.CoolingUntil != 0 {
				t.Fatalf("recovery not recorded: %+v err=%v", ac, err)
			}
		})
	}
}

func TestSuccessfulForcedProbeClearsBackoff(t *testing.T) {
	a, _ := setup(t)
	if _, _, err := a.Capture(adapter.Codex); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	a.Clock = clock.Fixed{T: start}
	hits := 0
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hits++
		status, body := 200, `{"rate_limit":{"primary_window":{"used_percent":98}}}`
		if hits == 1 {
			status, body = 500, `{"error":"temporary failure"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	if res := a.ProbeAccount(context.Background(), adapter.Codex, "acc-a"); res.Class != "unknown" {
		t.Fatalf("first probe should fail: %+v", res)
	}
	ac, err := a.State.GetAccount("codex", "acc-a")
	if err != nil || ac.HTTPBackoffUntil != start.Add(a.Cfg.Backoff()).Unix() {
		t.Fatalf("failed probe should set backoff: %+v err=%v", ac, err)
	}
	if res := a.ProbeAccount(context.Background(), adapter.Codex, "acc-a"); res.Class != "soft" {
		t.Fatalf("forced probe should recover: %+v", res)
	}
	ac, err = a.State.GetAccount("codex", "acc-a")
	if err != nil || ac.HTTPBackoffUntil != 0 {
		t.Fatalf("successful probe should clear backoff: %+v err=%v", ac, err)
	}
	a.Clock = clock.Fixed{T: start.Add(15 * time.Minute)}
	a.ProbeHTTP(context.Background(), adapter.Codex, false)
	if hits != 3 {
		t.Fatalf("scheduled probe should resume before the old backoff expires: %d HTTP requests", hits)
	}
}
