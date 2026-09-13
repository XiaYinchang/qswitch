package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	HTTPIntervalFloor = 10 * time.Minute
	DefaultHTTPMin    = 10 * time.Minute
	DefaultHTTPMax    = 2 * time.Hour
	DefaultETADivisor = 6.0
	DefaultJitter     = 2 * time.Minute
	DefaultBackoff    = 2 * time.Hour
	DefaultIdleGrace  = 30 * time.Second
	DefaultWriteback  = 60 * time.Second
	DefaultCursorPoll = 20 * time.Second
	// ChatGPT/Codex windows can reset before the advertised time.
	DefaultCodexRecover = 30 * time.Minute
)

type Config struct {
	General General `toml:"general"`
	Quota   Quota   `toml:"quota"`
	Notify  Notify  `toml:"notify"`
	Codex   Tool    `toml:"codex"`
	Grok    Tool    `toml:"grok"`
	Cursor  Tool    `toml:"cursor"`
	Web     Web     `toml:"web"`
}

type General struct {
	AutoSwitch      bool   `toml:"auto_switch"`
	IdleGrace       string `toml:"idle_grace"`
	WritebackWindow string `toml:"writeback_window"`
}

type Quota struct {
	IntervalMin       string  `toml:"interval_min"`
	IntervalMax       string  `toml:"interval_max"`
	ETAChecks         float64 `toml:"eta_checks"`
	Jitter            string  `toml:"jitter"`
	ProbeIdleAccounts bool    `toml:"probe_idle_accounts"`
	HTTPBackoff       string  `toml:"http_backoff"`
}

type Notify struct {
	Enabled bool `toml:"enabled"`
}

type Tool struct {
	Enabled bool `toml:"enabled"`
}

type Web struct {
	Enabled bool   `toml:"enabled"`
	Addr    string `toml:"addr"`
}

const DefaultWebAddr = "127.0.0.1:7432"

func Default() Config {
	return Config{
		General: General{
			AutoSwitch:      true,
			IdleGrace:       "30s",
			WritebackWindow: "60s",
		},
		Quota: Quota{
			IntervalMin:       "10m",
			IntervalMax:       "2h",
			ETAChecks:         DefaultETADivisor,
			Jitter:            "2m",
			ProbeIdleAccounts: false,
			HTTPBackoff:       "2h",
		},
		Notify: Notify{Enabled: true},
		Codex:  Tool{Enabled: true},
		Grok:   Tool{Enabled: true},
		Cursor: Tool{Enabled: true},
		Web:    Web{Enabled: true, Addr: DefaultWebAddr},
	}
}

func (c Config) IdleGrace() time.Duration { return parseDur(c.General.IdleGrace, DefaultIdleGrace) }
func (c Config) Writeback() time.Duration {
	return parseDur(c.General.WritebackWindow, DefaultWriteback)
}
func (c Config) IntervalMin() time.Duration {
	d := parseDur(c.Quota.IntervalMin, DefaultHTTPMin)
	if d < HTTPIntervalFloor {
		return HTTPIntervalFloor
	}
	return d
}

func (c Config) IntervalMax() time.Duration {
	d := parseDur(c.Quota.IntervalMax, DefaultHTTPMax)
	if d < c.IntervalMin() {
		return c.IntervalMin()
	}
	return d
}

func (c Config) ETAChecks() float64 {
	if c.Quota.ETAChecks < 2 {
		return DefaultETADivisor
	}
	return c.Quota.ETAChecks
}
func (c Config) Jitter() time.Duration  { return parseDur(c.Quota.Jitter, DefaultJitter) }
func (c Config) Backoff() time.Duration { return parseDur(c.Quota.HTTPBackoff, DefaultBackoff) }
func (c Config) CursorPoll() time.Duration {
	return DefaultCursorPoll
}

func (c Config) WebAddr() string {
	if strings.TrimSpace(c.Web.Addr) == "" {
		return DefaultWebAddr
	}
	return c.Web.Addr
}

func parseDur(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func (c Config) Save(path string) error {
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (c Config) ToolEnabled(name string) bool {
	switch name {
	case "codex":
		return c.Codex.Enabled
	case "grok":
		return c.Grok.Enabled
	case "cursor":
		return c.Cursor.Enabled
	default:
		return false
	}
}

func (c *Config) SetToolEnabled(name string, on bool) error {
	switch name {
	case "codex":
		c.Codex.Enabled = on
	case "grok":
		c.Grok.Enabled = on
	case "cursor":
		c.Cursor.Enabled = on
	case "", "all":
		c.Codex.Enabled = on
		c.Grok.Enabled = on
		c.Cursor.Enabled = on
	default:
		return fmt.Errorf("unknown tool %q", name)
	}
	return nil
}
