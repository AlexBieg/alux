package code

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A search crosses files, so the language cannot be decided once for the block: each line's own
// path decides it. "def x" is a keyword in one and two identifiers in the other.
func TestEachResultIsLexedForItsOwnFile(t *testing.T) {
	rows := strings.Split(Results("a.py:1:def x\nb.go:2:def x", 60), "\n")
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per result: %q", len(rows), rows)
	}
	if got := colours(rows[0]); !got[keywordSGR] {
		t.Errorf("the Python hit lost its keyword: %v", got)
	}
	if got := colours(rows[1]); got[keywordSGR] {
		t.Errorf("the Go hit was coloured with Python's rules: %v", got)
	}
}

func TestAResultKeepsItsPathAndLineNumberReadable(t *testing.T) {
	row := Results("internal/ui/view.go:315:\tall := strings.Split(text)", 80)
	if got := colours(row); !got[landmarkSGR] {
		t.Errorf("the path is not drawn as a landmark: %v", got)
	}
	if !strings.Contains(row, mutedSGR) {
		t.Errorf("the line number is not muted: %q", row)
	}
	// The text has to survive verbatim: a result is something the reader copies into a command.
	if got := ansi.Strip(row); !strings.HasPrefix(got, "internal/ui/view.go:315:") {
		t.Errorf("the prefix was rewritten: %q", got)
	}
}

// What the paths-only searches and the file finder return, against what the tool says about
// itself. One is a place to go; the other is not.
func TestABarePathIsALandmarkAndANoteIsNot(t *testing.T) {
	if got := colours(Results("internal/code/results.go", 60)); !got[landmarkSGR] {
		t.Errorf("a bare path is not a landmark: %v", got)
	}
	for _, line := range []string{
		"No matches.",
		"[400 results, showing the first 200 — narrow the search]",
		"rg: unrecognized flag --nope",
	} {
		row := Results(line, 60)
		if got := colours(row); got[landmarkSGR] {
			t.Errorf("%q was drawn as a path: %v", line, got)
		}
		if !strings.HasPrefix(row, mutedSGR) {
			t.Errorf("%q is not muted: %q", line, row)
		}
	}
}

// A deep path can leave a handful of columns for the match, which would spill it one or two
// characters per row. The path gets its own row instead.
func TestALongPathGivesTheMatchItsOwnRow(t *testing.T) {
	rows := strings.Split(Results("internal/some/deeply/nested/pkg/file.go:1234:x := compute(y)", 48), "\n")
	if len(rows) < 2 {
		t.Fatalf("the match stayed on the path's row: %q", rows)
	}
	if got := ansi.Strip(rows[0]); got != "internal/some/deeply/nested/pkg/file.go:1234:" {
		t.Errorf("the first row is not the path alone: %q", got)
	}
	if got := ansi.Strip(rows[1]); !strings.HasPrefix(got, resultIndent+"x := compute(y)") {
		t.Errorf("the match is not indented under the path: %q", got)
	}
	for i, row := range rows {
		if w := ansi.StringWidth(row); w > 48 {
			t.Errorf("row %d is %d columns, over the 48 available", i, w)
		}
	}
}

// A path can be longer than the pane on its own, and a row wider than the pane costs the reader
// the end of it.
func TestAPathWiderThanThePaneWraps(t *testing.T) {
	const prefix = "internal/some/deeply/nested/pkg/file.go:1234:"
	rows := strings.Split(Results(prefix+"x := compute(y)", 30), "\n")
	for i, row := range rows {
		if w := ansi.StringWidth(row); w > 30 {
			t.Errorf("row %d is %d columns, over the 30 available: %q", i, w, ansi.Strip(row))
		}
	}
	// Wrapped across rows, the path is still all there to be read back.
	if got := strings.Join(strings.Fields(ansi.Strip(strings.Join(rows, " "))), ""); !strings.Contains(got, prefix) {
		t.Errorf("the path did not survive wrapping: %q", got)
	}
}

// rg prints the leftmost gutter, and a match that contains one of its own must not be mistaken
// for it — the path is everything before the first "number:" and no more.
func TestAMatchContainingAGutterKeepsTheRealOne(t *testing.T) {
	row := Results("notes.md:7:see internal/ui/view.go:315: for the budget", 80)
	if got := ansi.Strip(row); !strings.HasPrefix(got, "notes.md:7:see") {
		t.Errorf("the wrong gutter won: %q", got)
	}
}
