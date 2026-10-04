package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"120", 2 * time.Minute}, {" 1800 ", 30 * time.Minute},
		{now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"-1", 0}, {"0", 0}, {"garbage", 0}, {"9999999999999999999", 0},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Fatalf("Retry-After %q: got %s want %s", tc.value, got, tc.want)
		}
	}
}

func TestAllQuotaProvidersPreserveRetryAfter(t *testing.T) {
	for _, status := range []int{429, 503} {
		for _, tool := range []string{"codex", "grok", "cursor", "devin", "kimi", "zcode"} {
			t.Run(tool+http.StatusText(status), func(t *testing.T) {
				h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"1200"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
				})}}
				var got Result
				var err error
				ctx := context.Background()
				switch tool {
				case "codex":
					got, err = h.Codex(ctx, "fixture", "fixture")
				case "grok":
					got, err = h.Grok(ctx, "fixture")
				case "cursor":
					got, err = h.Cursor(ctx, "fixture")
				case "devin":
					got, err = h.Devin(ctx, "fixture", "")
				case "kimi":
					got, err = h.Kimi(ctx, "fixture")
				case "zcode":
					got, err = h.ZCode(ctx, "fixture", "bigmodel")
				}
				if err != nil || got.Class != Unknown || got.RetryAfter != 20*time.Minute {
					t.Fatalf("throttled response lost retry delay: %+v err=%v", got, err)
				}
			})
		}
	}
}
