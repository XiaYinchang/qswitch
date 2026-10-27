package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
)

func writeCreds(t *testing.T, home string, oauth map[string]any) string {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"claudeAiOauth": oauth})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func validOAuth() map[string]any {
	return map[string]any{
		"accessToken":  "sk-ant-oat-access",
		"refreshToken": "sk-ant-ort-refresh",
		"expiresAt":    time.Now().Add(time.Hour).UnixMilli(),
		"scopes":       []string{"user:inference"},
	}
}

func TestParseCreds(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"claudeAiOauth": validOAuth()})
	c, err := ParseCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "sk-ant-oat-access" || c.RefreshToken != "sk-ant-ort-refresh" {
		t.Fatalf("unexpected creds %+v", c)
	}
	if c.NeedsRefresh(time.Now()) {
		t.Fatal("fresh token should not need refresh")
	}
	expired := validOAuth()
	expired["expiresAt"] = time.Now().Add(-time.Minute).UnixMilli()
	raw, _ = json.Marshal(map[string]any{"claudeAiOauth": expired})
	c, err = ParseCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !c.NeedsRefresh(time.Now()) {
		t.Fatal("expired token should need refresh")
	}
	for _, bad := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"claudeAiOauth":{}}`),
		[]byte(`{"claudeAiOauth":{"accessToken":"a"}}`),
		[]byte(`not json`),
	} {
		if _, err := ParseCreds(bad); err == nil {
			t.Fatalf("expected error for %s", bad)
		}
	}
}

func TestCaptureRestoreRoundtrip(t *testing.T) {
	home := t.TempDir()
	writeCreds(t, home, validOAuth())
	meta := `{"userID":"u","oauthAccount":{"accountUuid":"uuid-1","emailAddress":"a@example.com"}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	a := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	blobs, warn, err := a.Capture(home)
	if err != nil || len(blobs) != 1 {
		t.Fatalf("capture: %v %v", blobs, err)
	}
	if len(warn) != 0 {
		t.Fatalf("warnings: %v", warn)
	}
	id, err := a.IdentityOf(blobs[0])
	if err != nil {
		t.Fatal(err)
	}
	if id.StableID != "uuid-1" || id.Email != "a@example.com" {
		t.Fatalf("identity not seeded from ~/.claude.json: %+v", id)
	}

	// Switch away, then restore the snapshot.
	other := validOAuth()
	other["accessToken"] = "sk-ant-oat-other"
	writeCreds(t, home, other)
	if err := a.Restore(home, blobs[0], adapter.RestoreOpts{}); err != nil {
		t.Fatal(err)
	}
	c, err := ReadLiveCreds(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "sk-ant-oat-access" {
		t.Fatalf("restore wrote %q", c.AccessToken)
	}
}

func TestRestoreBlockedByRunningCLI(t *testing.T) {
	home := t.TempDir()
	writeCreds(t, home, validOAuth())
	a := Adapter{List: func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 42, Command: "/home/u/.local/bin/claude chat"}}, nil
	}}
	blobs, _, err := a.Capture(home)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Restore(home, blobs[0], adapter.RestoreOpts{})
	var busyErr *adapter.BusyError
	if !errors.As(err, &busyErr) {
		t.Fatalf("want BusyError, got %v", err)
	}
	// Restore never runs with a live CLI, so credentials stay untouched.
	c, _ := ReadLiveCreds(home)
	if c.AccessToken != "sk-ant-oat-access" {
		t.Fatal("credentials changed despite busy CLI")
	}
}

func TestIsClaudeCLI(t *testing.T) {
	yes := []string{
		"claude",
		"/home/u/.local/bin/claude --resume",
		"node /home/u/.npm-global/lib/node_modules/@anthropic-ai/claude-code/cli.js",
		"bun --smol /opt/claude",
	}
	no := []string{
		"",
		"vim",
		"/usr/bin/claude-manage",
		"node server.js",
		"grep claude",
	}
	for _, c := range yes {
		if !isClaudeCLI(c) {
			t.Errorf("want claude cli: %q", c)
		}
	}
	for _, c := range no {
		if isClaudeCLI(c) {
			t.Errorf("not a claude cli: %q", c)
		}
	}
}

func TestApplyRefresh(t *testing.T) {
	home := t.TempDir()
	writeCreds(t, home, validOAuth())
	a := Adapter{List: func() ([]adapter.Proc, error) { return nil, nil }}
	blobs, _, err := a.Capture(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	updated, err := ApplyRefresh(blobs[0], quota.ClaudeTokens{
		AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := CredsFromBlob(updated)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "new-access" || c.RefreshToken != "new-refresh" {
		t.Fatalf("refresh not applied: %+v", c)
	}
	if want := (now.Unix() + 3600) * 1000; c.ExpiresAt != want {
		t.Fatalf("expiresAt=%d want %d", c.ExpiresAt, want)
	}
	// Scopes survive rotation.
	if len(c.Scopes) != 1 || c.Scopes[0] != "user:inference" {
		t.Fatalf("scopes lost: %+v", c.Scopes)
	}
	if _, err := ApplyRefresh(blobs[0], quota.ClaudeTokens{AccessToken: "", RefreshToken: "r", ExpiresIn: 1}, now); err == nil {
		t.Fatal("empty access token must be rejected")
	}
}

func TestMergeOAuthAccountPreservesOtherKeys(t *testing.T) {
	meta := []byte(`{"userID":"u","oauthAccount":{"accountUuid":"old"},"numStartups":7}`)
	out, err := mergeOAuthAccount(meta, `{"accountUuid":"new","emailAddress":"b@example.com"}`)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["numStartups"]) != "7" {
		t.Fatalf("unrelated key lost: %s", out)
	}
	var acct OAuthAccount
	if err := json.Unmarshal(doc["oauthAccount"], &acct); err != nil {
		t.Fatal(err)
	}
	if acct.AccountUUID != "new" || acct.Email != "b@example.com" {
		t.Fatalf("oauthAccount not replaced: %+v", acct)
	}
}
