package managed

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const continuation = "Continue the work interrupted by the account usage limit from the last unfinished step. Do not repeat work already completed."

// Only documented interactive options are accepted. In particular, settings,
// plugin injection and credential overrides could disable the safety protocol.
var valueFlags = map[string]bool{
	"--model": true, "--fallback-model": true, "--effort": true,
	"--permission-mode": true, "--allowedTools": true, "--allowed-tools": true,
	"--disallowedTools": true, "--disallowed-tools": true,
	"--tools": true, "--agent": true, "--agents": true,
	"--system-prompt": true, "--append-system-prompt": true,
	"--system-prompt-file": true, "--append-system-prompt-file": true,
	"--max-budget-usd": true, "--name": true,
}
var boolFlags = map[string]bool{
	"--verbose": true, "--dangerously-skip-permissions": true,
	"--allow-dangerously-skip-permissions": true,
}

func parseArgs(args []string, resume string, needsContinue bool) ([]string, error) {
	var out []string
	var prompts []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			prompts = append(prompts, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			prompts = append(prompts, arg)
			continue
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		switch name {
		case "--resume", "-r", "--session-id":
			values := []string{arg}
			if !hasInline && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				values = append(values, args[i])
			} else if !hasInline && name == "--session-id" {
				return nil, errors.New("--session-id requires a value")
			}
			if hasInline && inline == "" {
				return nil, fmt.Errorf("%s requires a nonempty value", name)
			}
			if resume == "" {
				out = append(out, values...)
			}
		case "--continue", "-c":
			if hasInline {
				return nil, fmt.Errorf("%s does not accept a value", name)
			}
			if resume == "" {
				out = append(out, arg)
			}
		default:
			if valueFlags[name] {
				out = append(out, arg)
				if !hasInline {
					if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
						return nil, fmt.Errorf("%s requires a value", name)
					}
					i++
					out = append(out, args[i])
				}
			} else if boolFlags[name] && !hasInline {
				out = append(out, arg)
			} else {
				return nil, fmt.Errorf("managed Claude does not support %s; headless, bare, safe-mode, background, persistence, settings, plugin, credential overrides and unknown options are rejected", name)
			}
		}
	}
	if resume != "" {
		out = append(out, "--resume", resume)
		if needsContinue {
			out = append(out, "--", continuation)
		}
	} else if len(prompts) != 0 {
		if len(prompts) == 1 {
			switch prompts[0] {
			case "auth", "login", "logout", "setup-token", "update", "upgrade", "install", "doctor", "mcp", "plugin", "plugins", "agents", "config":
				return nil, errors.New("managed Claude requires an interactive session, not a CLI subcommand")
			}
		}
		out = append(out, "--")
		out = append(out, prompts...)
	}
	return out, nil
}

func supportedVersion(output string) bool {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return false
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) != 3 {
		return false
	}
	want := []int{2, 1, 312}
	var parsed [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return false
		}
		parsed[i] = n
	}
	for i, n := range parsed {
		if n > want[i] {
			return true
		}
		if n < want[i] {
			return false
		}
	}
	return true
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func recoveryCommand(bin, cwd, session string) string {
	if session == "" {
		return "session ID unavailable; inspect the original terminal and Claude session history before resuming"
	}
	return "cd " + shellQuote(cwd) + " && " + shellQuote(bin) + " --resume " + shellQuote(session)
}
