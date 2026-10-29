package notify

import (
	"os/exec"
	"strings"
	"sync"
)

type Notifier interface {
	Send(title, body string)
}

type OS struct{ Enabled bool }

func (o OS) Send(title, body string) {
	if !o.Enabled {
		return
	}
	title = sanitize(title)
	body = sanitize(body)
	script := `display notification "` + escapeAS(body) + `" with title "` + escapeAS(title) + `"`
	_ = exec.Command("osascript", "-e", script).Run()
}

// Log records notifications; daemon workers may send concurrently.
type Log struct {
	mu   sync.Mutex
	Msgs []string
}

func (l *Log) Send(title, body string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Msgs = append(l.Msgs, title+": "+body)
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 180 {
		s = s[:180]
	}
	return s
}

func escapeAS(s string) string {
	return strings.ReplaceAll(s, `"`, `'`)
}
