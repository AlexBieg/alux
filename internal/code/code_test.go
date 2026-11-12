package code

import (
	"regexp"
	"strings"
	"testing"
)

var sgr = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// colours reports every truecolour foreground a rendered row sets, as the "r;g;b" of its SGR
// argument. The parameters are walked rather than searched for, because a background carries
// numbers that a pattern would read as a foreground of its own.
func colours(row string) map[string]bool {
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

func hasBackground(row string) bool {
	for _, match := range sgr.FindAllStringSubmatch(row, -1) {
		for _, param := range strings.Split(match[1], ";") {
			if param == "48" {
				return true
			}
		}
	}
	return false
}

// The two named colours as they reach the wire, so a test can name what it expects to see.
const (
	landmarkSGR = "137;180;250" // Landmark, #89b4fa
	keywordSGR  = "203;166;247" // the theme's keyword mauve, #cba6f7
	mutedSGR    = "\x1b[90m"    // Muted, lipgloss.BrightBlack
)

func TestLineColoursCodeForThePathsLanguage(t *testing.T) {
	// Not a Go keyword, so Go leaves it plain and only the Python lexer calls it one.
	const line = "def x"
	if got := colours(Line(line, "a.py", nil)); !got[keywordSGR] {
		t.Errorf("def is a keyword in Python and came back %v", got)
	}
	if got := colours(Line(line, "b.go", nil)); got[keywordSGR] {
		t.Errorf("def is an identifier in Go and came back as a keyword: %v", got)
	}
}

// Nothing names a language, so there is nothing to lex and the line is one colour.
func TestLineWithoutAPathIsPlain(t *testing.T) {
	if got := colours(Line("def x", "", nil)); len(got) != 1 {
		t.Errorf("want the plain colour alone, got %v", got)
	}
}

// Line draws over whatever the caller is drawing on, and a file view draws on nothing: a
// background here would paint a read as if it were a change.
func TestLineTakesNoBackgroundUnlessGivenOne(t *testing.T) {
	if hasBackground(Line("func main() {}", "a.go", nil)) {
		t.Error("a nil background still set one")
	}
	if !hasBackground(Line("func main() {}", "a.go", Plain)) {
		t.Error("a background was given and not drawn")
	}
}

func TestRowsWrapsToWidth(t *testing.T) {
	text := strings.Repeat("ab ", 20)
	if rows := Rows(text, 24); len(rows) < 2 {
		t.Errorf("wrapping produced %d rows: %q", len(rows), rows)
	}
}

func TestBlocksBuildsOnceAndSurvivesAFullTable(t *testing.T) {
	var blocks Blocks
	builds := 0
	build := func() string {
		builds++
		return "rendered"
	}

	if got := blocks.Get("k", build); got != "rendered" {
		t.Errorf("first call returned %q", got)
	}
	if got := blocks.Get("k", build); got != "rendered" || builds != 1 {
		t.Errorf("the second call returned %q after %d builds, want the cached one", got, builds)
	}

	// Past the cap the table is dropped whole, which must not lose the answer being asked for.
	for i := 0; i < maxCached+2; i++ {
		if got := blocks.Get(string(rune('a'+i%26))+strings.Repeat("x", i), build); got != "rendered" {
			t.Fatalf("key %d returned %q", i, got)
		}
	}
	if len(blocks.table) > maxCached {
		t.Errorf("the table holds %d blocks, over the %d cap", len(blocks.table), maxCached)
	}
}
