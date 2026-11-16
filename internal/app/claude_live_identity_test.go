package app

import (
	"context"
	"net/http"
	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"testing"
	"time"
)

func TestClaudeRotatedLiveIdentityCannotSpendStaleParkedRefresh(t *testing.T) {
	for _, kind := range []string{"same", "different", "unknown", "changed"} {
		t.Run(kind, func(t *testing.T) {
			a := setupClaudeRecovery(t, "", true)
			blobs, _, err := a.Adapters[adapter.Claude].Capture(a.UserHome)
			if err != nil {
				t.Fatal(err)
			}
			b, err := claude.WriteIdentity(blobs[0], adapter.Identity{Tool: adapter.Claude, StableID: "verified-account"})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.saveBlob(b); err != nil {
				t.Fatal(err)
			}
			writeClaudeRecoveryLive(t, a, "live-new", "live-new-refresh", a.now().Add(time.Hour))
			exchanges := 0
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/oauth/profile" {
					switch kind {
					case "unknown":
						return claudeRecoveryResponse(req, 503, `{}`), nil
					case "different":
						return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"other-account"}}`), nil
					case "changed":
						writeClaudeRecoveryLive(t, a, "changed", "changed-refresh", a.now().Add(time.Hour))
					}
					return claudeRecoveryResponse(req, 200, `{"account":{"uuid":"verified-account"}}`), nil
				}
				exchanges++
				return claudeRecoveryResponse(req, 200, `{"access_token":"parked-new","refresh_token":"parked-refresh","expires_in":3600}`), nil
			})}
			_, token, err := a.keepAliveClaudeLocked(context.Background(), b, true, true)
			if kind == "different" {
				if err != nil || exchanges != 1 {
					t.Fatal("verified parked refresh failed", err)
				}
			} else if exchanges != 0 {
				t.Fatal("stale refresh was spent")
			}
			if kind == "same" {
				if err != nil || token != "live-new" {
					t.Fatal("native rotation not adopted", err)
				}
				saved, err := a.loadBlob(adapter.Claude, b.Identity.StableID)
				if err != nil {
					t.Fatal(err)
				}
				c, err := claude.CredsFromBlob(saved)
				if err != nil || c.AccessToken != "live-new" {
					t.Fatal("native rotation not persisted")
				}
			}
			if (kind == "unknown" || kind == "changed") && err == nil {
				t.Fatal("unverified live identity accepted")
			}
		})
	}
}
