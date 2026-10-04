package quota

import (
	"math"
	"time"
)

// classifyGrokBilling reads the current subscription pool only. Product usage,
// history and on-demand spending are different quantities, not its percentage.
func classifyGrokBilling(v any, source string) Result {
	unknown := Result{Class: Unknown, Source: source}
	root := asMap(v)
	if root == nil {
		return unknown
	}
	if _, exists := root["error"]; exists {
		return unknown
	}
	if _, exists := root["errors"]; exists {
		return unknown
	}
	cfg := asMap(root["config"])
	if cfg == nil {
		return unknown
	}
	period := asMap(cfg["currentPeriod"])
	bucketID := ""
	switch period["type"] {
	case "USAGE_PERIOD_TYPE_WEEKLY":
		bucketID = "weekly"
	case "USAGE_PERIOD_TYPE_MONTHLY":
		bucketID = "monthly"
	}
	start, startOK := grokTime(period["start"])
	end, endOK := grokTime(period["end"])
	completePeriod := bucketID != "" && startOK && endOK && end.After(start)

	var pct float64
	if value, exists := cfg["creditUsagePercent"]; exists {
		var ok bool
		pct, ok = grokPercent(value)
		if !ok {
			return unknown
		}
	} else {
		limit, limitOK := grokCent(cfg, "monthlyLimit")
		used, usedOK := grokCent(cfg, "used")
		if !limitOK || !usedOK {
			return unknown
		}
		unified, _ := cfg["isUnifiedBillingUser"].(bool)
		switch {
		case limit > 0:
			pct = used / limit * 100
		case unified && completePeriod:
			// The official UI defaults an omitted percentage to zero. Require
			// the unified billing shape and a valid period before applying that
			// display default; arbitrary empty/incomplete config stays unknown.
			pct = 0
		default:
			return unknown
		}
	}
	pct = math.Max(0, math.Min(100, pct))
	prepaid, prepaidOK := grokCent(cfg, "prepaidBalance")
	cap, capOK := grokCent(cfg, "onDemandCap")
	onDemandUsed, usedOK := grokCent(cfg, "onDemandUsed")
	if !prepaidOK || !capOK || !usedOK {
		return unknown
	}
	class := OK
	switch {
	case pct >= 100:
		class = Exhausted
		// Prepaid credit may be negative under the backend's accounting
		// convention. A remaining prepaid/PAYG balance still permits use.
		if math.Abs(prepaid) > 0 || (cap > 0 && math.Abs(onDemandUsed) < cap) {
			class = Soft
		}
	case pct >= 90:
		class = Soft
	}
	var reset int64
	if endOK {
		reset = end.Unix()
	} else if legacyEnd, ok := grokTime(cfg["billingPeriodEnd"]); ok {
		reset = legacyEnd.Unix()
	}
	r := Result{Class: class, UsedPct: pct, ResetsAt: reset, Source: source}
	if completePeriod {
		r.Buckets = []Bucket{{ID: bucketID, UsedPct: pct, ResetsAt: reset}}
	}
	return r
}

func grokPercent(v any) (float64, bool) {
	// Percent is a JSON number, unlike protobuf int64 Cent values which may
	// be encoded as strings. Do not turn a malformed percent into default zero.
	if _, isString := v.(string); isString {
		return 0, false
	}
	n, ok := asFloat(v)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func grokCent(cfg map[string]any, key string) (float64, bool) {
	v, exists := cfg[key]
	if !exists {
		return 0, true
	}
	m := asMap(v)
	if m == nil {
		return 0, false
	}
	v, exists = m["val"]
	if !exists {
		return 0, true // proto3 omits a zero-valued Cent.val.
	}
	n, ok := asFloat(v)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n
}

func grokTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}
