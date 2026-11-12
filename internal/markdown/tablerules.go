package markdown

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Private use area: glamour passes it through untouched and real text never contains it.
const rowRuleMark = "\uE000"

// glamour builds its tables with lipgloss, which can rule between body rows, but it never
// enables that and keeps the table on an unexported field. So the rule is smuggled through the
// source: markRowRules inserts a sentinel row between the body rows of every pipe table, and
// drawRowRules turns the line it rendered to into a rule.
func markRowRules(md string) string {
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	fenced := false

	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		if isFence(lines[i]) {
			fenced = !fenced
			continue
		}
		if fenced || !isTableRow(lines[i]) || i+1 >= len(lines) || !isDelimiterRow(lines[i+1]) {
			continue
		}

		out = append(out, lines[i+1])
		rule := sentinelRow(cellCount(lines[i]))
		i++
		for body := 0; i+1 < len(lines) && isTableRow(lines[i+1]); body++ {
			if body > 0 {
				out = append(out, rule)
			}
			out = append(out, lines[i+1])
			i++
		}
	}
	return strings.Join(out, "\n")
}

// drawRowRules replaces each rendered sentinel row with a copy of the divider above it, styled
// exactly as glamour styled that divider — which is not at all — so every rule in the table
// reads at the same brightness as the header's.
func drawRowRules(rendered string) string {
	if !strings.Contains(rendered, rowRuleMark) {
		return rendered
	}

	lines := strings.Split(rendered, "\n")
	out := make([]string, 0, len(lines))
	divider := ""
	for _, line := range lines {
		plain := strings.TrimRight(ansi.Strip(line), " ")
		if isDivider(plain) {
			divider = plain
		}
		switch {
		case !strings.Contains(plain, rowRuleMark):
			out = append(out, line)
		case divider != "":
			out = append(out, divider)
		}
	}
	return strings.Join(out, "\n")
}

func isFence(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func isTableRow(line string) bool {
	if len(line)-len(strings.TrimLeft(line, " ")) > 3 {
		return false // four spaces in, it is a code block
	}
	trimmed := strings.TrimSpace(line)
	return len(trimmed) > 1 && strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|")
}

func isDelimiterRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	return isTableRow(line) && strings.Contains(trimmed, "-") && strings.Trim(trimmed, "|:- \t") == ""
}

// The divider glamour draws under a header: box-drawing runes and nothing else.
func isDivider(plain string) bool {
	return strings.Contains(plain, "─") && strings.Trim(plain, "─┼") == ""
}

func cellCount(row string) int {
	inner := strings.Trim(strings.TrimSpace(row), "|")
	cells, escaped := 1, false
	for _, r := range inner {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '|':
			cells++
		}
	}
	return cells
}

func sentinelRow(cells int) string {
	return "|" + rowRuleMark + strings.Repeat("|", cells)
}
