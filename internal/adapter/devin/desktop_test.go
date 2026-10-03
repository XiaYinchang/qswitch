package devin

import (
	"testing"

	"qswitch/internal/adapter"
)

func TestIndependentDesktopDoesNotBlockCLISwitch(t *testing.T) {
	ad := Adapter{List: func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 42, Command: "/Applications/Devin.app/Contents/MacOS/Devin"}}, nil
	}}
	h, err := ad.AutoBlockers(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if h.AutoBusy() || h.ManualBusy() || len(h.AllPIDs()) != 0 {
		t.Fatal("independent desktop credentials must not block switching CLI credentials")
	}
}
