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
	KindDevin  Kind = "devin"
)

type Bucket struct {
	ID       string  `json:"id"`
	UsedPct  float64 `json:"used_pct"`
	ResetsAt int64   `json:"resets_at,omitempty"`
	Plan     string  `json:"plan,omitempty"`
}

type Result struct {
	Class    Class
	UsedPct  float64
	ResetsAt int64 // unix seconds, 0 if unknown
	Source   string
	Plan     string
	Buckets  []Bucket
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
			if kind == KindCursor {
				if r, ok := classifyCursorPeriod(v); ok {
					return withPlan(kind, r, v)
				}
			}
			if kind == KindDevin {
				if r, ok := ParseDevinStatus(body); ok {
					return r
				}
			}
			if kind == KindCodex {
				if r, ok := classifyCodexWindows(v, "http"); ok {
					return withPlan(kind, r, v)
				}
			}
			r := classifyValue(v, "http", kind)
			r = withPlan(kind, r, v)
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
	orig := v
	if kind == KindCodex {
		v = codexPlanNode(v)
	}
	limitReached := findBool(v, "limit_reached") || findBool(orig, "limit_reached")
	if t, ok := findString(orig, []string{"rate_limit_reached_type"}); ok && planReachedType(t) {
		limitReached = true
	}
	if t, ok := findString(v, []string{"rate_limit_reached_type"}); ok && planReachedType(t) {
		limitReached = true
	}
	pct, pctOK, winReset := planUsage(v)
	if !pctOK {
		pct, pctOK = findMaxFloat(v, []string{"used_percent", "usedPercent", "totalPercentUsed", "creditUsagePercent"})
	}
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
	resets := winReset
	if resets == 0 {
		resets = findReset(v)
	}
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

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func classifyCursorPeriod(v any) (Result, bool) {
	m := asMap(v)
	if m == nil {
		return Result{}, false
	}
	pu := asMap(m["planUsage"])
	if pu == nil {
		return Result{}, false
	}
	auto, autoOK := asFloat(pu["autoPercentUsed"])
	api, apiOK := asFloat(pu["apiPercentUsed"])
	total, totalOK := asFloat(pu["totalPercentUsed"])
	if !autoOK && !apiOK && !totalOK {
		return Result{}, false
	}
	resets := findReset(m)
	var buckets []Bucket
	if autoOK {
		buckets = append(buckets, Bucket{ID: "auto", UsedPct: auto, ResetsAt: resets})
	}
	if apiOK {
		buckets = append(buckets, Bucket{ID: "api", UsedPct: api, ResetsAt: resets})
	}
	used := 0.0
	switch {
	case autoOK && apiOK:
		used = auto
		if api > used {
			used = api
		}
	case totalOK:
		used = total
	case autoOK:
		used = auto
	default:
		used = api
	}
	class := OK
	switch {
	case autoOK && apiOK && auto >= 99.5 && api >= 99.5:
		class = Exhausted
	case (!autoOK || !apiOK) && used >= 99.5:
		class = Exhausted
	case (autoOK && auto >= 90) || (apiOK && api >= 90) || used >= 90:
		class = Soft
	}
	if class == Exhausted && cursorOnDemandAvailable(v) {
		class = Soft
	}
	return Result{Class: class, UsedPct: used, ResetsAt: resets, Source: "http", Buckets: buckets}, true
}

func ParseDevinStatus(body []byte) (Result, bool) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return Result{}, false
	}
	us := asMap(root["userStatus"])
	if us == nil {
		us = root
	}
	ps := asMap(us["planStatus"])
	if ps == nil {
		return Result{}, false
	}
	pi := asMap(ps["planInfo"])
	hideDaily := asBool(pi["hideDailyQuota"])
	hideWeekly := asBool(pi["hideWeeklyQuota"])
	dailyRem, dailyOK := asFloat(ps["dailyQuotaRemainingPercent"])
	weeklyRem, weeklyOK := asFloat(ps["weeklyQuotaRemainingPercent"])
	if !hideDaily && !dailyOK {
		dailyRem, dailyOK = 0, true
	}
	if !hideWeekly && !weeklyOK {
		weeklyRem, weeklyOK = 0, true
	}
	var buckets []Bucket
	if !hideDaily && dailyOK {
		used := clampUsed(100 - dailyRem)
		buckets = append(buckets, Bucket{ID: "daily", UsedPct: used, ResetsAt: asUnix(ps["dailyQuotaResetAtUnix"])})
	}
	if !hideWeekly && weeklyOK {
		used := clampUsed(100 - weeklyRem)
		buckets = append(buckets, Bucket{ID: "weekly", UsedPct: used, ResetsAt: asUnix(ps["weeklyQuotaResetAtUnix"])})
	}
	if len(buckets) == 0 {
		return Result{}, false
	}
	used := buckets[0].UsedPct
	resets := buckets[0].ResetsAt
	for _, b := range buckets[1:] {
		if b.UsedPct > used {
			used = b.UsedPct
		}
		if b.ResetsAt > 0 && (resets == 0 || b.ResetsAt < resets) {
			resets = b.ResetsAt
		}
	}
	class := OK
	switch {
	case used >= 99.5:
		class = Exhausted
	case used >= 90:
		class = Soft
	}
	plan := ""
	if s, _ := pi["planName"].(string); strings.TrimSpace(s) != "" {
		plan = formatDevinPlan(s)
	} else if s, _ := us["teamsTier"].(string); s != "" {
		plan = formatDevinPlan(s)
	}
	return Result{Class: class, UsedPct: used, ResetsAt: resets, Source: "http", Plan: plan, Buckets: buckets}, true
}

