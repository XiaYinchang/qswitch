package kimi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuthRefreshLockHeartbeatAndReplacement(t *testing.T) {
	t.Setenv("KIMI_CODE_HOME", "")
	home := t.TempDir()
	l, err := AcquireAuthRefreshLock(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	path := filepath.Join(home, ".kimi-code/oauth/kimi-code.lock")
	before, _ := os.Stat(path)
	if _, err := AcquireAuthRefreshLock(context.Background(), home); err == nil {
		t.Fatal("two owners acquired official lock")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		after, _ := os.Stat(path)
		if after.ModTime().After(before.ModTime()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat did not advance directory mtime")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.Remove(path)
	_ = os.Mkdir(path, 0700)
	select {
	case <-l.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("replaced lock did not cancel its exchange context")
	}
	if l.OwnsPath() {
		t.Fatal("replaced directory belongs to old owner")
	}
	_ = l.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("release deleted replacement lock")
	}
}

func TestRefreshThresholdLeavesTimeForKeepAliveTick(t *testing.T) {
	now := time.Now()
	for _, life := range []int64{120, 900, 3600} {
		threshold := int64(300)
		if life/2 > threshold {
			threshold = life / 2
		}
		if !(Creds{ExpiresAt: now.Unix() + threshold, ExpiresIn: life}).NeedsRefresh(now) ||
			(Creds{ExpiresAt: now.Unix() + threshold + 1, ExpiresIn: life}).NeedsRefresh(now) {
			t.Fatalf("official freshness threshold for lifetime %d", life)
		}
	}
}
