package app

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/devin"
)

func TestDevinIdentityFirstFailureDoesNotPersistTemporaryAccount(t *testing.T) {
	for _, failure := range []string{"http failure", "transport failure", "missing user ID"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", "")
			a, _ := setup(t)
			writeDevinLive(t, a.UserHome, "fixture-token")
			local, _, err := a.Adapters[adapter.Devin].Capture(a.UserHome)
			if err != nil || len(local) != 1 {
				t.Fatal("local fixture capture failed")
			}
			recovered := false
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "server.codeium.com" {
					t.Fatal("unexpected identity endpoint")
				}
				status := http.StatusOK
				body := `{"userStatus":{"userId":"verified-fixture","email":"fixture@example.test"}}`
				if !recovered {
					switch failure {
					case "http failure":
						status = http.StatusServiceUnavailable
					case "transport failure":
						return nil, errors.New("simulated temporary connection failure")
					case "missing user ID":
						body = `{"userStatus":{"planStatus":{"planInfo":{"planName":"Max"}}}}`
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			if _, _, err := a.Capture(adapter.Devin); err == nil {
				t.Fatal("unverified first capture must return a retryable error")
			}
			accounts, err := a.State.ListAccounts("devin")
			if err != nil || len(accounts) != 0 {
				t.Fatal("failed identity verification persisted an account")
			}
			if _, err := a.Vault.Get("devin", local[0].Identity.StableID); err == nil {
				t.Fatal("failed identity verification persisted a temporary vault entry")
			}
			p, _ := a.State.GetPointer("devin")
			if p.StableID != "" {
				t.Fatal("failed identity verification changed the live pointer")
			}
			recovered = true
			blobs, _, err := a.Capture(adapter.Devin)
			if err != nil || len(blobs) != 1 || blobs[0].Identity.StableID != "verified-fixture" {
				t.Fatalf("successful retry did not use the official identity: %v", err)
			}
			accounts, err = a.State.ListAccounts("devin")
			if err != nil || len(accounts) != 1 || accounts[0].StableID != "verified-fixture" {
				t.Fatal("successful retry did not persist one verified account")
			}
		})
	}
}

func TestDevinIdentityOfflineCaptureReusesVerifiedAccount(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	a, _ := setup(t)
	writeDevinLive(t, a.UserHome, "fixture-token")
	offline := false
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if offline {
			return nil, errors.New("simulated temporary connection failure")
		}
		body := `{"userStatus":{"userId":"verified-fixture","email":"fixture@example.test","planStatus":{"planInfo":{"planName":"Max"}}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	first, _, err := a.Capture(adapter.Devin)
	if err != nil || len(first) != 1 {
		t.Fatalf("initial capture failed: %v", err)
	}
	offline = true
	again, _, err := a.Capture(adapter.Devin)
	if err != nil || len(again) != 1 || again[0].Identity != first[0].Identity {
		t.Fatalf("offline capture did not retain verified identity: %v", err)
	}
	accounts, err := a.State.ListAccounts("devin")
	if err != nil || len(accounts) != 1 || accounts[0].StableID != "verified-fixture" {
		t.Fatal("offline capture duplicated the account")
	}
	p, err := a.State.GetPointer("devin")
	if err != nil || p.StableID != "verified-fixture" {
		t.Fatal("offline capture changed the verified live pointer")
	}
}

func TestDevinIdentityCannotReuseUnverifiedOrDifferentCredentials(t *testing.T) {
	for _, source := range []string{"temporary hash", "different API key"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", "")
			a, _ := setup(t)
			writeDevinLive(t, a.UserHome, "fixture-token")
			local, _, err := a.Adapters[adapter.Devin].Capture(a.UserHome)
			if err != nil || len(local) != 1 {
				t.Fatal("local fixture capture failed")
			}
			b := local[0]
			if source == "different API key" {
				id := b.Identity
				id.StableID = "verified-other"
				b, err = devin.WriteIdentity(b, id)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := a.saveBlob(b); err != nil {
				t.Fatal(err)
			}
			if source == "different API key" {
				writeDevinLive(t, a.UserHome, "new-unverified-fixture-token")
			}
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("simulated temporary connection failure")
			})}
			if _, _, err := a.Capture(adapter.Devin); err == nil {
				t.Fatal("capture reused unverified or unrelated credentials")
			}
			accounts, err := a.State.ListAccounts("devin")
			if err != nil || len(accounts) != 1 {
				t.Fatal("failed capture changed saved accounts")
			}
		})
	}
}
