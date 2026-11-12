package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Splits a rendered table into its divider lines and everything else, by shape.
func dividers(out string) (rules []string, other []string) {
	for _, line := range strings.Split(out, "\n") {
		if plain := ansi.Strip(line); isDivider(plain) {
			rules = append(rules, line)
		} else {
			other = append(other, line)
		}
	}
	return rules, other
}

// The point of the whole exercise: one rule per row boundary, and none between the lines a
// wrapped cell spills onto — which is what a naive post-pass over the rendered text would get
// wrong.
func TestRulesFallOnRowBoundariesNotWrappedLines(t *testing.T) {
	md := `| check | note |
| --- | --- |
| first | ` + strings.Repeat("spilling over the column ", 4) + ` |
| second | short |
| third | ` + strings.Repeat("also rather long here ", 4) + ` |
`
	out := Render(md, 60)
	rules, body := dividers(out)

	// One under the header, then one between each pair of the three body rows.
	if len(rules) != 3 {
		t.Fatalf("got %d divider lines, want 3:\n%s", len(rules), out)
	}
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(strings.Join(body, "\n"), want) {
			t.Errorf("row %q is missing from the body", want)
		}
	}
	if strings.Contains(out, rowRuleMark) {
		t.Errorf("the sentinel reached the screen:\n%q", out)
	}
}

// A rule is indistinguishable from the header divider: same glyphs, same styling, so the table
// reads at one brightness throughout rather than fading below the header.
func TestRulesAreIdenticalToTheHeaderDivider(t *testing.T) {
	out := Render("| a | b |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |\n", 40)
	rules, _ := dividers(out)
	if len(rules) != 2 {
		t.Fatalf("got %d divider lines, want 2:\n%s", len(rules), out)
	}

	header, rule := rules[0], rules[1]
	if rule != header {
		t.Errorf("rule differs from the header divider, so it does not read at the same brightness:\n%q\n%q", header, rule)
	}
	if strings.Contains(rule, "\x1b[") {
		t.Errorf("rule carries styling glamour did not put on the divider: %q", rule)
	}
}

// A table in a code fence is sample text, and rewriting it would show the reader something
// other than what the model wrote.
func TestTableInsideACodeFenceIsLeftAlone(t *testing.T) {
	md := "```\n| a | b |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |\n```\n"
	if got := markRowRules(md); got != md {
		t.Errorf("code fence was rewritten:\n%q", got)
	}
	if rules, _ := dividers(Render(md, 40)); len(rules) != 0 {
		t.Errorf("a fenced table was given rules:\n%s", Render(md, 40))
	}
}

func TestEscapedPipeDoesNotShiftTheColumnCount(t *testing.T) {
	if got := cellCount(`| a \| b | c |`); got != 2 {
		t.Errorf("cellCount = %d, want 2", got)
	}
}
