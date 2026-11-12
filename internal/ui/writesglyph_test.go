package ui

import (
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/classify"
	"github.com/charmbracelet/x/ansi"
)

const writesGlyph = "✎"

// The verdict lands after the box is on screen, so the glyph has to appear on a redraw rather
// than only on the box the start put up.
func TestAWriteVerdictMarksTheCommandAfterTheBoxIsDrawn(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.StartToolForTest("t1", "$ gofmt -w internal/agent/agent.go", agent.KindBash)
	s.ResolveToolForTest("t1", "$ gofmt -w internal/agent/agent.go", "ok", agent.KindBash)
	settle(m)

	if row := rowWith(t, m, "gofmt -w"); strings.Contains(row, writesGlyph) {
		t.Fatalf("unclassified command already marked: %q", ansi.Strip(row))
	}

	s.VerdictForTest("t1", classify.Writes)
	settle(m)

	row := rowWith(t, m, "gofmt -w")
	if !strings.Contains(row, writesGlyph) {
		t.Errorf("a write verdict did not mark the command: %q", ansi.Strip(row))
	}
}

func TestAReadVerdictLeavesTheCommandUnmarked(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.StartToolForTest("t1", "$ grep -rn foo internal/", agent.KindBash)
	s.ResolveToolForTest("t1", "$ grep -rn foo internal/", "no matches", agent.KindBash)
	s.VerdictForTest("t1", classify.Reads)
	settle(m)

	if row := rowWith(t, m, "grep -rn foo"); strings.Contains(row, writesGlyph) {
		t.Errorf("a read was marked as a write: %q", ansi.Strip(row))
	}
}

// The glyph widens the lead, which is also the hanging indent, so a wrapped command must line up
// under its own first line rather than under the marker.
func TestTheGlyphKeepsTheHangingIndent(t *testing.T) {
	const width = 40
	m, s := harness(t, width, 24)
	long := "$ gofmt -w internal/agent/agent.go internal/session/session.go internal/ui/render.go"
	s.StartToolForTest("t1", long, agent.KindBash)
	s.ResolveToolForTest("t1", long, "ok", agent.KindBash)
	s.VerdictForTest("t1", classify.Writes)
	settle(m)

	var head []string
	for _, line := range m.content.lines {
		plain := unframe(ansi.Strip(line))
		if strings.Contains(plain, "gofmt") || (len(head) > 0 && strings.Contains(plain, "session.go")) {
			head = append(head, plain)
		}
	}
	if len(head) < 2 {
		t.Fatalf("the command did not wrap at width %d, so the indent is untested:\n%s", width, screen(m))
	}
	for _, line := range head {
		if ansi.StringWidth(line) > width {
			t.Errorf("a head line ran past the pane: width %d of %d: %q", ansi.StringWidth(line), width, line)
		}
	}
	// The first line carries "▸ ✎ $ …", so continuations are indented to clear all four cells.
	if got := len(head[1]) - len(strings.TrimLeft(head[1], " ")); got != 4 {
		t.Errorf("wrapped line indented %d columns, want 4 to clear the marker and glyph: %q", got, head[1])
	}
}
