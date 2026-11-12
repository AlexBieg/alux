package code

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The point of the change: a viewed file is source, so it is coloured like source, while the
// numbers down the left are not and stay out of the way.
func TestAListingHighlightsSourceAndMutesTheGutter(t *testing.T) {
	rows := strings.Split(Listing("  1\tpackage main\n  2\tfunc f() {}", "main.go", 60), "\n")
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per line: %q", len(rows), rows)
	}
	for i, row := range rows {
		if !strings.HasPrefix(row, mutedSGR) {
			t.Errorf("row %d does not open with the muted gutter: %q", i, row)
		}
		if hasBackground(row) {
			t.Errorf("row %d of a view is painted like a change: %q", i, row)
		}
	}
	if got := colours(rows[1]); len(got) < 2 {
		t.Errorf("a Go line came back in %d colours, so it was not lexed: %v", len(got), got)
	}
}

// A view carries more than source: the directory listing a view of a folder returns, and the note
// that says where a long file was cut. Lexing those would colour a filename like an expression.
func TestOnlyNumberedLinesAreLexed(t *testing.T) {
	for _, line := range []string{
		"internal/ui/view.go",
		"[clipped at line 40 of 900 — view_range [41, 900] for the rest]",
		"internal/ui/view.go is empty.",
	} {
		row := Listing(line, "internal/ui/view.go", 60)
		if got := colours(row); len(got) != 0 {
			t.Errorf("%q was lexed as code: %v", line, got)
		}
		if !strings.HasPrefix(row, mutedSGR) {
			t.Errorf("%q is not muted: %q", line, row)
		}
	}
}

func TestAListingWrapsUnderItsOwnCode(t *testing.T) {
	long := "  7\t" + strings.Repeat("value + ", 12)
	rows := strings.Split(Listing(long, "main.go", 30), "\n")
	if len(rows) < 2 {
		t.Fatalf("the line did not wrap: %q", rows)
	}
	for i, row := range rows {
		if w := ansi.StringWidth(row); w > 30 {
			t.Errorf("row %d is %d columns, over the 30 available: %q", i, w, ansi.Strip(row))
		}
	}
	// The gutter is "  7" plus its two-space separator, so the code starts at column 5 and the
	// continuation has to line up under it rather than under the number.
	if got := ansi.Strip(rows[1]); !strings.HasPrefix(got, strings.Repeat(" ", 5)+"value") {
		t.Errorf("the continuation row is not aligned under the code: %q", got)
	}
}

// The tool writes a tab after the number, which would land on a terminal tab stop instead of
// beside the gutter, and an indented line would then start in a column nothing else uses.
func TestAListingExpandsTabs(t *testing.T) {
	row := Listing("  9\t\tif err != nil {", "main.go", 60)
	if strings.Contains(row, "\t") {
		t.Errorf("a tab survived into the output: %q", row)
	}
	if got := ansi.Strip(row); !strings.HasPrefix(got, "  9  "+strings.Repeat(" ", TabWidth)+"if") {
		t.Errorf("the indent is not %d spaces after the gutter: %q", TabWidth, got)
	}
}
