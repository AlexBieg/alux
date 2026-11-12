package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

func TestAFinishedRunIsOneTranscriptRowAndItsOutputIsInTheRunView(t *testing.T) {
	m, id := runHarness(t, 80, subagent.Run{
		Agent: "coder", Name: "wrap fix", State: subagent.Done, Step: 37,
		Started: time.Now().Add(-372 * time.Second), Ended: time.Now(),
		Result: "REPORTTEXT\n\nmore of it", Trail: []subagent.Entry{{Kind: subagent.EntryTool, Tool: agent.KindBash, Text: "TRAILTEXT"}},
	})
	shown := ansi.Strip(m.viewport.View())
	if !strings.Contains(shown, "wrap fix · coder · done in 6m12s · 37 steps") {
		t.Errorf("the transcript lacks the outcome:\n%s", shown)
	}
	for _, gone := range []string{"REPORTTEXT", "TRAILTEXT"} {
		if strings.Contains(shown, gone) {
			t.Errorf("%q reached the transcript:\n%s", gone, shown)
		}
	}
	if got := len(m.itemLines(m.snap.Items[0], m.contentWidth())); got != 1 {
		t.Errorf("a finished run takes %d transcript rows, want one", got)
	}
	m.openRun(id)
	if view := ansi.Strip(m.pane.View()); !strings.Contains(view, "REPORTTEXT") || !strings.Contains(view, "TRAILTEXT") {
		t.Errorf("the run view lacks the output:\n%s", view)
	}
}

func TestAFailedRunIsOneRedRowAndItsErrorIsInTheRunView(t *testing.T) {
	m, id := runHarness(t, 80, subagent.Run{
		Agent: "coder", Name: "wrap fix", State: subagent.Failed, Started: time.Now(), Ended: time.Now(),
		Err: "go test failed\nsecond line",
	})
	row := rowWith(t, m, "wrap fix")
	if !strings.Contains(ansi.Strip(row), "failed after") || !sameInk(inkOf(t, row, "wrap fix"), errorColor) {
		t.Errorf("the failed row reads %q", ansi.Strip(row))
	}
	if shown := ansi.Strip(m.viewport.View()); strings.Contains(shown, "go test failed") {
		t.Errorf("the error reached the transcript:\n%s", shown)
	}
	m.openRun(id)
	if view := ansi.Strip(m.pane.View()); !strings.Contains(view, "second line") {
		t.Errorf("the run view lacks the whole error:\n%s", view)
	}
}

func TestTheRunViewIsTheRunningBoxExpandedOverTheTranscript(t *testing.T) {
	m, id := runHarness(t, 140, subagent.Run{Agent: "coder", Name: "wrap fix", State: subagent.Done, Step: 37,
		Started: time.Now().Add(-372 * time.Second), Ended: time.Now(), Result: strings.Repeat("line\n", 60)})
	top, width := m.transcriptTop(), m.mainWidth()
	m.openRun(id)
	if m.transcriptTop() != top || m.mainWidth() != width {
		t.Errorf("expanding the box moved the transcript: top %d width %d, was %d and %d", m.transcriptTop(), m.mainWidth(), top, width)
	}
	x, y, w, h := m.panelRect()
	if w != 97 || x != 5 || y < top || y+h > top+m.sidebarCap() {
		t.Errorf("the expanded box sits at %d,%d %dx%d, want 97 wide from column 5 between the top bar and the input", x, y, w, h)
	}
	rows := strings.Split(screen(m), "\n")
	head := []rune(rows[y])
	if string(head[x:]) != strings.TrimLeft(rows[y], " ") || !strings.HasPrefix(rows[y][len(string(head[:x])):], "╭─ running ─") {
		t.Errorf("the box's top edge reads %q", rows[y])
	}
	if !strings.Contains(rows[y+1], "│ ● wrap fix · coder · done in 6m12s · 37 steps") {
		t.Errorf("the box's head names the run as %q", rows[y+1])
	}
	if !strings.HasPrefix(string([]rune(rows[y+h-1])[x:]), "╰") {
		t.Errorf("the box's bottom edge is not on row %d: %q", y+h-1, rows[y+h-1])
	}
	if n := strings.Count(screen(m), "╭─ running"); n != 2 {
		t.Errorf("%d running boxes show, want the expanded one beside the open sidebar:\n%s", n, screen(m))
	}
	send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.panelOpen() || strings.Contains(screen(m), "● wrap fix · coder") {
		t.Errorf("esc left the box expanded:\n%s", screen(m))
	}
}

