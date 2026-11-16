package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"time"
)

// AntigravityQuotaURL is the consumer quota-summary endpoint verified with
// Antigravity 2.32.1. It is an internal, versioned API, not the Gemini API-key
// quota service. The endpoint requires the native product user agent.
const AntigravityQuotaURL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"

const antigravityQuotaUserAgent = "antigravity/2.32.1"

type antigravityQuotaSummary struct {
	Groups []struct {
		Buckets []antigravityQuotaBucket `json:"buckets"`
	} `json:"groups"`
}

type antigravityQuotaBucket struct {
	ID                string          `json:"bucketId"`
	Window            string          `json:"window"`
	RemainingFraction *float64        `json:"remainingFraction"`
	RemainingAmount   json.RawMessage `json:"remainingAmount"`
	Disabled          bool            `json:"disabled"`
	ResetTime         string          `json:"resetTime"`
}

// ParseAntigravityQuota accepts either the cloud quota summary or the local
// language-server envelope {"response": ...}. Each model group is constrained
// by every one of its windows; an account is exhausted only if BOTH groups
// are constrained. Paid AI credits are a separate balance and do not change
// this subscription-quota policy.
func ParseAntigravityQuota(status int, body []byte, retryAfter time.Duration) Result {
	return parseAntigravityQuota(status, body, retryAfter, time.Now())
}

func parseAntigravityQuota(status int, body []byte, retryAfter time.Duration, now time.Time) Result {
	r := Result{Class: Unknown, Source: "http"}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		r.Class = Expired
		return r
	}
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		r.Source, r.RetryAfter = "http_throttled", retryAfter
		return r
	}
	if status != http.StatusOK {
		return r
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return r
	}
	for _, field := range []string{"error", "errors", "code"} {
		if _, present := root[field]; present {
			return r
		}
	}
	if response, present := root["response"]; present {
		body = response
		if json.Unmarshal(body, &root) != nil || root == nil {
			return r
		}
		for _, field := range []string{"error", "errors", "code"} {
			if _, present := root[field]; present {
				return r
			}
		}
	}
	var summary antigravityQuotaSummary
	if json.Unmarshal(body, &summary) != nil || summary.Groups == nil {
		return r
	}
	r.Authenticated = true

	// IDs, not translated display names or array order, identify the two
	// groups. Require the complete four-window contract before authorizing
	// a switch; absent, disabled, duplicate or unrecognized values are not zero.
	windows := map[string]string{
		"gemini-5h": "5h", "gemini-weekly": "weekly",
		"3p-5h": "5h", "3p-weekly": "weekly",
	}
	buckets := make(map[string]Bucket, len(windows))
	for _, group := range summary.Groups {
		bindingGroup := false
		for _, raw := range group.Buckets {
			if _, known := windows[raw.ID]; known {
				bindingGroup = true
				break
			}
		}
		if !bindingGroup {
			continue // Credits and unrelated quota groups have another policy.
		}
		for _, raw := range group.Buckets {
			window, known := windows[raw.ID]
			if !known {
				return r // A changed binding-window contract needs inspection.
			}
			if _, duplicate := buckets[raw.ID]; duplicate || raw.Disabled || raw.Window != window || raw.RemainingFraction == nil || len(raw.RemainingAmount) != 0 {
				return r
			}
			remaining := *raw.RemainingFraction
			if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 1 {
				return r
			}
			var reset int64
			if raw.ResetTime != "" {
				t, err := time.Parse(time.RFC3339Nano, raw.ResetTime)
				if err != nil || t.Unix() <= now.Unix() {
					return r // A stale window cannot prove current exhaustion.
				}
				reset = t.Unix()
			}
			used := (1 - remaining) * 100
			if remaining > 0 && used >= 100 {
				used = math.Nextafter(100, 0) // Float rounding is not exhaustion.
			}
			buckets[raw.ID] = Bucket{ID: antigravityBucketID(raw.ID), UsedPct: used, ResetsAt: reset}
		}
	}
	for _, id := range []string{"gemini-5h", "gemini-weekly", "3p-5h", "3p-weekly"} {
		if bucket, ok := buckets[id]; ok {
			r.Buckets = append(r.Buckets, bucket)
		}
	}
	if len(buckets) != len(windows) {
		return r
	}

	gemini := antigravityGroup(buckets["gemini-5h"], buckets["gemini-weekly"])
	thirdParty := antigravityGroup(buckets["3p-5h"], buckets["3p-weekly"])
	// The less-used available group determines the account's remaining room.
	// Taking the maximum here would turn one exhausted group into a switch.
	r.UsedPct = math.Min(gemini.used, thirdParty.used)
	r.Class = OK
	if gemini.exhausted && thirdParty.exhausted {
		// Exhaustion is cached until ResetsAt. If either group's blocking
		// window has no reset, that group could recover before the other
		// group's known deadline, so no safe exhaustion lease exists.
		if gemini.recovery == 0 || thirdParty.recovery == 0 {
			r.Class = Unknown
			return r
		}
		r.Class, r.UsedPct = Exhausted, 100
		// All blocking windows of one group must recover, then either group
		// makes the account usable again.
		r.ResetsAt = earlierAntigravityReset(gemini.recovery, thirdParty.recovery)
	} else {
		if r.UsedPct >= 90 {
			r.Class = Soft
		}
		// Refresh any still-usable observation at its earliest known boundary.
		for _, bucket := range r.Buckets {
			r.ResetsAt = earlierAntigravityReset(r.ResetsAt, bucket.ResetsAt)
		}
	}
	return r
}

func antigravityBucketID(id string) string {
	switch id {
	case "gemini-5h":
		return "gemini_5h"
	case "gemini-weekly":
		return "gemini_weekly"
	case "3p-5h":
		return "third_party_5h"
	case "3p-weekly":
		return "third_party_weekly"
	}
	return ""
}

type antigravityGroupState struct {
	used      float64
	exhausted bool
	recovery  int64
}

func antigravityGroup(buckets ...Bucket) antigravityGroupState {
	var group antigravityGroupState
	unknownReset := false
	for _, bucket := range buckets {
		group.used = math.Max(group.used, bucket.UsedPct)
		if bucket.UsedPct < 100 {
			continue
		}
		group.exhausted = true
		if bucket.ResetsAt == 0 {
			unknownReset = true
		}
		if bucket.ResetsAt > group.recovery {
			group.recovery = bucket.ResetsAt
		}
	}
	if unknownReset {
		group.recovery = 0
	}
	return group
}

func earlierAntigravityReset(a, b int64) int64 {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// Antigravity reads one account without changing the active desktop or CLI
// identity. The caller refreshes expired OAuth tokens and owns vault writes.
func (h HTTP) Antigravity(ctx context.Context, accessToken, project string) (Result, error) {
	unknown := Result{Class: Unknown, Source: "http"}
	if accessToken == "" {
		return unknown, errors.New("antigravity: missing access token")
	}
	body, err := json.Marshal(struct {
		Project string `json:"project,omitempty"`
	}{Project: project})
	if err != nil {
		return unknown, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AntigravityQuotaURL, bytes.NewReader(body))
	if err != nil {
		return unknown, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", antigravityQuotaUserAgent)
	client := *h.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("antigravity: quota endpoint redirected")
	}
	res, err := client.Do(req)
	if err != nil {
		return unknown, err
	}
	defer res.Body.Close()
	body, err = io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return unknown, err
	}
	if len(body) > 1<<20 {
		return unknown, errors.New("antigravity: quota response exceeds size limit")
	}
	return ParseAntigravityQuota(res.StatusCode, body, responseRetryAfter(res)), nil
}
