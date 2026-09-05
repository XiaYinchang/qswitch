package quota

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

type Class string

const (
	OK        Class = "ok"
	Soft      Class = "soft"
	Exhausted Class = "exhausted"
	Unknown   Class = "unknown"
	Expired   Class = "expired"
)

type Kind string

const (
	KindCodex  Kind = "codex"
	KindGrok   Kind = "grok"
	KindCursor Kind = "cursor"
)

type Result struct {
	Class    Class
	UsedPct  float64
	ResetsAt int64 // unix seconds, 0 if unknown
	Source   string
}

func ClassifyHTTP(status int, body []byte) Result {
	return Classify(Kind(""), status, body)
}

func Classify(kind Kind, status int, body []byte) Result {
	if status == 401 || status == 403 {
		return Result{Class: Expired, Source: "http"}
	}
	text := string(body)
	if kind == KindCursor && cursorCapacityText(text) {
		return Result{Class: Unknown, Source: "http_capacity"}
	}
	if kind == KindGrok && grokQuotaExhausted(status, text) {
		return Result{Class: Exhausted, UsedPct: 100, Source: "http_402", ResetsAt: resetFromBody(body)}
	}
	if status == 200 && json.Valid(body) && len(body) > 0 {
		var v any
		if err := json.Unmarshal(body, &v); err == nil {
			r := classifyValue(v, "http", kind)
			if r.Class != Unknown {
				return r
			}
		}
	}
	if kind == KindCodex && codexPlanExhaustedText(text) {
		return Result{Class: Exhausted, UsedPct: 100, Source: "http_usage_limit", ResetsAt: resetFromBody(body)}
	}
	if kind == KindCursor && cursorAccountStoppedText(text) {
		return Result{Class: Exhausted, UsedPct: 100, Source: "http_usage_limit", ResetsAt: resetFromBody(body)}
	}
	if status == 429 || status >= 500 || status == 0 || status != 200 {
		return Result{Class: Unknown, Source: "http"}
	}
	return Result{Class: Unknown, Source: "http"}
}

func ClassifyLocalMap(v any, source string) Result {
	if v == nil {
		return Result{Class: Unknown, Source: source}
	}
	return classifyValue(v, source, Kind(""))
}

func classifyValue(v any, source string, kind Kind) Result {
	limitReached := findBool(v, "limit_reached")
	if t, ok := findString(v, []string{"rate_limit_reached_type"}); ok && planReachedType(t) {
		limitReached = true
	}
	pct, pctOK := findMaxFloat(v, []string{"used_percent", "usedPercent", "totalPercentUsed", "creditUsagePercent"})
	remaining, remOK := findFloat(v, []string{"remaining"})
	limit, limOK := findFloat(v, []string{"limit", "onDemandCap"})
	if used, uok := findFloat(v, []string{"onDemandUsed"}); uok {
		if capv, cok := findFloat(v, []string{"onDemandCap"}); cok && capv > 0 {
			pct = used / capv * 100
			pctOK = true
		}
	}
	if !pctOK && !limitReached && !(remOK && limOK) {
		return Result{Class: Unknown, Source: source}
	}
	resets := findReset(v)
	includedGone := limitReached || (pctOK && pct >= 99.5) || (remOK && limOK && limit > 0 && remaining <= 0)
	if includedGone {
		if !pctOK {
			pct = 100
		}
		if kind == KindCursor && cursorOnDemandAvailable(v) {
			return Result{Class: Soft, UsedPct: pct, Source: source, ResetsAt: resets}
		}
		if kind == KindGrok && grokPrepaidOrOnDemand(v) {
			return Result{Class: Soft, UsedPct: pct, Source: source, ResetsAt: resets}
		}
		return Result{Class: Exhausted, UsedPct: pct, Source: source, ResetsAt: resets}
	}
	if kind == KindCursor && cursorOnDemandHit(v) && remOK && remaining <= 0 {
		return Result{Class: Exhausted, UsedPct: 100, Source: source, ResetsAt: resets}
	}
	if pctOK && pct >= 90 {
		return Result{Class: Soft, UsedPct: pct, Source: source, ResetsAt: resets}
	}
	if pctOK {
		return Result{Class: OK, UsedPct: pct, Source: source, ResetsAt: resets}
	}
	return Result{Class: OK, UsedPct: 0, Source: source, ResetsAt: resets}
}

