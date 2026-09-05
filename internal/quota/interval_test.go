package quota

import (
	"testing"
	"time"
)

func ts(h float64) time.Time {
	return time.Unix(1_700_000_000, 0).Add(time.Duration(h * float64(time.Hour)))
}

func TestIntervalBurnRate(t *testing.T) {
	min, max := 10*time.Minute, 2*time.Hour
	t.Run("small plan burns fast", func(t *testing.T) {
		// 0% → 40% in 20 minutes, remaining 60% → eta 30m → /6 = 5m → clamp 10m
		s := []Sample{
			{T: ts(0), UsedPct: 0, Class: OK},
			{T: ts(20.0 / 60), UsedPct: 40, Class: OK},
		}
		d := Interval(s, min, max, 6)
		if d != min {
			t.Fatalf("got %s want min", d)
		}
	})
	t.Run("large plan burns slow", func(t *testing.T) {
		// 0% → 2% in 2h, remaining 98%, burn 1%/h, eta 98h → /6 > max
		s := []Sample{
			{T: ts(0), UsedPct: 0, Class: OK},
			{T: ts(2), UsedPct: 2, Class: OK},
		}
		d := Interval(s, min, max, 6)
		if d != max {
			t.Fatalf("got %s want max", d)
		}
	})
	t.Run("90 percent but idle weekly cap", func(t *testing.T) {
		s := []Sample{
			{T: ts(0), UsedPct: 89, Class: Soft},
			{T: ts(48), UsedPct: 90, Class: Soft},
		}
		d := Interval(s, min, max, 6)
		if d != max {
			t.Fatalf("idle 90%% should not poll fast, got %s", d)
		}
	})
	t.Run("90 percent and still dumping", func(t *testing.T) {
		s := []Sample{
			{T: ts(0), UsedPct: 70, Class: OK},
			{T: ts(0.25), UsedPct: 90, Class: Soft},
		}
		d := Interval(s, min, max, 6)
		if d != min {
			t.Fatalf("fast burn at 90%% got %s", d)
		}
	})
	t.Run("reset then slow", func(t *testing.T) {
		s := []Sample{
			{T: ts(0), UsedPct: 95, Class: Soft},
			{T: ts(1), UsedPct: 10, Class: OK},
			{T: ts(3), UsedPct: 12, Class: OK},
		}
		d := Interval(s, min, max, 6)
		if d < time.Hour {
			t.Fatalf("after reset should be slow, got %s", d)
		}
	})
	t.Run("no history", func(t *testing.T) {
		if Interval(nil, min, max, 6) != max {
			t.Fatal("empty")
		}
	})
}