func TestClickingTheExpandedBoxTitleOrTheTranscriptAroundItCollapsesIt(t *testing.T) {
	m, id := runHarness(t, 140, subagent.Run{Agent: "research", Name: "btop style", State: subagent.Running, MaxSteps: 25})
	m.openRun(id)
	x, y, _, _ := m.panelRect()
	clickAt(m, x+4, y)
	if m.panelOpen() {
		t.Fatal("a click on the title left the box expanded")
	}
	m.openRun(id)
	clickAt(m, x-2, y+3)
	if m.panelOpen() {
		t.Fatal("a click on the transcript beside the box left it expanded")
	}
	m.openRun(id)
	clickAt(m, x+4, y+3)
	if !m.panelOpen() {
		t.Error("a click inside the box collapsed it")
	}
}

// The run view draws the run's transcript file as boxes, and a click opens a tool's output.
func TestARunViewToolOpensToShowItsOutput(t *testing.T) {
	m, id := runHarness(t, 80, subagent.Run{Agent: "research", Name: "btop style", Dispatch: "d1", State: subagent.Running, Step: 2, MaxSteps: 25})
	m.readTranscript = func(dispatch string) ([]subagent.TranscriptEvent, error) {
		if dispatch != "d1" {
			t.Errorf("read the transcript of %q, want d1", dispatch)
		}
		return []subagent.TranscriptEvent{
			{Kind: subagent.TranscriptTask, Text: "find the bug"},
			{Kind: subagent.TranscriptText, Text: "Looking at the tests first."},
			{Kind: subagent.TranscriptTool, Label: "$ go test ./...", Tool: agent.KindBash, Output: "OUTPUT1\nl2\nl3\nl4\nl5\nl6\nl7"},
		}, nil
	}
	m.Update(m.openRun(id)())
	shown := ansi.Strip(m.pane.View())
	if !strings.Contains(shown, "$ go test ./...") || !strings.Contains(shown, "Looking at the tests first.") {
		t.Fatalf("the run view does not draw the transcript:\n%s", screen(m))
	}
	if strings.Contains(shown, "OUTPUT1") {
		t.Fatalf("a long tool output should start closed:\n%s", screen(m))
	}
	m.runView.follow = false
	m.pane.GotoTop()
	clickAt(m, 6, rowContaining(t, m, "$ go test ./..."))
	if !strings.Contains(ansi.Strip(m.pane.View()), "OUTPUT1") {
		t.Errorf("clicking the tool did not open its output:\n%s", screen(m))
	}
	clickAt(m, 6, rowContaining(t, m, "$ go test ./..."))
	if strings.Contains(ansi.Strip(m.pane.View()), "OUTPUT1") {
		t.Errorf("a second click did not close it again")
	}

	settle(m)
	if m.runView.loading {
		t.Errorf("an unmoved run is being read again")
	}
	roster(m).UpdateForTest(id, func(r *subagent.Run) { r.Step = 3 })
	settle(m)
	if !m.runView.loading {
		t.Errorf("a run that took a step is not being read again")
	}
}

func TestALongRunRowIsCutToOneRow(t *testing.T) {
	m, _ := runHarness(t, 40, subagent.Run{Agent: "coder", Name: strings.Repeat("long name ", 6), State: subagent.Done,
		Started: time.Now(), Ended: time.Now()})
	lines := m.itemLines(m.snap.Items[0], m.contentWidth())
	if len(lines) != 1 || ansi.StringWidth(lines[0]) > m.contentWidth() {
		t.Errorf("a long row should be cut to one row of the width, got %d rows: %q", len(lines), lines)
	}
}

