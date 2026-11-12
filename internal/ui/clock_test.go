package ui

import "testing"

func TestAnIdlePaneStopsTickingUntilItsNextBlink(t *testing.T) {
	m, _ := harness(t, 80, 24)
	m.poseTick = 1000
	m.Update(m.spin.Tick())
	if m.ticking {
		t.Fatal("an idle pane kept the twelve-a-second tick going")
	}
	if want := nextTick(1001, aluxBlinkGap, 0); m.wakeAt != want {
		t.Errorf("idle wake set for tick %d, want the blink at %d", m.wakeAt, want)
	}

	m.snap.Busy = true
	m.Update(m.spin.Tick())
	if !m.ticking {
		t.Error("a busy pane stopped ticking")
	}
}
