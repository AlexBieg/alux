package ui

// Text selection, owned by the application rather than the terminal.
//
// Mouse reporting is all-or-nothing at the terminal level, so a pane that wants click-to-expand
// has taken drag-select away from the terminal and owes the user a replacement. This file is
// that replacement: the geometry of a selection, the highlight drawn over already-styled text,
// and the plain text a copy produces.
//
// The one idea that makes it tractable: never splice ANSI strings. A rendered transcript line is
// decomposed into a grid of styled cells, the selected cells get a background, and the grid is
// rendered back. Syntax colouring, diff tints and hyperlinks survive because they are properties
// of the cells, not of the string.

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// point is a position in the transcript: an index into the rendered lines, plus a cell column
// within that line. Columns are cell indices and not byte offsets, which is what makes them
// comparable with the coordinates bubbletea reports for the mouse.
type point struct {
	line, col int
}

// before reports whether p comes earlier in the transcript than q.
func (p point) before(q point) bool {
	return p.line < q.line || (p.line == q.line && p.col < q.col)
}

// selection is a range over the rendered transcript, held as the point the gesture started from
// and the point it has reached. They are kept in gesture order rather than sorted so that
// dragging back above the anchor keeps the anchor fixed, which is what makes a drag feel right.
type selection struct {
	anchor, head point

	// dragging is true between press and release. A selection outlives the drag — it stays
	// highlighted until something replaces or clears it — so this is not the same as being live.
	dragging bool
}

// empty reports whether there is nothing to highlight or copy. A press that has not moved
// selects nothing, which is what lets a plain click keep its own meaning.
func (s selection) empty() bool {
	return s.anchor == s.head
}

// span orders the two ends. The returned end column is exclusive; the start is inclusive.
func (s selection) span() (point, point) {
	from, to := s.anchor, s.head
	if to.before(from) {
		from, to = to, from
	}
	// The cell under the pointer is part of the selection, so the exclusive end is one past it.
	// Without this a drag would always fall one character short of where it visibly reaches.
	to.col++
	return from, to
}

// cellsOf decomposes one rendered line into styled cells. Cell index is display column, so a
// double-width glyph occupies its own cell plus a zero-width continuation cell — which is
// exactly the coordinate space the mouse reports in.
//
// The buffer is sized to the line's own display width rather than the pane's: a line is only as
// long as its content, and a wider buffer would pad every row with trailing spaces that a copy
// would then have to strip back off.
func cellsOf(line string) uv.Line {
	w := lipgloss.Width(line)
	if w == 0 {
		return nil
	}
	// GraphemeWidth, not the ScreenBuffer default of WcWidth: lipgloss measures in graphemes,
	// and a disagreement here would put the cell grid out of step with the width everything
	// else in the pane was laid out to.
	buf := uv.NewScreenBuffer(w, 1)
	buf.Method = ansi.GraphemeWidth
	uv.NewStyledString(line).Draw(buf, uv.Rect(0, 0, w, 1))
	return buf.Line(0)
}

// highlightLines returns lines with the selected range painted. Rows outside the range are
// returned untouched — and as the same string, so the common case of no selection costs nothing.
func highlightLines(lines []string, from, to point, bg color.Color) []string {
	if from.line > to.line {
		return lines
	}
	out := make([]string, len(lines))
	copy(out, lines)

	for row := max(from.line, 0); row <= to.line && row < len(out); row++ {
		start, end := 0, lipgloss.Width(out[row])
		if row == from.line {
			start = from.col
		}
		if row == to.line {
			end = min(end, to.col)
		}
		out[row] = highlightRange(out[row], start, end, bg)
	}
	return out
}

// highlightRange paints a background over one line's cells in [from, to) and renders it back.
// The foreground is left alone, so highlighted code keeps its syntax colours.
func highlightRange(line string, from, to int, bg color.Color) string {
	cells := cellsOf(line)
	if len(cells) == 0 {
		return line
	}
	from, to = max(from, 0), min(to, len(cells))
	if from >= to {
		return line
	}
	for i := from; i < to; i++ {
		cells[i].Style.Bg = bg
	}
	return cells.Render()
}

// selectedText is what a copy puts on the clipboard: the plain text of the selected cells, one
// line per transcript row.
//
// It is what is on the screen, not the source it was rendered from. alux wraps prose and code
// to the pane width before styling it, so the original line breaks are already gone by the time
// a selection can be made — reconstructing them would be guesswork, and guessing wrong is worse
// than copying what the user can see.
func selectedText(lines []string, from, to point) string {
	return clippedText(lines, from, to, nil)
}

// clippedText is selectedText reading each row within clip's columns, skipping rows clip refuses.
func clippedText(lines []string, from, to point, clip func(row, width int) (lo, hi int, keep bool)) string {
	if from.line > to.line {
		return ""
	}
	var rows []string
	for row := max(from.line, 0); row <= to.line && row < len(lines); row++ {
		cells := cellsOf(lines[row])
		start, end := 0, len(cells)
		if clip != nil {
			lo, hi, keep := clip(row, len(cells))
			if !keep {
				continue
			}
			start, end = lo, hi
		}
		if row == from.line {
			start = max(start, from.col)
		}
		if row == to.line {
			end = min(end, to.col)
		}
		rows = append(rows, cellText(cells, start, end))
	}
	return strings.Join(rows, "\n")
}

// cellText reads the text out of a cell range. Zero-width cells are skipped: they are the
// continuation columns of double-width glyphs, and the glyph itself already came from the cell
// before. Trailing blanks go too, because no one wants the padding at the end of a styled row.
func cellText(cells uv.Line, from, to int) string {
	from, to = max(from, 0), min(to, len(cells))
	var b strings.Builder
	for i := from; i < to; i++ {
		if cells[i].Width == 0 {
			continue
		}
		b.WriteString(cells[i].Content)
	}
	return strings.TrimRight(b.String(), " ")
}
