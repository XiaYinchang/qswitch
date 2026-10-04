package app

import (
	"qswitch/internal/adapter"
	"qswitch/internal/state"
)

// IdentityLabel matches the account label shown by the dashboard.
func IdentityLabel(id adapter.Identity) string {
	if id.Tool == adapter.Kimi && id.Phone != "" {
		return id.Phone
	}
	if id.Tool == adapter.Kimi && id.DisplayName != "" {
		return id.DisplayName
	}
	if id.Email != "" {
		return id.Email
	}
	if id.DisplayName != "" {
		return id.DisplayName
	}
	if id.Phone != "" {
		return id.Phone
	}
	return id.StableID
}

func accountLabel(ac state.Account) string {
	return IdentityLabel(adapter.Identity{Tool: adapter.Tool(ac.Tool), StableID: ac.StableID, Email: ac.Email, Phone: ac.Phone, DisplayName: ac.DisplayName})
}
