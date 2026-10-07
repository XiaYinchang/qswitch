package quota

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// ParseKimiUsage follows the managed /usages contract. Missing windows stay
// missing; limit_month_code is the code share of monthly total, not a limit.
// The response also carries a limits[] array (window duration + limit/
// remaining detail); windows absent from the usages map, notably a monthly
// window, are recovered from there so quota survives either encoding.
func ParseKimiUsage(status int, body []byte) Result {
	r := Result{Class: Unknown, Source: "http"}
	if status == 401 || status == 403 {
		r.Class = Expired
		return r
	}
	if status != 200 {
		return r
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return r
	}
	if _, ok := root["error"]; ok {
		return r
	}
	if _, ok := root["errors"]; ok {
		return r
	}
	r.Authenticated = true
	usages := asMap(root["usages"])
	for _, window := range []struct{ key, id string }{
		{"limit_5h", "5h"}, {"limit_7d", "weekly"}, {"limit_month_total", "monthly"},
	} {
		raw, present := usages[window.key]
		if !present {
			continue
		}
		b, ok := kimiBucket(raw, window.id)
		if !ok {
			return Result{Class: Unknown, Source: "http", Authenticated: true}
		}
		if len(r.Buckets) == 0 || b.UsedPct > r.UsedPct {
			r.UsedPct, r.ResetsAt = b.UsedPct, b.ResetsAt
		}
		r.Buckets = append(r.Buckets, b)
	}
	have := make(map[string]bool, len(r.Buckets))
	for _, b := range r.Buckets {
		have[b.ID] = true
	}
	for _, b := range kimiLimitsBuckets(root) {
		if have[b.ID] {
			continue
		}
		have[b.ID] = true
		if len(r.Buckets) == 0 || b.UsedPct > r.UsedPct {
			r.UsedPct, r.ResetsAt = b.UsedPct, b.ResetsAt
		}
		r.Buckets = append(r.Buckets, b)
	}
	if len(r.Buckets) == 0 {
		return r
	}
	r.Class = OK
	if r.UsedPct >= 100 {
		r.Class = Exhausted
		// Every exhausted window must reset before subscription use resumes.
		r.ResetsAt = 0
		unknownReset := false
		for _, b := range r.Buckets {
			if b.UsedPct < 100 {
				continue
			}
			if b.ResetsAt <= 0 {
				unknownReset = true
			}
			if b.ResetsAt > r.ResetsAt {
				r.ResetsAt = b.ResetsAt
			}
		}
		if unknownReset {
			r.ResetsAt = 0
		}
		if kimiBoosterMayBeAvailable(asMap(root["boosterWallet"])) {
			// /usages exposes balance and cap, but its public contract does not
			// expose the Extra Usage enable switch. Do not assume availability
			// or auto-switch an account that may still have spendable credit.
			r.Class = Unknown
		}
	} else if r.UsedPct >= 90 {
		r.Class = Soft
	}
	if _, ok := kimiBucket(usages["limit_month_total"], "monthly"); ok {
		if b, ok := kimiBucket(usages["limit_month_code"], "monthly_code"); ok {
			r.Buckets = append(r.Buckets, b)
		}
	}
	return r
}

func kimiBucket(raw any, id string) (Bucket, bool) {
	m := asMap(raw)
	ratio, ok := kimiNumber(m["used_ratio"])
	if !ok || ratio < 0 {
		return Bucket{}, false
	}
	b := Bucket{ID: id, UsedPct: math.Min(100, math.Max(0, ratio*100))}
	if reset, ok := m["reset_time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, reset); err == nil {
			b.ResetsAt = t.Unix()
		}
	}
	return b, true
}

// kimiLimitsBuckets converts the limits[] array entries. Each entry carries a
// window (duration + timeUnit) and a detail of limit/remaining amounts.
// Malformed entries are skipped without invalidating the usages map result.
func kimiLimitsBuckets(root map[string]any) []Bucket {
	arr, _ := root["limits"].([]any)
	var out []Bucket
	for _, item := range arr {
		m := asMap(item)
		if m == nil {
			continue
		}
		id, ok := kimiWindowID(asMap(m["window"]))
		if !ok {
			continue
		}
		d := asMap(m["detail"])
		limit, lok := kimiNumber(d["limit"])
		remaining, rok := kimiNumber(d["remaining"])
		if !lok || !rok || limit <= 0 {
			continue
		}
		used := (limit - remaining) / limit
		if used < 0 {
			used = 0
		}
		b := Bucket{ID: id, UsedPct: math.Min(100, math.Max(0, used*100))}
		if reset, ok := d["resetTime"].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, reset); err == nil {
				b.ResetsAt = t.Unix()
			}
		}
		out = append(out, b)
	}
	return out
}

func kimiWindowID(w map[string]any) (string, bool) {
	if w == nil {
		return "", false
	}
	duration, dok := kimiNumber(w["duration"])
	if !dok || duration <= 0 {
		return "", false
	}
	unit, _ := w["timeUnit"].(string)
	switch u := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(unit, "TIME_UNIT_"))); u {
	case "MONTH":
		return "monthly", true
	case "WEEK":
		return "weekly", true
	case "SECOND":
		return kimiWindowScaleID(duration, 1)
	case "MINUTE":
		return kimiWindowScaleID(duration, 60)
	case "HOUR":
		return kimiWindowScaleID(duration, 3600)
	case "DAY":
		return kimiWindowScaleID(duration, 86400)
	default:
		return "", false
	}
}

