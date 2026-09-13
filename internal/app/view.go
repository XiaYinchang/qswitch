package app

import (
	"context"
	"strings"

	"qswitch/internal/adapter"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

type Overview struct {
	Tools []ToolView `json:"tools"`
}

type ToolView struct {
	Tool        string        `json:"tool"`
	LiveID      string        `json:"live_id"`
	Enabled     bool          `json:"enabled"`
	Auto        bool          `json:"auto"`
	Desync      bool          `json:"desync"`
	Busy        bool          `json:"busy"`
	ChatGPTApp  bool          `json:"chatgpt_app"`
	CursorApp   bool          `json:"cursor_app"`
	GrokBotApp  bool          `json:"grok_bot_app"`
	BlockedPIDs []int         `json:"blocked_by_pid"`
	PendingTo   string        `json:"pending_to,omitempty"`
	Accounts    []AccountView `json:"accounts"`
}

type AccountView struct {
	Tool           string         `json:"tool"`
	StableID       string         `json:"stable_id"`
	Email          string         `json:"email"`
	Plan           string         `json:"plan"`
	Live           bool           `json:"live"`
	Desktop        bool           `json:"desktop"`
	Class          string         `json:"class"`
	UsedPct        float64        `json:"used_pct"`
	RemainingPct   *float64       `json:"remaining_pct"`
	CoolingUntil   int64          `json:"cooling_until"`
	ResetsAt       int64          `json:"resets_at"`
	Incomplete     bool           `json:"incomplete"`
	StaleCLI       bool           `json:"stale_cli"`
	LastProbedAt   int64          `json:"last_probed_at"`
	LastCapturedAt int64          `json:"last_captured_at"`
	Buckets        []quota.Bucket `json:"buckets,omitempty"`
	Derived        bool           `json:"derived,omitempty"`
}

func remainingPct(class string, used float64) *float64 {
	switch quota.Class(class) {
	case quota.OK, quota.Soft, quota.Exhausted:
		r := 100 - used
		if r < 0 {
			r = 0
		}
		if r > 100 {
			r = 100
		}
		return &r
	default:
		return nil
	}
}

func (a *App) Overview() Overview {
	var out Overview
	now := a.now().Unix()
	for _, t := range adapter.AllTools() {
		p, _ := a.State.GetPointer(string(t))
		ad := a.Adapters[t]
		man, _ := ad.ManualBlockers(a.UserHome)
		aut, _ := ad.AutoBlockers(a.UserHome)
		pend, _ := a.State.GetPending(string(t))
		tv := ToolView{
			Tool:        string(t),
			LiveID:      p.StableID,
			Enabled:     a.Cfg.ToolEnabled(string(t)),
			Auto:        a.Cfg.General.AutoSwitch && a.Cfg.ToolEnabled(string(t)),
			Desync:      p.Desync,
			Busy:        man.ManualBusy(),
			ChatGPTApp:  aut.ChatGPTApp,
			CursorApp:   aut.CursorApp || man.CursorApp,
			GrokBotApp:  aut.GrokBotApp,
			BlockedPIDs: man.ManualPIDs(),
			PendingTo:   pend.ToID,
		}
		accs, _ := a.State.ListAccounts(string(t))
		for _, ac := range accs {
			if hideOverviewAccount(t, ac) {
				continue
			}
			tv.Accounts = append(tv.Accounts, accountView(ac, p.StableID, now))
		}
		if t == adapter.Cursor {
			cursor, bot := splitCursorBot(tv)
			out.Tools = append(out.Tools, cursor)
			if len(bot.Accounts) > 0 {
				out.Tools = append(out.Tools, bot)
			}
			continue
		}
		out.Tools = append(out.Tools, tv)
	}
	return out
}

func hideOverviewAccount(_ adapter.Tool, ac state.Account) bool {
	return strings.HasPrefix(ac.StableID, "desktop:")
}

func splitCursorBot(tv ToolView) (ToolView, ToolView) {
	bot := ToolView{
		Tool:      "grokbot",
		LiveID:    tv.LiveID,
		Busy:      tv.Busy,
		CursorApp: tv.CursorApp,
	}
	cursor := tv
	cursor.Accounts = nil
	for _, ac := range tv.Accounts {
		var rest []quota.Bucket
		var botB *quota.Bucket
		for i := range ac.Buckets {
			if ac.Buckets[i].ID == "bot" {
				b := ac.Buckets[i]
				botB = &b
				continue
			}
			rest = append(rest, ac.Buckets[i])
		}
		ac.Buckets = rest
		cursor.Accounts = append(cursor.Accounts, ac)
		if botB == nil || ac.Desktop {
			continue
		}
		used := botB.UsedPct
		class := "ok"
		switch {
		case used >= 99.5:
			class = "exhausted"
		case used >= 90:
			class = "soft"
		}
		bc := ac
		bc.Tool = "cursor"
		bc.Plan = botCardPlan(botB.Plan)
		bc.Class = class
		bc.UsedPct = used
		bc.RemainingPct = remainingPct(class, used)
		bc.ResetsAt = botB.ResetsAt
		bc.CoolingUntil = 0
		bc.Buckets = nil
		bc.StaleCLI = false
		bc.Derived = true
		bot.Accounts = append(bot.Accounts, bc)
	}
	return cursor, bot
}

func botCardPlan(label string) string {
	s := strings.TrimSpace(label)
	if s == "" {
		return "-"
	}
	folded := strings.ToLower(strings.Join(strings.Fields(s), " "))
	folded = strings.ReplaceAll(folded, "-", " ")
	switch folded {
	case "grok bot", "grokbot", "grok bot plan", "grokbot plan":
		return "-"
	}
	return s
}

func visibleCooling(ac state.Account, now int64) int64 {
	if quota.Class(ac.LastQuotaClass) != quota.Exhausted {
		return 0
	}
	if ac.CoolingUntil <= now {
		return 0
	}
	return ac.CoolingUntil
}

func accountView(ac state.Account, liveID string, now int64) AccountView {
	plan := quota.DisplayPlan(ac.Tool, ac.PlanHint)
	if plan == "" {
		plan = "-"
	}
	class := ac.LastQuotaClass
	if class == "" {
		class = "unknown"
	}
	return AccountView{
		Tool:           ac.Tool,
		StableID:       ac.StableID,
		Email:          ac.Email,
		Plan:           plan,
		Live:           ac.StableID == liveID && liveID != "",
		Desktop:        strings.HasPrefix(ac.StableID, "desktop:"),
		Class:          class,
		UsedPct:        ac.LastUsedPct,
		RemainingPct:   remainingPct(class, ac.LastUsedPct),
		CoolingUntil:   visibleCooling(ac, now),
		ResetsAt:       ac.LastResetsAt,
		Incomplete:     ac.Incomplete,
		StaleCLI:       ac.StaleCLI,
		LastProbedAt:   ac.LastProbedAt,
		LastCapturedAt: ac.LastCapturedAt,
		Buckets:        quota.DecodeBuckets(ac.QuotaDetail),
	}
}

func (a *App) ProbeAccount(ctx context.Context, tool adapter.Tool, id string) quota.Result {
	return a.probeAccount(ctx, tool, id, false)
}
