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
	"qswitch/internal/adapter/grok"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
)

func findGrokBin() string {
	if p, err := exec.LookPath("grok"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/opt/homebrew/bin/grok",
		"/usr/local/bin/grok",
		filepath.Join(home, ".local", "bin", "grok"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func (a *App) LoginGrok(ctx context.Context, announce func(userCode, verifyURL string)) (adapter.Identity, error) {
	if !secutil.InTest() {
		if bin := findGrokBin(); bin != "" {
			return a.loginGrokOfficial(ctx, bin)
		}
	}
	return a.loginGrokHTTP(ctx, announce)
}

func (a *App) loginGrokOfficial(ctx context.Context, bin string) (adapter.Identity, error) {
	dir, err := os.MkdirTemp("", "qswitch-grok-login-")
	if err != nil {
		return adapter.Identity{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return adapter.Identity{}, err
	}
	sock := filepath.Join(dir, "leader.sock")
	cmd := exec.CommandContext(ctx, bin, "login", "--device-auth", "--leader-socket", sock)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = isolatedGrokEnv(os.Environ(), dir)
	if err := cmd.Run(); err != nil {
		return adapter.Identity{}, fmt.Errorf("grok login --device-auth: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return adapter.Identity{}, fmt.Errorf("grok login produced no auth.json: %w", err)
	}
	return a.enrollGrokAuthJSON(ctx, raw)
}

func isolatedGrokEnv(parent []string, home string) []string {
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "GROK_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GROK_HOME="+home)
}

func (a *App) loginGrokHTTP(ctx context.Context, announce func(userCode, verifyURL string)) (adapter.Identity, error) {
	st, err := a.BeginGrokDeviceLogin(ctx)
	if err != nil {
		return adapter.Identity{}, err
	}
	if announce != nil {
		announce(st.UserCode, st.VerifyURL)
	}
	return a.CompleteGrokDeviceLogin(ctx, st)
}

func (a *App) BeginGrokDeviceLogin(ctx context.Context) (quota.GrokDeviceStart, error) {
	if a.HTTP.Client == nil && secutil.InTest() {
		return quota.GrokDeviceStart{}, errors.New("grok login: no http client")
	}
	return a.HTTP.GrokDeviceStart(ctx)
}

func (a *App) CompleteGrokDeviceLogin(ctx context.Context, st quota.GrokDeviceStart) (adapter.Identity, error) {
	deadline := st.ExpiresAt
	if deadline.IsZero() {
		deadline = a.now().Add(30 * time.Minute)
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
			return adapter.Identity{}, errors.New("grok login: device code expired")
		}
		tok, err := a.HTTP.GrokDevicePoll(ctx, st)
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
		if tok.Email == "" {
			if em, uerr := a.HTTP.GrokUserinfo(ctx, tok.AccessToken); uerr == nil {
				tok.Email = em
			}
		}
		blob, err := grok.BlobFromTokens(tok, a.now())
		if err != nil {
			return adapter.Identity{}, err
		}
		return a.enrollGrokBlob(ctx, blob)
	}
}

func (a *App) enrollGrokAuthJSON(ctx context.Context, raw []byte) (adapter.Identity, error) {
	blob, err := grok.BlobFromAuthJSON(raw, a.now())
	if err != nil {
		return adapter.Identity{}, err
	}
	return a.enrollGrokBlob(ctx, blob)
}

func (a *App) enrollGrokBlob(ctx context.Context, blob adapter.Blob) (adapter.Identity, error) {
	if err := a.saveBlob(blob); err != nil {
		return adapter.Identity{}, err
	}
	_ = a.probeAccount(ctx, adapter.Grok, blob.Identity.StableID, false)
	label := blob.Identity.DisplayName
	if label == "" {
		label = blob.Identity.Email
	}
	if label == "" {
		label = blob.Identity.StableID
	}
	a.Notify.Send("qswitch", fmt.Sprintf("收录新账号 grok %s", label))
	return blob.Identity, nil
}
