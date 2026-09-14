package busy

import (
	"bufio"
	"bytes"
	"os/exec"
	"path/filepath"
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
		out, err = exec.Command("ps", "-axo", "pid=,command=").Output()
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
		base := firstArg(c)
		name := filepath.Base(base)
		if name == "codex" || strings.HasPrefix(name, "codex-") {
			out = append(out, p)
		}
	}
	return out
}

func ChatGPTApp(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		if strings.Contains(p.Command, "ChatGPT.app/") && strings.Contains(p.Command, "MacOS/ChatGPT") {
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
		base := firstArg(c)
		name := filepath.Base(base)
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
		if strings.Contains(c, "Cursor.app/Contents/MacOS/Cursor") && !strings.Contains(c, "Helper") {
			out = append(out, p)
		}
	}
	return out
}

func GrokBotApp(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		if strings.Contains(p.Command, "Grok Bot.app/Contents/MacOS/Grok Bot") && !strings.Contains(p.Command, "Helper") && !strings.Contains(p.Command, "local-exec-daemon") {
			out = append(out, p)
		}
	}
	return out
}

func CursorAgent(list []adapter.Proc) []adapter.Proc {
	var out []adapter.Proc
	for _, p := range list {
		name := filepath.Base(firstArg(p.Command))
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
		name := filepath.Base(firstArg(c))
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

func firstArg(command string) string {
	command = strings.TrimSpace(command)
	if i := strings.IndexByte(command, ' '); i >= 0 {
		return command[:i]
	}
	return command
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run()
	return err == nil
}
