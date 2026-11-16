package busy

import (
	"qswitch/internal/adapter"
	"testing"
)

func TestNativeProcessNamesAcrossPlatforms(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		pick          func([]adapter.Proc) []adapter.Proc
	}{
		{"codex Windows", `"C:\Program Files\Codex\codex.exe" --resume`, CodexCLI},
		{"grok Windows", `C:\Tools\grok.exe`, GrokCLI},
		{"cursor Windows", `"C:\Program Files\Cursor\cursor-agent.exe"`, CursorAgent},
		{"devin Windows", `C:\Tools\devin.exe`, DevinCLI},
		{"codex Unix", `'/opt/Agent Tools/codex' resume`, CodexCLI},
		{"cursor desktop", `"C:\Program Files\Cursor\Cursor.exe"`, CursorApp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pick([]adapter.Proc{{PID: 42, Command: tc.command}}); len(got) != 1 {
				t.Fatal("native client was missed")
			}
		})
	}
	for _, pick := range []func([]adapter.Proc) []adapter.Proc{CodexCLI, GrokCLI, CursorAgent, DevinCLI} {
		if got := pick([]adapter.Proc{{PID: 42, Command: `python.exe "codex.exe grok.exe cursor-agent.exe devin.exe"`}}); len(got) != 0 {
			t.Fatal("argument text mistaken for executable")
		}
	}
	if got := CursorApp([]adapter.Proc{{PID: 42, Command: `"C:\Cursor\Cursor.exe" --type=renderer`}}); len(got) != 0 {
		t.Fatal("renderer mistaken for app")
	}
}
