package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseDevinStatusDailyWeekly(t *testing.T) {
	body := []byte(`{"userStatus":{"email":"a@x.com","userId":"user-1","teamsTier":"TEAMS_TIER_DEVIN_PRO","planStatus":{
		"planInfo":{"planName":"Pro","hideDailyQuota":false},
		"dailyQuotaRemainingPercent":30,
		"weeklyQuotaRemainingPercent":50,
		"dailyQuotaResetAtUnix":"1789372800",
		"weeklyQuotaResetAtUnix":"1789891200"
	}}}`)
	r, ok := ParseDevinStatus(body)
	if !ok || r.Class != OK || r.Plan != "Pro" || r.UsedPct != 70 {
		t.Fatalf("%+v ok=%v", r, ok)
	}
	if len(r.Buckets) != 2 || r.Buckets[0].ID != "daily" || r.Buckets[0].UsedPct != 70 || r.Buckets[1].ID != "weekly" || r.Buckets[1].UsedPct != 50 {
		t.Fatalf("buckets %+v", r.Buckets)
	}
	if r.Buckets[0].ResetsAt != 1789372800 || r.Buckets[1].ResetsAt != 1789891200 {
		t.Fatalf("resets %+v", r.Buckets)
	}
}

func TestParseDevinStatusOmittedDailyIsExhausted(t *testing.T) {
	body := []byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"},"weeklyQuotaRemainingPercent":50,"weeklyQuotaResetAtUnix":1789891200}}}`)
	r, ok := ParseDevinStatus(body)
	if !ok || r.Class != Exhausted {
		t.Fatalf("%+v ok=%v", r, ok)
	}
	if len(r.Buckets) != 2 || r.Buckets[0].ID != "daily" || r.Buckets[0].UsedPct != 100 || r.Buckets[1].UsedPct != 50 {
		t.Fatalf("buckets %+v", r.Buckets)
	}
}

func TestParseDevinStatusHideDaily(t *testing.T) {
	body := []byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Max","hideDailyQuota":true},"weeklyQuotaRemainingPercent":80}}}`)
	r, ok := ParseDevinStatus(body)
	if !ok || r.Plan != "Max" || len(r.Buckets) != 1 || r.Buckets[0].ID != "weekly" || r.Buckets[0].UsedPct != 20 || r.Class != OK {
		t.Fatalf("%+v ok=%v", r, ok)
	}
}

func TestDevinIdentity(t *testing.T) {
	uid, email, plan := DevinIdentity([]byte(`{"userStatus":{"userId":"user-9","email":"b@x.com","planStatus":{"planInfo":{"planName":"Pro"}}}}`))
	if uid != "user-9" || email != "b@x.com" || plan != "Pro" {
		t.Fatalf("%s %s %s", uid, email, plan)
	}
}

func TestDevinHTTP(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "GetUserStatus") {
			t.Fatalf("path %s", req.URL.Path)
		}
		body := `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"},"dailyQuotaRemainingPercent":90,"weeklyQuotaRemainingPercent":70}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}}
	res, err := h.Devin(context.Background(), "devin-session-token$abc", "https://server.codeium.com")
	if err != nil || res.Plan != "Pro" || res.Class != OK || len(res.Buckets) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}
