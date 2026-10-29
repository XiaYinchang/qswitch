package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/claude"
	"qswitch/internal/livefile"
	"qswitch/internal/quota"
)

func findClaudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/usr/local/bin/claude",
		filepath.Join(home, ".local", "bin", "claude"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// LoginClaude runs the official OAuth command on the caller's terminal, then
// enrolls its isolated credentials without changing the live account.
func (a *App) LoginClaude(ctx context.Context, announce func(instructions string)) (adapter.Identity, error) {
	if runtime.GOOS != "linux" {
		return adapter.Identity{}, errors.New("claude: isolated login is supported on Linux; macOS Keychain is not supported")
	}
	bin := findClaudeBin()
	if bin == "" {
		return adapter.Identity{}, errors.New("claude: official CLI not found; install Claude Code first")
	}
	return a.loginClaudeOfficial(ctx, bin, announce)
}

func (a *App) loginClaudeOfficial(ctx context.Context, bin string, announce func(string)) (adapter.Identity, error) {
	// Linux parent-death signals follow the thread that creates each child.
	// Keep that thread alive until both children have been waited for.
	releaseThread := ownClaudeLoginThread()
	defer releaseThread()
	session, err := newClaudeLoginSession(a.DataDir)
	if err != nil {
		return adapter.Identity{}, err
	}
	defer session.Close()
	dir := session.dir
	if announce != nil {
		announce("在当前终端完成 Claude 官方授权；授权成功后自动收录，当前登录保持不变。")
	}
	cmd := exec.CommandContext(ctx, bin, "auth", "login", "--claudeai")
	cmd.Dir = dir
	cmd.Env = isolatedClaudeEnv(os.Environ(), dir)
	protectClaudeLoginChild(cmd)
	cmd.ExtraFiles = []*os.File{session.lease}
	// Claude 2.1.311 switches a TTY stdin to raw/no-echo mode, but auth
	// login's line reader does not render typed characters. Keep the terminal
	// in canonical mode and let cat forward lines to a non-TTY stdin. A child
	// process makes the blocked terminal read cancellable even when OAuth
	// completes through the browser callback without any pasted input.
	input, output, err := os.Pipe()
	if err != nil {
		return adapter.Identity{}, err
	}
	defer input.Close()
	defer output.Close()
	inputCtx, stopInput := context.WithCancel(ctx)
	defer stopInput()
	forward := exec.CommandContext(inputCtx, "cat")
	forward.Stdin = os.Stdin
	forward.Stdout = output
	forward.Stderr = os.Stderr
	protectClaudeLoginChild(forward)
	forward.ExtraFiles = []*os.File{session.lease}
	if err := forward.Start(); err != nil {
		return adapter.Identity{}, fmt.Errorf("claude: start terminal input forwarding: %w", err)
	}
	_ = output.Close()
	defer func() {
		stopInput()
		_ = forward.Wait()
	}()
	cmd.Stdin = input
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return adapter.Identity{}, fmt.Errorf("claude auth login: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return adapter.Identity{}, fmt.Errorf("claude: login produced no isolated credentials: %w", err)
	}
	if _, err := claude.ParseCreds(raw); err != nil {
		return adapter.Identity{}, err
	}
	blob, err := claudeBlobFromRaw(raw, claude.OAuthAccount{}, a.now())
	if err != nil {
		return adapter.Identity{}, err
	}
	return a.enrollClaudeBlob(ctx, blob)
}

func isolatedClaudeEnv(parent []string, dir string) []string {
	out := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CLAUDE_CONFIG_DIR="+dir, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+dir)
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
	defer lock.Close()
	// A new login has fresh tokens. Verify identity before any persistence;
	// do not spend its one-use refresh token during enrollment.
	c, err := claude.CredsFromBlob(blob)
	if err != nil {
		return adapter.Identity{}, err
	}
	raw, status, err := a.HTTP.ClaudeProfile(ctx, c.AccessToken)
	if err != nil || status != 200 {
		return adapter.Identity{}, errors.New("claude: unable to verify account identity after login")
	}
	uuid, _, _, _ := quota.ClaudeIdentity(raw)
	if uuid == "" {
		return adapter.Identity{}, errors.New("claude: profile missing account uuid")
	}
	ident := claude.EnrichIdentity(adapter.Identity{Tool: adapter.Claude}, raw)
	var payload struct {
		Credentials string `json:"credentials_json"`
	}
	if err := json.Unmarshal(blob.Payload, &payload); err != nil {
		return adapter.Identity{}, err
	}
	nb, err := claudeBlobFromRaw([]byte(payload.Credentials), claude.OAuthAccount{AccountUUID: ident.StableID, Email: ident.Email}, blob.CapturedAt)
	if err != nil {
		return adapter.Identity{}, err
	}
	nb, err = claude.WriteIdentity(nb, ident)
	if err != nil {
		return adapter.Identity{}, err
	}
	if err := a.saveBlob(nb); err != nil {
		return adapter.Identity{}, err
	}
	if err := lock.Close(); err != nil {
		return adapter.Identity{}, err
	}
	_ = a.probeAccount(ctx, adapter.Claude, nb.Identity.StableID, false)
	a.Notify.Send("qswitch", fmt.Sprintf("收录新账号 claude %s", ident.Email))
	return nb.Identity, nil
}
