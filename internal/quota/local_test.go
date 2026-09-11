package quota

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTailCodex(t *testing.T) {
	t.Run("percent over old usage_limit", func(t *testing.T) {
		p := writeJSONL(t,
			`{"type":"event_msg","payload":{"type":"error","message":"usage_limit_reached"}}`,
			`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":16.0,"resets_at":1788702939},"rate_limit_reached_type":null}}}`,
		)
		r, ok := Tail(KindCodex, p)
		if !ok || r.Class != OK || r.UsedPct != 16 {
			t.Fatalf("got ok=%v class=%s pct=%v", ok, r.Class, r.UsedPct)
		}
		if r.ResetsAt != 1788702939 {
			t.Fatalf("resets %d", r.ResetsAt)
		}
	})
	t.Run("usage_limit", func(t *testing.T) {
		p := writeJSONL(t, `{"error":{"code":"usage_limit_reached"}}`)
		r, ok := Tail(KindCodex, p)
		if !ok || r.Class != Exhausted {
			t.Fatalf("got ok=%v class=%s", ok, r.Class)
		}
	})
	t.Run("tpm 429 is not exhausted", func(t *testing.T) {
		p := writeJSONL(t, `{"error":{"code":"rate_limit_exceeded","status":429}}`)
		r, ok := Tail(KindCodex, p)
		if ok {
			t.Fatalf("tpm 429 should be ignored, got %+v", r)
		}
	})
	t.Run("bare status 429 is not exhausted", func(t *testing.T) {
		p := writeJSONL(t, `{"status":429,"message":"too many requests"}`)
		if _, ok := Tail(KindCodex, p); ok {
			t.Fatal("bare 429 should be ignored")
		}
	})
}

func TestTailGrok(t *testing.T) {
	t.Run("billing percent", func(t *testing.T) {
		p := writeJSONL(t, `{"msg":"billing: fetched credits config","ctx":{"config":{"creditUsagePercent":2.0,"prepaidBalance":{"val":0},"currentPeriod":{"end":"2026-08-30T14:04:34Z"}},"subscriptionTier":"SuperGrok Heavy"}}`)
		r, ok := Tail(KindGrok, p)
		if !ok || r.Class != OK || r.UsedPct != 2 {
			t.Fatalf("got ok=%v class=%s pct=%v", ok, r.Class, r.UsedPct)
		}
		if r.ResetsAt == 0 {
			t.Fatal("expected period end")
		}
	})
	t.Run("402 after billing wins", func(t *testing.T) {
		p := writeJSONL(t,
			`{"msg":"billing: fetched credits config","ctx":{"config":{"creditUsagePercent":99.0}}}`,
			`{"msg":"shell.turn.inference_failed","ctx":{"message":"API error (status 402 Payment Required): Grok Build usage balance exhausted"}}`,
		)
		r, ok := Tail(KindGrok, p)
		if !ok || r.Class != Exhausted {
			t.Fatalf("got ok=%v class=%s", ok, r.Class)
		}
	})
	t.Run("uuid 402 is not exhausted", func(t *testing.T) {
		p := writeJSONL(t, `{"id":"40298877-066d-46c9-ba70-8a5ca83afd80","msg":"ok"}`)
		if _, ok := Tail(KindGrok, p); ok {
			t.Fatal("bare 402 in uuid should be ignored")
		}
	})
	t.Run("session dump mentioning 402 is not quota", func(t *testing.T) {
		p := writeJSONL(t,
			`{"msg":"billing: fetched credits config","ctx":{"config":{"creditUsagePercent":7.0,"currentPeriod":{"end":"2026-09-13T14:04:34Z"}}}}`,
			`{"method":"session/update","params":{"update":{"content":[{"text":"func grokQuotaExhausted status 402 payment required usage balance exhausted"}]}}}`,
		)
		r, ok := Tail(KindGrok, p)
		if !ok || r.Class != OK || r.UsedPct != 7 {
			t.Fatalf("got ok=%v class=%s pct=%v", ok, r.Class, r.UsedPct)
		}
	})
}

func TestTailCursor(t *testing.T) {
	t.Run("hard limit", func(t *testing.T) {
		p := writeJSONL(t, `{"type":"turn_ended","status":"error","error":"You've hit your hard limit"}`)
		r, ok := Tail(KindCursor, p)
		if !ok || r.Class != Exhausted {
			t.Fatalf("got ok=%v class=%s", ok, r.Class)
		}
	})
	t.Run("model pool is not account exhausted", func(t *testing.T) {
		p := writeJSONL(t, `{"type":"turn_ended","status":"error","error":"You've hit your usage limit You've saved $400. Switch to a different model or set a Spend Limit to continue with Sonnet."}`)
		if _, ok := Tail(KindCursor, p); ok {
			t.Fatal("model-specific limit should not exhaust the account")
		}
	})
	t.Run("resource_exhausted is capacity", func(t *testing.T) {
		p := writeJSONL(t, `{"type":"turn_ended","status":"error","error":"[resource_exhausted] Error"}`)
		if _, ok := Tail(KindCursor, p); ok {
			t.Fatal("resource_exhausted is not quota")
		}
	})
	t.Run("high load", func(t *testing.T) {
		p := writeJSONL(t, `{"error":"ERROR_RESOURCE_EXHAUSTED","details":{"title":"High Load","isRetryable":true}}`)
		if _, ok := Tail(KindCursor, p); ok {
			t.Fatal("high load is not quota")
		}
	})
}

func TestPrefer(t *testing.T) {
	got := Prefer(Result{Class: OK, UsedPct: 2}, Result{Class: Exhausted, UsedPct: 100})
	if got.Class != Exhausted {
		t.Fatalf("got %s", got.Class)
	}
}
