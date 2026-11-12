package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/graphview"
	"github.com/alexbieg/alux/internal/session"
)

// pictureRows paints each node's cells in its style and the edges between them in edge.
func pictureRows(pic graphview.Picture, styles []lipgloss.Style, edge lipgloss.Style) []string {
	rows := make([]string, len(pic.Cells))
	for y, row := range pic.Cells {
		var b strings.Builder
		for x := 0; x < len(row); {
			owner := row[x].Node
			var run strings.Builder
			for ; x < len(row) && row[x].Node == owner; x++ {
				run.WriteString(row[x].S)
			}
			text := run.String()
			switch {
			case owner >= 0:
				b.WriteString(styles[owner].Render(text))
			case strings.TrimSpace(text) == "":
				b.WriteString(text)
			default:
				b.WriteString(edge.Render(text))
			}
		}
		rows[y] = strings.TrimRight(b.String(), " ")
	}
	return rows
}

// owned is rows with the detail box each belongs to, noOwner for rows that take no click.
type owned struct {
	lines []string
	owner []int
}

func (o *owned) add(lines []string, id int) {
	for _, l := range lines {
		o.lines = append(o.lines, l)
		o.owner = append(o.owner, id)
	}
}

// drawItems draws transcript items the way the main transcript does: framed, a blank between, and a tool's run of calls under one bar.
func (m *Model) drawItems(o *owned, items []session.Item, width int) {
	var prev session.Item
	for i, item := range items {
		_, framed := frameFor(item, width)
		if i > 0 && !(framed && groupable(prev, item)) {
			o.add([]string{""}, noOwner)
		}
		o.add(m.framedLines(item, width), m.boxID(item))
		prev = item
	}
}
