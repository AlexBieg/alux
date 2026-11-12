package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// wrapPlain breaks text into rows of at most width columns, dropping the space each break leaves.
func wrapPlain(text string, width int) []string {
	lines := strings.Split(ansi.Wrap(text, max(1, width), ""), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return lines
}

// styledWrap wraps before styling, since a style laid over a wrapped string stays open across its breaks.
func styledWrap(text string, st lipgloss.Style, width int) []string {
	lines := wrapPlain(text, width)
	for i, line := range lines {
		if line != "" {
			lines[i] = st.Render(line)
		}
	}
	return lines
}

// wrapRows is styledWrap capped at rows, 0 for no cap.
func wrapRows(text string, st lipgloss.Style, width, rows int) []string {
	return clipRows(styledWrap(text, st, width), rows, "")
}

// hang wraps text under its first line after prefix, or under prefix when too narrow for that.
func hang(prefix string, prefixW int, text string, st lipgloss.Style, width int) []string {
	text = strings.TrimSpace(text)
	if width-prefixW < 10 {
		return append([]string{oneRow(strings.TrimRight(prefix, " "), width)}, styledWrap(text, st, width)...)
	}
	lines := styledWrap(text, st, width-prefixW)
	pad := strings.Repeat(" ", prefixW)
	for i, line := range lines {
		switch {
		case i == 0:
			lines[i] = prefix + line
		case line != "":
			lines[i] = pad + line
		}
	}
	return lines
}

// clipRows keeps the first rows lines, the last saying how many were dropped; rows <= 0 keeps all.
func clipRows(lines []string, rows int, indent string) []string {
	if rows <= 0 || len(lines) <= rows {
		return lines
	}
	kept := append([]string{}, lines[:rows-1]...)
	return append(kept, dimStyle.Render(fmt.Sprintf("%s… %d more lines", indent, len(lines)-rows+1)))
}

// oneRow cuts s to width columns. Only for text that must stay on one row; everything else wraps.
func oneRow(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}
