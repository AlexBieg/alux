package code

import (
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// The gutter the view tool writes: a right-aligned line number, a tab, then the source.
var numbered = regexp.MustCompile(`^( *)(\d+)\t(.*)$`)

// The tab the tool writes would land on whichever terminal tab stop comes next, which puts the
// code in a different column for every gutter width. A fixed separator keeps it put, and two
// spaces keep the number from crowding the first token.
const gutterSep = "  "

var listings Blocks

// Listing draws a numbered file view for a pane of the given width: the gutter muted, the source
// after it highlighted for path's language, a wrapped line hanging under its own code.
//
// A line without a gutter is not source and is drawn as the plain text it is — a directory
// listing, an "is empty" line, the bracketed note that says where a long view was cut. Lexing
// those would colour a filename as if it were an expression.
func Listing(text, path string, width int) string {
	return listings.Get(strconv.Itoa(width)+"|"+path+"|"+text, func() string {
		var rows []string
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			rows = append(rows, listingRows(line, path, width)...)
		}
		return strings.Join(rows, "\n")
	})
}

func listingRows(line, path string, width int) []string {
	match := numbered.FindStringSubmatch(line)
	if match == nil {
		return mutedRows(line, width)
	}

	gutter := match[1] + match[2] + gutterSep
	head := lipgloss.NewStyle().Foreground(Muted).Render(gutter)
	indent := strings.Repeat(" ", len(gutter))
	codeWidth := max(1, width-len(gutter))

	var rows []string
	for i, chunk := range Rows(ExpandTabs(match[3]), codeWidth) {
		row := head
		if i > 0 {
			row = indent
		}
		rows = append(rows, row+Line(chunk, path, nil))
	}
	return rows
}

// Not code, and carrying no background, so the rows are not padded either: trailing spaces would
// be copied along with the text.
func mutedRows(line string, width int) []string {
	style := lipgloss.NewStyle().Foreground(Muted)
	var rows []string
	for _, chunk := range Rows(ExpandTabs(line), max(1, width)) {
		rows = append(rows, style.Render(chunk))
	}
	return rows
}
