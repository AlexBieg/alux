package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/session"
	"github.com/charmbracelet/x/ansi"
)

// storedBody is what the bash_background tool leaves in the box: a sentence about a process, and
// nothing of its output. It is the thing a live box replaces and reverts to.
const storedBody = "Started as proc 1. It is still running."

// startedProc puts up the box a background command leaves behind — the head the call stored and
// that sentence under it — and starts a real process under the same call id.
func startedProc(t *testing.T, s *session.Session, call, command string) {
	t.Helper()
	label := "$ " + command + " &"
	s.StartToolForTest(call, label, agent.KindBash)
	s.ResolveToolForTest(call, label, storedBody, agent.KindBash)
	startProc(t, s, call, command)
}

// tick is the spinner frame the whole program already runs on, which is the clock a tailing box
// redraws against. Sent directly, since a test does not run the command loop that produces it.
func tick(m *Model) { send(m, spinner.TickMsg{}) }

// hasBodyLine reports whether the transcript holds a line of exactly this text. Exactly, because
// the head of a bash box quotes the command: a substring search for what the command echoes
// would find the echo itself and pass before a byte of output had been written.
func hasBodyLine(m *Model, text string) bool {
	for _, line := range m.content.lines {
		if strings.TrimSpace(unframe(ansi.Strip(line))) == text {
			return true
		}
	}
	return false
}

// A background command's box is the only place its output is visible without the agent going and
// reading it, so the last few lines belong under the head as they arrive without a click, because
// nobody clicks a box to find out whether their dev server came up.
func TestALiveProcessTailsItsOutputWithoutAClick(t *testing.T) {
	m, s := harness(t, 80, 24)
	startedProc(t, s, "call_1", "echo LIVE-OUTPUT; sleep 30")
	settle(m)

	waitForUI(t, "the output to reach the box", func() bool {
		tick(m)
		return hasBodyLine(m, "LIVE-OUTPUT")
	})
	if len(m.expanded) != 0 {
		t.Fatalf("the box was expanded, so this proves nothing: %v", m.expanded)
	}
	if hasBodyLine(m, storedBody) {
		t.Errorf("the box is still showing what the call stored rather than the output:\n%s", screen(m))
	}
}

// Nothing in the session changes when a process writes, so the pane has to go and look. The
// spinner tick is what makes it look, and it is also what must not be swallowed: a box that only
// updated when the conversation did would freeze until the agent said something.
func TestATailingBoxUpdatesWithoutTheSessionChanging(t *testing.T) {
	m, s := harness(t, 80, 24)
	startedProc(t, s, "call_1", "for i in 1 2 3 4 5; do echo line-$i; sleep 0.02; done; sleep 30")
	settle(m)

	waitForUI(t, "the first line", func() bool {
		tick(m)
		return hasBodyLine(m, "line-1")
	})
	waitForUI(t, "a later line, with no session change in between", func() bool {
		tick(m)
		return hasBodyLine(m, "line-5")
	})
}

// The exit's output goes only to the model, so the box must keep it, with the status.
func TestTheBoxKeepsTheFinalOutputWhenTheProcessExits(t *testing.T) {
	m, s := harness(t, 80, 24)
	startedProc(t, s, "call_1", "echo FIRST; sleep 0.3; echo LAST; exit 4")
	settle(m)

	waitForUI(t, "the box to hold the final output", func() bool {
		send(m, sessionChanged{})
		tick(m)
		return hasBodyLine(m, "LAST") && hasBodyLine(m, "[exit 4]")
	})
	if !hasBodyLine(m, "FIRST") {
		t.Errorf("the final box lost the early output:\n%s", screen(m))
	}
	if hasBodyLine(m, storedBody) {
		t.Errorf("the box went back to what the call stored:\n%s", screen(m))
	}
}

// A bash call is tailed from its first line, and stays off the process row while in the foreground.
func TestAForegroundCommandTailsInItsRunningBox(t *testing.T) {
	m, s := harness(t, 80, 24)
	withShells(t, s)
	label := "$ echo FOREGROUND-LINE; sleep 30"
	s.StartToolForTest("call_1", label, agent.KindBash)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		input, _ := json.Marshal(map[string]any{"command": "echo FOREGROUND-LINE; sleep 30"})
		s.Procs.Bash()(ctx, agent.Call{ID: "call_1", Name: "bash", Input: input}, agent.Callbacks{})
	}()
	t.Cleanup(func() { cancel(); <-done })
	settle(m)

	waitForUI(t, "the foreground output to reach the running box", func() bool {
		tick(m)
		return hasBodyLine(m, "FOREGROUND-LINE")
	})
	if len(m.rowProcs()) != 0 {
		t.Errorf("a foreground command was put on the process row: %+v", m.rowProcs())
	}
}

