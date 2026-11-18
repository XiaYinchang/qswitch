package quota

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

var antigravityTestNow = time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)

// Handwritten synthetic contract data: no account identity or captured
// production quota response is embedded in these tests.
func antigravityTestSummary(fractions [4]float64) map[string]any {
	ids := []string{"gemini-5h", "gemini-weekly", "3p-5h", "3p-weekly"}
	windows := []string{"5h", "weekly", "5h", "weekly"}
	buckets := make([]any, len(ids))
	for i, id := range ids {
		buckets[i] = map[string]any{
			"bucketId": id, "window": windows[i], "remainingFraction": fractions[i],
			"resetTime": antigravityTestNow.Add(time.Duration(i+1) * time.Hour).Format(time.RFC3339),
		}
	}
	return map[string]any{"groups": []any{
		map[string]any{"displayName": "Translated group A", "buckets": buckets[:2]},
		map[string]any{"displayName": "Translated group B", "buckets": buckets[2:]},
	}}
}

func antigravityTestBucket(summary map[string]any, index int) map[string]any {
	groups := summary["groups"].([]any)
	return groups[index/2].(map[string]any)["buckets"].([]any)[index%2].(map[string]any)
}

func antigravityTestParse(t *testing.T, summary any) Result {
	t.Helper()
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	return parseAntigravityQuota(200, body, 0, antigravityTestNow)
}

func TestAntigravityQuotaRequiresBothModelGroups(t *testing.T) {
	// All combinations catch replacing either OR within a group or AND
	// across groups with the wrong operator.
	for mask := 0; mask < 16; mask++ {
		fractions := [4]float64{0.8, 0.7, 0.6, 0.5}
		for i := range fractions {
			if mask&(1<<i) != 0 {
				fractions[i] = 0
			}
		}
		r := antigravityTestParse(t, antigravityTestSummary(fractions))
		wantExhausted := mask&3 != 0 && mask&12 != 0
		if (r.Class == Exhausted) != wantExhausted || !r.Authenticated || len(r.Buckets) != 4 {
			t.Fatalf("mask %04b: %+v, exhausted want %v", mask, r, wantExhausted)
		}
		if !wantExhausted && r.UsedPct >= 100 {
			t.Fatalf("mask %04b: single exhausted group raised account usage to 100", mask)
		}
	}
}

func TestAntigravityQuotaDoesNotRoundUpToExhaustion(t *testing.T) {
	for _, remaining := range []float64{0.005, 0.00001, 1e-18} {
		r := antigravityTestParse(t, antigravityTestSummary([4]float64{0, 0, remaining, 0.5}))
		if r.Class != Soft || r.UsedPct >= 100 {
			t.Fatalf("remaining %g: %+v", remaining, r)
		}
	}
}

func TestAntigravityQuotaUsesGroupRecoveryNotEarliestBucket(t *testing.T) {
	summary := antigravityTestSummary([4]float64{0, 0, 0, 0.5})
	// Gemini: both 5h and weekly block until hour 9. Third-party: the 5h
	// window blocks until hour 3. The account recovers at 3, not at 1 or 9.
	antigravityTestBucket(summary, 1)["resetTime"] = antigravityTestNow.Add(9 * time.Hour).Format(time.RFC3339)
	r := antigravityTestParse(t, summary)
	if r.Class != Exhausted || r.ResetsAt != antigravityTestNow.Add(3*time.Hour).Unix() {
		t.Fatalf("unexpected recovery: %+v", r)
	}
	antigravityTestBucket(summary, 3)["remainingFraction"] = 0
	r = antigravityTestParse(t, summary)
	if r.ResetsAt != antigravityTestNow.Add(4*time.Hour).Unix() {
		t.Fatalf("third-party weekly constraint ignored: %+v", r)
	}
	delete(antigravityTestBucket(summary, 1), "resetTime")
	r = antigravityTestParse(t, summary)
	if r.Class != Unknown || r.ResetsAt != 0 {
		t.Fatalf("unknown group recovery borrowed another group's expiry: %+v", r)
	}
	delete(antigravityTestBucket(summary, 2), "resetTime")
	r = antigravityTestParse(t, summary)
	if r.Class != Unknown || r.ResetsAt != 0 {
		t.Fatalf("unknown blocking resets authorized a switch: %+v", r)
	}
}

