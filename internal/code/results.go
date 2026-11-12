package code

import (
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// rg under --no-heading --line-number prints "path:line:text". The path is matched lazily so the
// first number-and-colon wins: a match inside the text may look like a gutter of its own, and the
// leftmost one is the real one.
var result = regexp.MustCompile(`^([^\s:]+):(\d+):(.*)$`)

// A continuation row hangs two columns in, far enough to tell it from a result of its own.
const resultIndent = "  "

var results Blocks

// Results draws search output for a pane of the given width: the path and line number as
// landmarks, the matching source highlighted for whatever language that path names. The lexer is
// chosen per line rather than once for the block, because one search crosses files.
//
// Lines that are not matches keep out of the way. A bare path — what the paths-only searches and
// the file finder return — is drawn as a landmark whole, and anything else is muted: "No matches.",
// the bracketed note that says how many results were withheld, a message from the tool itself.
func Results(text string, width int) string {
	return results.Get(strconv.Itoa(width)+"|"+text, func() string {
		var rows []string
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			rows = append(rows, resultRows(line, width)...)
		}
		return strings.Join(rows, "\n")
	})
}

func resultRows(line string, width int) []string {
	match := result.FindStringSubmatch(line)
	if match == nil {
		if isBarePath(line) {
			return landmarkRows(line, width)
		}
		return mutedRows(line, width)
	}

	path, number, text := match[1], match[2], match[3]
	prefix := path + ":" + number + ":"
	head := lipgloss.NewStyle().Foreground(Landmark).Render(path) +
		lipgloss.NewStyle().Foreground(Muted).Render(":"+number+":")

	// A path long enough to leave no room for the match gets its own row, with the whole match
	// under it, rather than one column of code per row. The prefix is drawn whole rather than in
	// its two colours, because a path that has to wrap would have the split land anywhere.
	if width-len(prefix) < minCodeWidth {
		rows := landmarkRows(prefix, width)
		for _, chunk := range Rows(ExpandTabs(text), max(1, width-len(resultIndent))) {
			rows = append(rows, resultIndent+Line(chunk, path, nil))
		}
		return rows
	}

	var rows []string
	for i, chunk := range Rows(ExpandTabs(text), max(1, width-len(prefix))) {
		row := head
		if i > 0 {
			row = resultIndent
		}
		rows = append(rows, row+Line(chunk, path, nil))
	}
	return rows
}

// Below this there is not enough of a match visible on the first row to be worth reading.
const minCodeWidth = 12

// A path as the search tools print one: no spaces, and not the bracketed note or a sentence from
// the tool. Everything the two of them emit that is not a match is one or the other.
func isBarePath(line string) bool {
	return line != "" && !strings.ContainsAny(line, " \t") && !strings.HasPrefix(line, "[")
}

func landmarkRows(line string, width int) []string {
	style := lipgloss.NewStyle().Foreground(Landmark)
	var rows []string
	for _, chunk := range Rows(line, max(1, width)) {
		rows = append(rows, style.Render(chunk))
	}
	return rows
}
