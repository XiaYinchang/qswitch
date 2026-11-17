package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/config"
	"qswitch/internal/managed"
	"qswitch/internal/quota"
	"qswitch/internal/state"
)

type Overview struct {
	Instance InstanceInfo `json:"instance"`
	Tools    []ToolView   `json:"tools"`
}

type ToolView struct {
	DerivedFrom  string                `json:"derived_from,omitempty"`
	AutoReason   string                `json:"auto_reason,omitempty"`
	Capabilities map[string]Capability `json:"capabilities"`
	CaptureError string                `json:"capture_error,omitempty"`
	Tool         string                `json:"tool"`
	LiveID       string                `json:"live_id"`
	Enabled      bool                  `json:"enabled"`
	Auto         bool                  `json:"auto"`
	Switchable   bool                  `json:"switchable"`
	Desync       bool                  `json:"desync"`
	Busy         bool                  `json:"busy"`
	ChatGPTApp   bool                  `json:"chatgpt_app"`
	CursorApp    bool                  `json:"cursor_app"`
	GrokBotApp   bool                  `json:"grok_bot_app"`
	DevinApp     bool                  `json:"devin_app"`
	BlockedPIDs  []int                 `json:"blocked_by_pid"`
	PendingTo    string                `json:"pending_to,omitempty"`
	Managed      []ManagedView         `json:"managed,omitempty"`
	// ManagedUnavailable means runner state could not be read this time;
	// blocking PIDs may still be managed sessions.
	ManagedUnavailable bool          `json:"managed_unavailable,omitempty"`
	Accounts           []AccountView `json:"accounts"`
}

// ManagedView reports the sessions that qswitch coordinates during rotation.
// Plain Claude processes are neither shown as blockers nor controlled.
type ManagedView struct {
	PID           int    `json:"pid"`
	Ready         bool   `json:"ready"`
	NeedsContinue bool   `json:"needs_continue"`
	Reason        string `json:"reason,omitempty"`
	Recovery      string `json:"recovery,omitempty"`
}

