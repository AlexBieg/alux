package ui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// panelInner is the width a panel's rows get once its border and padding are taken off.
func panelInner(width int) int {
	return max(0, width-4)
}

// panelLead is the top edge before a panel's title.
const panelLead = "╭─ "

// panelHeadTitle is the title as a top edge this wide draws it.
func panelHeadTitle(title string, width int) string {
	return ellipsize(title, max(1, width-6))
}

// framed is rows in a rounded border width wide, pad columns inside each side; an empty title leaves the top edge bare.
func framed(title string, rows []string, width, pad int, edge lipgloss.Style) []string {
	inner := max(0, width-2-2*pad)
	side := edge.Render("│")
	gap := strings.Repeat(" ", pad)
	head := "╭"
	if title != "" {
		head = panelLead + panelHeadTitle(title, width) + " "
	}
	lines := []string{edge.Render(head + strings.Repeat("─", max(0, width-ansi.StringWidth(head)-1)) + "╮")}
	for _, r := range rows {
		r = ansi.Truncate(r, inner, "")
		lines = append(lines, side+gap+r+strings.Repeat(" ", max(0, inner-ansi.StringWidth(r)))+gap+side)
	}
	return append(lines, edge.Render("╰"+strings.Repeat("─", max(0, width-2))+"╯"))
}

// statusRow is a state dot, an optional styled lead such as a face, and a name, then the first of
// rights (widest first) that leaves the name room, flush right.
func statusRow(tone color.Color, lead, name string, paint func(...string) string, width int, rights ...string) string {
	left := 2
	if lead != "" {
		left += ansi.StringWidth(lead) + 1
	}
	right, rightW := "", 0
	for i, r := range rights {
		w := ansi.StringWidth(r)
		room := width - left - 1 - w
		if room >= 8 || (i == len(rights)-1 && room >= 4) {
			right, rightW = r, w
			break
		}
	}
	room := width - left
	if right != "" {
		room -= 1 + rightW
	}
	name = ellipsize(name, max(1, room))
	row := lipgloss.NewStyle().Foreground(tone).Render("●") + " "
	if lead != "" {
		row += lead + " "
	}
	row += paint(name)
	if right != "" {
		row += strings.Repeat(" ", max(1, width-left-ansi.StringWidth(name)-rightW)) + right
	}
	return row
}
