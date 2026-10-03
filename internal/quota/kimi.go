package quota

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// ParseKimiUsage follows the managed /usages contract. Missing windows stay
// missing; limit_month_code is the code share of monthly total, not a limit.
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
