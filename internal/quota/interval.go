package quota

import "time"

type Sample struct {
	T       time.Time
	UsedPct float64
	Class   Class
}

const (
	DefaultIntervalMin = 10 * time.Minute
	DefaultIntervalMax = 2 * time.Hour
	DefaultETADivisor  = 6.0
)

// Interval from observed burn rate, not a fixed used% threshold.
//
// Small plans drop used% fast → short ETA → check often.
// Large plans / idle accounts drop slowly → long ETA → check rarely.
// 90% used with no recent burn still waits a long time (weekly cap sitting full).
func Interval(samples []Sample, min, max time.Duration, etaDiv float64) time.Duration {
	if min <= 0 {
		min = DefaultIntervalMin
	}
	if max < min {
		max = DefaultIntervalMax
	}
	if etaDiv < 2 {
		etaDiv = DefaultETADivisor
	}
	pts := usable(samples)
	if len(pts) == 0 {
		return max
	}
	last := pts[len(pts)-1]
	remaining := 100 - last.UsedPct
	if remaining < 0 {
		remaining = 0
	}
	burn := burnPctPerHour(pts)
	if burn <= 1e-6 {
		return noBurnInterval(remaining, min, max)
	}
	etaHours := remaining / burn
	d := time.Duration(etaHours / etaDiv * float64(time.Hour))
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
}

func usable(samples []Sample) []Sample {
	var out []Sample
	for _, s := range samples {
		if s.Class == Unknown || s.Class == Expired {
			continue
		}
		if s.UsedPct < 0 || s.UsedPct > 100 {
			continue
		}
		out = append(out, s)
	}
	return out
}

// burnPctPerHour uses the suffix after the last usage drop (quota window reset).
func burnPctPerHour(pts []Sample) float64 {
	if len(pts) < 2 {
		return 0
	}
	start := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].UsedPct+4 < pts[i-1].UsedPct {
			start = i
		}
	}
	pts = pts[start:]
	if len(pts) < 2 {
		return 0
	}
	first, last := pts[0], pts[len(pts)-1]
	dt := last.T.Sub(first.T).Hours()
	if dt < 1.0/60 {
		return 0
	}
	du := last.UsedPct - first.UsedPct
	if du <= 0 {
		return 0
	}
	return du / dt
}

// No rate yet: interpolate by remaining only. Not a 90% cliff.
func noBurnInterval(remaining float64, min, max time.Duration) time.Duration {
	if remaining <= 0 {
		return min
	}
	if remaining >= 100 {
		return max
	}
	span := float64(max - min)
	d := min + time.Duration(remaining/100*span)
	return d
}
