package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/state"
)

func TestClaudeIsolatedEnrollment(t *testing.T) {
	for _, outcome := range []string{"success", "profile unavailable", "missing identity", "command failure", "missing credentials", "invalid credentials", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			a, _ := setup(t)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			liveDir := filepath.Join(a.UserHome, ".claude")
			if err := os.MkdirAll(liveDir, 0o700); err != nil {
				t.Fatal(err)
			}
			live := []byte(`{"claudeAiOauth":{"accessToken":"live-access","refreshToken":"live-refresh","expiresAt":4102444800000}}`)
			livePath := filepath.Join(liveDir, ".credentials.json")
			if err := os.WriteFile(livePath, live, 0o600); err != nil {
				t.Fatal(err)
			}
			pointer := state.Pointer{Tool: "claude", StableID: "live-account", Gen: 7}
			if err := a.State.SetPointer(pointer); err != nil {
				t.Fatal(err)
			}
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			script := `#!/bin/sh
set -eu
[ "$CLAUDE_CONFIG_DIR" = "$CLAUDE_SECURESTORAGE_CONFIG_DIR" ]
[ -z "${ANTHROPIC_API_KEY+x}" ]
[ -z "${CLAUDE_CODE_OAUTH_TOKEN+x}" ]
`
			switch outcome {
			case "command failure":
				script += "exit 1\n"
			case "missing credentials":
				script += "exit 0\n"
			case "invalid credentials":
				script += `printf '{}' > "$CLAUDE_SECURESTORAGE_CONFIG_DIR/.credentials.json"` + "\n"
			default:
				script += `printf '%s' '{"claudeAiOauth":{"accessToken":"new-access","refreshToken":"new-refresh","expiresAt":4102444800000}}' > "$CLAUDE_SECURESTORAGE_CONFIG_DIR/.credentials.json"` + "\n"
			}
			bin := filepath.Join(t.TempDir(), "claude")
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ANTHROPIC_API_KEY", "must-not-inherit")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-inherit")
			t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", liveDir)
			a.HTTP.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status := http.StatusOK
				body := `{"five_hour":{"utilization":10},"seven_day":{"utilization":20}}`
				switch req.URL.Path {
				case "/api/oauth/profile":
					if req.Header.Get("Authorization") != "Bearer new-access" {
						t.Fatal("enrollment used live credentials")
					}
					body = `{"account":{"uuid":"new-account","email":"new@example.test"},"organization":{"organization_type":"claude_max"}}`
					if outcome == "profile unavailable" {
						status = http.StatusServiceUnavailable
					}
					if outcome == "missing identity" {
						body = `{}`
					}
				case "/api/oauth/usage":
				default:
					t.Fatalf("unexpected request (enrollment must not refresh tokens): %s", req.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if outcome == "cancelled" {
				cancel()
			}
			id, err := a.loginClaudeOfficial(ctx, bin, nil)
			if outcome == "success" {
				if err != nil || id.StableID != "new-account" || id.Email != "new@example.test" {
					t.Fatalf("enrollment: %+v, %v", id, err)
				}
				blob, err := a.loadBlob(adapter.Claude, id.StableID)
				if err != nil {
					t.Fatal(err)
				}
				creds, err := claude.CredsFromBlob(blob)
				if err != nil || creds.AccessToken != "new-access" || creds.RefreshToken != "new-refresh" {
					t.Fatal("vault does not contain the isolated login")
				}
				var payload struct {
					Account string `json:"oauth_account_json"`
				}
				if err := json.Unmarshal(blob.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				var account claude.OAuthAccount
				if err := json.Unmarshal([]byte(payload.Account), &account); err != nil || account.AccountUUID != id.StableID || account.Email != id.Email {
					t.Fatal("enrollment did not retain verified switch metadata")
				}
			} else {
				if err == nil {
					t.Fatal("unsuccessful login reported success")
				}
				accounts, err := a.State.ListAccounts("claude")
				if err != nil || len(accounts) != 0 {
					t.Fatal("unsuccessful login persisted an account")
				}
				if _, err := a.Vault.Get("claude", "new-account"); err == nil {
					t.Fatal("unsuccessful login persisted credentials")
				}
			}
			got, err := os.ReadFile(livePath)
			if err != nil || !bytes.Equal(got, live) {
				t.Fatal("login changed live credentials")
			}
			p, err := a.State.GetPointer("claude")
			if err != nil || p.StableID != pointer.StableID || p.Gen != pointer.Gen {
				t.Fatal("login changed the current account pointer")
			}
			dirs, err := filepath.Glob(filepath.Join(a.DataDir, "claude-enrollments", "session-*"))
			if err != nil || len(dirs) != 0 {
				t.Fatal("login left temporary credentials behind")
			}
		})
	}
}
