// Package wake tells when the machine slept: Go's monotonic clock stops during sleep and the wall clock does not.
package wake

import (
	"context"
	"strings"
	"time"
)

const (
	// Slice is the longest Sleep waits before looking whether the machine slept, so a wake is seen within it.
	Slice = 5 * time.Second
	// Threshold is the least lead of wall time over awake time counted as sleep, not a clock adjustment.
	Threshold = 30 * time.Second
)

// Asleep is how long the machine slept since since, which must carry a monotonic reading, or 0 for under Threshold.
func Asleep(since time.Time) time.Duration {
	now := time.Now()
	if d := now.Round(0).Sub(since.Round(0)) - now.Sub(since); d >= Threshold {
		return d
	}
	return 0
}

// Sleep waits d of awake time, returning early with how long the machine slept when it wakes; ok is false when ctx ends.
func Sleep(ctx context.Context, d time.Duration) (asleep time.Duration, ok bool) {
	for {
		if ctx.Err() != nil {
			return 0, false
		}
		if d <= 0 {
			return 0, true
		}
		mark := time.Now()
		step := min(d, Slice)
		t := time.NewTimer(step)
		select {
		case <-ctx.Done():
			t.Stop()
			return 0, false
		case <-t.C:
		}
		if a := Asleep(mark); a > 0 {
			return a, true
		}
		d -= step
	}
}

// Brief renders a time asleep to the minute, as 45s, 12m or 2h5m.
func Brief(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
