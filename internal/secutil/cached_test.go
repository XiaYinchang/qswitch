package secutil

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingProvider struct {
	calls atomic.Int32
	key   []byte
	err   error
	delay time.Duration
}

func (p *countingProvider) GetOrCreate([]string) ([]byte, error) {
	p.calls.Add(1)
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	if p.err != nil {
		return nil, p.err
	}
	return append([]byte(nil), p.key...), nil
}

func TestCachedAsksProviderOnce(t *testing.T) {
	p := &countingProvider{key: bytes.Repeat([]byte{7}, 32)}
	c := &Cached{Provider: p}
	first, err := c.GetOrCreate(nil)
	if err != nil {
		t.Fatal(err)
	}
	Zero(first) // vault callers zero their copy after use
	second, err := c.GetOrCreate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, p.key) {
		t.Error("cached key changed after a caller zeroed its copy")
	}
	if n := p.calls.Load(); n != 1 {
		t.Errorf("provider calls = %d, want 1", n)
	}
}

func TestCachedConcurrentCallersShareOneLookup(t *testing.T) {
	p := &countingProvider{key: bytes.Repeat([]byte{9}, 32), delay: 50 * time.Millisecond}
	c := &Cached{Provider: p}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetOrCreate(nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := p.calls.Load(); n != 1 {
		t.Errorf("provider calls = %d, want 1", n)
	}
}

func TestCachedFailureIsNotRetriedUntilRetryAfter(t *testing.T) {
	locked := errors.New("keychain locked")
	p := &countingProvider{err: locked}
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	c := &Cached{Provider: p, RetryAfter: 10 * time.Minute, Now: func() time.Time { return now }}
	for i := range 3 {
		if _, err := c.GetOrCreate(nil); !errors.Is(err, locked) {
			t.Fatalf("call %d: err = %v, want the provider's error", i, err)
		}
		now = now.Add(time.Minute)
	}
	if n := p.calls.Load(); n != 1 {
		t.Fatalf("provider calls within RetryAfter = %d, want 1", n)
	}

	now = now.Add(10 * time.Minute)
	p.err = nil
	p.key = bytes.Repeat([]byte{3}, 32)
	got, err := c.GetOrCreate(nil)
	if err != nil || !bytes.Equal(got, p.key) {
		t.Fatalf("after RetryAfter: err = %v, key matches = %v", err, bytes.Equal(got, p.key))
	}
	if n := p.calls.Load(); n != 2 {
		t.Errorf("provider calls = %d, want 2", n)
	}
}
