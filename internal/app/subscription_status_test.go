package app

import (
	"strings"
	"testing"
)

func TestStatusZCodeNeverAdvertisesAutomaticSwitching(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.General.AutoSwitch = true
	a.Cfg.ZCode.Enabled = true
	for _, line := range strings.Split(a.Status(), "\n") {
		if strings.HasPrefix(line, "zcode ") {
			if !strings.Contains(line, "auto=false") {
				t.Fatal("monitor-only ZCode advertised automatic switching")
			}
			return
		}
	}
	t.Fatal("ZCode absent from CLI status")
}
