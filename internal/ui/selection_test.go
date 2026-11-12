package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The whole approach rests on this: a styled line can be taken apart into cells and put back
// together without losing anything. If the round trip is not faithful, every highlighted row in
// the transcript is quietly corrupted.
func TestDecomposingAStyledLineRoundTrips(t *testing.T) {
	for _, line := range []string{
		"plain text",
		lipgloss.NewStyle().Foreground(lipgloss.Red).Render("red") + " and " +
			lipgloss.NewStyle().Bold(true).Render("bold"),
		"  indented and spaced",
		"wörld 漢字 and an émoji 🙂",
		"",
	} {
		cells := cellsOf(line)
		back := cells.Render()
		if got, want := ansi.Strip(back), ansi.Strip(line); got != want {
			t.Errorf("round trip changed the text:\n got %q\nwant %q", got, want)
		}
		if got, want := lipgloss.Width(back), lipgloss.Width(line); got != want {
			t.Errorf("round trip changed the width of %q: %d, want %d", line, got, want)
		}
	}
}

// The cell grid is only faithful if transcript lines are measurable, and a tab is not: it
// reports zero columns to lipgloss and vanishes when a line is decomposed into cells. So no
// transcript line may contain one. Bash output is full of them, which is why this is enforced
// where every line enters rather than in each renderer.
//
// This also fixes a bug that predates selection: a zero-width tab threw off the wrapping of
// whatever line held it.
func TestNoTranscriptLineContainsATab(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ go test", "ok\talux/internal/ui\t1.2s\nFAIL\talux/internal/code\t0.1s\n"+numberedLines(10))
	settle(m)
	expand(t, m, rowContaining(t, m, "$ go test"))

	for i, line := range m.content.lines {
		if strings.Contains(line, "\t") {
			t.Fatalf("transcript line %d carries a tab, so its width is a lie: %q", i, line)
		}
	}
	// And the text survived the expansion rather than being dropped.
	if out := screen(m); !strings.Contains(out, "alux/internal/ui") {
		t.Errorf("the tabbed body went missing:\n%s", out)
	}
}

// A selection holds transcript line numbers, and the transcript can get shorter under it — a new
// session empties it outright. Every path that reads a selection has to survive pointing past
// the end, because the alternative is a panic in the render loop.
func TestASelectionPointingPastTheEndIsHarmless(t *testing.T) {
	m, s := harness(t, 80, 24)
	for i := range 20 {
		s.Show(fmt.Sprintf("line %d with words on it", i))
	}
	settle(m)

	row := rowContaining(t, m, "line 19")
	dragSelect(m, 3, row, 12, row)
	if m.sel.empty() {
		t.Fatal("nothing was selected")
	}

	// The session is replaced, so the lines the selection names no longer exist.
	s.NewSession()
	settle(m)

	// Any of these would panic on an unclamped index.
	_ = m.View()
	if got := m.selectedTextForTest(); got != "" {
		t.Errorf("a selection past the end copied %q, want nothing", got)
	}
	send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
}

// A highlight must not disturb the colours already on the line — that is the entire reason for
// going through a cell grid instead of splicing escape sequences. The red stays red; it merely
// gains a background.
func TestHighlightKeepsTheForegroundColour(t *testing.T) {
	line := lipgloss.NewStyle().Foreground(lipgloss.Red).Render("alpha") + " beta"
	out := highlightRange(line, 0, 5, lipgloss.Color("#444444"))

	if got, want := ansi.Strip(out), "alpha beta"; got != want {
		t.Fatalf("text changed: %q, want %q", got, want)
	}
	if !strings.Contains(out, "31") {
		t.Errorf("the red foreground was lost:\n%q", out)
	}
	if !strings.Contains(out, "48;2;68;68;68") {
		t.Errorf("the highlight background was not applied:\n%q", out)
	}
	if got, want := lipgloss.Width(out), lipgloss.Width(line); got != want {
		t.Errorf("highlighting changed the line width: %d, want %d", got, want)
	}
}

