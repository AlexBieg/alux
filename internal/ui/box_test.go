package ui

import (
	"image/color"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/diff"
	"github.com/charmbracelet/x/ansi"
)

func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func TestBarredRowsNeverPassTheWidth(t *testing.T) {
	const width = 30
	lines := []string{
		"",
		"short",
		strings.Repeat("x", width),
		"wide 漢字 glyphs " + strings.Repeat("漢", width),
	}
	out := barred(lines, width, toolColor[agent.KindBash])
	if len(out) != len(lines) {
		t.Fatalf("got %d rows for %d lines", len(out), len(lines))
	}
	for i, row := range out {
		if w := ansi.StringWidth(row); w > width {
			t.Errorf("row %d is %d wide, past %d: %q", i, w, width, ansi.Strip(row))
		}
	}
}

func TestBarredDrawsOnlyALeftBarInTheTone(t *testing.T) {
	tone := toolColor[agent.KindEdit]
	out := barred([]string{dimStyle.Render("dim") + " plain"}, 24, tone)
	if got := ansi.Strip(out[0]); got != "│ dim plain" {
		t.Errorf("row = %q, want only a left bar", got)
	}
	if setsBackground(out[0]) {
		t.Errorf("row sets a background: %q", out[0])
	}
	if !sameColor(cellsOf(out[0])[0].Style.Fg, tone) {
		t.Errorf("bar is not in the tone")
	}
}

func TestBarredKeepsACellsOwnBackground(t *testing.T) {
	const width = 40
	line := strings.Split(diff.Render("+added line", "x.go", barInner(width)), "\n")[0]
	before := cellsOf(line)
	if before[0].Style.Bg == nil {
		t.Fatalf("diff fixture carries no background: %q", line)
	}
	after := cellsOf(barred([]string{line}, width, toolColor[agent.KindEdit])[0])
	for i, c := range before {
		if c.Style.Bg == nil {
			continue
		}
		if !sameColor(after[barCols+i].Style.Bg, c.Style.Bg) {
			t.Errorf("cell %d %q lost its own background", i, c.Content)
		}
	}
}
