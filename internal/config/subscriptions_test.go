package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewSubscriptionsPreserveExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[general]\nauto_switch = false\n[devin]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.General.AutoSwitch || cfg.Devin.Enabled || !cfg.Kimi.Enabled || !cfg.ZCode.Enabled {
		t.Fatal("old settings or new defaults changed")
	}
	for _, tool := range []string{"kimi", "zcode", "devin"} {
		if err := cfg.SetToolEnabled(tool, false); err != nil {
			t.Fatal(err)
		}
		if cfg.ToolEnabled(tool) {
			t.Fatalf("%s not disabled", tool)
		}
	}
	if err := cfg.SetToolEnabled("all", true); err != nil {
		t.Fatal(err)
	}
	if !cfg.Kimi.Enabled || !cfg.ZCode.Enabled || !cfg.Devin.Enabled {
		t.Fatal("all omitted subscriptions")
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	saved, err := Load(path)
	if err != nil || saved != cfg {
		t.Fatalf("config roundtrip: %v", err)
	}
}
