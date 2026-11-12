// Package diff draws the +/- blocks an edit produces: a soft tint behind each side, the code on
// top of it syntax highlighted.
//
// The highlighting, the palette and the wrapping come from internal/code, which every code surface
// in the pane shares; what is here is the diff shape alone.
package diff

import (
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/code"
	"github.com/charmbracelet/x/ansi"
)

// The tints are dark so a pastel syntax palette stays legible over them; the markers are the same
// two hues at full strength, which is what makes the left edge scannable.
var (
	addTint   = lipgloss.Color("#1d3b2a")
	delTint   = lipgloss.Color("#45242c")
	addMarker = lipgloss.Color("#a6e3a1")
	delMarker = lipgloss.Color("#f38ba8")
)

// Render lays a diff out for a pane of the given width, wrapping a long line under its own code.
func Render(text, path string, width int) string {
	return render(text, path, width, code.Muted)
}

// RenderText draws prose that may hold +/- lines: the lines outside them keep the terminal's own
// foreground, since they are the text being read rather than a change's unchanged context.
func RenderText(text string, width int) string {
	return render(text, "", width, nil)
}

func render(text, path string, width int, context color.Color) string {
	key := strconv.Itoa(width) + "|" + strconv.FormatBool(context == nil) + "|" + path + "|" + text
	return blocks.Get(key, func() string {
		var rows []string
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			rows = append(rows, lineRows(line, path, width, context)...)
		}
		return strings.Join(rows, "\n")
	})
}

var blocks code.Blocks

func lineRows(line, path string, width int, context color.Color) []string {
	var marker string
	var tint, markerColor color.Color
	switch {
	case strings.HasPrefix(line, "+"):
		marker, tint, markerColor = "+", addTint, addMarker
	case strings.HasPrefix(line, "-"):
		marker, tint, markerColor = "-", delTint, delMarker
	case strings.HasPrefix(line, "@@"):
		return contextRows(line, code.Landmark, width)
	default:
		return contextRows(line, context, width)
	}

	// The marker and its separating space are drawn here, so one already in the text would be
	// doubled. Continuation rows indent by the same two columns.
	source := code.ExpandTabs(strings.TrimPrefix(line[1:], " "))
	gutter := lipgloss.NewStyle().Foreground(markerColor).Background(tint).Render(marker) +
		lipgloss.NewStyle().Background(tint).Render(" ")
	indent := lipgloss.NewStyle().Background(tint).Render("  ")

	codeWidth := max(1, width-2)
	var rows []string
	for i, chunk := range code.Rows(source, codeWidth) {
		row := gutter
		if i > 0 {
			row = indent
		}
		rows = append(rows, row+code.Line(chunk, path, tint)+pad(chunk, codeWidth, tint))
	}
	return rows
}

// A line that is not part of the change carries no tint, so it is not padded either: trailing
// spaces would be copied along with the text.
func contextRows(line string, fg color.Color, width int) []string {
	style := lipgloss.NewStyle()
	if fg != nil {
		style = style.Foreground(fg)
	}
	var rows []string
	for _, chunk := range code.Rows(code.ExpandTabs(line), max(1, width)) {
		rows = append(rows, style.Render(chunk))
	}
	return rows
}

// The tint has to reach the right edge or it reads as a highlight on the text rather than a diff,
// which is what the padding buys.
func pad(chunk string, width int, tint color.Color) string {
	n := width - ansi.StringWidth(chunk)
	if n <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Background(tint).Render(strings.Repeat(" ", n))
}
