package diff

import (
	"regexp"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/code"
	"github.com/charmbracelet/x/ansi"
)

var sgr = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// backgrounds reports every background colour a rendered row sets, as the "r;g;b" of its SGR
// argument. The parameters are walked rather than searched for, because an extended foreground
// carries numbers that a pattern would read as a background of its own.
func backgrounds(row string) []string {
	var out []string
	for _, match := range sgr.FindAllStringSubmatch(row, -1) {
		params := strings.Split(match[1], ";")
		for i := 0; i < len(params); i++ {
			if params[i] != "48" || i+4 >= len(params) || params[i+1] != "2" {
				continue
			}
			out = append(out, strings.Join(params[i+2:i+5], ";"))
			i += 4
		}
	}
	return out
}

func foregrounds(row string) map[string]bool {
	out := map[string]bool{}
	for _, match := range sgr.FindAllStringSubmatch(row, -1) {
		params := strings.Split(match[1], ";")
		for i := 0; i < len(params); i++ {
			if params[i] != "38" || i+4 >= len(params) || params[i+1] != "2" {
				continue
			}
			out[strings.Join(params[i+2:i+5], ";")] = true
			i += 4
		}
	}
	return out
}

func only(t *testing.T, colours []string) string {
	t.Helper()
	if len(colours) == 0 {
		t.Fatal("the row carries no background colour")
	}
	for _, c := range colours[1:] {
		if c != colours[0] {
			t.Fatalf("the row carries more than one background: %v", colours)
		}
	}
	return colours[0]
}

func TestTheTwoSidesAreTintedDifferentlyAcrossTheWholeWidth(t *testing.T) {
	rows := strings.Split(Render("- was := 1\n+ now := 2", "x.go", 40), "\n")
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per line: %q", len(rows), rows)
	}
	removed, added := only(t, backgrounds(rows[0])), only(t, backgrounds(rows[1]))
	if removed == added {
		t.Errorf("both sides are tinted %s; the diff reads as one block", added)
	}
	for i, row := range rows {
		if w := ansi.StringWidth(row); w != 40 {
			t.Errorf("row %d is %d columns, want the tint to fill 40: %q", i, w, ansi.Strip(row))
		}
	}
}

// The point of the change: a tinted line is still code, so it is coloured like code.
func TestCodeIsHighlightedOverTheTint(t *testing.T) {
	const line = "+ func main() { return nil }"
	highlighted := foregrounds(Render(line, "main.go", 60))
	if len(highlighted) < 3 {
		t.Errorf("a Go line came back in %d colours, so it was not lexed: %v", len(highlighted), highlighted)
	}
	// Nothing names the language, so there is nothing to lex and the line is one colour.
	if plain := foregrounds(Render(line, "", 60)); len(plain) != 2 {
		t.Errorf("without a path the line should be the marker's colour and the code's, got %v", plain)
	}
}

// A file the lexers do not know must still come back tinted rather than not at all.
func TestAnUnknownExtensionStillRenders(t *testing.T) {
	row := Render("+ whatever this is", "notes.zzz", 40)
	if only(t, backgrounds(row)) == "" || !strings.Contains(ansi.Strip(row), "whatever this is") {
		t.Errorf("the line was dropped: %q", row)
	}
}

func TestALongLineWrapsUnderItsOwnCode(t *testing.T) {
	rows := strings.Split(Render("+ "+strings.Repeat("ab ", 20), "x.go", 24), "\n")
	if len(rows) < 2 {
		t.Fatalf("the line did not wrap: %q", rows)
	}
	for i, row := range rows {
		if w := ansi.StringWidth(row); w != 24 {
			t.Errorf("row %d is %d columns, want 24: %q", i, w, ansi.Strip(row))
		}
		if only(t, backgrounds(row)) == "" {
			t.Errorf("row %d lost the tint: %q", i, row)
		}
	}
	if got := ansi.Strip(rows[1]); !strings.HasPrefix(got, "  a") {
		t.Errorf("the continuation row is not indented under the code: %q", got)
	}
}

// A tab measures zero columns, so one left in the text pads the row to the wrong width and puts
// the indent wherever the terminal's own tab stops happen to be. The path is left empty because
// a lexer normalises tabs on its own, and the unlexed line is the one at risk.
func TestTabsBecomeSpaces(t *testing.T) {
	row := Render("+\t\treturn nil", "", 40)
	if strings.Contains(row, "\t") {
		t.Errorf("a tab survived into the output: %q", row)
	}
	if got := ansi.Strip(row); !strings.HasPrefix(got, "+ "+strings.Repeat(" ", 2*code.TabWidth)+"return") {
		t.Errorf("the indent is not %d spaces per tab: %q", code.TabWidth, got)
	}
	if w := ansi.StringWidth(row); w != 40 {
		t.Errorf("the row measures %d columns, want 40", w)
	}
}

// Anything that is not a change keeps out of the way: no tint, and so no padding either, since
// trailing spaces are copied along with the text.
func TestUnchangedLinesAreNotTinted(t *testing.T) {
	rows := strings.Split(Render("some prose\n@@ -1 +1 @@", "x.go", 40), "\n")
	for i, row := range rows {
		if bg := backgrounds(row); len(bg) != 0 {
			t.Errorf("row %d is tinted %v", i, bg)
		}
		if w := ansi.StringWidth(row); w == 40 {
			t.Errorf("row %d was padded to the full width: %q", i, ansi.Strip(row))
		}
	}
	if len(foregrounds(rows[1])) == 0 {
		t.Errorf("a hunk header is a landmark and reads as prose: %q", rows[1])
	}
}

func TestProseKeepsTheTerminalsForegroundWhileADiffMutesItsContext(t *testing.T) {
	if row := RenderText("some prose", 40); sgr.MatchString(row) {
		t.Errorf("prose outside the +/- lines was coloured: %q", row)
	}
	if row := Render("some prose", "x.go", 40); !sgr.MatchString(row) {
		t.Errorf("a diff's unchanged line lost its muted colour: %q", row)
	}
}