func resetFromBody(body []byte) int64 {
	if len(body) == 0 || !json.Valid(body) {
		return 0
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	return findReset(v)
}

func findBool(v any, key string) bool {
	var found bool
	walk(v, func(k string, val any) {
		if strings.EqualFold(k, key) {
			switch t := val.(type) {
			case bool:
				if t {
					found = true
				}
			}
		}
	})
	return found
}

func findFloat(v any, keys []string) (float64, bool) {
	want := map[string]bool{}
	for _, k := range keys {
		want[strings.ToLower(k)] = true
	}
	var out float64
	ok := false
	walk(v, func(k string, val any) {
		if !want[strings.ToLower(k)] {
			return
		}
		if n, good := asFloat(val); good {
			out, ok = n, true
		} else if n, good := objectVal(val); good {
			out, ok = n, true
		}
	})
	return out, ok
}

func findMaxFloat(v any, keys []string) (float64, bool) {
	want := map[string]bool{}
	for _, k := range keys {
		want[strings.ToLower(k)] = true
	}
	max := math.Inf(-1)
	ok := false
	walk(v, func(k string, val any) {
		if !want[strings.ToLower(k)] {
			return
		}
		if n, good := asFloat(val); good {
			if n > max {
				max = n
				ok = true
			}
		} else if n, good := objectVal(val); good {
			if n > max {
				max = n
				ok = true
			}
		}
	})
	if !ok {
		return 0, false
	}
	return max, true
}

func findString(v any, keys []string) (string, bool) {
	want := map[string]bool{}
	for _, k := range keys {
		want[strings.ToLower(k)] = true
	}
	var out string
	ok := false
	walk(v, func(k string, val any) {
		if !want[strings.ToLower(k)] {
			return
		}
		if s, good := val.(string); good && s != "" {
			out, ok = s, true
		}
	})
	return out, ok
}

func findReset(v any) int64 {
	if n, ok := findFloat(v, []string{"resets_at", "reset_at", "billingCycleEnd"}); ok && n > 1e12 {
		return int64(n / 1000)
	}
	if n, ok := findFloat(v, []string{"resets_at", "reset_at"}); ok && n > 1e9 {
		return int64(n)
	}
	if n, ok := findFloat(v, []string{"reset_after_seconds"}); ok && n > 0 {
		return 0
	}
	var best int64
	walk(v, func(k string, val any) {
		lk := strings.ToLower(k)
		if lk != "end" && lk != "billingperiodend" && lk != "billingcycleend" && lk != "resets_at" && lk != "reset_at" {
			return
		}
		s, ok := val.(string)
		if !ok || s == "" {
			return
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			best = ts.Unix()
			return
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			best = ts.Unix()
		}
	})
	return best
}

func objectVal(v any) (float64, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	return asFloatMap(m, "val")
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		n, err := t.Float64()
		return n, err == nil
	case string:
		n, err := json.Number(t).Float64()
		return n, err == nil
	}
	return 0, false
}

func walk(v any, fn func(key string, val any)) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			fn(k, val)
			walk(val, fn)
		}
	case []any:
		for _, val := range t {
			walk(val, fn)
		}
	}
}

func planReachedType(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "rate_limit_reached",
		"workspace_owner_credits_depleted",
		"workspace_member_credits_depleted",
		"workspace_owner_usage_limit_reached",
		"workspace_member_usage_limit_reached":
		return true
	default:
		return false
	}
}

func containsAny(low string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(low, n) {
			return true
		}
	}
	return false
}

func codexPlanExhaustedText(s string) bool {
	low := strings.ToLower(s)
	return containsAny(low,
		"usage_limit_reached",
		"usage-limit",
		"you've hit your usage limit",
		"you have hit your usage limit",
		"workspace_owner_credits_depleted",
		"workspace_member_credits_depleted",
		"workspace_owner_usage_limit_reached",
		"workspace_member_usage_limit_reached",
		`"rate_limit_reached"`,
	)
}

