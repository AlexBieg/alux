package ui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

func colored(c color.Color, text string) string {
	return lipgloss.NewStyle().Foreground(c).Render(text)
}

func cmdS(m *Model) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModSuper})
	return cmd
}

func cmdE(m *Model) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModSuper})
	return cmd
}

func TestEnterOnAServedSessionRowSwitchesToIt(t *testing.T) {
	for _, served := range []bool{true, false} {
		m, _ := harness(t, 140, 30)
		m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
		m.CanAttach = func(pid int) bool { return served && pid == 4242 }
		send(m, SessionsMsg{Sessions: []registry.Info{{PID: 4242, Name: "other", State: registry.Idle}}})
		cmdE(m)
		press(m, "enter")
		switch {
		case served && m.SwitchTo != 4242:
			t.Errorf("enter on a served session's row set SwitchTo %d, want 4242", m.SwitchTo)
		case !served && m.SwitchTo != 0:
			t.Errorf("enter on an unserved session's row set SwitchTo %d", m.SwitchTo)
		}
	}
}

func TestAnIdleSessionWithTasksRunningSaysHowManyInsteadOfIdle(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	m.setSidebar(true)
	send(m, SessionsMsg{Sessions: []registry.Info{{PID: 4242, Name: "other", State: registry.Idle, Tasks: 2}}})
	box, _ := m.sidebar()
	for _, row := range strings.Split(ansi.Strip(box), "\n") {
		if strings.Contains(row, "other") && (!strings.Contains(row, "2 tasks") || strings.Contains(row, "idle")) {
			t.Errorf("the running box shows an idle session with two tasks as %q", row)
		}
	}
}

func TestSpawnSwitchesThePaneToTheNewSessionWithoutQueueingIt(t *testing.T) {
	m, s := harness(t, 140, 30)
	m.Spawn = func() (int, error) { return 4242, nil }
	batch, _ := typeLine(m, "/spawn")().(tea.BatchMsg)
	for _, cmd := range batch {
		runCmd(m, cmd)
	}
	if m.SwitchTo != 4242 || len(s.Snapshot().Queued) != 0 {
		t.Errorf("/spawn set SwitchTo %d with %d queued, want 4242 and none", m.SwitchTo, len(s.Snapshot().Queued))
	}
}

func TestKillAndQuitCloseThePaneForTheSessionToBeStoppedWithoutQueueingIt(t *testing.T) {
	for line, only := range map[string]bool{"/kill": false, "/quit": true} {
		m, s := harness(t, 140, 30)
		m.CanKill = true
		typeLine(m, line)
		if !m.Kill || m.KillOnly != only || len(s.Snapshot().Queued) != 0 {
			t.Errorf("%s set Kill %t KillOnly %t with %d queued, want true, %t and none", line, m.Kill, m.KillOnly, len(s.Snapshot().Queued), only)
		}
	}
}

func TestCtrlCKillsTheSessionAndClosesThePaneAndDetachLeavesItRunning(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.CanKill = true
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !m.quit || !m.Kill || !m.KillOnly {
		t.Errorf("ctrl+c set quit %t Kill %t KillOnly %t, want true, true, true", m.quit, m.Kill, m.KillOnly)
	}

	m, s := harness(t, 140, 30)
	m.CanKill = true
	typeLine(m, "/detach")
	if !m.quit || m.Kill || len(s.Snapshot().Queued) != 0 {
		t.Errorf("/detach set quit %t Kill %t with %d queued, want true, false and none", m.quit, m.Kill, len(s.Snapshot().Queued))
	}
}

func TestXTwiceOnASessionOrWorkerRowStopsThatSession(t *testing.T) {
	for _, s := range []registry.Info{
		{PID: 7, Name: "headless", Worker: "ABC-42", State: registry.Working},
		{PID: 8, Name: "other", State: registry.Idle},
	} {
		m, _ := harness(t, 140, 30)
		m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
		var stopped []int
		m.StopSession = func(pid int) error {
			stopped = append(stopped, pid)
			return nil
		}
		send(m, SessionsMsg{Sessions: []registry.Info{s}})
		cmdE(m)
		press(m, "x")
		if len(stopped) != 0 {
			t.Fatalf("one x on %s stopped %v", s.Name, stopped)
		}
		if cmd := press(m, "x"); cmd != nil {
			send(m, cmd())
		}
		if !slices.Equal(stopped, []int{s.PID}) {
			t.Errorf("x twice on %s stopped %v, want %d", s.Name, stopped, s.PID)
		}
	}
}