func DevinIdentity(body []byte) (userID, email, plan string) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return "", "", ""
	}
	us := asMap(root["userStatus"])
	if us == nil {
		return "", "", ""
	}
	userID, _ = us["userId"].(string)
	email, _ = us["email"].(string)
	ps := asMap(us["planStatus"])
	pi := asMap(ps["planInfo"])
	if s, _ := pi["planName"].(string); s != "" {
		plan = formatDevinPlan(s)
	}
	return strings.TrimSpace(userID), strings.TrimSpace(email), plan
}

func asBool(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func asUnix(v any) int64 {
	f, ok := asFloat(v)
	if !ok || f <= 0 {
		return 0
	}
	return int64(f)
}

func clampUsed(n float64) float64 {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func ParseCursorBot(body []byte) (Bucket, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return Bucket{}, false
	}
	p, ok := asFloat(m["usagePercent"])
	if !ok {
		return Bucket{}, false
	}
	b := Bucket{ID: "bot", UsedPct: p}
	if s, _ := m["nextResetTimestampUtc"].(string); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			b.ResetsAt = t.Unix()
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			b.ResetsAt = t.Unix()
		}
	}
	if s, _ := m["grokPlanLabel"].(string); strings.TrimSpace(s) != "" {
		b.Plan = strings.TrimSpace(s)
	}
	return b, true
}

func MergeBuckets(old, new []Bucket) []Bucket {
	if len(new) == 0 {
		return old
	}
	if len(old) == 0 {
		return new
	}
	by := make(map[string]Bucket, len(old)+len(new))
	order := make([]string, 0, len(old)+len(new))
	for _, b := range old {
		if _, ok := by[b.ID]; !ok {
			order = append(order, b.ID)
		}
		by[b.ID] = b
	}
	for _, b := range new {
		if _, ok := by[b.ID]; !ok {
			order = append(order, b.ID)
		}
		by[b.ID] = b
	}
	out := make([]Bucket, 0, len(order))
	for _, id := range order {
		out = append(out, by[id])
	}
	return out
}

func EncodeBuckets(b []Bucket) string {
	if len(b) == 0 {
		return ""
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return ""
	}
	return string(raw)
}

func DecodeBuckets(s string) []Bucket {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var b []Bucket
	if json.Unmarshal([]byte(s), &b) != nil {
		return nil
	}
	return b
}

func codexPlanNode(v any) any {
	m := asMap(v)
	if m == nil {
		return v
	}
	if x := asMap(m["rate_limit"]); x != nil {
		return x
	}
	if x := asMap(m["rate_limits"]); x != nil {
		return x
	}
	return v
}

func classifyCodexWindows(v any, source string) (Result, bool) {
	m := asMap(codexPlanNode(v))
	if m == nil {
		return Result{}, false
	}
	var primary, secondary map[string]any
	if w := asMap(m["primary_window"]); w != nil {
		primary = w
		secondary = asMap(m["secondary_window"])
	} else if w := asMap(m["primary"]); w != nil {
		primary = w
		secondary = asMap(m["secondary"])
	}
	if primary == nil || secondary == nil {
		return Result{}, false
	}
	pUsed, pOK := windowUsed(primary)
	sUsed, sOK := windowUsed(secondary)
	if !pOK || !sOK {
		return Result{}, false
	}
	buckets := []Bucket{
		{ID: windowBucketID(primary, "5h"), UsedPct: pUsed, ResetsAt: windowReset(primary)},
		{ID: windowBucketID(secondary, "weekly"), UsedPct: sUsed, ResetsAt: windowReset(secondary)},
	}
	used := pUsed
	if sUsed > used {
		used = sUsed
	}
	resets := buckets[0].ResetsAt
	if buckets[1].UsedPct >= buckets[0].UsedPct && buckets[1].ResetsAt > 0 {
		resets = buckets[1].ResetsAt
	} else if resets == 0 {
		resets = buckets[1].ResetsAt
	}
	class := OK
	limitReached := findBool(m, "limit_reached")
	switch {
	case limitReached || used >= 99.5:
		class = Exhausted
	case used >= 90:
		class = Soft
	}
	return Result{Class: class, UsedPct: used, ResetsAt: resets, Source: source, Buckets: buckets}, true
}

func windowBucketID(w map[string]any, fallback string) string {
	sec, ok := asFloat(w["limit_window_seconds"])
	if !ok {
		return fallback
	}
	switch {
	case sec <= 8*3600:
		return "5h"
	case sec <= 36*3600:
		return "1d"
	default:
		return "weekly"
	}
}

func planUsage(v any) (float64, bool, int64) {
	m := asMap(v)
	if m == nil {
		return 0, false, 0
	}
	var bestPct float64
	var bestReset int64
	found := false
	for _, key := range []string{"primary_window", "secondary_window", "primary", "secondary"} {
		w := asMap(m[key])
		if w == nil {
			continue
		}
		p, ok := windowUsed(w)
		if !ok {
			continue
		}
		r := windowReset(w)
		if !found || p > bestPct {
			found, bestPct, bestReset = true, p, r
		}
	}
	if !found {
		return 0, false, 0
	}
	return bestPct, true, bestReset
}

func windowUsed(w map[string]any) (float64, bool) {
	if n, ok := asFloat(w["used_percent"]); ok {
		return n, true
	}
	if n, ok := asFloat(w["usedPercent"]); ok {
		return n, true
	}
	return 0, false
}

func windowReset(w map[string]any) int64 {
	for _, k := range []string{"reset_at", "resets_at"} {
		n, ok := asFloat(w[k])
		if !ok {
			continue
		}
		if n > 1e12 {
			return int64(n / 1000)
		}
		if n > 1e9 {
			return int64(n)
		}
	}
	return 0
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
	if status == 402 && containsAny(low, "usage balance", "run out of credits", "spending-limit") {
		return true
	}
	return false
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
