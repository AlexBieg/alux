package ui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const barCols = 2

// barInner is the width content has to be wrapped to before barred draws it at width.
func barInner(width int) int {
	return max(0, width-barCols)
}

func barred(lines []string, width int, tone color.Color) []string {
	inner := barInner(width)
	if inner == 0 {
		return lines
	}
	if tone == nil {
		tone = mutedColor
	}
	bar := lipgloss.NewStyle().Foreground(tone).Render("│") + " "
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = bar + ansi.Truncate(line, inner, "")
	}
	return out
}

const bracketCols = 2

// bracketed is lines behind the bracket that ties one compose call's steps together.
func bracketed(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = bracketRow("│ ") + line
	}
	return out
}

func bracketRow(s string) string { return lipgloss.NewStyle().Foreground(mutedColor).Render(s) }

// frameCols is what a boxed prompt's border and padding take on each side.
const frameCols = 2

// frameInner is the width a boxed prompt's text is wrapped to, zero where the box cannot fit.
func frameInner(width int) int {
	if inner := width - 2*frameCols; inner >= minFrameInner {
		return inner
	}
	return 0
}

// minFrameInner fits the caret and a few cells of text.
const minFrameInner = 6

// promptBox draws lines in a rounded box width wide, the way the input box draws what is typed.
func promptBox(lines []string, width int, tone color.Color) []string {
	inner := frameInner(width)
	edge := lipgloss.NewStyle().Foreground(tone)
	rule := strings.Repeat("─", width-2)
	out := make([]string, 0, len(lines)+2)
	out = append(out, edge.Render("╭"+rule+"╮"))
	for _, line := range lines {
		line = ansi.Truncate(line, inner, "")
		pad := strings.Repeat(" ", inner-ansi.StringWidth(line))
		out = append(out, edge.Render("│")+" "+line+pad+" "+edge.Render("│"))
	}
	return append(out, edge.Render("╰"+rule+"╯"))
}
