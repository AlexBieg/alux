package markdown

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/code"
	"github.com/charmbracelet/x/ansi"
)

func widest(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if n := lipgloss.Width(line); n > w {
			w = n
		}
	}
	return w
}

// A table wider than the pane makes the terminal rewrap its border rows into unreadable rubble.
func TestWideTableStaysInsideThePane(t *testing.T) {
	md := `| column one | column two |
| --- | --- |
| ` + strings.Repeat("long cell content ", 8) + ` | ` + strings.Repeat("more text here ", 8) + ` |
`
	for _, width := range []int{40, 60, 100} {
		out := Render(md, width)
		if got := widest(out); got > width {
			t.Errorf("width %d: rendered table is %d columns wide", width, got)
		}
	}
}

// A word longer than its column must be broken across lines, not cut off. cli-table3 truncated
// it, and a lost word is worse than a cramped table — so this asserts the whole word is still
// recoverable from the output, which is what distinguishes wrapping from an ellipsis.
func TestLongUnbreakableWordIsWrappedNotTruncated(t *testing.T) {
	word := "supercalifragilisticexpialidocious_and_then_a_great_deal_more_besides"
	out := Render("| a |\n| --- |\n| "+word+" |\n", 30)

	// Rejoin the wrapped cell: strip styling, then remove the layout whitespace and borders the
	// renderer inserted between the fragments.
	flat := ansi.Strip(out)
	for _, cut := range []string{" ", "\n", "│", "|"} {
		flat = strings.ReplaceAll(flat, cut, "")
	}
	if !strings.Contains(flat, word) {
		t.Errorf("the long word was truncated rather than wrapped; recovered %q from:\n%s", flat, out)
	}
}

func TestProseWrapsToTheWidth(t *testing.T) {
	out := Render(strings.Repeat("word ", 100), 50)
	if got := widest(out); got > 50 {
		t.Errorf("prose is %d columns wide, want <= 50", got)
	}
}

// glamour's stock document style opens each block with a blank line and indents it two columns,
// which is right for a document and wrong for a transcript line that must sit beside a prompt
// marker. It also pads every line out to the wrap width with styled spaces — invisible, but they
// make a short reply measure the full pane and they are what gets copied when it is selected.
func TestOutputIsTightEnoughToSitBesideAMarker(t *testing.T) {
	out := Render("hello there", 40)

	if strings.HasPrefix(out, "\n") {
		t.Errorf("output opens with a blank line, so it would drop below its marker: %q", out)
	}
	if plain := ansi.Strip(out); strings.HasPrefix(plain, " ") {
		t.Errorf("output is indented, so it would not align with its marker: %q", plain)
	}
	for i, line := range strings.Split(out, "\n") {
		if plain := ansi.Strip(line); plain != strings.TrimRight(plain, " \t") {
			t.Errorf("line %d is padded out to the wrap width: %q", i, plain)
		}
	}
	if got, want := ansi.StringWidth(out), ansi.StringWidth("hello there"); got != want {
		t.Errorf("rendered width = %d, want %d — the line should end where its text ends", got, want)
	}
}

// The same tightness, for the blocks that now open with a multi-byte glyph. These are the lines
// that caught trimBlock measuring the text in bytes and the width in columns: a bullet or a bar
// made the text measure longer than it drew, so the padding was never cut. A quote also arrived
// with a blank line in front of it, which would drop it below the marker it has to sit beside.
func TestAGlyphBlockIsTightToo(t *testing.T) {
	for _, md := range []string{"- item text", "1. item text", "> quoted text", "# A heading"} {
		out := Render(md, 40)
		if ansi.StringWidth(strings.Split(out, "\n")[0]) == 0 {
			t.Errorf("%q opens with a blank line: %q", md, out)
		}
		for i, line := range strings.Split(out, "\n") {
			if plain := ansi.Strip(line); plain != strings.TrimRight(plain, " \t") {
				t.Errorf("%q line %d is padded out to the wrap width: %q", md, i, plain)
			}
		}
	}
}

func TestUnparseableInputFallsBackToSource(t *testing.T) {
	if got := Render("", 40); got != "" {
		t.Errorf("empty input = %q", got)
	}
}

// A fenced snippet and the same line shown by a tool are the same code, so they are the same
// colours. The expectation is taken from internal/code rather than written down here: hard-coding
// the hue would keep passing if the two palettes drifted apart, which is the only thing this test
// is for.
func TestAFencedBlockIsColouredLikeTheToolsCode(t *testing.T) {
	const line = "func main() { return nil }"
	out := Render("```go\n"+line+"\n```", 60)

	var missing []string
	for hue := range truecolours(code.Line(line, "x.go", nil)) {
		if !strings.Contains(out, hue) {
			missing = append(missing, hue)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the fence is missing %d of the colours internal/code draws for the same line: %s",
			len(missing), strings.Join(missing, " "))
	}
}

