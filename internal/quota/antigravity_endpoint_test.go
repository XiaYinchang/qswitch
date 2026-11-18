package quota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAntigravityHTTPUsesOnlyValidatedNativeOrigins(t *testing.T) {
	for _, endpoint := range []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com", "https://daily-cloudcode-pa.googleapis.com/"} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != strings.TrimSuffix(endpoint, "/")+"/v1internal:retrieveUserQuotaSummary" {
					t.Fatal("native origin or fixed method path changed")
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-native-token" {
					t.Fatal("missing synthetic authorization")
				}
				body, _ := json.Marshal(antigravityTestSummary([4]float64{1, 1, .008, .501}))
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
			})}}
			result, err := h.AntigravityAt(context.Background(), "synthetic-native-token", "", endpoint)
			if err != nil || calls != 1 || result.Class != OK || len(result.Buckets) != 4 || result.Buckets[2].UsedPct < 99 || result.UsedPct != 0 {
				t.Fatalf("quota result=%+v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestAntigravityHTTPRejectsUnsafeOriginsBeforeTransport(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe endpoint reached transport")
		return nil, nil
	})}}
	for _, endpoint := range []string{
		"", "http://daily-cloudcode-pa.googleapis.com", "https://daily-cloudcode-pa.googleapis.com:443", "https://daily-cloudcode-pa.googleapis.com:8443", "https://daily-cloudcode-pa.googleapis.com.evil.invalid", "https://evil.invalid", "https://user:password@daily-cloudcode-pa.googleapis.com", "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "https://daily-cloudcode-pa.googleapis.com/?token=synthetic", "https://daily-cloudcode-pa.googleapis.com#fragment", "https://daily-cloudcode-pa.googleapis.com//", "https://daily-cloudcode-pa.googleapis.com\\evil", "https://daily-cloudcode-pa.googleapis.com\n",
	} {
		result, err := h.AntigravityAt(context.Background(), "synthetic-native-token", "", endpoint)
		if err == nil || result.Class != Unknown || strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "password") {
			t.Fatalf("unsafe origin was accepted or echoed: result=%+v err=%v", result, err)
		}
	}
}

func TestAntigravityHTTPOverrideStillRefusesRedirects(t *testing.T) {
	calls := 0
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {AntigravityQuotaURL}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}}
	result, err := h.AntigravityAt(context.Background(), "synthetic-native-token", "", "https://cloudcode-pa.googleapis.com")
	if err == nil || result.Class != Unknown || calls != 1 {
		t.Fatalf("override followed redirect: %+v err=%v calls=%d", result, err, calls)
	}
}
