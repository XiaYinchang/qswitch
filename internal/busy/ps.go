package busy

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	"qswitch/internal/adapter"
)

type PS struct {
	Run func() ([]byte, error)
}

func (p PS) List() ([]adapter.Proc, error) {
	var out []byte
	var err error
	if p.Run != nil {
		out, err = p.Run()
	} else {
		return platformProcesses()
	}
	if err != nil {
		return nil, err
	}
	var list []adapter.Proc
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		pidStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil {
			continue
		}
		list = append(list, adapter.Proc{PID: pid, Command: strings.TrimSpace(rest)})
	}
	return list, sc.Err()
}

func CodexCLI(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		c := p.Command
		if strings.Contains(c, "ChatGPT.app/") {
			continue
		}
		name := ExecutableName(firstArg(c))
		if name == "codex" || strings.HasPrefix(name, "codex-") {
			out = append(out, p)
		}
	}
	return out
}

func ChatGPTApp(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		if (strings.Contains(p.Command, "ChatGPT.app/") && strings.Contains(p.Command, "MacOS/ChatGPT")) || ExecutableName(firstArg(p.Command)) == "chatgpt" {
			out = append(out, p)
		}
	}
	return out
}

func GrokCLI(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		c := p.Command
		if strings.Contains(c, "Grok Bot.app/") {
			continue
		}
		name := ExecutableName(firstArg(c))
		if name == "grok" || strings.HasPrefix(name, "grok-") {
			out = append(out, p)
		}
	}
	return out
}

func CursorApp(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		c := p.Command
		if (strings.Contains(c, "Cursor.app/Contents/MacOS/Cursor") || ExecutableName(firstArg(c)) == "cursor") && !strings.Contains(c, "--type=") && !strings.Contains(c, "Helper") {
			out = append(out, p)
		}
	}
	return out
}

func GrokBotApp(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		if (strings.Contains(p.Command, "Grok Bot.app/Contents/MacOS/Grok Bot") || ExecutableName(firstArg(p.Command)) == "grok bot") && !strings.Contains(p.Command, "--type=") && !strings.Contains(p.Command, "Helper") && !strings.Contains(p.Command, "local-exec-daemon") {
			out = append(out, p)
		}
	}
	return out
}

func CursorAgent(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		name := ExecutableName(firstArg(p.Command))
		if name == "cursor-agent" || name == "agent" && strings.Contains(p.Command, "cursor-agent") {
			out = append(out, p)
		}
	}
	return out
}

func DevinCLI(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		c := p.Command
		if strings.Contains(c, "Devin.app/") {
			continue
		}
		name := ExecutableName(firstArg(c))
		if name == "devin" {
			out = append(out, p)
		}
	}
	return out
}

func DevinApp(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		if strings.Contains(p.Command, "Devin.app/Contents/MacOS/Devin") && !strings.Contains(p.Command, "Helper") {
			out = append(out, p)
		}
	}
	return out
}

// CommandArgs splits native process command lines without executing shell syntax.
// Backslashes remain path characters, including quoted Windows executables.
func CommandArgs(command string) []string {
	var args []string
	var word strings.Builder
	var quote rune
	for _, r := range strings.TrimSpace(command) {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '"' || r == '\'' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			if word.Len() > 0 {
				args = append(args, word.String())
				word.Reset()
			}
			continue
		}
		word.WriteRune(r)
	}
	if word.Len() > 0 {
		args = append(args, word.String())
	}
	return args
}

func ExecutableName(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(path), ".exe")
}

func firstArg(command string) string {
	args := CommandArgs(command)
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return processAlive(pid)
}