// glamour's stock dark palette, which the fence must no longer be using.
func TestTheFenceDroppedGlamoursOwnPalette(t *testing.T) {
	out := Render("```go\nfunc main() {}\n```", 60)
	if strings.Contains(out, "0;170;255") { // #00AAFF, DarkStyleConfig's keyword
		t.Errorf("the fence still draws glamour's keyword blue:\n%q", out)
	}
}

var headingSources = []string{"# One", "## Two", "### Three", "#### Four", "##### Five", "###### Six"}

// A heading is markup the reader already saw in what they typed, and the stock H1 is a highlight
// bar — text on a background colour, padded with a space either side — which reads as emphasis
// inside a line rather than as a heading above one.
func TestHeadingsDropTheirMarkupAndTheHighlightBar(t *testing.T) {
	for _, md := range headingSources {
		out := Render(md, 40)
		word := strings.TrimLeft(md, "# ")
		if text := ansi.Strip(strings.Split(out, "\n")[0]); !strings.Contains(strings.ToLower(text), strings.ToLower(word)) {
			t.Errorf("%q rendered as %q, which does not contain %q", md, text, word)
		}
		if strings.Contains(ansi.Strip(out), "#") {
			t.Errorf("%q kept its markup: %q", md, ansi.Strip(out))
		}
		if background(out) {
			t.Errorf("%q draws a background colour, so it is still a bar: %q", md, out)
		}
	}
}

// The point of the weight ramp: a level is told apart by how much ink it carries, not by recalling
// which hue means what. So no two levels may render the same way — if any pair collides, the
// reader is back to guessing between them. This is the property the whole scheme exists for, and
// it is why the test compares full signatures rather than any single attribute.
func TestNoTwoHeadingLevelsLookAlike(t *testing.T) {
	seen := map[string]string{}
	for _, md := range headingSources {
		out := Render(md, 40)
		first := strings.Split(out, "\n")[0]
		signature := fmt.Sprintf("upper=%t attrs=%s hue=%v rule=%t",
			ansi.Strip(first) == strings.ToUpper(ansi.Strip(first)),
			attributesOf(first), truecolours(first),
			strings.Contains(ansi.Strip(out), "═"))

		if other, ok := seen[signature]; ok {
			t.Errorf("%q and %q render identically (%s)", md, other, signature)
		}
		seen[signature] = md
	}
}

// Only h1 gets a rule, and only h2 an underline. Both are load-bearing: h1 is rare and reads as
// the reply's title, where a rule under every h2 was visibly heavy, and the underline is what
// separates h2 from h3 without spending a row.
func TestOnlyTheTopTwoLevelsCarryTheirOwnDecoration(t *testing.T) {
	for i, md := range headingSources {
		out := Render(md, 40)
		if ruled := strings.Contains(ansi.Strip(out), "═"); ruled != (i == 0) {
			t.Errorf("%q has a rule = %t, want %t", md, ruled, i == 0)
		}
		if underlined := strings.Contains(attributesOf(out), "underline"); underlined != (i == 1) {
			t.Errorf("%q is underlined = %t, want %t", md, underlined, i == 1)
		}
	}
}

// Weight sorts the levels into tiers but does not separate all six, so hue carries the levels that
// share a tier: h3 against h4, h5 against h6. Every level therefore needs a hue of its own, and
// each has to survive glamour's cascade from the shared Heading style — a level that silently
// inherited its neighbour's colour would be indistinguishable from it.
func TestEveryHeadingLevelDrawsItsOwnHue(t *testing.T) {
	seen := map[string]string{}
	for _, md := range headingSources {
		hues := truecolours(strings.Split(Render(md, 40), "\n")[0])
		if len(hues) != 1 {
			t.Errorf("%q drew %d hues, want exactly 1: %v", md, len(hues), hues)
			continue
		}
		for hue := range hues {
			if other, ok := seen[hue]; ok {
				t.Errorf("%q draws the same hue as %q (%s)", md, other, hue)
			}
			seen[hue] = md
		}
	}
}

