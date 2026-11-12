package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

// aluxAt is the screen column of his hat, or -1 when he is not drawn.
func aluxAt(m *Model) int {
	x, _ := aluxCell(m)
	return x
}

// aluxCell is the column and row of his hat, both -1 when he is not drawn. The last hat on
// screen is his; the ones above it are the running box's runs.
func aluxCell(m *Model) (int, int) {
	lines := strings.Split(screen(m), "\n")
	for y := len(lines) - 1; y >= 0; y-- {
		if i := strings.Index(lines[y], aluxHatRow); i >= 0 {
			return ansi.StringWidth(lines[y][:i]), y
		}
	}
	return -1, -1
}

func TestTheAluxStaysWholeAndInPlaceBesideCommandSuggestions(t *testing.T) {
	m, _ := tiledHarness(t, 140, running("first", 1, 40))
	_, before := aluxCell(m)
	for _, typed := range []string{"/", "/sk"} {
		m.input.SetValue(typed)
		m.layout()
		if m.renderCommands() == "" {
			t.Fatalf("typing %q showed no suggestions", typed)
		}
		x, y := aluxCell(m)
		if y != before {
			t.Fatalf("suggestions for %q moved him from row %d to %d:\n%s", typed, before, y, screen(m))
		}
		lines := strings.Split(screen(m), "\n")
		if feet := ansi.Cut(lines[y+2], x, x+aluxWide); strings.TrimSpace(feet) == "" {
			t.Errorf("suggestions for %q hid his feet:\n%s", typed, screen(m))
		}
	}
	m.input.SetValue("")
	m.layout()
	if _, y := aluxCell(m); y != before {
		t.Errorf("with the suggestions gone he is at row %d, want %d", y, before)
	}
}

func TestThePRPickerHidesTheAlux(t *testing.T) {
	m, _ := pickerModel(t)
	m.sidebarOpen = true
	send(m, tea.WindowSizeMsg{Width: 140, Height: 40})
	if aluxAt(m) < 0 {
		t.Fatalf("he is not drawn before the picker opens:\n%s", screen(m))
	}
	runCmd(m, typeLine(m, "/prs"))
	if m.picker == nil {
		t.Fatal("/prs did not open the picker")
	}
	if x := aluxAt(m); x >= 0 {
		t.Errorf("he is drawn at column %d over the PR picker", x)
	}
}

func TestTheAluxStandsInTheOpenSidebarAndNotOnTheRail(t *testing.T) {
	m, _ := tiledHarness(t, 140, running("first", 1, 40))
	if x := aluxAt(m); x < m.mainWidth() {
		t.Fatalf("with the sidebar open he is at column %d, want inside the sidebar from %d:\n%s", x, m.mainWidth(), screen(m))
	}
	m.setSidebar(false)
	if x := aluxAt(m); x >= 0 {
		t.Errorf("he is drawn at column %d on the collapsed rail", x)
	}
}

func TestTheAluxStaysInTheSidebarBesideAnExpandedRun(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 1, 40))
	m.openRun(ids[0])
	if x := aluxAt(m); x < m.mainWidth() {
		t.Fatalf("with a run expanded he is at column %d, want inside the sidebar from %d:\n%s", x, m.mainWidth(), screen(m))
	}
}

func TestTheAluxWalksWithinTheSidebarOnlyWhileATurnRuns(t *testing.T) {
	m, _ := tiledHarness(t, 140, running("first", 1, 40))
	start := aluxAt(m)
	for range 10 {
		tick(m)
	}
	if aluxAt(m) != start {
		t.Fatal("he walked while no turn was running")
	}
	m.snap.Busy = true
	seen := map[int]bool{}
	for range 4 * aluxStepEvery * sidebarWide {
		tick(m)
		x := aluxAt(m)
		if x < m.mainWidth() || x+aluxWide > m.width {
			t.Fatalf("he walked out of the sidebar to column %d", x)
		}
		seen[x] = true
	}
	if !seen[m.mainWidth()] || !seen[m.width-aluxWide] {
		t.Errorf("he never reached both ends of the sidebar: %v", seen)
	}
}

func busyAlux(t *testing.T) (*Model, func(id, label string, kind agent.ToolKind)) {
	t.Helper()
	m, s := harness(t, 120, 30)
	s.SetBusyForTest(true)
	settle(m)
	tick(m)
	return m, func(id, label string, kind agent.ToolKind) {
		s.StartToolForTest(id, label, kind)
		s.ResolveToolForTest(id, label, "ok", kind)
		settle(m)
	}
}

func ticks(m *Model, n int) {
	for range n {
		tick(m)
	}
}

func TestACallTooQuickToSeeRunningStillPosesHim(t *testing.T) {
	m, call := busyAlux(t)
	call("1", "$ jj status", agent.KindBash)
	tick(m)
	if m.alux.mood != moodWait {
		t.Fatalf("after a bash call he is %v, want waiting", m.alux.mood)
	}
	ticks(m, aluxGrace-2)
	if m.alux.mood != moodWait {
		t.Errorf("he dropped the pose %d ticks into the gap between calls", aluxGrace-2)
	}
	ticks(m, aluxHold)
	if m.alux.mood != moodTalk {
		t.Errorf("long after the call he is still %v, want walking", m.alux.mood)
	}
}

