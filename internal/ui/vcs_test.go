package ui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/vcs"
)

// reads records what the status line asked jj for, and whether each read was allowed to
// snapshot the working copy.
type reads struct{ live []bool }

// vcsHarness pins the clock so the throttle is driven rather than waited on, and swaps the jj
// reader for a recorder.
func vcsHarness(t *testing.T) (*Model, *session.Session, *reads, func(d time.Duration)) {
	t.Helper()
	m, s := harness(t, 80, 24)
	r := &reads{}
	m.readVCS = func(live bool, _ vcs.Scope) *vcs.Status {
		r.live = append(r.live, live)
		return &vcs.Status{ChangeID: "abcdefgh"}
	}
	now := time.Now()
	m.now = func() time.Time { return now }
	m.vcsAt = now.Add(-time.Hour)
	return m, s, r, func(d time.Duration) { now = now.Add(d) }
}

// drive sends a message and runs whatever commands come back, so a recorded read is a read the
// program would really have performed.
func drive(m *Model, msg tea.Msg) {
	_, cmd := m.Update(msg)
	run(cmd)
}

func run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range batch {
			run(c)
		}
	}
}

// The complaint this answers: during a long turn the status line froze at whatever the working
// copy was when the turn started, so there was no telling where the branch had got to.
func TestToolBoxRefreshesTheStatusLine(t *testing.T) {
	m, s, r, advance := vcsHarness(t)
	s.SetBusyForTest(true)
	drive(m, sessionChanged{})
	before := len(r.live)

	s.StartToolForTest("call-1", "jj commit -m work", agent.KindBash)
	advance(vcsInterval)
	drive(m, sessionChanged{})

	if len(r.live) == before {
		t.Fatal("a tool box appeared mid-turn and the status line was never re-read")
	}
}

// A tool that finishes is the moment the working copy is most likely to have moved: the box
// resolving has to count as its own trigger, since the item total does not change.
func TestResolvedToolRefreshesTheStatusLine(t *testing.T) {
	m, s, r, advance := vcsHarness(t)
	s.SetBusyForTest(true)
	s.StartToolForTest("call-1", "jj commit -m work", agent.KindBash)
	advance(vcsInterval)
	drive(m, sessionChanged{})
	before := len(r.live)

	s.ResolveToolForTest("call-1", "jj commit -m work", "Working copy now at: xyz", agent.KindBash)
	advance(vcsInterval)
	drive(m, sessionChanged{})

	if len(r.live) == before {
		t.Fatal("a tool finished mid-turn and the status line was never re-read")
	}
}

// Reading jj while the agent is working must not snapshot the working copy: a snapshot taken
// under the agent's own jj command makes that command fail.
func TestMidTurnReadDoesNotSnapshot(t *testing.T) {
	m, s, r, advance := vcsHarness(t)
	s.SetBusyForTest(true)
	s.StartToolForTest("call-1", "jj new", agent.KindBash)
	advance(vcsInterval)
	drive(m, sessionChanged{})

	if len(r.live) == 0 {
		t.Fatal("no read at all")
	}
	for i, live := range r.live {
		if live {
			t.Errorf("read %d snapshotted the working copy while the agent was working", i)
		}
	}
}

// The turn ending is still the authoritative read, and the only one that may snapshot.
func TestTurnEndReadSnapshots(t *testing.T) {
	m, s, r, _ := vcsHarness(t)
	s.SetBusyForTest(true)
	drive(m, sessionChanged{})
	r.live = nil

	s.SetBusyForTest(false)
	drive(m, sessionChanged{})

	if len(r.live) != 1 || !r.live[0] {
		t.Errorf("turn end reads = %v; want one snapshotting read", r.live)
	}
}

// Streaming deltas arrive many times a second. Each one forks jj if nothing holds them back.
func TestStatusLineReadsAreThrottled(t *testing.T) {
	m, s, r, advance := vcsHarness(t)
	s.SetBusyForTest(true)
	drive(m, sessionChanged{})
	r.live = nil

	for i := 0; i < 10; i++ {
		s.StartToolForTest("call", "bash", agent.KindBash)
		advance(vcsInterval / 10)
		drive(m, sessionChanged{})
	}

	if len(r.live) > 2 {
		t.Errorf("%d reads inside one throttle window; want at most 2", len(r.live))
	}
	if len(r.live) == 0 {
		t.Error("the throttle swallowed every read")
	}
}

// A change that arrives inside the cooldown is not lost: the next thing to happen picks it up,
// because a throttled refresh leaves the transcript signature unmatched.
func TestAThrottledChangeIsPickedUpLater(t *testing.T) {
	m, s, r, advance := vcsHarness(t)
	s.SetBusyForTest(true)
	s.StartToolForTest("call-1", "bash", agent.KindBash)
	drive(m, sessionChanged{}) // reads, and starts the cooldown

	s.StartToolForTest("call-2", "bash", agent.KindBash)
	drive(m, sessionChanged{}) // inside the cooldown, dropped
	r.live = nil

	advance(vcsInterval)
	drive(m, sessionChanged{}) // nothing new happened, but the dropped change is still owed

	if len(r.live) != 1 {
		t.Errorf("reads after the cooldown = %d; want the dropped refresh to land", len(r.live))
	}
}

// Coming back to a pane that has sat idle for an hour: the working copy may have moved under it
// from another terminal entirely.
func TestFocusRefreshesTheStatusLine(t *testing.T) {
	m, _, r, advance := vcsHarness(t)
	drive(m, sessionChanged{})
	r.live = nil

	advance(time.Hour)
	drive(m, tea.FocusMsg{})

	if len(r.live) != 1 {
		t.Fatalf("focus reads = %d; want one", len(r.live))
	}
	if !r.live[0] {
		t.Error("an idle pane read without snapshotting; between turns the full read is safe")
	}
}

// Focus and blur fire as the user clicks between panes, which must not fork jj each time.
func TestRefocusingQuicklyDoesNotRead(t *testing.T) {
	m, _, r, _ := vcsHarness(t)
	drive(m, tea.FocusMsg{})
	r.live = nil

	drive(m, tea.BlurMsg{})
	drive(m, tea.FocusMsg{})

	if len(r.live) != 0 {
		t.Errorf("refocusing immediately read jj %d times", len(r.live))
	}
}
