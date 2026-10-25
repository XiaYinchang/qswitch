package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
)

func findClaudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
		filepath.Join(home, ".local", "bin", "claude"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// LoginClaude enrolls a new Claude account without touching the live
// credentials. It launches the official CLI with an isolated
// CLAUDE_CONFIG_DIR so the OAuth flow writes to a temporary directory;
// the resulting credentials are captured into the vault and the temp
// dir is removed. The live ~/.claude/.credentials.json is never read
// or written by this flow.
func (a *App) LoginClaude(ctx context.Context, announce func(instructions string)) (adapter.Identity, error) {
	bin := findClaudeBin()
	if bin == "" {
		return adapter.Identity{}, errors.New("claude: CLI not found in PATH")
	}
	if secutil.InTest() {
		return adapter.Identity{}, errors.New("claude: login requires official CLI in tests")
	}

	dir, err := os.MkdirTemp("", "qswitch-claude-login-")
	if err != nil {
		return adapter.Identity{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return adapter.Identity{}, err
	}

	if announce != nil {
		announce(fmt.Sprintf("在弹出的浏览器里登录新 Claude 账号。当前登录的账号不会被登出，也不会被覆盖。\n配置目录: %s", dir))
	}

	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = isolatedClaudeEnv(os.Environ(), dir)
	if err := cmd.Run(); err != nil {
		return adapter.Identity{}, fmt.Errorf("claude login: %w", err)
	}

	credPath := filepath.Join(dir, ".credentials.json")
	raw, err := os.ReadFile(credPath)
	if err != nil {
		return adapter.Identity{}, fmt.Errorf("claude login produced no credentials: %w", err)
	}
	if _, err := claude.ParseCreds(raw); err != nil {
		return adapter.Identity{}, err
	}

	// The CLI may also write account metadata; read it for the identity hint.
	var acct claude.OAuthAccount
	if b, err := os.ReadFile(filepath.Join(dir, "..", ".claude.json")); err == nil {
		var doc struct {
			OAuthAccount claude.OAuthAccount `json:"oauthAccount"`
		}
		if json.Unmarshal(b, &doc) == nil {
			acct = doc.OAuthAccount
		}
	}

	blob, err := claudeBlobFromRaw(raw, acct, a.now())
	if err != nil {
		return adapter.Identity{}, err
	}
	return a.enrollClaudeBlob(ctx, blob)
}

func isolatedClaudeEnv(parent []string, home string) []string {
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CLAUDE_CONFIG_DIR="+home)
}

func claudeBlobFromRaw(raw []byte, acct claude.OAuthAccount, now time.Time) (adapter.Blob, error) {
	id := adapter.Identity{Tool: adapter.Claude}
	if acct.AccountUUID != "" {
		id.StableID = acct.AccountUUID
		id.Email = acct.Email
	}
	var acctJSON string
	if acct.AccountUUID != "" {
		if b, err := json.Marshal(acct); err == nil {
			acctJSON = string(b)
		}
	}
	p := map[string]any{
		"kind":             "claude.oauth.v1",
		"identity":         id,
		"credentials_json": string(raw),
	}
	if acctJSON != "" {
		p["oauth_account_json"] = acctJSON
	}
	b, err := json.Marshal(p)
	if err != nil {
		return adapter.Blob{}, err
	}
	return adapter.Blob{
		Tool:       adapter.Claude,
		Identity:   id,
		Payload:    b,
		CapturedAt: now,
	}, nil
}

func (a *App) enrollClaudeBlob(ctx context.Context, blob adapter.Blob) (adapter.Identity, error) {
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "claude.lock"))
	if err != nil {
		return adapter.Identity{}, err
	}
	// Confirm identity via the official profile endpoint before persisting.
	nb, _, err := a.keepAliveClaudeLocked(ctx, blob, true, true)
	if err != nil {
		// keepAlive may fail when the access token is fresh; try profile directly.
		nb = blob
	}
	c, err := claude.CredsFromBlob(nb)
	if err != nil {
		lock.Close()
		return adapter.Identity{}, err
	}
	raw, status, err := a.HTTP.ClaudeProfile(ctx, c.AccessToken)
	if err != nil || status != 200 {
		lock.Close()
		return adapter.Identity{}, errors.New("claude: unable to verify account identity after login")
	}
	uuid, _, _, _ := quota.ClaudeIdentity(raw)
	if uuid == "" {
		lock.Close()
		return adapter.Identity{}, errors.New("claude: profile missing account uuid")
	}
	ident := claude.EnrichIdentity(nb.Identity, raw)
	nb, err = claude.WriteIdentity(nb, ident)
	if err != nil {
		lock.Close()
		return adapter.Identity{}, err
	}
	err = a.saveBlob(nb)
	closeErr := lock.Close()
	if err != nil {
		return adapter.Identity{}, err
	}
	if closeErr != nil {
		return adapter.Identity{}, closeErr
	}
	_ = a.probeAccount(ctx, adapter.Claude, nb.Identity.StableID, false)
	a.Notify.Send("qswitch", fmt.Sprintf("收录新账号 claude %s", ident.Email))
	return nb.Identity, nil
}
