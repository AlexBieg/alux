// Package report is a table a command answers with, carried as data so the pane can draw it at its own width.
package report

import (
	"fmt"
	"strings"
)

// Tone is how a cell reads; the pane maps it to a colour.
type Tone string

const (
	Default Tone = ""
	Muted   Tone = "muted"
	OK      Tone = "ok"
	Active  Tone = "active"
	Bad     Tone = "bad"
	Warn    Tone = "warn"
)

type Cell struct {
	Text string
	Tone Tone
}

// Row is one line of cells; Detail is a full-width line drawn under it, "" for none.
type Row struct {
	Cells      []Cell
	Detail     string
	DetailTone Tone
}

type Table struct {
	// Preamble is plain text shown before the table, already laid out.
	Preamble string
	Head     []string
	Rows     []Row
	// Footer is a muted line under the table, "" for none.
	Footer string
	// Empty is shown as plain text in place of the table when Rows is empty.
	Empty string
}

// Plain lays the table out in space-padded columns, for anything that cannot draw it.
func (t Table) Plain() string {
	var b strings.Builder
	if t.Preamble != "" {
		b.WriteString(t.Preamble + "\n\n")
	}
	if len(t.Rows) == 0 {
		b.WriteString(t.Empty)
		return b.String()
	}
	widths := make([]int, len(t.Head))
	for i, h := range t.Head {
		widths[i] = len([]rune(h))
	}
	for _, r := range t.Rows {
		for i, c := range r.Cells {
			if i < len(widths) {
				widths[i] = max(widths[i], len([]rune(c.Text)))
			}
		}
	}
	line := func(cells []string) string {
		var l strings.Builder
		for i, c := range cells {
			l.WriteString("  " + c + strings.Repeat(" ", max(0, widths[i]-len([]rune(c)))))
		}
		return strings.TrimRight(l.String(), " ")
	}
	rule := -2
	for _, w := range widths {
		rule += w + 2
	}
	fmt.Fprintln(&b, line(t.Head))
	fmt.Fprintln(&b, "  "+strings.Repeat("─", rule))
	for _, r := range t.Rows {
		cells := make([]string, len(r.Cells))
		for i, c := range r.Cells {
			cells[i] = c.Text
		}
		fmt.Fprintln(&b, line(cells))
		if r.Detail != "" {
			fmt.Fprintln(&b, "  "+strings.Repeat(" ", widths[0]+2)+r.Detail)
		}
	}
	if t.Footer != "" {
		fmt.Fprintf(&b, "\n  %s", t.Footer)
	}
	return strings.TrimRight(b.String(), "\n")
}
