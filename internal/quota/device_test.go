package quota

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCodexDeviceStartAndPoll(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if ua := req.Header.Get("User-Agent"); strings.Contains(strings.ToLower(ua), "qswitch") {
			t.Fatalf("ua %s", ua)
		}
		if req.Header.Get("originator") != "" && req.URL.Path != "/oauth/token" {
			t.Fatalf("device auth should not send originator")
		}
		switch {
		case req.URL.Path == "/api/accounts/deviceauth/usercode":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"device_auth_id":"did","user_code":"ABCD","expires_in":900,"interval":3}`)), Header: make(http.Header), Request: req}, nil
		case req.URL.Path == "/api/accounts/deviceauth/token":
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: req}, nil
		default:
			t.Fatalf("unexpected %s", req.URL)
			return nil, nil
		}
	})}}
	st, err := h.CodexDeviceStart(context.Background())
	if err != nil || st.UserCode != "ABCD" || st.DeviceAuthID != "did" {
		t.Fatalf("%+v %v", st, err)
	}
	_, _, err = h.CodexDevicePoll(context.Background(), st)
	if !errors.Is(err, ErrDevicePending) {
		t.Fatalf("want pending, got %v", err)
	}
}

func TestCodexDevicePollOKAndExchange(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/accounts/deviceauth/token":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"authorization_code":"ac","code_verifier":"cv"}`)), Header: make(http.Header), Request: req}, nil
		case "/oauth/token":
			if ct := req.Header.Get("Content-Type"); !strings.Contains(ct, "application/x-www-form-urlencoded") {
				t.Fatalf("content-type %s", ct)
			}
			b, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(b), "grant_type=authorization_code") {
				t.Fatalf("form %s", b)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"a","id_token":"i","refresh_token":"r"}`)), Header: make(http.Header), Request: req}, nil
		default:
			t.Fatalf("unexpected %s", req.URL)
			return nil, nil
		}
	})}}
	code, ver, err := h.CodexDevicePoll(context.Background(), CodexDeviceStart{DeviceAuthID: "d", UserCode: "u"})
	if err != nil || code != "ac" || ver != "cv" {
		t.Fatalf("%s %s %v", code, ver, err)
	}
	tok, err := h.CodexExchangeCode(context.Background(), code, ver)
	if err != nil || tok.AccessToken != "a" || tok.RefreshToken != "r" {
		t.Fatalf("%+v %v", tok, err)
	}
}

func TestCodexDeviceSlowDown(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"slow_down"}`)), Header: make(http.Header), Request: req}, nil
	})}}
	_, _, err := h.CodexDevicePoll(context.Background(), CodexDeviceStart{DeviceAuthID: "d", UserCode: "u"})
	if !errors.Is(err, ErrDeviceSlowDown) {
		t.Fatalf("got %v", err)
	}
}