func TestClickingTheMarkerOpensTheRunAndEscRestoresTheTranscript(t *testing.T) {
	m, s := harness(t, 80, 24)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	for range 40 {
		s.Show("filler")
	}
	id := roster.AddForTest(subagent.Run{Agent: "research", Name: "btop style", State: subagent.Running, MaxSteps: 25,
		Trail: []subagent.Entry{{Kind: subagent.EntryTool, Tool: agent.KindWeb, Text: "TRAILTEXT"}}})
	s.ShowRunForTest(id)
	for range 3 {
		s.Show("after")
	}
	settle(m)
	m.viewport.ScrollUp(2)
	m.follow = false
	saved := m.viewport.YOffset()

	expand(t, m, rowContaining(t, m, "btop style · research · dispatched"))
	if m.runView == nil || m.runView.id != id {
		t.Fatal("clicking the marker did not open its run")
	}
	if !strings.Contains(ansi.Strip(m.pane.View()), "TRAILTEXT") {
		t.Errorf("the run view lacks the trail:\n%s", screen(m))
	}
	if !strings.Contains(screen(m), runViewHint) {
		t.Errorf("the status line does not say how to leave:\n%s", screen(m))
	}
	if got := m.viewport.YOffset(); got != saved || m.follow {
		t.Errorf("the transcript under the run view moved to line %d following=%v, was %d", got, m.follow, saved)
	}

	send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.runView != nil {
		t.Fatal("esc did not close the run view")
	}
	if got := m.viewport.YOffset(); got != saved {
		t.Errorf("the transcript came back at line %d, not where it was (%d)", got, saved)
	}
}

func TestALiveRunViewFollowsNewStepsUntilScrolledUp(t *testing.T) {
	m, id := runHarness(t, 80, subagent.Run{Agent: "research", State: subagent.Running, MaxSteps: 99})
	m.openRun(id)
	step := func(n int) {
		roster(m).UpdateForTest(id, func(r *subagent.Run) {
			r.Trail = append(r.Trail, subagent.Entry{Kind: subagent.EntryTool, Tool: agent.KindBash, Text: fmt.Sprintf("STEP%d", n)})
		})
		settle(m)
	}
	for i := range 40 {
		step(i)
	}
	if !strings.Contains(ansi.Strip(m.pane.View()), "STEP39") {
		t.Fatalf("the view is not showing the newest step:\n%s", screen(m))
	}
	send(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	step(40)
	if strings.Contains(ansi.Strip(m.pane.View()), "STEP40") {
		t.Errorf("a view scrolled up was dragged back down by a new step")
	}
}

func TestAPairIsOneTranscriptEntryAndARunViewEach(t *testing.T) {
	m, s := harness(t, 80, 24)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	driver := roster.AddForTest(subagent.Run{Agent: "pair", Name: "refactor", State: subagent.Running, MaxSteps: 60})
	nav := roster.AddForTest(subagent.Run{Agent: "navigator", State: subagent.Idle, MaxSteps: 6, Parent: driver,
		Trail: []subagent.Entry{{Kind: subagent.EntryExchange, From: "driver", To: "navigator", Prompt: "sound?", Text: "NAVREPLY"}}})
	s.ShowRunForTest(driver)
	s.ShowRunForTest(nav)
	settle(m)

	if shown := ansi.Strip(m.viewport.View()); strings.Count(shown, "dispatched") != 1 || strings.Contains(shown, "navigator") {
		t.Errorf("a pair should be one marker, the driver's:\n%s", shown)
	}
	m.openRun(driver)
	if strings.Contains(ansi.Strip(m.pane.View()), "NAVREPLY") {
		t.Errorf("the driver's view carries its navigator's exchanges:\n%s", screen(m))
	}
	m.openRun(nav)
	if !strings.Contains(ansi.Strip(m.pane.View()), "NAVREPLY") {
		t.Errorf("the navigator's view lacks its exchanges:\n%s", screen(m))
	}
}

func TestKeysInTheRunViewDoNotReachTheInput(t *testing.T) {
	m, id := runHarness(t, 80, subagent.Run{Agent: "research", State: subagent.Running, MaxSteps: 25})
	m.openRun(id)
	send(m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if got := m.input.Value(); got != "" {
		t.Errorf("a key in the run view was typed into the input: %q", got)
	}
}