// cursorOn is the target of the item the box's cursor is drawn on.
func cursorOn(t *testing.T, m *Model) sideTarget {
	t.Helper()
	_, targets := m.sidebar()
	stops := sideStops(targets)
	i := m.focusedStop(stops)
	if !m.sideFocused() || i < 0 {
		t.Fatalf("the box has no cursor:\n%s", plainBox(m))
	}
	return stops[i].target
}

func TestCmdEFocusesTheBoxAndEnterOpensTheRunUnderTheCursor(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 1, 40), running("second", 2, 40))
	cmdE(m)
	if got := cursorOn(t, m); got != (sideTarget{run: ids[0]}) {
		t.Fatalf("the cursor starts on %+v, want the first run", got)
	}
	press(m, "j")
	if m.input.Value() != "" {
		t.Errorf("j reached the prompt as %q while the box had the keys", m.input.Value())
	}
	press(m, "enter")
	if m.runView == nil || m.runView.id != ids[1] {
		t.Fatalf("enter on the second run opened %+v", m.runView)
	}
	if m.sideFocus.on {
		t.Error("the box kept the keys after opening a run")
	}
}

func TestTheCursorWrapsAtBothEnds(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 1, 40), running("second", 2, 40))
	cmdE(m)
	press(m, "up")
	if got := cursorOn(t, m); got != (sideTarget{run: ids[1]}) {
		t.Fatalf("up from the first item went to %+v, want the last", got)
	}
	press(m, "down")
	if got := cursorOn(t, m); got != (sideTarget{run: ids[0]}) {
		t.Errorf("down from the last item went to %+v, want the first", got)
	}
}

func TestTheCursorFollowsARunThatMovesToNeedsYou(t *testing.T) {
	m, s := harness(t, 140, 40)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	var ids []int
	for _, name := range []string{"a", "b", "c"} {
		ids = append(ids, roster.AddForTest(running(name, 1, 40)))
	}
	settle(m)
	cmdE(m)
	press(m, "down")
	press(m, "down")
	roster.SetStateForTest(ids[2], subagent.Waiting)
	settle(m)
	if got := cursorOn(t, m); got != (sideTarget{run: ids[2]}) {
		t.Fatalf("the cursor is on %+v after its run moved to NEEDS YOU:\n%s", got, plainBox(m))
	}
	press(m, "enter")
	if m.runView == nil || m.runView.id != ids[2] {
		t.Errorf("enter opened %+v, want run %d", m.runView, ids[2])
	}
}

func TestCmdEFromTheRailOpensTheBoxAndEscGivesTheKeysBack(t *testing.T) {
	m, _ := tiledHarness(t, 140, running("first", 1, 40))
	m.setSidebar(false)
	cmdE(m)
	if !m.sidebarDrawnOpen() || !m.sideFocused() {
		t.Fatalf("cmd+e on the rail left open=%v focused=%v", m.sidebarDrawnOpen(), m.sideFocused())
	}
	if !strings.Contains(screen(m), sideFocusHint) {
		t.Errorf("the status line does not say how to drive the box:\n%s", screen(m))
	}
	press(m, "esc")
	typeText(m, "x")
	if m.sideFocused() || m.input.Value() != "x" {
		t.Errorf("after esc focused=%v prompt=%q, want the prompt to have the keys", m.sideFocused(), m.input.Value())
	}
}

func TestCmdEOverARunViewShowsTheBox(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 1, 40), running("second", 2, 40))
	m.openRun(ids[0])
	cmdE(m)
	if m.runView != nil || !m.sideFocused() {
		t.Fatalf("cmd+e over a run view left view=%v focused=%v", m.runView, m.sideFocused())
	}
}

