package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/kimi"
)

func TestKimiPhoneCapturePersistsThroughVaultAndOverview(t *testing.T) {
	a, now := kimiApp(t)
	before := writeKimiLive(t, a, "phone-account", now.Add(time.Hour).Unix())
	a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return kimiResponse(req, 200, `{"user_id":"phone-user","nickname":"Kimi user","phone":{"country_code":"86","number":"176****0000"},"user_level_name":"Allegretto"}`), nil
	})}
	if _, _, err := a.Capture(adapter.Kimi); err != nil {
		t.Fatal(err)
	}
	saved, err := a.State.GetAccount("kimi", "phone-user")
	if err != nil || saved.Phone != "+86 176****0000" || saved.DisplayName != "Kimi user" || saved.Email != "" {
		t.Fatalf("stored identity: %+v %v", saved, err)
	}
	blob, err := a.loadBlob(adapter.Kimi, "phone-user")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := (kimi.Adapter{}).IdentityOf(blob)
	if err != nil || identity.Phone != saved.Phone || identity.StableID != "phone-user" {
		t.Fatal("vault identity lost phone")
	}
	for _, tool := range a.Overview().Tools {
		if tool.Tool == "kimi" && (len(tool.Accounts) != 1 || tool.Accounts[0].Phone != saved.Phone || tool.Accounts[0].DisplayName != saved.DisplayName || tool.Accounts[0].Email != "") {
			t.Fatal("overview lost phone")
		}
	}
	if _, _, err := a.keepAliveKimiLocked(context.Background(), blob, false, false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(a.UserHome, ".kimi-code", "credentials", "kimi-code.json"))
	if string(before) != string(after) {
		t.Fatal("identity enrichment rewrote live credentials")
	}
}
