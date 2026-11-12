package ui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/charmbracelet/x/ansi"
)

var skillStateColor = map[skills.State]color.Color{
	skills.Done:       readyColor,
	skills.OutOfOrder: pendingColor,
	skills.Skipped:    mutedColor,
	skills.Running:    workingColor,
	skills.Waived:     pendingColor,
}

// nodeStyle is a node drawn in its state's colour, or the waived colour for a stage that holds a waiver.
func nodeStyle(n skills.NodeResult) lipgloss.Style {
	st := lipgloss.NewStyle()
	if n.Waived > 0 {
		return st.Foreground(skillStateColor[skills.Waived])
	}
	if c, ok := skillStateColor[n.State]; ok {
		st = st.Foreground(c)
	}
	return st
}

func skillDone(v *session.SkillView) int {
	done := 0
	for _, n := range v.Nodes {
		if n.State == skills.Done || n.State == skills.OutOfOrder || n.State == skills.Waived {
			done++
		}
	}
	return done
}

// skillProgress is the skill's title with how far it has got, and each node's style by its state.
func skillProgress(v *session.SkillView) (string, []lipgloss.Style) {
	styles := make([]lipgloss.Style, len(v.Nodes))
	for i, n := range v.Nodes {
		styles[i] = nodeStyle(n)
	}
	title := fmt.Sprintf("skill %s · %d of %d done", v.Name, skillDone(v), len(v.Nodes))
	if v.Complete {
		title += " · complete"
	}
	return title, styles
}

// chainRows is the nodes in order joined by arrows, each name whole on a row in its state's colour.
func chainRows(v *session.SkillView, styles []lipgloss.Style, width int) []string {
	width = max(1, width)
	arrow := lipgloss.NewStyle().Foreground(mutedColor).Render(" " + skillGlyph)
	var lines []string
	var row strings.Builder
	rowW := 0
	for i, n := range v.Nodes {
		w := ansi.StringWidth(n.Name)
		if i < len(v.Nodes)-1 {
			w += 2
		}
		if rowW > 0 && rowW+1+w > width {
			lines = append(lines, row.String())
			row.Reset()
			rowW = 0
		}
		if rowW > 0 {
			row.WriteString(" ")
			rowW++
		}
		for _, part := range wrapPlain(n.Name, width) {
			if row.Len() > 0 && rowW+ansi.StringWidth(part) > width {
				lines = append(lines, row.String())
				row.Reset()
				rowW = 0
			}
			row.WriteString(styles[i].Render(part))
			rowW += ansi.StringWidth(part)
		}
		if i < len(v.Nodes)-1 {
			row.WriteString(arrow)
			rowW += 2
		}
	}
	return append(lines, row.String())
}
