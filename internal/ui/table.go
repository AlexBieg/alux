package ui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/alexbieg/alux/internal/report"
	"github.com/charmbracelet/x/ansi"
)

var toneColor = map[report.Tone]color.Color{
	report.Muted:  mutedColor,
	report.OK:     lipgloss.Green,
	report.Active: workingColor,
	report.Bad:    errorColor,
	report.Warn:   pendingColor,
}

func toneStyle(t report.Tone) lipgloss.Style {
	if c, ok := toneColor[t]; ok {
		return lipgloss.NewStyle().Foreground(c)
	}
	return lipgloss.NewStyle()
}

// rowGap fills the rule lipgloss/table draws between rows, which is where each row's Detail goes.
// Every cell pads with ordinary spaces, so only that rule is made wholly of it.
const rowGap = "\u00a0"

// renderTable draws t two columns in, shrinking and wrapping its columns to fit width.
func renderTable(t *report.Table, width int) string {
	var out []string
	if t.Preamble != "" {
		out = append(append(out, indented(t.Preamble, width)...), "")
	}
	if len(t.Rows) == 0 {
		return strings.Join(append(out, indented(t.Empty, width)...), "\n")
	}

	rows := make([][]string, len(t.Rows))
	for i, r := range t.Rows {
		for _, c := range r.Cells {
			rows[i] = append(rows[i], c.Text)
		}
	}
	last := len(t.Head) - 1
	style := func(row, col int) lipgloss.Style {
		st := lipgloss.NewStyle()
		switch {
		case row == table.HeaderRow:
			st = st.Foreground(mutedColor).Bold(true)
		case row < len(t.Rows) && col < len(t.Rows[row].Cells):
			st = toneStyle(t.Rows[row].Cells[col].Tone)
		}
		if col < last {
			st = st.PaddingRight(2)
		}
		return st
	}
	build := func() *table.Table {
		return table.New().
			Border(lipgloss.Border{Top: "─", Bottom: rowGap}).
			BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).BorderColumn(false).
			BorderHeader(true).BorderRow(true).
			BorderStyle(dimStyle).
			Headers(t.Head...).Rows(rows...).StyleFunc(style)
	}
	inner := max(1, width-2)
	drawn := build().String()
	if lipgloss.Width(drawn) > inner {
		drawn = build().Width(inner).String()
	}

	lines := strings.Split(drawn, "\n")
	indent := secondColumn(ansi.Strip(lines[0]), t.Head) + 2
	pad := strings.Repeat(" ", indent)
	row := 0
	detail := func() {
		if row < len(t.Rows) && t.Rows[row].Detail != "" {
			out = append(out, hang(pad, indent, t.Rows[row].Detail, toneStyle(t.Rows[row].DetailTone), width)...)
		}
		row++
	}
	for _, line := range lines {
		if plain := ansi.Strip(line); plain != "" && strings.Trim(plain, rowGap) == "" {
			detail()
			continue
		}
		out = append(out, "  "+line)
	}
	detail()

	if t.Footer != "" {
		out = append(append(out, ""), hang("  ", 2, t.Footer, dimStyle, width)...)
	}
	return strings.Join(out, "\n")
}

// secondColumn is where the second header starts in the drawn header line.
func secondColumn(header string, head []string) int {
	if len(head) < 2 {
		return 0
	}
	runes := []rune(header)
	i := min(len([]rune(head[0])), len(runes))
	for i < len(runes) && runes[i] == ' ' {
		i++
	}
	return i
}

// indented wraps each line of text under its own leading spaces.
func indented(text string, width int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		n := len(line) - len(strings.TrimLeft(line, " "))
		out = append(out, hang(line[:n], n, line[n:], lipgloss.NewStyle(), width)...)
	}
	return out
}
