package config

import (
	"testing"
	"time"
)

func TestIntervalBounds(t *testing.T) {
	c := Default()
	if c.IntervalMin() != 10*time.Minute {
		t.Fatalf("min %s", c.IntervalMin())
	}
	if c.IntervalMax() != 2*time.Hour {
		t.Fatalf("max %s", c.IntervalMax())
	}
	if c.ETAChecks() != 6 {
		t.Fatalf("eta %v", c.ETAChecks())
	}
}