func TestCmdEWithNothingRunningSaysSo(t *testing.T) {
	m, _ := harness(t, 140, 30)
	cmdE(m)
	if m.sideFocused() || !strings.Contains(screen(m), nothingToOpen) {
		t.Errorf("an empty box took focus=%v:\n%s", m.sideFocused(), screen(m))
	}
}

// boxColumn is the screen column the box's left border is drawn in on row y, -1 when it is not.
func boxColumn(m *Model, y int) int {
	row := []rune(strings.Split(screen(m), "\n")[y])
	at := m.mainWidth()
	if at < len(row) && strings.ContainsRune("╭│╰", row[at]) {
		return at
	}
	return -1
}

func TestTheBoxStartsOpen(t *testing.T) {
	m, _ := harness(t, 140, 30)
	if !m.sidebarOpen || m.sidebarWidth() != sidebarWide {
		t.Fatalf("the box starts %d wide, open=%v; want it open", m.sidebarWidth(), m.sidebarOpen)
	}
}

func TestAnEmptyRailShowsADot(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.setSidebar(false)
	if got := strings.Split(plainBox(m), "\n"); len(got) != 3 || got[1] != "│ · │" {
		t.Errorf("an empty rail reads:\n%s", plainBox(m))
	}
}

func TestCtrlSOpensAndCollapsesTheBoxAndTheMainColumnFollows(t *testing.T) {
	m, s := harness(t, 140, 30)
	m.setSidebar(false)
	s.Show("hello")
	settle(m)
	rail := m.mainWidth()
	cmdS(m)
	if !m.sidebarOpen || m.mainWidth() != rail-(sidebarWide-sidebarRail) || m.viewport.Width() != m.mainWidth() {
		t.Fatalf("after cmd+s open=%v main=%d viewport=%d, want open with main %d", m.sidebarOpen, m.mainWidth(), m.viewport.Width(), rail-27)
	}
	if !strings.Contains(strings.Split(screen(m), "\n")[m.sidebarTop()], "╭─ running ─") {
		t.Errorf("the open box has no titled top edge:\n%s", screen(m))
	}
	cmdS(m)
	if m.sidebarOpen || m.mainWidth() != rail {
		t.Errorf("a second cmd+s left open=%v main=%d", m.sidebarOpen, m.mainWidth())
	}
}

func TestClickingTheRailOpensAndTheTitleCollapses(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.setSidebar(false)
	clickAt(m, m.mainWidth()+2, m.sidebarTop()+1)
	if !m.sidebarOpen {
		t.Fatal("a click on the rail did not open the box")
	}
	clickAt(m, m.mainWidth()+1, m.sidebarTop())
	if !m.sidebarOpen {
		t.Fatal("a click on the corner, not the title, collapsed the box")
	}
	x := screenXOf(t, m, m.sidebarTop(), "running")
	clickAt(m, x+3, m.sidebarTop())
	if m.sidebarOpen {
		t.Error("a click on the title did not collapse the box")
	}
}

func TestANarrowScreenRefusesToOpenTheBox(t *testing.T) {
	for _, how := range []struct {
		name string
		do   func(m *Model) tea.Cmd
	}{
		{"cmd+s", cmdS},
		{"cmd+e", cmdE},
		{"rail click", func(m *Model) tea.Cmd {
			_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: m.mainWidth() + 2, Y: m.sidebarTop() + 1})
			return cmd
		}},
	} {
		t.Run(how.name, func(t *testing.T) {
			m, _ := harness(t, 80, 30)
			m.setSidebar(false)
			how.do(m)
			if m.sidebarOpen || m.sidebarWidth() != sidebarRail {
				t.Errorf("a %d-column screen opened the box", m.width)
			}
			if !strings.Contains(screen(m), "too narrow to open the running box") {
				t.Errorf("no notice said why:\n%s", screen(m))
			}
		})
	}
}

