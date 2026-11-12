package subagent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
)

func detachedRun(r *Roster) *handle {
	h := r.start("coder", "fix parser", "fix the parser", 60, true)
	step(h, 1)
	return h
}

// step is a step as the run loop reports it.
func step(h *handle, n int) {
	h.step(n)
	h.observeStep(n)
}

func bash(input string) agent.Call {
	return agent.Call{Name: "bash", Input: json.RawMessage(input)}
}

// The same call made repeatedly is flagged once, whatever order its input's keys came in.
func TestARepeatedCallIsFlaggedOnce(t *testing.T) {
	r := NewRoster(nil)
	h := detachedRun(r)
	h.observeCall(bash(`{"command":"go test","timeout":5}`))
	h.observeCall(bash(`{"timeout":5,"command":"go test"}`))
	h.observeCall(bash(`{"command":"go test","timeout":5}`))
	if got := r.TakeCheckIns(); len(got) != 0 {
		t.Fatalf("flagged after three calls: %+v", got)
	}
	h.observeCall(bash(`{"timeout":5,"command":"go test"}`))
	h.observeCall(bash(`{"command":"go test","timeout":5}`))
	got := r.TakeCheckIns()
	if len(got) != 1 || !strings.Contains(got[0].Text, "same call 4 times") {
		t.Fatalf("check-ins = %+v, want one repeat alert", got)
	}
}

// A run with nothing new for stallAfter is flagged once, and again only after it moves and stalls anew.
func TestAStalledRunIsFlaggedOncePerQuietSpell(t *testing.T) {
	r := NewRoster(nil)
	h := detachedRun(r)
	start := time.Now()
	r.scanStalls(start.Add(stallAfter - time.Minute))
	r.scanStalls(start.Add(stallAfter + time.Minute))
	r.scanStalls(start.Add(stallAfter + 2*time.Minute))
	if got := r.TakeCheckIns(); len(got) != 1 || !strings.Contains(got[0].Text, "looks stalled") {
		t.Fatalf("check-ins = %+v, want one stall alert", got)
	}
	step(h, 2)
	r.scanStalls(time.Now().Add(stallAfter + time.Minute))
	if got := r.TakeCheckIns(); len(got) != 1 {
		t.Fatalf("check-ins = %+v, want the second stall flagged", got)
	}
}

// A run waiting on the main agent is not stuck, however long the wait.
func TestARunWaitingOnTheMainAgentIsNotFlaggedAsStalled(t *testing.T) {
	r := NewRoster(nil)
	h := detachedRun(r)
	if _, err := r.post(h, KindQuestion, "which file?"); err != nil {
		t.Fatal(err)
	}
	r.scanStalls(time.Now().Add(stallAfter + time.Minute))
	if got := r.TakeCheckIns(); len(got) != 0 {
		t.Fatalf("a run blocked on a question was flagged: %+v", got)
	}
}

// A progress line comes once per progressEvery steps, even when a refunded step repeats a number.
func TestProgressIsReportedOncePerStretchOfSteps(t *testing.T) {
	r := NewRoster(nil)
	h := detachedRun(r)
	for _, n := range []int{24, 25, 25, 26, 49} {
		h.observeStep(n)
	}
	if got := r.TakeCheckIns(); len(got) != 1 || !strings.HasPrefix(got[0].Text, "progress:") {
		t.Fatalf("check-ins = %+v, want one progress line", got)
	}
}

// An idle session is woken by at most one alert per checkInWakeGap.
func TestAlertsWakeAnIdleSessionAtMostOncePerGap(t *testing.T) {
	r := NewRoster(nil)
	wakes := 0
	r.OnCheckIn = func() { wakes++ }
	a, b := detachedRun(r), detachedRun(r)
	for _, h := range []*handle{a, b} {
		for range repeatCalls {
			h.observeCall(bash(`{"command":"make"}`))
		}
	}
	r.scanStalls(time.Now().Add(stallAfter + time.Minute))
	if wakes != 1 {
		t.Errorf("wakes = %d, want 1", wakes)
	}
	if got := r.TakeCheckIns(); len(got) != 4 {
		t.Errorf("check-ins = %d, want all four queued for the next turn", len(got))
	}
}

// After a clear, a run from before it reports nothing, and nothing queued survives.
func TestClearingDropsCheckIns(t *testing.T) {
	r := NewRoster(nil)
	h := detachedRun(r)
	h.observeStep(25)
	r.DiscardOutcomes()
	for range repeatCalls {
		h.observeCall(bash(`{"command":"make"}`))
	}
	if got := r.TakeCheckIns(); len(got) != 0 {
		t.Fatalf("check-ins survived the clear: %+v", got)
	}
}
