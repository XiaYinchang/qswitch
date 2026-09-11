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

func TestGrokDeviceStartAndPoll(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(strings.ToLower(req.Header.Get("User-Agent")), "qswitch") {
			t.Fatalf("ua %s", req.Header.Get("User-Agent"))
		}
		switch req.URL.Path {
		case "/oauth2/device/code":
			b, _ := io.ReadAll(req.Body)
			form := string(b)
			if !strings.Contains(form, "client_id="+GrokOAuthClientID) || !strings.Contains(form, "grok-cli") {
				t.Fatalf("form %s", form)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"device_code":"dc","user_code":"AB-CD","verification_uri":"https://accounts.x.ai/oauth2/device","expires_in":1800,"interval":5}`)), Header: make(http.Header), Request: req}, nil
		case "/oauth2/token":
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"authorization_pending"}`)), Header: make(http.Header), Request: req}, nil
		default:
			t.Fatalf("unexpected %s", req.URL)
			return nil, nil
		}
	})}}
	st, err := h.GrokDeviceStart(context.Background())
	if err != nil || st.UserCode != "AB-CD" || st.DeviceCode != "dc" {
		t.Fatalf("%+v %v", st, err)
	}
	_, err = h.GrokDevicePoll(context.Background(), st)
	if !errors.Is(err, ErrDevicePending) {
		t.Fatalf("want pending, got %v", err)
	}
}

func TestGrokDevicePollOK(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		form := string(b)
		if !strings.Contains(form, "grant_type=") || !strings.Contains(form, "device_code") {
			t.Fatalf("form %s", form)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"ak","refresh_token":"rt","expires_in":21600}`)), Header: make(http.Header), Request: req}, nil
	})}}
	tok, err := h.GrokDevicePoll(context.Background(), GrokDeviceStart{DeviceCode: "dc"})
	if err != nil || tok.AccessToken != "ak" || tok.RefreshToken != "rt" {
		t.Fatalf("%+v %v", tok, err)
	}
}
