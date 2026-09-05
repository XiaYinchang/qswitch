package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/codex"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
)

func findCodexBin() string {
	if p, err := exec.LookPath("codex"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
		filepath.Join(home, ".local", "bin", "codex"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func (a *App) LoginCodex(ctx context.Context, announce func(userCode, verifyURL string)) (adapter.Identity, error) {
	if !secutil.InTest() {
		if bin := findCodexBin(); bin != "" {
			return a.loginCodexOfficial(ctx, bin)
		}
	}
	return a.loginCodexHTTP(ctx, announce)
}

func (a *App) loginCodexOfficial(ctx context.Context, bin string) (adapter.Identity, error) {
	dir, err := os.MkdirTemp("", "qswitch-codex-login-")
	if err != nil {
		return adapter.Identity{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return adapter.Identity{}, err
	}
	cfg := []byte("cli_auth_credentials_store = \"file\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), cfg, 0o600); err != nil {
		return adapter.Identity{}, err
	}

	cmd := exec.CommandContext(ctx, bin, "login", "--device-auth", "-c", "cli_auth_credentials_store=file")
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = isolatedCodexEnv(os.Environ(), dir)
	if err := cmd.Run(); err != nil {
		return adapter.Identity{}, fmt.Errorf("codex login --device-auth: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return adapter.Identity{}, fmt.Errorf("codex login produced no auth.json: %w", err)
	}
	return a.enrollCodexAuthJSON(ctx, raw)
}

func isolatedCodexEnv(parent []string, home string) []string {
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CODEX_HOME="+home)
}

func (a *App) loginCodexHTTP(ctx context.Context, announce func(userCode, verifyURL string)) (adapter.Identity, error) {
	if a.HTTP.Client == nil && secutil.InTest() {
		return adapter.Identity{}, errors.New("codex login: no http client")
	}
	st, err := a.HTTP.CodexDeviceStart(ctx)
	if err != nil {
		return adapter.Identity{}, err
	}
	if announce != nil {
		announce(st.UserCode, quota.CodexDeviceURL)
	}
	deadline := st.ExpiresAt
	if deadline.IsZero() {
		deadline = a.now().Add(15 * time.Minute)
	}
	wait := st.Interval
	if wait < time.Second {
		wait = 5 * time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return adapter.Identity{}, err
		}
		if a.now().After(deadline) {
			return adapter.Identity{}, errors.New("codex login: device code expired")
		}
		code, ver, err := a.HTTP.CodexDevicePoll(ctx, st)
		if errors.Is(err, quota.ErrDeviceSlowDown) {
			wait += 5 * time.Second
			err = quota.ErrDevicePending
		}
		if errors.Is(err, quota.ErrDevicePending) {
			select {
			case <-ctx.Done():
				return adapter.Identity{}, ctx.Err()
			case <-time.After(wait):
				continue
			}
		}
		if err != nil {
			return adapter.Identity{}, err
		}
		tok, err := a.HTTP.CodexExchangeCode(ctx, code, ver)
		if err != nil {
			return adapter.Identity{}, err
		}
		blob, err := codex.BlobFromTokens(tok, a.now())
		if err != nil {
			return adapter.Identity{}, err
		}
		return a.enrollCodexBlob(ctx, blob)
	}
}

func (a *App) enrollCodexAuthJSON(ctx context.Context, raw []byte) (adapter.Identity, error) {
	blob, err := codex.BlobFromAuthJSON(raw, a.now())
	if err != nil {
		return adapter.Identity{}, err
	}
	return a.enrollCodexBlob(ctx, blob)
}

func (a *App) enrollCodexBlob(ctx context.Context, blob adapter.Blob) (adapter.Identity, error) {
	if err := a.saveBlob(blob); err != nil {
		return adapter.Identity{}, err
	}
	_ = a.probeAccount(ctx, adapter.Codex, blob.Identity.StableID, false)
	label := blob.Identity.DisplayName
	if label == "" {
		label = blob.Identity.Email
	}
	a.Notify.Send("qswitch", fmt.Sprintf("收录新账号 codex %s", label))
	return blob.Identity, nil
}
