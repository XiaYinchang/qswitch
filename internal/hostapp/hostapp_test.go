package hostapp

import (
	"testing"
	"time"
)

func TestWaitUntil(t *testing.T) {
	c := Controller{Timeout: time.Second, Sleep: func(time.Duration) {}}
	n := 0
	if err := c.WaitUntil(func() bool {
		n++
		return n >= 3
	}); err != nil {
		t.Fatal(err)
	}
	c.Timeout = time.Millisecond
	if err := c.WaitUntil(func() bool { return false }); err == nil {
		t.Fatal("want timeout")
	}
}
