package quota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
)

// ZCode reads the personal Coding Plan used by the desktop's active account.
// The official monitor endpoint takes the plan API key verbatim, not an OAuth
// token or a synthesized Bearer token. Team plans require a separate contract.
func (h HTTP) ZCode(ctx context.Context, key, provider string) (Result, error) {
	unknown := Result{Class: Unknown, Source: "http"}
	var origin string
	switch provider {
	case "bigmodel":
		origin = "https://bigmodel.cn"
	case "zai":
		origin = "https://api.z.ai"
	default:
		return unknown, errors.New("zcode: unsupported Coding Plan provider")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return unknown, errors.New("zcode: missing Coding Plan API key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return unknown, err
	}
	if err := checkHost(req.URL); err != nil {
		return unknown, err
	}
	req.Header.Set("Authorization", key)
	client := *h.client()
	previousRedirectCheck := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" || next.URL.Host != req.URL.Host {
			return errors.New("zcode: quota redirect changed the credential destination")
		}
		if previousRedirectCheck != nil {
			return previousRedirectCheck(next, via)
		}
		if len(via) >= 10 {
			return errors.New("zcode: too many quota redirects")
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		return unknown, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return unknown, err
	}
	return ClassifyZCode(res.StatusCode, body), nil
}

type zcodeLimit struct {
	Type          string   `json:"type"`
	Unit          int      `json:"unit"`
	Number        int      `json:"number"`
	Usage         *float64 `json:"usage"`
	CurrentValue  *float64 `json:"currentValue"`
	Remaining     *float64 `json:"remaining"`
	Percentage    *float64 `json:"percentage"`
	NextResetTime *float64 `json:"nextResetTime"`
}

// ClassifyZCode checks both HTTP and the provider's business envelope. Model
// windows determine availability; the separate monthly MCP limit does not.
func ClassifyZCode(status int, body []byte) Result {
	r := Result{Class: Unknown, Source: "http"}
	if status == 401 || status == 403 {
		r.Class = Expired
		return r
	}
	if status != http.StatusOK {
		return r
	}
	var env struct {
		Code    *int  `json:"code"`
		Success *bool `json:"success"`
		Data    *struct {
			Level  string       `json:"level"`
			Limits []zcodeLimit `json:"limits"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil || env.Data == nil ||
		(env.Success != nil && !*env.Success) ||
		(env.Code != nil && *env.Code != 0 && *env.Code != 200) ||
		(env.Code == nil && (env.Success == nil || !*env.Success)) {
		return r
	}
	r.Authenticated = true
	r.Plan = strings.TrimSpace(env.Data.Level)
	buckets := make(map[string]Bucket)
	modelKnown := false
	var exhaustedReset int64
	exhaustedResetUnknown := false
	for _, limit := range env.Data.Limits {
		id := zcodeBucketID(limit)
		if id == "" {
			continue
		}
		if _, duplicate := buckets[id]; duplicate {
			return r
		}
		pct, ok := zcodeUsedPercent(limit)
		if !ok {
			if id != "mcp" {
				return r
			}
			continue
		}
		var reset int64
		// The desktop passes nextResetTime directly to JavaScript Date: ms.
		if limit.NextResetTime != nil && *limit.NextResetTime > 0 && *limit.NextResetTime < math.MaxInt64 {
			reset = int64(*limit.NextResetTime / 1000)
		}
		buckets[id] = Bucket{ID: id, UsedPct: pct, ResetsAt: reset}
		if id != "mcp" {
			if !modelKnown || pct > r.UsedPct || (pct == r.UsedPct && reset > r.ResetsAt) {
				r.UsedPct, r.ResetsAt = pct, reset
			}
			if pct >= 100 {
				if reset == 0 {
					exhaustedResetUnknown = true
				}
				if reset > exhaustedReset {
					exhaustedReset = reset
				}
			}
			modelKnown = true
		}
	}
	if !modelKnown {
		return r
	}
	for _, id := range []string{"5h", "weekly", "mcp"} {
		if b, ok := buckets[id]; ok {
			r.Buckets = append(r.Buckets, b)
		}
	}
	switch {
	case r.UsedPct >= 100:
		r.Class = Exhausted
		r.ResetsAt = exhaustedReset
		if exhaustedResetUnknown {
			r.ResetsAt = 0
		}
	case r.UsedPct >= 90:
		r.Class = Soft
	default:
		r.Class = OK
	}
	return r
}

func zcodeBucketID(l zcodeLimit) string {
	if l.Type == "TOKENS_LIMIT" || l.Type == "CREDIT_LIMIT" {
		if l.Unit == 3 && l.Number == 5 {
			return "5h"
		}
		if l.Unit == 6 && l.Number == 1 {
			return "weekly"
		}
	}
	if l.Type == "TIME_LIMIT" && l.Unit == 5 && l.Number == 1 {
		return "mcp"
	}
	return ""
}

func zcodeUsedPercent(l zcodeLimit) (float64, bool) {
	// A precise exhausted counter takes precedence over a rounded percentage.
	if l.Usage != nil && *l.Usage > 0 && l.Remaining != nil && *l.Remaining == 0 {
		return 100, true
	}
	var pct float64
	switch {
	case l.Percentage != nil:
		pct = *l.Percentage
	case l.Usage != nil && *l.Usage > 0 && l.Remaining != nil:
		if *l.Remaining < 0 || *l.Remaining > *l.Usage {
			return 0, false
		}
		pct = (1 - *l.Remaining / *l.Usage) * 100
	case l.Usage != nil && *l.Usage > 0 && l.CurrentValue != nil:
		pct = *l.CurrentValue / *l.Usage * 100
	default:
		return 0, false
	}
	if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
		return 0, false
	}
	return math.Min(pct, 100), true
}