// Highlighting past the end of a line must clamp rather than pad. A selection that runs to the
// end of a short row would otherwise paint trailing blanks and change the row's width, which
// shifts everything laid out beside it.
func TestHighlightClampsToTheLine(t *testing.T) {
	line := "short"
	out := highlightRange(line, 2, 99, lipgloss.Color("#444444"))

	if got, want := ansi.Strip(out), "short"; got != want {
		t.Errorf("text changed: %q, want %q", got, want)
	}
	if got, want := lipgloss.Width(out), 5; got != want {
		t.Errorf("width changed: %d, want %d", got, want)
	}
}

// Dragging backwards is the same selection as dragging forwards over it. The anchor stays put;
// only the ordering of the resolved span changes.
func TestSpanOrdersItsEndsEitherWayRound(t *testing.T) {
	lines := []string{"alpha", "beta", "gamma"}

	forward := selection{anchor: point{0, 2}, head: point{2, 1}}
	backward := selection{anchor: point{2, 1}, head: point{0, 2}}

	f1, f2 := forward.span()
	b1, b2 := backward.span()
	if f1 != b1 || f2 != b2 {
		t.Errorf("direction changed the span: forward (%v,%v), backward (%v,%v)", f1, f2, b1, b2)
	}
	if got, want := selectedText(lines, f1, f2), "pha\nbeta\nga"; got != want {
		t.Errorf("selected %q, want %q", got, want)
	}
}

// The cell under the pointer is inside the selection. Off by one here is the difference between
// a selection that visibly covers a character and a copy that omits it.
func TestSelectionIncludesTheCellUnderThePointer(t *testing.T) {
	lines := []string{"abcdef"}
	s := selection{anchor: point{0, 1}, head: point{0, 3}}
	from, to := s.span()

	if got, want := selectedText(lines, from, to), "bcd"; got != want {
		t.Errorf("selected %q, want %q", got, want)
	}
}

// A copy takes the text and none of the styling, across however many rows it spans.
func TestSelectedTextIsPlainAcrossRows(t *testing.T) {
	lines := []string{
		lipgloss.NewStyle().Foreground(lipgloss.Red).Render("first line"),
		lipgloss.NewStyle().Bold(true).Render("second line"),
	}
	s := selection{anchor: point{0, 0}, head: point{1, 10}}
	from, to := s.span()

	got := selectedText(lines, from, to)
	want := "first line\nsecond line"
	if got != want {
		t.Errorf("copied %q, want %q", got, want)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("the copy carried escape sequences: %q", got)
	}
}

// Double-width glyphs occupy two columns but one cell, and the second column is a zero-width
// continuation. A copy that did not skip it would double the character.
func TestCopyingWideGlyphsDoesNotDoubleThem(t *testing.T) {
	lines := []string{"a漢字b"}
	s := selection{anchor: point{0, 0}, head: point{0, 5}}
	from, to := s.span()

	if got, want := selectedText(lines, from, to), "a漢字b"; got != want {
		t.Errorf("copied %q, want %q", got, want)
	}
}

// Only the rows the selection touches are rewritten, and the untouched ones come back byte for
// byte — a redraw of an unselected row must not depend on the cell grid being lossless.
func TestHighlightLinesLeavesOtherRowsAlone(t *testing.T) {
	lines := []string{"one", "two", "three", "four"}
	out := highlightLines(lines, point{1, 0}, point{2, 5}, lipgloss.Color("#444444"))

	if out[0] != "one" || out[3] != "four" {
		t.Errorf("rows outside the selection were rewritten: %q, %q", out[0], out[3])
	}
	for _, row := range []int{1, 2} {
		if !strings.Contains(out[row], "48;2;68;68;68") {
			t.Errorf("row %d was not highlighted: %q", row, out[row])
		}
	}
}
