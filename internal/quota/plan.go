package quota

import (
	"encoding/json"
	"strings"
	"unicode"
)

// ExtractPlan returns the first non-empty plan/tier string in v.
func ExtractPlan(v any) string {
	for _, key := range []string{
		"subscription_tier_display",
		"subscriptionTier",
		"subscription_tier",
		"plan_type",
		"chatgpt_plan_type",
		"planName",
	} {
		if s, ok := findString(v, []string{key}); ok {
			s = strings.TrimSpace(s)
			if s != "" && s != "-" {
				return s
			}
		}
	}
	if m := proMultiplier(v); m != "" {
		return "pro " + m
	}
	return ""
}

func PlanFromJSON(body []byte) string {
	if len(body) == 0 || !json.Valid(body) {
		return ""
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return ExtractPlan(v)
}

func ParseCursorPlan(body []byte) string {
	if len(body) == 0 || !json.Valid(body) {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	pi, _ := m["planInfo"].(map[string]any)
	if pi == nil {
		return DisplayPlan("cursor", ExtractPlan(m))
	}
	if s, _ := pi["planName"].(string); strings.TrimSpace(s) != "" {
		return formatCursorPlan(s)
	}
	return DisplayPlan("cursor", ExtractPlan(pi))
}

func withPlan(kind Kind, r Result, v any) Result {
	if r.Plan == "" {
		r.Plan = DisplayPlan(string(kind), ExtractPlan(v))
	}
	if r.Plan == "" {
		if m := proMultiplier(v); m != "" && kind == KindCodex {
			r.Plan = "pro " + m
		}
	}
	return r
}

// DisplayPlan turns a stored/raw plan into the badge label.
func DisplayPlan(tool, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "-" || strings.EqualFold(raw, "desktop") {
		return ""
	}
	switch tool {
	case "codex":
		return formatCodexPlan(raw)
	case "grok":
		return formatGrokPlan(raw)
	case "cursor":
		return formatCursorPlan(raw)
	default:
		return raw
	}
}

// MergePlan keeps a more specific label (pro 5x) when the new value is generic (pro).
func MergePlan(old, new string) string {
	new = strings.TrimSpace(new)
	if new == "" || new == "-" {
		return old
	}
	old = strings.TrimSpace(old)
	if old == "" || old == "-" {
		return new
	}
	ol, nl := strings.ToLower(old), strings.ToLower(new)
	if ol == nl {
		return new
	}
	if strings.HasPrefix(ol, nl+" ") {
		return old
	}
	return new
}

func formatCodexPlan(raw string) string {
	s := foldPlan(raw)
	switch {
	case s == "free":
		return "free"
	case s == "go":
		return "go"
	case s == "plus":
		return "plus"
	case s == "team":
		return "team"
	case s == "business":
		return "business"
	case s == "enterprise":
		return "enterprise"
	case s == "edu" || s == "education" || s == "eduplus" || s == "edu plus":
		return "edu"
	case looksPro20x(s):
		return "pro 20x"
	case looksPro5x(s):
		return "pro 5x"
	case s == "pro":
		return "pro"
	default:
		return strings.TrimSpace(raw)
	}
}

func formatGrokPlan(raw string) string {
	s := strings.TrimSpace(raw)
	key := strings.ToLower(strings.ReplaceAll(s, " ", ""))
	switch key {
	case "supergrokheavy":
		return "SuperGrok Heavy"
	case "supergrokplus":
		return "SuperGrok Plus"
	case "supergroklite":
		return "SuperGrok Lite"
	case "supergrokpro":
		return "SuperGrok Pro"
	case "supergrok":
		return "SuperGrok"
	case "xpremiumplus", "premiumplus":
		return "Premium+"
	case "xpremium", "premium":
		return "Premium"
	}
	if strings.Contains(s, " ") {
		return s
	}
	return s
}

func formatCursorPlan(raw string) string {
	s := foldPlan(raw)
	s = strings.ReplaceAll(s, "+", "plus")
	s = strings.ReplaceAll(s, " ", "")
	switch s {
	case "ultra":
		return "Ultra"
	case "proplus":
		return "Pro+"
	case "pro":
		return "Pro"
	case "free", "hobby":
		return "Hobby"
	case "business":
		return "Business"
	case "team":
		return "Team"
	case "enterprise":
		return "Enterprise"
	default:
		// GetPlanInfo already returns "Ultra"
		if raw == strings.TrimSpace(raw) && hasUpper(raw) {
			return raw
		}
		return strings.TrimSpace(raw)
	}
}

func foldPlan(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	return strings.Join(strings.Fields(s), " ")
}

func looksPro20x(s string) bool {
	if s == "20x" || s == "pro20x" || s == "pro 20x" {
		return true
	}
	return strings.Contains(s, "pro") && strings.Contains(s, "20") && strings.Contains(s, "x")
}

func looksPro5x(s string) bool {
	if s == "5x" || s == "pro5x" || s == "pro 5x" {
		return true
	}
	return strings.Contains(s, "pro") && strings.Contains(s, "5") && strings.Contains(s, "x") && !looksPro20x(s)
}

func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func proMultiplier(v any) string {
	var found string
	walk(v, func(k string, val any) {
		lk := strings.ToLower(k)
		if !strings.Contains(lk, "plan") && !strings.Contains(lk, "tier") && !strings.Contains(lk, "multipl") && lk != "promo" {
			return
		}
		s, ok := val.(string)
		if !ok || s == "" {
			return
		}
		fs := foldPlan(s)
		switch {
		case looksPro20x(fs):
			found = "20x"
		case looksPro5x(fs) && found != "20x":
			found = "5x"
		}
	})
	return found
}
