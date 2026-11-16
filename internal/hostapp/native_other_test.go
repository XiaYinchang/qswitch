//go:build !darwin

package hostapp

import "testing"

func TestDesktopCommandOnlySelectsAntigravityMain(t *testing.T) {
	for _, test := range []struct {
		command string
		want    bool
	}{
		{`/usr/share/antigravity/antigravity --reuse-window`, true},
		{`"C:\Program Files\Antigravity\Antigravity.exe" --reuse-window`, true},
		{`"C:\Program Files\Antigravity\Antigravity.exe" --type=renderer`, false},
		{`/usr/share/antigravity/antigravity --type=utility`, false},
		{`/bin/sh -c antigravity`, false},
		{`/usr/bin/antigravity-agent`, false},
		{`/opt/Other/antigravity-helper`, false},
		{``, false},
	} {
		if got := desktopCommand(test.command, "antigravity"); got != test.want {
			t.Errorf("desktopCommand(%q)=%v, want %v", test.command, got, test.want)
		}
	}
}
