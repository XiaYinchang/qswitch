package identityjwt

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

type Claims struct {
	Email     string
	AccountID string
	UserID    string
	Plan      string
	Exp       int64
}

func Parse(idToken string) Claims {
	var c Claims
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return c
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		b, err2 := base64.StdEncoding.DecodeString(parts[1])
		if err2 != nil {
			return c
		}
		raw = b
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return c
	}
	if s, ok := m["email"].(string); ok {
		c.Email = s
	}
	if n, ok := m["exp"].(float64); ok {
		c.Exp = int64(n)
	}
	if nested, ok := m["https://api.openai.com/auth"].(map[string]any); ok {
		if s, ok := nested["chatgpt_account_id"].(string); ok {
			c.AccountID = s
		}
		if s, ok := nested["chatgpt_plan_type"].(string); ok {
			c.Plan = s
		}
		if s, ok := nested["chatgpt_user_id"].(string); ok {
			c.UserID = s
		}
	}
	return c
}

func Expired(idToken string, now time.Time) bool {
	c := Parse(idToken)
	if c.Exp == 0 {
		return false
	}
	return now.Unix() >= c.Exp
}