func TestShrinkingPastTheMinimumDrawsTheRailAndKeepsTheMode(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if !m.sidebarOpen || m.sidebarWidth() != sidebarRail || boxColumn(m, m.sidebarTop()) != 75 {
		t.Fatalf("at 80 columns open=%v width=%d; want the rail drawn with the mode kept", m.sidebarOpen, m.sidebarWidth())
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	if m.sidebarWidth() != sidebarWide {
		t.Errorf("widening again did not bring the open box back")
	}
}

func TestClickingAnAgentRowOpensItsRun(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 1, 40), running("second", 2, 40))
	_, row := boxLine(t, m, sideTarget{run: ids[1]})
	clickAt(m, m.mainWidth()+8, m.sidebarTop()+2+row)
	if m.runView == nil || m.runView.id != ids[1] {
		t.Fatalf("clicking the second line of run %d's row opened %+v", ids[1], m.runView)
	}
}

func TestClickingAnAgentRowWhileARunIsExpandedSwitchesTheViewToIt(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 1, 40), running("second", 2, 40))
	m.openRun(ids[0])
	_, row := boxLine(t, m, sideTarget{run: ids[1]})
	clickAt(m, m.mainWidth()+8, m.sidebarTop()+2+row)
	if m.runView == nil || m.runView.id != ids[1] {
		t.Fatalf("clicking run %d's row with run %d expanded left %+v", ids[1], ids[0], m.runView)
	}
}

func TestClickingAFileRowOpensItsDiffUntilEsc(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.sidebarOpen = true
	m.snap.Files = []session.File{{Path: "internal/a.go"}, {Path: "b.go", Created: true}}
	m.layout()
	_, row := boxLine(t, m, sideTarget{file: "internal/a.go"})
	clickAt(m, m.mainWidth()+8, m.sidebarTop()+1+row)
	if m.fileDiff == nil || m.fileDiff.path != "internal/a.go" {
		t.Fatalf("clicking a.go's row opened %+v", m.fileDiff)
	}
	send(m, fileDiffMsg{path: "internal/a.go", patch: "@@ -1 +1 @@\n-old line\n+new line\n"})
	if got := screen(m); !strings.Contains(got, "old line") || !strings.Contains(got, "new line") {
		t.Errorf("the open diff does not show the change:\n%s", got)
	}
	press(m, "esc")
	if m.fileDiff != nil {
		t.Error("esc left the diff open")
	}
}

func TestAWaitingRunNeedsYouAndIsNotAlsoAnAgent(t *testing.T) {
	waiting := running("asks a question", 5, 40)
	waiting.State = subagent.Waiting
	m, ids := tiledHarness(t, 140, running("busy", 1, 40), waiting)
	box := strings.Split(plainBox(m), "\n")
	need, agents := -1, -1
	for i, line := range box {
		switch {
		case strings.Contains(line, "NEEDS YOU"):
			need = i
		case strings.Contains(line, "AGENTS"):
			agents = i
		}
	}
	if need < 0 || agents < need {
		t.Fatalf("NEEDS YOU does not come before AGENTS:\n%s", plainBox(m))
	}
	if got := strings.Count(plainBox(m), "asks a question"); got != 1 {
		t.Errorf("the waiting run is listed %d times:\n%s", got, plainBox(m))
	}
	_, row := boxLine(t, m, sideTarget{run: ids[1]})
	if row+1 > agents {
		t.Errorf("the waiting run is under AGENTS, not NEEDS YOU:\n%s", plainBox(m))
	}
	clickAt(m, m.mainWidth()+8, m.sidebarTop()+1+row)
	if m.runView == nil || m.runView.id != ids[1] {
		t.Errorf("clicking the NEEDS YOU row did not open run %d", ids[1])
	}
}

func TestClickingAToLandRowScrollsToItsRun(t *testing.T) {
	m, s := harness(t, 140, 24)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	id := roster.AddForTest(subagent.Run{Agent: "coder", Name: "wrap fix", State: subagent.Done,
		Started: time.Now().Add(-time.Minute), Ended: time.Now(), Kept: []string{"a.go", "b.go"}})
	s.ShowRunForTest(id)
	for range 60 {
		s.Show("filler")
	}
	m.sidebarOpen = true
	settle(m)

	line, row := boxLine(t, m, sideTarget{run: id, scroll: true})
	if plain := ansi.Strip(line); !strings.Contains(plain, landGlyph+" wrap fix") || !strings.Contains(plain, "2 files") {
		t.Errorf("the to-land row reads %q", plain)
	}
	if !m.follow {
		t.Fatal("the pane is not at the bottom, so the test proves nothing")
	}
	clickAt(m, m.mainWidth()+8, m.sidebarTop()+1+row)
	item, _ := m.itemFor(sideTarget{run: id})
	if m.runView != nil || m.follow || m.viewport.YOffset() != m.content.first[item] {
		t.Errorf("the click left view=%v following=%v at line %d, want the run's line %d", m.runView, m.follow, m.viewport.YOffset(), m.content.first[item])
	}
}