type AccountView struct {
	Tool            string         `json:"tool"`
	StableID        string         `json:"stable_id"`
	Email           string         `json:"email"`
	Phone           string         `json:"phone,omitempty"`
	DisplayName     string         `json:"display_name,omitempty"`
	Plan            string         `json:"plan"`
	Live            bool           `json:"live"`
	Desktop         bool           `json:"desktop"`
	Class           string         `json:"class"`
	UsedPct         float64        `json:"used_pct"`
	RemainingPct    *float64       `json:"remaining_pct"`
	CoolingUntil    int64          `json:"cooling_until"`
	ResetsAt        int64          `json:"resets_at"`
	Incomplete      bool           `json:"incomplete"`
	StaleCLI        bool           `json:"stale_cli"`
	LastProbedAt    int64          `json:"last_probed_at"`
	LastProbeClass  string         `json:"last_probe_class,omitempty"`
	LastProbeSource string         `json:"last_probe_source,omitempty"`
	LastProbeAt     int64          `json:"last_probe_at,omitempty"`
	LastQuotaAt     int64          `json:"last_quota_at,omitempty"`
	QuotaStale      bool           `json:"quota_stale"`
	LastCapturedAt  int64          `json:"last_captured_at"`
	Buckets         []quota.Bucket `json:"buckets,omitempty"`
	Derived         bool           `json:"derived,omitempty"`
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
	out := Overview{Instance: a.Instance()}
	cfg := a.cfgSnapshot()
	now := a.now().Unix()
	for _, t := range a.ManagedTools() {
		p, _ := a.State.GetPointer(string(t))
		ad := a.Adapters[t]
		man, _ := ad.ManualBlockers(a.UserHome)
		aut, _ := ad.AutoBlockers(a.UserHome)
		if t == adapter.Claude {
			man = aut
		}
		pend, _ := a.State.GetPending(string(t))
		caps := a.Capabilities(t)
		tv := ToolView{
			Capabilities: caps,
			CaptureError: a.captureStatus(t),
			Tool:         string(t),
			LiveID:       p.StableID,
			Enabled:      cfg.ToolEnabled(string(t)),
			Auto:         cfg.General.AutoSwitch && cfg.ToolEnabled(string(t)) && caps["auto_switch"].Available,
			Switchable:   caps["switch"].Available,
			Desync:       p.Desync && (t != adapter.Cursor || a.cursorDesktopSync()),
			Busy:         man.ManualBusy(),
			ChatGPTApp:   aut.ChatGPTApp,
			CursorApp:    aut.CursorApp || man.CursorApp,
			GrokBotApp:   aut.GrokBotApp,
			DevinApp:     aut.DevinApp,
			BlockedPIDs:  man.ManualPIDs(),
			PendingTo:    pend.ToID,
		}
		if !tv.Auto {
			switch {
			case !caps["auto_switch"].Available:
				tv.AutoReason = caps["auto_switch"].Reason
			case !cfg.General.AutoSwitch:
				tv.AutoReason = "全局自动切换已关闭；仅开启此工具不会开启全局开关"
			case !cfg.ToolEnabled(string(t)):
				tv.AutoReason = "此工具的自动切换已关闭，额度查询继续运行"
			}
		}
		if t == adapter.Antigravity {
			tv.CaptureError = a.antigravityCaptureStatus()
		}
		if t == adapter.Claude {
			sessions, err := managed.Status(a.DataDir)
			tv.ManagedUnavailable = err != nil
			for _, s := range sessions {
				if s.Exited && s.Recovery == "" {
					continue
				}
				tv.Managed = append(tv.Managed, ManagedView{PID: s.ChildPID, Ready: s.Ready, NeedsContinue: s.NeedsContinue, Reason: s.Reason, Recovery: s.Recovery})
			}
		}
		accs, _ := a.State.ListAccounts(string(t))
		for _, ac := range accs {
			if hideOverviewAccount(t, ac) {
				continue
			}
			view := accountView(ac, p.StableID, now)
			if last, err := a.State.LatestQuota(ac.Tool, ac.StableID); err == nil {
				view.LastProbeClass, view.LastProbeSource, view.LastProbeAt = last.Class, last.Source, last.Ts
				view.QuotaStale = view.QuotaStale || last.Class == string(quota.Unknown) || last.Class == string(quota.Expired)
				switch quota.Class(last.Class) {
				case quota.OK, quota.Soft, quota.Exhausted:
					view.LastQuotaAt = last.Ts
				default:
					if confirmed, err := a.State.LatestConfirmedQuota(ac.Tool, ac.StableID); err == nil {
						view.LastQuotaAt = confirmed.Ts
					}
				}
			}
			maxAge := cfg.IntervalMax() + 2*time.Minute // Allow one daemon tick.
			if !view.Live {
				maxAge = config.IdleQuotaInterval + 2*time.Minute
			}
			view.QuotaStale = view.QuotaStale || view.LastQuotaAt <= 0 || view.LastQuotaAt > now ||
				now-view.LastQuotaAt >= int64(maxAge.Seconds()) ||
				(ac.LastResetsAt > view.LastQuotaAt && ac.LastResetsAt <= now)
			// A confirmed exhaustion holds until its reset; the daemon does not
			// ask again before then, and a failed request does not withdraw it.
			if known, _ := a.knownExhausted(t, ac); known {
				view.QuotaStale = false
			}
			tv.Accounts = append(tv.Accounts, view)
		}
		if t == adapter.Cursor {
			cursor, bot := splitCursorBotAt(tv, now, int64(max(cfg.IntervalMax(), config.IdleQuotaInterval).Seconds())+120)
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
	return splitCursorBotAt(tv, time.Now().Unix(), int64(config.IdleQuotaInterval.Seconds())+120)
}

func splitCursorBotAt(tv ToolView, now, maxAge int64) (ToolView, ToolView) {
	bot := ToolView{
		DerivedFrom:  "cursor",
		Capabilities: map[string]Capability{},
		Tool:         "grokbot",
		LiveID:       tv.LiveID,
		Busy:         tv.Busy,
		CursorApp:    tv.CursorApp,
	}
	for _, action := range []string{"capture", "probe", "switch", "auto_switch", "login", "run", "desktop_sync", "live_refresh", "parked_refresh", "continuation"} {
		bot.Capabilities[action] = Capability{Reason: "只支持查询用量，账号在 Cursor 中管理"}
	}
	bot.Capabilities["probe"] = Capability{Available: true}
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
		class := string(botB.Class)
		if class == "" || botB.ObservedAt <= 0 {
			class = string(quota.Unknown)
		}
		bc := ac
		bc.Tool = "cursor"
		bc.Plan = botCardPlan(botB.Plan)
		bc.Class = class
		bc.LastProbedAt = botB.ObservedAt
		bc.LastProbeAt = botB.ObservedAt
		bc.LastProbeClass = class
		bc.LastQuotaAt = botB.ObservedAt
		bc.QuotaStale = class == string(quota.Unknown) || class == string(quota.Expired) || botB.ObservedAt <= 0 || botB.ObservedAt > now || now-botB.ObservedAt >= maxAge || (botB.ResetsAt > botB.ObservedAt && botB.ResetsAt <= now)
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
		Phone:          ac.Phone,
		DisplayName:    ac.DisplayName,
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
		QuotaStale:     class == string(quota.Expired) || class == string(quota.Unknown),
		LastCapturedAt: ac.LastCapturedAt,
		Buckets:        quota.DecodeBuckets(ac.QuotaDetail),
	}
}

func (a *App) ProbeAccount(ctx context.Context, tool adapter.Tool, id string) quota.Result {
	return a.probeAccount(ctx, tool, id, false)
}

// ProbeAccountRef resolves an explicit account before making a provider request.
func (a *App) ProbeAccountRef(ctx context.Context, tool adapter.Tool, ref string) (quota.Result, error) {
	if err := a.CheckAction(tool, "probe"); err != nil {
		return quota.Result{}, err
	}
	id, err := a.resolveAccount(string(tool), ref)
	if err != nil {
		return quota.Result{}, err
	}
	if _, err := a.State.GetAccount(string(tool), id); err != nil {
		return quota.Result{}, fmt.Errorf("%s account not found", tool)
	}
	return a.ProbeAccount(ctx, tool, id), nil
}
