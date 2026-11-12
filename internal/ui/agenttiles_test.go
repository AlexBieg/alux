package ui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/subagent"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// tiledHarness is a pane with the running box open and these runs on the roster.
func tiledHarness(t *testing.T, width int, runs ...subagent.Run) (*Model, []int) {
	t.Helper()
	m, s := harness(t, width, 40)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	var ids []int
	for _, run := range runs {
		ids = append(ids, roster.AddForTest(run))
	}
	m.sidebarOpen = true
	settle(m)
	return m, ids
}

func running(name string, step, of int) subagent.Run {
	return subagent.Run{Agent: "research", Name: name, State: subagent.Running, Step: step, MaxSteps: of,
		Activity: "web_fetch pkg.go.dev", Started: time.Now().Add(-2 * time.Minute)}
}

// boxLine is the first line of the drawn box whose click goes to want, and its row inside the border.
func boxLine(t *testing.T, m *Model, want sideTarget) (string, int) {
	t.Helper()
	box, targets := m.sidebar()
	for i, got := range targets {
		if got == want {
			return strings.Split(box, "\n")[i+1], i
		}
	}
	t.Fatalf("no row of the box goes to %+v:\n%s", want, ansi.Strip(box))
	return "", -1
}

// agentLine is a run's first row in the box, as cells.
func agentLine(t *testing.T, m *Model, run int) []uv.Cell {
	t.Helper()
	line, _ := boxLine(t, m, sideTarget{run: run})
	return cellsOf(line)
}

func plainBox(m *Model) string {
	box, _ := m.sidebar()
	return ansi.Strip(box)
}

// cellOf is the first cell of text in row.
func cellOf(t *testing.T, row []uv.Cell, text string) uv.Cell {
	t.Helper()
	var plain strings.Builder
	for _, c := range row {
		plain.WriteString(c.Content)
	}
	at := strings.Index(plain.String(), text)
	if at < 0 {
		t.Fatalf("%q is not in the row %q", text, plain.String())
	}
	return row[len([]rune(plain.String()[:at]))]
}

func TestAnAgentIsItsHatAndNameOverItsKindAndAge(t *testing.T) {
	coder := running("a long task name here", 37, 40)
	coder.Agent = "coder"
	m, ids := tiledHarness(t, 140, running("btop style", 14, 40), coder)
	box := strings.Split(plainBox(m), "\n")
	for i, want := range []struct{ head, kind string }{
		{runHat(ids[0]) + " btop style", "research"},
		{runHat(ids[1]) + " a long task name", "coder"},
	} {
		_, row := boxLine(t, m, sideTarget{run: ids[i]})
		head, under := box[row+1], box[row+2]
		if !strings.Contains(head, "● ") || !strings.Contains(head, want.head) {
			t.Errorf("run %d's first line reads %q", i, head)
		}
		if !strings.Contains(under, " "+want.kind+" ") || !strings.HasSuffix(under, " 2m │") {
			t.Errorf("run %d's second line lacks its kind or age: %q", i, under)
		}
	}
	if strings.Contains(plainBox(m), "web_fetch") || strings.ContainsRune(plainBox(m), '\ue0b6') {
		t.Errorf("the box shows a tool or a pill:\n%s", plainBox(m))
	}
}

func TestEveryHatFitsAndNoneIsTheAluxsOwn(t *testing.T) {
	for _, hat := range runHats {
		if w := ansi.StringWidth(hat); w != aluxWide {
			t.Errorf("hat %q is %d cells wide", hat, w)
		}
		if hat == aluxHatRow {
			t.Errorf("a run wears the main alux's hat")
		}
	}
}

func TestRunsSideBySideGetMixedHatsAndColours(t *testing.T) {
	hats, colours := map[string]bool{}, map[string]bool{}
	for id := 1; id <= 6; id++ {
		hats[runHat(id)] = true
		colours[runHatStyle(id).Render("x")] = true
	}
	if len(hats) < 3 || len(colours) < 3 {
		t.Errorf("six runs drew only %d hats and %d colours", len(hats), len(colours))
	}
}

func TestTheDotCarriesTheRunsState(t *testing.T) {
	waiting := running("asks", 5, 40)
	waiting.State = subagent.Waiting
	failed := running("broke", 9, 40)
	failed.State, failed.Ended = subagent.Failed, time.Now()
	m, ids := tiledHarness(t, 140, running("going", 1, 40), waiting, failed)
	m.poseTick = 0
	for i, want := range []any{runColor(subagent.Running), pulse(0, waitingPulse), errorColor} {
		if dot := cellOf(t, agentLine(t, m, ids[i]), "●"); !sameColor(dot.Style.Fg, want.(interface{ RGBA() (r, g, b, a uint32) })) {
			t.Errorf("run %d's dot is not its state colour", i)
		}
	}
}

