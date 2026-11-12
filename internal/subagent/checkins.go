package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/agent"
)

const (
	stallAfter     = 8 * time.Minute
	stallTick      = 30 * time.Second
	repeatCalls    = 4
	progressEvery  = 25
	checkInsPerRun = 6
	checkInWakeGap = 15 * time.Minute
)

// CheckIn is something alux noticed about a detached run that is still going.
type CheckIn struct {
	Run  int
	Text string
}

// watch is what check-ins remember about one detached run.
type watch struct {
	generation int
	call       string
	streak     int
	repeated   bool
	stalledAt  time.Time
	progress   int
	alerts     int
}

// watchFor returns the run's watch, creating it; nil for a run nobody is waiting on. Lock held.
func (r *Roster) watchFor(h *handle) *watch {
	if !h.run.Background || h.run.Parent != 0 {
		return nil
	}
	if r.watches == nil {
		r.watches = map[int]*watch{}
	}
	w := r.watches[h.run.ID]
	if w == nil {
		w = &watch{generation: h.generation}
		r.watches[h.run.ID] = w
	}
	return w
}

// queueCheckIn records a check-in and returns the waker when it may wake an idle session. Lock held.
func (r *Roster) queueCheckIn(run *Run, w *watch, text string, alert bool, now time.Time) func() {
	if w.generation != r.generation {
		return nil
	}
	if alert {
		if w.alerts >= checkInsPerRun {
			return nil
		}
		w.alerts++
	}
	r.checkins = append(r.checkins, CheckIn{Run: run.ID, Text: text})
	if !alert || r.OnCheckIn == nil || (!r.lastWake.IsZero() && now.Sub(r.lastWake) < checkInWakeGap) {
		return nil
	}
	r.lastWake = now
	return r.OnCheckIn
}

func (r *Roster) fire(wakes ...func()) {
	r.changed()
	for _, wake := range wakes {
		if wake != nil {
			wake()
		}
	}
}

func who(run *Run) string {
	return fmt.Sprintf("run %d (%s · %s)", run.ID, run.Label(), run.Agent)
}

// observeStep queues a progress line each time the run crosses another progressEvery steps.
func (h *handle) observeStep(n int) {
	r := h.roster
	r.mu.Lock()
	var wake func()
	if w := r.watchFor(h); w != nil && n/progressEvery > w.progress {
		w.progress = n / progressEvery
		wake = r.queueCheckIn(h.run, w, "progress: "+statusLine(*h.run, time.Now()), false, time.Now())
	}
	r.mu.Unlock()
	r.fire(wake)
}

// observeCall notes a finished tool call, and raises an alert when the same call keeps repeating.
func (h *handle) observeCall(call agent.Call) {
	now := time.Now()
	fp := fingerprint(call)
	r := h.roster
	r.mu.Lock()
	h.run.LastActive = now
	var wake func()
	if w := r.watchFor(h); w != nil && call.Name != AskTool {
		if fp == w.call {
			w.streak++
		} else {
			w.call, w.streak, w.repeated = fp, 1, false
		}
		if w.streak >= repeatCalls && !w.repeated {
			w.repeated = true
			wake = r.queueCheckIn(h.run, w, fmt.Sprintf("%s has made the same call %d times in a row: %s. It may be "+
				"going in circles; check it with %s or stop it with %s.", who(h.run), w.streak, firstLine(fp), StatusTool, CancelTool),
				true, now)
		}
	}
	r.mu.Unlock()
	r.fire(wake)
}

// fingerprint is the call with its input re-encoded, so key order does not hide a repeat.
func fingerprint(call agent.Call) string {
	input := []byte(call.Input)
	var v any
	if json.Unmarshal(call.Input, &v) == nil {
		if canonical, err := json.Marshal(v); err == nil {
			input = canonical
		}
	}
	return call.Name + " " + string(input)
}

// scanStalls raises an alert for each watched run with nothing new for stallAfter, once per quiet spell.
func (r *Roster) scanStalls(now time.Time) {
	r.mu.Lock()
	var wakes []func()
	for _, run := range r.runs {
		w := r.watches[run.ID]
		if w == nil || run.State != Running {
			continue
		}
		if _, asking := r.requests[run.ID]; asking {
			continue
		}
		last := run.LastStep
		if run.LastActive.After(last) {
			last = run.LastActive
		}
		if last.IsZero() || now.Sub(last) < stallAfter || w.stalledAt.Equal(last) {
			continue
		}
		w.stalledAt = last
		doing := run.Activity
		if doing == "" {
			doing = "thinking"
		}
		wakes = append(wakes, r.queueCheckIn(run, w, fmt.Sprintf("%s looks stalled: nothing new for %s, currently %s at "+
			"step %d/%d. Check it with %s, or stop it with %s.", who(run), short(now.Sub(last)), doing, run.Step, run.MaxSteps,
			StatusTool, CancelTool), true, now))
	}
	r.mu.Unlock()
	r.fire(wakes...)
}

func (r *Roster) watchStalls(ctx context.Context) {
	tick := time.NewTicker(stallTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			r.scanStalls(now)
		}
	}
}

// TakeCheckIns drains the check-ins the main agent has not been shown.
func (r *Roster) TakeCheckIns() []CheckIn {
	r.mu.Lock()
	out := r.checkins
	r.checkins = nil
	r.mu.Unlock()
	return out
}

// CheckInBlock is how check-ins are put to the main agent.
func CheckInBlock(checkins []CheckIn) string {
	if len(checkins) == 0 {
		return ""
	}
	lines := make([]string, 0, len(checkins))
	for _, c := range checkins {
		lines = append(lines, "- "+c.Text)
	}
	return "<subagent-check-ins>\nalux noticed these about detached runs that are still going. Nothing is blocked " +
		"on you; act only where one needs it.\n\n" + strings.Join(lines, "\n") + "\n</subagent-check-ins>"
}
