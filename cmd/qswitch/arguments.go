package main

import (
	"fmt"
	"qswitch/internal/adapter"
	"strings"
)

// Validate qswitch arguments before opening the data directory or key store.
// Native arguments after `run <provider>` belong to that provider unchanged.
func validateArguments(args []string) (adapter.Tool, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("missing command")
	}
	cmd, rest := args[0], args[1:]
	if cmd == "run" {
		if len(rest) == 0 {
			return "", fmt.Errorf("run requires antigravity or claude")
		}
		switch rest[0] {
		case "antigravity", "agy":
			return adapter.Antigravity, nil
		case "claude":
			return adapter.Claude, nil
		default:
			return "", fmt.Errorf("unsupported run tool %q", rest[0])
		}
	}
	allowed := map[string]bool{"capture": true, "list": true, "status": true, "doctor": true, "serve": true, "probe": true, "apply": true, "enable-auto": true, "disable-auto": true}
	known := map[string]bool{"init": true, "sessions": true, "switch": true, "login": true, "forget": true, "web": true, "version": true, "--version": true}
	if !allowed[cmd] && !known[cmd] {
		return "", fmt.Errorf("unknown command %q", cmd)
	}
	var selected adapter.Tool
	seen := false
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if arg != "--tool" && !strings.HasPrefix(arg, "--tool=") {
			continue
		}
		if !allowed[cmd] {
			return "", fmt.Errorf("--tool is not supported by %s", cmd)
		}
		if seen {
			return "", fmt.Errorf("--tool may be specified only once")
		}
		seen = true
		value := strings.TrimPrefix(arg, "--tool=")
		if arg == "--tool" {
			i++
			if i == len(rest) {
				return "", fmt.Errorf("--tool requires a value")
			}
			value = rest[i]
		}
		tool, err := adapter.ParseTool(value)
		if err != nil {
			return "", err
		}
		selected = tool
	}
	if cmd == "switch" || cmd == "forget" || cmd == "login" {
		need := 2
		if cmd == "login" {
			need = 1
		}
		if len(rest) < need {
			return "", fmt.Errorf("%s requires a tool and account where applicable", cmd)
		}
		tool, err := adapter.ParseTool(rest[0])
		if err != nil {
			return "", err
		}
		selected = tool
		if cmd == "login" && tool != adapter.Codex && tool != adapter.Grok && tool != adapter.Claude {
			return "", fmt.Errorf("use native %s login, then qswitch capture --tool %s", tool, tool)
		}
	}
	if cmd == "probe" {
		if value, ok := flagVal(rest, "--account"); ok || has(rest, "--account") {
			if selected == "" || strings.TrimSpace(value) == "" || strings.HasPrefix(value, "--") {
				return "", fmt.Errorf("--account requires an account and --tool")
			}
		}
	}
	return selected, nil
}
