package ui

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

func runHarness(t *testing.T, width int, run subagent.Run) (*Model, int) {
	t.Helper()
	m, s := harness(t, width, 24)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	id := roster.AddForTest(run)
	s.ShowRunForTest(id)
	settle(m)
	return m, id
}

func boxLines(t *testing.T, m *Model) []string {
	t.Helper()
	lines := m.buildTranscript().lines
	for _, line := range lines {
		if w := lipgloss.Width(line); w > m.contentWidth() {
			t.Errorf("a %d-cell line in %d columns: %q", w, m.contentWidth(), ansi.Strip(line))
		}
	}
	return lines
}

func TestALiveSubagentHeadKeepsOneHeight(t *testing.T) {
	for _, width := range []int{80, 30} {
		m, id := runHarness(t, width, subagent.Run{
			Agent: "research", State: subagent.Running, Step: 3, MaxSteps: 25, Activity: "reading a.go",
			Task: "Investigate why the transcript flickers while a subagent box updates its step counter",
		})
		want := len(boxLines(t, m))
		steps := []func(*subagent.Run){
			func(r *subagent.Run) { r.Activity = "" },
			func(r *subagent.Run) {
				r.Activity = "reading internal/ui/some/really/long/path/to/a/file_with_a_long_name_test.go"
			},
			func(r *subagent.Run) { r.Activity, r.Step = "", 4 },
			func(r *subagent.Run) { r.Activity, r.Step = "reading b.go", 10 },
		}
		for i, step := range steps {
			roster(m).UpdateForTest(id, step)
			settle(m)
			if got := boxLines(t, m); len(got) != want {
				t.Errorf("width %d, step %d: the box went from %d lines to %d:\n%s", width, i, want, len(got),
					ansi.Strip(strings.Join(got, "\n")))
			}
		}
	}
}

// The live state is the tile's and the run view's: the transcript keeps a dim marker.
func TestALiveRunIsADimMarker(t *testing.T) {
	m, _ := runHarness(t, 80, subagent.Run{
		Name: "render flicker", Agent: "research", Model: "claude-sonnet-5-5", State: subagent.Running,
		Step: 4, MaxSteps: 25, Activity: "reading internal/ui/render.go", Task: "look into the flicker",
		Trail: []subagent.Entry{{Kind: subagent.EntryTool, Tool: agent.KindBash, Text: "go test ./..."}},
	})
	row := rowWith(t, m, "render flicker")
	head := ansi.Strip(row)
	if !strings.Contains(head, "render flicker · research · dispatched") {
		t.Errorf("the marker reads %q", head)
	}
	for _, gone := range []string{"reading", "4/25", "go test"} {
		if strings.Contains(ansi.Strip(m.viewport.View()), gone) {
			t.Errorf("the transcript still shows %q, which belongs to the tile and the run view", gone)
		}
	}
	if !sameInk(inkOf(t, row, "dispatched"), mutedColor) {
		t.Errorf("the marker is not dim: %q", row)
	}
}

// opened is the run view of the one run in a harness, read back as its lines.
func opened(t *testing.T, width int, run subagent.Run) []string {
	t.Helper()
	m, id := runHarness(t, width, run)
	m.openRun(id)
	return strings.Split(m.pane.View(), "\n")
}

func lineWith(t *testing.T, lines []string, text string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(ansi.Strip(line), text) {
			return i
		}
	}
	t.Fatalf("no line contains %q:\n%s", text, ansi.Strip(strings.Join(lines, "\n")))
	return -1
}

// inkOf is the foreground of the first cell of text on a rendered line, read from the cell grid.
func inkOf(t *testing.T, line, text string) color.Color {
	t.Helper()
	cells := cellsOf(line)
	plain := make([]string, len(cells))
	for i, c := range cells {
		plain[i] = c.Content
	}
	i := strings.Index(strings.Join(plain, "\x00"), strings.Join(strings.Split(text, ""), "\x00"))
	if i < 0 {
		t.Fatalf("%q is not on the line %q", text, ansi.Strip(line))
	}
	return cells[strings.Count(strings.Join(plain, "\x00")[:i], "\x00")].Style.Fg
}

func sameInk(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return ar == br && ag == bg && ab == bb
}

func TestTheRunViewIsStyledBySectionInStoryOrder(t *testing.T) {
	lines := opened(t, 80, subagent.Run{
		Agent: "research", Name: "code read", State: subagent.Failed, Task: "find the bug", Result: "found it", Err: "boom",
		Trail: []subagent.Entry{{Kind: subagent.EntryTool, Tool: agent.KindBash, Text: "go test ./..."}},
	})
	task, tool := lineWith(t, lines, "find the bug"), lineWith(t, lines, "go test ./...")
	result, failure := lineWith(t, lines, "found it"), lineWith(t, lines, "boom")
	if sameInk(inkOf(t, lines[result], "found it"), mutedColor) {
		t.Errorf("the result is drawn dim: %q", lines[result])
	}
	if !sameInk(inkOf(t, lines[failure], "boom"), errorColor) {
		t.Errorf("the error is not in the error style: %q", lines[failure])
	}
	if !sameInk(inkOf(t, lines[tool], "go test ./..."), toolColor[agent.KindBash]) {
		t.Errorf("the tool call is not in its tool's tone: %q", lines[tool])
	}
	if !(task < tool && tool < result && result < failure) {
		t.Errorf("sections out of order: task %d, trail %d, report %d, error %d", task, tool, result, failure)
	}
}

func TestAPairExchangeNamesBothSpeakers(t *testing.T) {
	for _, width := range []int{80, 30} {
		lines := opened(t, width, subagent.Run{
			Agent: "navigator", Name: "code read", State: subagent.Done,
			Trail: []subagent.Entry{{Kind: subagent.EntryExchange, From: "driver", To: "navigator",
				Prompt: "is it sound?", Text: "No.\n\nKey it."}},
		})
		asked, answered := lineWith(t, lines, "driver → navigator:"), lineWith(t, lines, "navigator: No.")
		if asked >= answered {
			t.Errorf("width %d: the reply comes before the question", width)
		}
		driver, navigator := inkOf(t, lines[asked], "driver"), inkOf(t, lines[answered], "navigator")
		if !sameInk(driver, driverStyle.GetForeground()) || !sameInk(navigator, navigatorStyle.GetForeground()) ||
			sameInk(driver, navigator) {
			t.Errorf("width %d: the two speakers are not told apart by colour:\n%q\n%q", width, lines[asked], lines[answered])
		}
		if reply := lineWith(t, lines, "Key it."); reply <= answered {
			t.Errorf("width %d: the reply's second paragraph is not under it", width)
		}
		if width == 80 && !strings.Contains(ansi.Strip(lines[asked]), "driver → navigator: is it sound?") {
			t.Errorf("the question is not on its speaker's line: %q", ansi.Strip(lines[asked]))
		}
	}
}
