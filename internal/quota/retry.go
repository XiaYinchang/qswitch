package quota

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func responseRetryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return 0
	}
	return parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
}

// MarkThrottled labels an unknown result from a 429 or 503 response, so
// manual refreshes wait out the server's retry deadline for every provider.
func MarkThrottled(r Result, status int) Result {
	if r.Class == Unknown && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
		r.Source = "http_throttled"
	}
	return r
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