func TestAPoseHoldsThroughABurstOfOtherCalls(t *testing.T) {
	m, call := busyAlux(t)
	call("1", "view a.go", agent.KindEdit)
	tick(m)
	call("2", "edit a.go", agent.KindEdit)
	ticks(m, aluxHold-2)
	if m.alux.mood != moodLook {
		t.Fatalf("he switched to %v before holding the first pose", m.alux.mood)
	}
	ticks(m, 2)
	if m.alux.mood != moodWork {
		t.Errorf("after the hold he is %v, want working", m.alux.mood)
	}
}

func TestTheBusiestRunningCallWinsABatch(t *testing.T) {
	m, s := harness(t, 120, 30)
	s.SetBusyForTest(true)
	s.StartToolForTest("1", "grep foo", agent.KindSearch)
	s.StartToolForTest("2", "edit a.go", agent.KindEdit)
	s.StartToolForTest("3", "view b.go", agent.KindEdit)
	settle(m)
	tick(m)
	if m.alux.mood != moodWork {
		t.Errorf("with an edit among reads running he is %v, want working", m.alux.mood)
	}
}

func TestAQuestionCutsThroughTheHold(t *testing.T) {
	m, call := busyAlux(t)
	call("1", "grep foo", agent.KindSearch)
	tick(m)
	m.snap.Pending = "run this?"
	tick(m)
	if m.alux.mood != moodNeed {
		t.Errorf("with an approval waiting he is %v, want asking", m.alux.mood)
	}
}

func TestWhileAskingQuestionMarksSwapSidesAndHeStaysGrounded(t *testing.T) {
	m, _ := harness(t, 120, 30)
	m.alux.mood, m.alux.at = moodNeed, 0
	var tops []string
	for _, f := range []int{0, aluxBeat} {
		m.poseTick = f
		rows := m.aluxRows()
		if ansi.Strip(rows[3]) != aluxFeetApart {
			t.Fatalf("frame %d: his feet left the ground: %q", f, ansi.Strip(rows[3]))
		}
		top := ansi.Strip(rows[0])
		if !strings.Contains(top, "?") || !strings.Contains(ansi.Strip(rows[1]), "?") {
			t.Fatalf("frame %d: no question marks over and beside his hat: %q / %q", f, top, ansi.Strip(rows[1]))
		}
		tops = append(tops, top)
	}
	if tops[0] == tops[1] {
		t.Errorf("the question marks did not move between beats: %q", tops[0])
	}
}

func TestAnErrorFlinchesAndAFinishedTurnHops(t *testing.T) {
	m, s := harness(t, 120, 30)
	s.SetBusyForTest(true)
	settle(m)
	ticks(m, aluxHold)
	s.PushErrorForTest("overloaded")
	settle(m)
	if m.alux.mood != moodOops {
		t.Fatalf("after an error he is %v, want flinching", m.alux.mood)
	}
	ticks(m, holdFor(moodOops))
	s.SetBusyForTest(false)
	settle(m)
	if m.alux.mood != moodDone {
		t.Fatalf("when the turn ends he is %v, want hopping", m.alux.mood)
	}
	ticks(m, holdFor(moodDone))
	if m.alux.mood != moodStand {
		t.Errorf("after the hop he is %v, want standing", m.alux.mood)
	}
}

func TestErrorsAlreadyInTheTranscriptDoNotStartle(t *testing.T) {
	m, s := harness(t, 120, 30)
	s.PushErrorForTest("from before")
	settle(m)
	if m.alux.mood == moodOops {
		t.Errorf("an error that was there when the pane opened made him flinch")
	}
}

func TestNoPoseOpensAnEyeOntoTheHeadsEdge(t *testing.T) {
	m, _ := harness(t, 120, 30)
	faces := append([]string{}, runGazes...)
	for mood := moodStand; mood <= moodDoze; mood++ {
		for f := range 4 * aluxHold {
			m.alux.mood, m.alux.at, m.poseTick = mood, 0, f
			faces = append(faces, ansi.Strip(m.aluxRows()[2]))
		}
	}
	for _, face := range faces {
		if strings.ContainsAny(face, "▛▜") {
			t.Fatalf("face %q has an eye hole in the head's lower half", face)
		}
	}
}

func TestEveryPoseFillsTheSameFourRows(t *testing.T) {
	m, _ := harness(t, 120, 30)
	for mood := moodStand; mood <= moodDoze; mood++ {
		for f := range 4 * aluxHold {
			m.alux.mood, m.alux.at, m.poseTick = mood, 0, f
			rows := m.aluxRows()
			if len(rows) != 4 {
				t.Fatalf("mood %v frame %d has %d rows", mood, f, len(rows))
			}
			for _, r := range rows {
				if w := ansi.StringWidth(r); w != aluxWide {
					t.Fatalf("mood %v frame %d has a row %q %d cells wide", mood, f, ansi.Strip(r), w)
				}
			}
		}
	}
}