// A selection is absolute line numbers into the rendered transcript and the copy happens on
// release, so a box that grows above it mid-drag hands the user rows they never dragged across.
// Tailing therefore stops while anything is selected, and picks up again when it is dropped.
func TestOutputArrivingUnderASelectionDoesNotMoveIt(t *testing.T) {
	m, s := harness(t, 80, 24)
	startedProc(t, s, "call_1", "for i in $(seq 1 40); do echo line-$i; sleep 0.02; done; sleep 30")
	s.Show("SELECT-ME alpha beta")
	settle(m)

	waitForUI(t, "the box to start tailing", func() bool {
		tick(m)
		return hasBodyLine(m, "line-1")
	})

	// Below the box, so output arriving above it is what would slide the selection.
	row := rowContaining(t, m, "SELECT-ME alpha beta")
	dragSelect(m, screenXOf(t, m, row, "SELECT-ME"), row, screenXOf(t, m, row, "beta")+3, row)
	want := m.selectedTextForTest()
	if want == "" {
		t.Fatal("nothing was selected, so this proves nothing")
	}

	held, behind := transcriptOf(m), latestTail(t, s)
	waitForUI(t, "more output to arrive behind the selection", func() bool {
		tick(m)
		return latestTail(t, s) != behind
	})
	for range 20 {
		tick(m)
		if got := m.selectedTextForTest(); got != want {
			t.Fatalf("the selection slid onto other text: %q, want %q", got, want)
		}
	}
	if transcriptOf(m) != held {
		t.Errorf("the box moved under the selection:\n%s", transcriptOf(m))
	}

	// Dropping the selection lets it catch up again, or the freeze would be permanent.
	send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	waitForUI(t, "tailing to resume", func() bool {
		tick(m)
		return transcriptOf(m) != held
	})
}

// The freeze has to begin at the press, not at the first motion. A press alone selects nothing,
// but it has already pinned the anchor to a line number, and output growing a box above that line
// moves the text out from under an anchor the user cannot see to correct.
func TestOutputArrivingAfterThePressDoesNotMoveTheAnchor(t *testing.T) {
	m, s := harness(t, 80, 24)
	startedProc(t, s, "call_1", "for i in $(seq 1 40); do echo line-$i; sleep 0.02; done; sleep 30")
	s.Show("ANCHOR-HERE alpha beta")
	settle(m)

	waitForUI(t, "the box to start tailing", func() bool {
		tick(m)
		return hasBodyLine(m, "line-1")
	})

	// Pressed and held, with no motion yet: the gesture that is only an anchor.
	row := rowContaining(t, m, "ANCHOR-HERE alpha beta")
	from, to := screenXOf(t, m, row, "ANCHOR-HERE"), screenXOf(t, m, row, "beta")+4
	clickAt(m, from, row)

	behind := latestTail(t, s)
	waitForUI(t, "output to arrive while the press is held", func() bool {
		tick(m)
		return latestTail(t, s) != behind
	})

	// Finishing the drag now must select the words the press landed on, not whatever the output
	// pushed down into those rows.
	send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: to, Y: row})
	releaseAt(m, to, row)

	if got := m.selectedTextForTest(); !strings.Contains(got, "ANCHOR-HERE alpha beta") {
		t.Errorf("the anchor slid off the pressed text: selected %q", got)
	}
}

// transcriptOf is the rendered conversation alone, without the chrome around it, which is what a
// selection's line numbers index into.
func transcriptOf(m *Model) string { return strings.Join(m.content.lines, "\n") }

// latestTail is what the live process has written as the session would hand it over, which is
// what the pane would be showing if it were not holding still for a selection.
func latestTail(t *testing.T, s *session.Session) string {
	t.Helper()
	live := s.LiveProcs()
	if len(live) != 1 {
		t.Fatalf("got %d live processes, want 1", len(live))
	}
	return live[0].Tail
}