func grokQuotaExhausted(status int, s string) bool {
	low := strings.ToLower(s)
	if looksLikeX402(low) {
		return false
	}
	if containsAny(low, "usage balance exhausted", "run out of credits", "personal-team-blocked:spending-limit") {
		return true
	}
	if status == 402 && containsAny(low, "payment required", "spending-limit") {
		return true
	}
	if status == 402 {
		return true
	}
	return strings.Contains(low, "status 402 payment required")
}

func looksLikeX402(low string) bool {
	return strings.Contains(low, "x402") || (strings.Contains(low, "accepts") && strings.Contains(low, "payment"))
}

func grokPrepaidOrOnDemand(v any) bool {
	if n, ok := findFloat(v, []string{"prepaidBalance"}); ok && n > 0 {
		return true
	}
	capv, cok := findFloat(v, []string{"onDemandCap"})
	used, uok := findFloat(v, []string{"onDemandUsed"})
	if cok && capv > 0 && (!uok || used < capv) {
		return true
	}
	return false
}

func cursorCapacityText(s string) bool {
	low := strings.ToLower(s)
	return containsAny(low, "resource_exhausted", "error_resource_exhausted", "high load", "high demand")
}

func cursorAccountStoppedText(s string) bool {
	if cursorCapacityText(s) {
		return false
	}
	low := strings.ToLower(s)
	if strings.Contains(low, "switch to a different model") {
		return false
	}
	if containsAny(low, "you've hit your hard limit", `spendlimithit":true`, "spendlimithit: true") {
		return true
	}
	if containsAny(low, "reached your monthly limit", "set a new on-demand limit to continue") {
		return true
	}
	if strings.Contains(low, "you've hit your usage limit") && !strings.Contains(low, "usage limit for ") {
		return !strings.Contains(low, "switch to")
	}
	return false
}

func cursorOnDemandAvailable(v any) bool {
	sl := spendLimitNode(v)
	if sl == nil {
		return false
	}
	if n, ok := asFloatMap(sl, "individualRemaining"); ok && n > 0 {
		if lim, lok := asFloatMap(sl, "individualLimit"); !lok || lim > 0 {
			return true
		}
	}
	if n, ok := asFloatMap(sl, "pooledRemaining"); ok && n > 0 {
		if lim, lok := asFloatMap(sl, "pooledLimit"); !lok || lim > 0 {
			return true
		}
	}
	indLim, indOK := asFloatMap(sl, "individualLimit")
	indUsed, usedOK := asFloatMap(sl, "individualUsed")
	if indOK && indLim > 0 && usedOK && indUsed < indLim {
		return true
	}
	poolLim, pOK := asFloatMap(sl, "pooledLimit")
	poolUsed, puOK := asFloatMap(sl, "pooledUsed")
	if pOK && poolLim > 0 && puOK && poolUsed < poolLim {
		return true
	}
	return false
}

func cursorOnDemandHit(v any) bool {
	sl := spendLimitNode(v)
	if sl == nil {
		return false
	}
	if n, ok := asFloatMap(sl, "individualRemaining"); ok {
		if lim, lok := asFloatMap(sl, "individualLimit"); lok && lim > 0 && n <= 0 {
			return true
		}
	}
	if n, ok := asFloatMap(sl, "pooledRemaining"); ok {
		if lim, lok := asFloatMap(sl, "pooledLimit"); lok && lim > 0 && n <= 0 {
			return true
		}
	}
	indLim, indOK := asFloatMap(sl, "individualLimit")
	indUsed, usedOK := asFloatMap(sl, "individualUsed")
	if indOK && usedOK && indLim > 0 && indUsed >= indLim {
		return true
	}
	poolLim, pOK := asFloatMap(sl, "pooledLimit")
	poolUsed, puOK := asFloatMap(sl, "pooledUsed")
	return pOK && puOK && poolLim > 0 && poolUsed >= poolLim
}

func spendLimitNode(v any) map[string]any {
	var found map[string]any
	walk(v, func(k string, val any) {
		if !strings.EqualFold(k, "spendLimitUsage") {
			return
		}
		if m, ok := val.(map[string]any); ok {
			found = m
		}
	})
	return found
}

func asFloatMap(m map[string]any, key string) (float64, bool) {
	for k, val := range m {
		if strings.EqualFold(k, key) {
			return asFloat(val)
		}
	}
	return 0, false
}
