package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCodexRefreshOK(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "auth.openai.com" || req.Method != http.MethodPost {
			t.Fatalf("url %s %s", req.Method, req.URL)
		}
		if strings.Contains(strings.ToLower(req.Header.Get("User-Agent")), "qswitch") {
			t.Fatalf("ua %s", req.Header.Get("User-Agent"))
		}
		if req.Header.Get("originator") != CodexOriginator {
			t.Fatalf("originator %s", req.Header.Get("originator"))
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"a","id_token":"i","refresh_token":"r2"}`)), Header: make(http.Header), Request: req}, nil
	})}}
	tok, err := h.CodexRefresh(context.Background(), "r1")
	if err != nil || tok.AccessToken != "a" || tok.RefreshToken != "r2" {
		t.Fatalf("%+v %v", tok, err)
	}
}

func TestGrokRefreshOK(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "auth.x.ai" || req.Method != http.MethodPost || req.URL.Path != "/oauth2/token" {
			t.Fatalf("url %s %s", req.Method, req.URL)
		}
		if ct := req.Header.Get("Content-Type"); !strings.Contains(ct, "application/x-www-form-urlencoded") {
			t.Fatalf("content-type %s", ct)
		}
		b, _ := io.ReadAll(req.Body)
		form := string(b)
		if !strings.Contains(form, "grant_type=refresh_token") || !strings.Contains(form, "client_id="+GrokOAuthClientID) {
			t.Fatalf("form %s", form)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"ak","refresh_token":"rt2","expires_in":21600}`)), Header: make(http.Header), Request: req}, nil
	})}}
	tok, err := h.GrokRefresh(context.Background(), "rt1", "")
	if err != nil || tok.AccessToken != "ak" || tok.RefreshToken != "rt2" || tok.ExpiresIn != 21600 {
		t.Fatalf("%+v %v", tok, err)
	}
}

func TestGrokRefreshPermanent(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)), Header: make(http.Header), Request: req}, nil
	})}}
	tok, err := h.GrokRefresh(context.Background(), "rt1", "cid")
	if err == nil || !tok.Permanent {
		t.Fatalf("want permanent err, got %+v %v", tok, err)
	}
}

func TestCodexRefreshPermanent(t *testing.T) {
	h := HTTP{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"refresh_token_reused"}`)), Header: make(http.Header), Request: req}, nil
	})}}
	tok, err := h.CodexRefresh(context.Background(), "r1")
	if err == nil || !tok.Permanent {
		t.Fatalf("want permanent err, got %+v %v", tok, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
