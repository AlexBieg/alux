package ui

import (
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/awake"
	"github.com/alexbieg/alux/internal/session"
	"github.com/charmbracelet/x/ansi"
)

func TestFitStatusDropsTheLowestRankFirst(t *testing.T) {
	parts := []statusPart{
		{"NOW", rankNow}, {"AWAKE", rankAwake}, {"VCS", rankVCS}, {"MODE", rankMode}, {"HINT", rankHint},
	}
	w := ansi.StringWidth
	full := "NOW ─ AWAKE ─ VCS ─ MODE ─ HINT"
	cases := []struct {
		width int
		want  string
	}{
		{w(full), full},
		{w(full) - 1, "NOW ─ VCS ─ MODE ─ HINT"},
		{w("NOW ─ MODE ─ HINT"), "NOW ─ MODE ─ HINT"},
		{w("NOW ─ HINT"), "NOW ─ HINT"},
		{w("NOW"), "NOW"},
	}
	for _, c := range cases {
		if got := ansi.Strip(fitStatus(parts, " ─ ", c.width)); got != c.want {
			t.Errorf("at %d columns got %q, want %q", c.width, got, c.want)
		}
	}
}

// edgeRow is the input box's upper border as drawn on screen, the row carrying the status line.
func edgeRow(t *testing.T, m *Model) string {
	t.Helper()
	rows := strings.Split(screen(m), "\n")
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.Contains(rows[i], "╭") {
			return strings.TrimRight(rows[i], " ")
		}
	}
	t.Fatalf("no input box on screen:\n%s", screen(m))
	return ""
}

// Reported by a user: the status line ran off the screen. It now drops whole parts, so what is left
// is never cut mid-word and the turn's own state is the last thing to go.
func TestTheStatusLineDropsWholePartsToFit(t *testing.T) {
	for _, width := range []int{30, 90} {
		m, _ := harness(t, width, 16)
		m.snap.Busy, m.snap.Awake, m.snap.Mode = true, awake.Holding, session.Modes[0]
		m.layout()
		status := edgeRow(t, m)
		if w := ansi.StringWidth(status); w > width {
			t.Errorf("at %d the status line is %d columns: %q", width, w, status)
		}
		if !strings.Contains(status, "working…") {
			t.Errorf("at %d the turn's state was dropped: %q", width, status)
		}
		if fits := width == 90; strings.Contains(status, awakeMark) != fits {
			t.Errorf("at %d the awake note is shown=%v, want %v: %q", width, !fits, fits, status)
		}
	}
}