// glamour's renderText never reads StylePrimitive.Faint, so a faint step in the ramp would be
// accepted and silently dropped — the same trap as Format. This pins the workaround: nothing in
// the ramp may depend on Faint.
func TestTheRampDoesNotRelyOnFaint(t *testing.T) {
	for _, md := range headingSources {
		if strings.Contains(attributesOf(Render(md, 40)), "faint") {
			t.Errorf("%q draws faint, which glamour does not render", md)
		}
	}
}

// attributesOf names the SGR attributes a string carries. The colour triples have to be stepped
// over rather than split blindly: "38;2;137;180;250" contains a bare "2", which reads as faint,
// and "48" turns up inside plenty of triples too.
func attributesOf(s string) string {
	names := map[string]string{"1": "bold", "2": "faint", "3": "italic", "4": "underline", "7": "inverse"}
	var out []string
	seen := map[string]bool{}
	for _, match := range sgrPattern.FindAllStringSubmatch(s, -1) {
		params := strings.Split(match[1], ";")
		for i := 0; i < len(params); i++ {
			if (params[i] == "38" || params[i] == "48") && i+1 < len(params) && params[i+1] == "2" {
				i += 4 // step over the r;g;b triple
				continue
			}
			if name := names[params[i]]; name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return strings.Join(out, "+")
}

// A rule is the one element whose text depends on the pane width, which is why the style is built
// per width. If that ever reverts to a fixed string the rule stops reaching across the pane — or
// overshoots it and the terminal rewraps it onto a second line.
func TestARuleSpansThePane(t *testing.T) {
	for _, width := range []int{30, 72, 100} {
		var rules []string
		for _, line := range strings.Split(Render("above\n\n---\n\nbelow", width), "\n") {
			if plain := ansi.Strip(line); strings.HasPrefix(plain, "─") {
				rules = append(rules, plain)
			}
		}
		if len(rules) != 1 {
			t.Fatalf("width %d: found %d rules, want 1", width, len(rules))
		}
		if got := lipgloss.Width(rules[0]); got != width {
			t.Errorf("width %d: the rule is %d columns", width, got)
		}
	}
}

// glamour draws a BlockPrefix with the parent block's style, so the stock bullet cannot be
// coloured where the stock style keeps it — the colour is accepted and then dropped. This pins the
// move to Prefix, and pins that it only accents the marker: tinting the item's text too would make
// a list read as a block of coloured prose.
func TestABulletIsAccentedAndItsTextIsNot(t *testing.T) {
	const text = "item text"
	out := Render("- "+text, 40)

	if !strings.Contains(styleBefore(out, "•"), rgb(markerGreen)) {
		t.Errorf("the bullet is not drawn in the marker colour: %q", out)
	}
	if strings.Contains(styleBefore(out, text), rgb(markerGreen)) {
		t.Errorf("the item's text is drawn in the marker colour too: %q", out)
	}
}

func TestAQuoteHangsOffABarAndIsItalic(t *testing.T) {
	out := Render("> quoted", 40)

	if got := ansi.Strip(out); !strings.HasPrefix(got, "▏ ") {
		t.Errorf("the quote has no bar: %q", got)
	}
	style := styleBefore(out, "quoted")
	if !strings.Contains(style, rgb(quoteLavender)) {
		t.Errorf("the quote is not drawn in the quote colour: %q", out)
	}
	if !slices.Contains(strings.Split(style, ";"), "3") {
		t.Errorf("the quote is not italic: %q", out)
	}
}

// background reports whether anything is drawn on a background colour. The parameters have to be
// split before they are compared: "48" occurs inside plenty of foreground triples — 148;226;213
// among them — so a substring search finds a background that is not there.
func background(s string) bool {
	for _, match := range sgrPattern.FindAllStringSubmatch(s, -1) {
		for _, param := range strings.Split(match[1], ";") {
			if param == "48" || param == "100" { // set-background, and bright-black-background
				return true
			}
		}
	}
	return false
}

// rgb turns a hex colour from the style config into the SGR parameters it is rendered as, so a
// test names the colour the code names rather than a triple nobody can read.
func rgb(hex string) string {
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
}

// styleBefore returns the parameters of the last SGR sequence to open before substr — the style
// substr is actually drawn in.
func styleBefore(s, substr string) string {
	cut := strings.Index(s, substr)
	if cut < 0 {
		return ""
	}
	matches := sgrPattern.FindAllStringSubmatch(s[:cut], -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

func truecolours(s string) map[string]bool {
	out := map[string]bool{}
	for _, match := range sgrPattern.FindAllStringSubmatch(s, -1) {
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

var sgrPattern = regexp.MustCompile(`\x1b\[([0-9;]*)m`)
