package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Tool string

const (
	Codex  Tool = "codex"
	Grok   Tool = "grok"
	Cursor Tool = "cursor"
)

func ParseTool(s string) (Tool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "codex":
		return Codex, nil
	case "grok":
		return Grok, nil
	case "cursor":
		return Cursor, nil
	default:
		return "", fmt.Errorf("unknown tool %q", s)
	}
}

func AllTools() []Tool { return []Tool{Codex, Grok, Cursor} }

type Identity struct {
	Tool        Tool   `json:"tool"`
	StableID    string `json:"stable_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	PlanHint    string `json:"plan_hint,omitempty"`
}

type Warning string

const (
	WarnDesyncDesktop     Warning = "desync_desktop"
	WarnDesyncCLI         Warning = "desync_cli"
	WarnStaleCLITokens    Warning = "stale_cli_tokens"
	WarnApiKeyMode        Warning = "api_key_mode"
	WarnIncompleteBlob    Warning = "incomplete_blob"
	WarnChatGPTAppRunning Warning = "chatgpt_app_running"
	WarnCursorAppRunning  Warning = "cursor_app_running"
	WarnGrokDualBinary    Warning = "grok_dual_binary"
)

type Blob struct {
	Tool       Tool
	Identity   Identity
	CapturedAt time.Time
	Incomplete bool
	StaleCLI   bool
	Payload    json.RawMessage // never log
}

type Proc struct {
	PID     int
	Command string
}

type Holders struct {
	Manual     []Proc
	AutoExtra  []Proc // ChatGPT.app for Codex
	ChatGPTApp bool
	CursorApp  bool
	Why        []string
}

func (h Holders) ManualBusy() bool { return len(h.Manual) > 0 }
func (h Holders) AutoBusy() bool   { return h.ManualBusy() || len(h.AutoExtra) > 0 }

func (h Holders) ManualPIDs() []int {
	var out []int
	for _, p := range h.Manual {
		out = append(out, p.PID)
	}
	return out
}

func (h Holders) AllPIDs() []int {
	var out []int
	for _, p := range h.Manual {
		out = append(out, p.PID)
	}
	for _, p := range h.AutoExtra {
		out = append(out, p.PID)
	}
	return out
}

type BusyError struct {
	Holders Holders
}

func (e *BusyError) Error() string {
	pids := e.Holders.ManualPIDs()
	return fmt.Sprintf("busy blocked_by_pid=%v %s", pids, strings.Join(e.Holders.Why, ";"))
}

type Adapter interface {
	Name() Tool
	LivePaths(home string) []string
	Fingerprint(home string) ([32]byte, error)
	Capture(home string) ([]Blob, []Warning, error)
	Restore(home string, blob Blob, opts RestoreOpts) error
	IdentityOf(blob Blob) (Identity, error)
	ManualBlockers(home string) (Holders, error)
	AutoBlockers(home string) (Holders, error)
	Idle(home string, grace time.Duration) bool
	KillCLI(home string) error
}

type RestoreOpts struct {
	ForceAlign    bool
	AllowStaleCLI bool
	AllowChatGPT  bool // unused; ChatGPT is not a manual blocker
	CLIOnly       bool // Cursor: write ~/.cursor only, leave state.vscdb / Cursor.app alone
}

type Lister interface {
	List() ([]Proc, error)
}