func TestACrowdedBoxGoesToOneLineAgentsThenCutsTheLargestGroup(t *testing.T) {
	waiting := running("needs me", 5, 40)
	waiting.State = subagent.Waiting
	runs := []subagent.Run{waiting}
	for i := range 6 {
		runs = append(runs, running(fmt.Sprintf("task %d", i), 3, 40))
	}
	m, _ := tiledHarness(t, 140, runs...)
	if strings.Contains(plainBox(m), "+") {
		t.Fatalf("40 rows should hold seven runs uncut:\n%s", plainBox(m))
	}

	m.Update(tea.WindowSizeMsg{Width: 140, Height: 18})
	box := plainBox(m)
	if slices.ContainsFunc(runHats, func(h string) bool { return strings.Contains(box, h) }) || strings.Contains(box, "more") {
		t.Errorf("a box that fits once its agents are one line each still cut or kept two-line rows:\n%s", box)
	}

	m.Update(tea.WindowSizeMsg{Width: 140, Height: 14})
	box = plainBox(m)
	if !strings.Contains(box, "needs me") || !strings.Contains(box, "+") || !strings.Contains(box, "more") {
		t.Errorf("a box past its cap should keep NEEDS YOU and end AGENTS with +N more:\n%s", box)
	}
	if got, cap := len(strings.Split(box, "\n")), m.sidebarCap(); got > cap {
		t.Errorf("the box is %d rows, past its cap of %d", got, cap)
	}
}

func TestADragIntoTheBoxStopsAtTheMainColumn(t *testing.T) {
	m, s := harness(t, 140, 24)
	s.Show(strings.Repeat("word ", 40))
	settle(m)
	y := rowOf(t, m, "word")
	clickAt(m, 2, y)
	send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: m.width - 2, Y: y})
	if m.sel.head.col > m.mainWidth()-2 {
		t.Errorf("the drag reached column %d, past the main column's last %d", m.sel.head.col, m.mainWidth()-2)
	}
}

func TestTheBoxSitsInOneColumnOnEveryRow(t *testing.T) {
	m, _ := tiledHarness(t, 140, running("first", 1, 40))
	box, _ := m.sidebar()
	top := m.sidebarTop()
	for y := top; y < top+len(strings.Split(box, "\n")); y++ {
		if got := boxColumn(m, y); got != m.mainWidth() {
			t.Errorf("row %d has the box's border at %d, want %d:\n%s", y, got, m.mainWidth(), screen(m))
		}
	}
}

func TestTheRailCountsEachKindInItsMostUrgentColour(t *testing.T) {
	waiting := running("asks", 5, 40)
	waiting.State = subagent.Waiting
	m, s := harness(t, 140, 30)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	roster.AddForTest(running("a", 1, 40))
	roster.AddForTest(running("b", 1, 40))
	roster.AddForTest(waiting)
	roster.AddForTest(subagent.Run{Agent: "coder", State: subagent.Done, Ended: time.Now(), Kept: []string{"a.go"}})
	m.setSidebar(false)
	settle(m)
	rows := strings.Split(plainBox(m), "\n")
	want := []string{"│!1 │", "│●2 │", "│" + landGlyph + "1 │"}
	for i, w := range want {
		if rows[i+1] != w {
			t.Errorf("rail row %d is %q, want %q:\n%s", i, rows[i+1], w, plainBox(m))
		}
	}
	if box, _ := m.sidebar(); !strings.Contains(box, colored(runColor(subagent.Running), dot)) {
		t.Errorf("the agents' glyph is not in a running run's colour")
	}
}