func TestAntigravityQuotaMissingOrChangedContractIsUnknown(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing_group":    func(s map[string]any) { s["groups"] = s["groups"].([]any)[:1] },
		"missing_fraction": func(s map[string]any) { delete(antigravityTestBucket(s, 2), "remainingFraction") },
		"null_fraction":    func(s map[string]any) { antigravityTestBucket(s, 2)["remainingFraction"] = nil },
		"amount_only": func(s map[string]any) {
			delete(antigravityTestBucket(s, 2), "remainingFraction")
			antigravityTestBucket(s, 2)["remainingAmount"] = "0"
		},
		"conflicting_oneof": func(s map[string]any) { antigravityTestBucket(s, 2)["remainingAmount"] = "0" },
		"negative_fraction": func(s map[string]any) { antigravityTestBucket(s, 2)["remainingFraction"] = -0.1 },
		"large_fraction":    func(s map[string]any) { antigravityTestBucket(s, 2)["remainingFraction"] = 1.1 },
		"string_fraction":   func(s map[string]any) { antigravityTestBucket(s, 2)["remainingFraction"] = "0" },
		"disabled":          func(s map[string]any) { antigravityTestBucket(s, 2)["disabled"] = true },
		"changed_id":        func(s map[string]any) { antigravityTestBucket(s, 2)["bucketId"] = "other-5h" },
		"changed_window":    func(s map[string]any) { antigravityTestBucket(s, 2)["window"] = "daily" },
		"duplicate":         func(s map[string]any) { antigravityTestBucket(s, 2)["bucketId"] = "gemini-5h" },
		"invalid_reset":     func(s map[string]any) { antigravityTestBucket(s, 2)["resetTime"] = "invalid" },
		"past_reset": func(s map[string]any) {
			antigravityTestBucket(s, 2)["resetTime"] = antigravityTestNow.Add(-time.Second).Format(time.RFC3339)
		},
		"reset_boundary": func(s map[string]any) {
			antigravityTestBucket(s, 2)["resetTime"] = antigravityTestNow.Format(time.RFC3339)
		},
		"same_second_reset": func(s map[string]any) {
			antigravityTestBucket(s, 2)["resetTime"] = antigravityTestNow.Add(500 * time.Millisecond).Format(time.RFC3339Nano)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := antigravityTestSummary([4]float64{})
			change(s)
			if r := antigravityTestParse(t, s); r.Class != Unknown {
				t.Fatalf("incomplete evidence authorized a quota verdict: %+v", r)
			}
		})
	}
	// Removing any of the four meaningful inputs must withdraw exhaustion.
	for missing := 0; missing < 4; missing++ {
		s := antigravityTestSummary([4]float64{})
		g := s["groups"].([]any)[missing/2].(map[string]any)
		bs := g["buckets"].([]any)
		g["buckets"] = append(bs[:missing%2], bs[missing%2+1:]...)
		if r := antigravityTestParse(t, s); r.Class != Unknown {
			t.Fatalf("removed bucket %d: %+v", missing, r)
		}
	}
}

func TestAntigravityQuotaEnvelopeAndCredits(t *testing.T) {
	s := antigravityTestSummary([4]float64{})
	s["buckets"] = []any{map[string]any{"bucketId": "paid-credits", "remainingAmount": "900"}}
	s["groups"] = append(s["groups"].([]any), map[string]any{"buckets": s["buckets"]})
	r := antigravityTestParse(t, map[string]any{"response": s})
	if r.Class != Exhausted || len(r.Buckets) != 4 {
		t.Fatalf("paid credits changed subscription exhaustion: %+v", r)
	}
	s["error"] = map[string]any{"code": 503}
	if r := antigravityTestParse(t, map[string]any{"response": s}); r.Class != Unknown || r.Authenticated {
		t.Fatalf("nested error was ignored: %+v", r)
	}
	for _, body := range []string{"null", "[]", "{}", "not json", `{"response":null}`, `{"error":"failed"}`} {
		r := ParseAntigravityQuota(200, []byte(body), 0)
		if r.Class != Unknown || r.Authenticated {
			t.Fatalf("invalid response %q: %+v", body, r)
		}
	}
}

func TestAntigravityQuotaHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  Class
	}{
		{401, Expired}, {403, Expired}, {429, Unknown}, {503, Unknown}, {500, Unknown}, {402, Unknown},
	} {
		r := ParseAntigravityQuota(tc.status, []byte(`{"error":"quota exhausted"}`), 90*time.Second)
		if r.Class != tc.class || r.Authenticated {
			t.Fatalf("status %d: %+v", tc.status, r)
		}
		if (tc.status == 429 || tc.status == 503) && r.RetryAfter != 90*time.Second {
			t.Fatalf("status %d lost Retry-After: %+v", tc.status, r)
		}
	}
}

func TestAntigravityHTTPContract(t *testing.T) {
	for _, project := range []string{"", "synthetic-project"} {
		calls := 0
		h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method != http.MethodPost || req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary" || req.Header.Get("Authorization") != "Bearer synthetic-token" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("User-Agent") != "antigravity/2.32.1" {
				t.Fatal("quota request contract mismatch")
			}
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["project"] != project {
				t.Fatalf("project body: %#v, %v", body, err)
			}
			if project == "" && len(body) != 0 {
				t.Fatalf("empty project was sent: %#v", body)
			}
			payload, _ := json.Marshal(antigravityTestSummary([4]float64{0, 0.5, 0.4, 0.6}))
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
		})}}
		r, err := h.Antigravity(context.Background(), "synthetic-token", project)
		if err != nil || r.Class != OK || calls != 1 || math.Abs(r.UsedPct-60) > 0.001 {
			t.Fatalf("quota request: %+v, %v, calls=%d", r, err, calls)
		}
	}
}

func TestAntigravityHTTPRefusesRedirect(t *testing.T) {
	calls := 0
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://unrelated.invalid/quota"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}}
	r, err := h.Antigravity(context.Background(), "synthetic-token", "")
	if err == nil || r.Class != Unknown || calls != 1 {
		t.Fatalf("redirect followed: %+v, err=%v, calls=%d", r, err, calls)
	}
}

func TestAntigravityHTTPRejectsMissingTokenAndOversize(t *testing.T) {
	calls := 0
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", (1<<20)+1)))}, nil
	})}}
	if _, err := h.Antigravity(context.Background(), "", ""); err == nil || calls != 0 {
		t.Fatal("empty credentials reached the transport")
	}
	if r, err := h.Antigravity(context.Background(), "synthetic-token", ""); err == nil || r.Class != Unknown || calls != 1 {
		t.Fatalf("oversized response accepted: %+v, %v", r, err)
	}
}