func kimiWindowScaleID(duration, scale float64) (string, bool) {
	switch h := duration * scale / 3600; {
	case h <= 8:
		return "5h", true
	case h <= 36:
		return "1d", true
	case h >= 600:
		return "monthly", true
	case h >= 120:
		return "weekly", true
	default:
		return "", false
	}
}

func kimiNumber(v any) (float64, bool) {
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return 0, false
	}
	n, ok := asFloat(v)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func kimiBoosterMayBeAvailable(wallet map[string]any) bool {
	b := asMap(wallet["balance"])
	if b["type"] != "BOOSTER" {
		return false
	}
	total, totalOK := kimiNumber(b["amount"])
	left, leftOK := kimiNumber(b["amountLeft"])
	if !totalOK || !leftOK || total <= 0 || left <= 0 {
		return false
	}
	enabled, ok := wallet["monthlyChargeLimitEnabled"].(bool)
	if !ok {
		return false
	}
	if !enabled {
		return true
	}
	limit, limitOK := kimiNumber(asMap(wallet["monthlyChargeLimit"])["priceInCents"])
	used, usedOK := kimiNumber(asMap(wallet["monthlyUsed"])["priceInCents"])
	return limitOK && usedOK && limit > 0 && used >= 0 && used < limit
}

// KimiIdentity uses the managed /me response, never a token-derived hash.
func KimiIdentity(body []byte) (uid, email, plan string) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return
	}
	if _, ok := m["error"]; ok {
		return
	}
	if _, ok := m["errors"]; ok {
		return
	}
	uid, _ = m["user_id"].(string)
	email, _ = m["email"].(string)
	plan, _ = m["user_level_name"].(string)
	return strings.TrimSpace(uid), strings.TrimSpace(email), strings.TrimSpace(plan)
}

// ParseKimiSubscriptionStats reads the web membership gateway's
// GetSubscriptionStats response, the same call the Kimi desktop app's
// subscription page makes. subscriptionBalance is the monthly quota:
// amountUsedRatio is the used share of it and kimiCodeUsedRatio is the
// coding share, mirroring limit_month_total / limit_month_code.
func ParseKimiSubscriptionStats(status int, body []byte) (Result, bool) {
	r := Result{Class: Unknown, Source: "web_subscription"}
	if status != 200 || len(body) == 0 || !json.Valid(body) {
		return r, false
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return r, false
	}
	b := asMap(root["subscriptionBalance"])
	total, ok := kimiNumber(b["amountUsedRatio"])
	if !ok || total < 0 {
		return r, false
	}
	monthly := Bucket{ID: "monthly", UsedPct: math.Min(100, math.Max(0, total*100))}
	if s, _ := b["expireTime"].(string); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			monthly.ResetsAt = t.Unix()
		}
	}
	r.Buckets = append(r.Buckets, monthly)
	if code, ok := kimiNumber(b["kimiCodeUsedRatio"]); ok && code >= 0 {
		r.Buckets = append(r.Buckets, Bucket{ID: "monthly_code", UsedPct: math.Min(100, math.Max(0, code*100)), ResetsAt: monthly.ResetsAt})
	}
	r.UsedPct, r.ResetsAt = monthly.UsedPct, monthly.ResetsAt
	r.Class = OK
	switch {
	case monthly.UsedPct >= 99.5:
		// Unlike /usages, the wallet here exposes its real state, so an
		// active wallet with spendable balance is a genuine soft state.
		if kimiWebBoosterAvailable(root["boosterWallets"]) {
			r.Class = Soft
		} else {
			r.Class = Exhausted
		}
	case monthly.UsedPct >= 90:
		r.Class = Soft
	}
	return r, true
}

func kimiWebBoosterAvailable(v any) bool {
	arr, _ := v.([]any)
	for _, item := range arr {
		w := asMap(item)
		if w == nil {
			continue
		}
		if s, _ := w["status"].(string); s != "" && s != "STATUS_ACTIVE" {
			continue
		}
		if kimiMoneyCents(w["moneyLeft"]) <= 0 {
			continue
		}
		if enabled, ok := w["monthlyChargeLimitEnabled"].(bool); ok && enabled {
			limit := kimiMoneyCents(w["monthlyChargeLimit"])
			used := kimiMoneyCents(w["monthlyUsed"])
			if limit > 0 && used >= limit {
				continue
			}
		}
		return true
	}
	return false
}

func kimiMoneyCents(v any) float64 {
	m := asMap(v)
	if m == nil {
		return -1
	}
	n, ok := kimiNumber(m["priceInCents"])
	if !ok {
		return -1
	}
	return n
}

// MergeKimiWebStats folds the web monthly quota into a /usages result. A
// monthly window already reported by /usages keeps precedence; an
// unauthenticated or unconfirmed observation keeps its status and only
// gains the display buckets.
func MergeKimiWebStats(r, web Result) Result {
	if len(web.Buckets) == 0 || KimiHasBucket(r.Buckets, "monthly") {
		return r
	}
	r.Buckets = MergeBuckets(r.Buckets, web.Buckets)
	webResets := web.ResetsAt
	if webResets == 0 {
		webResets = web.Buckets[0].ResetsAt
	}
	if web.UsedPct > r.UsedPct {
		r.UsedPct, r.ResetsAt = web.UsedPct, webResets
	}
	switch r.Class {
	case OK:
		if web.Class == Exhausted || web.Class == Soft {
			r.Class = web.Class
		}
	case Soft:
		if web.Class == Exhausted {
			r.Class = Exhausted
		}
	}
	return r
}

// KimiHasBucket reports whether a bucket id is already present.
func KimiHasBucket(buckets []Bucket, id string) bool {
	for _, b := range buckets {
		if b.ID == id {
			return true
		}
	}
	return false
}