// gutters is each run's rows' first column in the open box, in order.
func gutters(m *Model) map[int][]string {
	rows, targets := m.openRows(m.sideItems(), panelInner(sidebarWide), 40)
	out := map[int][]string{}
	for i, tg := range targets {
		if tg.run != 0 {
			out[tg.run] = append(out[tg.run], ansi.Truncate(ansi.Strip(rows[i]), 1, ""))
		}
	}
	return out
}

func TestAPairIsTwoRowsJoinedByABracket(t *testing.T) {
	m, s := harness(t, 140, 40)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	driver := roster.AddForTest(subagent.Run{Agent: "pair", Name: "refactor", State: subagent.Running, MaxSteps: 60})
	other := roster.AddForTest(subagent.Run{Agent: "coder", Name: "tests", State: subagent.Running, MaxSteps: 60})
	nav := roster.AddForTest(subagent.Run{Agent: "navigator", State: subagent.Idle, MaxSteps: 6, Parent: driver})
	m.sidebarOpen = true
	settle(m)

	got := gutters(m)
	want := map[int][]string{driver: {"╭", "│"}, nav: {"╰", " "}, other: {" ", " "}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gutters %v, want the pair bracketed and the coder clear of it", got)
	}
	if rail := strings.Join(m.railRows(m.sideItems()), ""); !strings.Contains(ansi.Strip(rail), dot+"2") {
		t.Errorf("the rail counts a pair as two agents: %q", ansi.Strip(rail))
	}
}

func TestANavigatorStaysWithItsDriverWhenTheDriverNeedsYou(t *testing.T) {
	m, s := harness(t, 140, 40)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	roster.AddForTest(subagent.Run{Agent: "coder", Name: "tests", State: subagent.Running, MaxSteps: 60})
	driver := roster.AddForTest(subagent.Run{Agent: "pair", Name: "refactor", State: subagent.Waiting, MaxSteps: 60})
	nav := roster.AddForTest(subagent.Run{Agent: "navigator", State: subagent.Idle, MaxSteps: 6, Parent: driver})
	m.sidebarOpen = true
	settle(m)

	it := m.sideItems()
	if len(it.needRuns) != 2 || it.needRuns[0].ID != driver || it.needRuns[1].ID != nav {
		t.Errorf("needs you holds %+v, want the driver then its navigator", it.needRuns)
	}
}

func TestOpeningARunMutesTheOtherRows(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 20, 40), running("second", 20, 40))
	m.openRun(ids[1])
	if dot := cellOf(t, agentLine(t, m, ids[0]), "●"); !sameColor(dot.Style.Fg, mutedColor) {
		t.Errorf("a row not being viewed kept its colour")
	}
	viewed := agentLine(t, m, ids[1])
	if !sameColor(cellOf(t, viewed, "●").Style.Fg, runColor(subagent.Running)) {
		t.Errorf("the viewed row lost its colour")
	}
	if cellOf(t, viewed, "second").Style.Attrs&uv.AttrBold == 0 {
		t.Errorf("the viewed row's name is not bold")
	}
	m.closeRun()
	if dot := cellOf(t, agentLine(t, m, ids[0]), "●"); sameColor(dot.Style.Fg, mutedColor) {
		t.Errorf("closing the view left a row muted")
	}
}

func TestTheFrameTakesTheViewedRunsColour(t *testing.T) {
	m, ids := tiledHarness(t, 140, running("first", 20, 40), running("second", 20, 40))
	corner := func() uv.Cell {
		box, _ := m.sidebar()
		return cellOf(t, cellsOf(strings.Split(box, "\n")[0]), "╭")
	}
	if !sameColor(corner().Style.Fg, mutedColor) {
		t.Fatal("with no run view open the frame should be muted")
	}
	m.openRun(ids[0])
	if !sameColor(corner().Style.Fg, runColor(subagent.Running)) {
		t.Errorf("the frame did not take the viewed run's colour")
	}
	m.closeRun()
	if !sameColor(corner().Style.Fg, mutedColor) {
		t.Errorf("closing the view left the frame coloured")
	}
}

func TestARunningNameShimmersAndNothingButTheFaceMoves(t *testing.T) {
	m, _ := tiledHarness(t, 140, running("btop style", 30, 40))
	faceless := func() string {
		box, _ := m.sidebar()
		for _, eyes := range runGazes {
			box = strings.ReplaceAll(box, eyes, "")
		}
		return box
	}
	m.poseTick = 0
	before, styled := ansi.Strip(faceless()), faceless()
	moved := false
	for tick := range 30 {
		m.poseTick = tick
		if ansi.Strip(faceless()) != before {
			t.Fatalf("tick %d changed the box's text", tick)
		}
		if faceless() != styled {
			moved = true
		}
	}
	if !moved {
		t.Errorf("a running name never shimmered")
	}
}

func TestAnUnnamedRunIsTitledByItsAgent(t *testing.T) {
	m, ids := tiledHarness(t, 140, subagent.Run{Agent: "research", State: subagent.Running, MaxSteps: 25})
	if box := plainBox(m); !strings.Contains(box, runHat(ids[0])+" research ") {
		t.Errorf("an unnamed run's row should read its agent alone: %q", box)
	}
}
