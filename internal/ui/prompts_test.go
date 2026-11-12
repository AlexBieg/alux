package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/session"
	"github.com/charmbracelet/x/ansi"
)

// twoTurns is two prompts, each followed by a screen and more of prose, so either can scroll away.
func twoTurns(t *testing.T) (*Model, *session.Session) {
	t.Helper()
	m, s := harness(t, 60, 16)
	s.PushUserForTest("FIRSTPROMPT")
	s.Show(paragraphs(30))
	s.PushUserForTest("SECONDPROMPT")
	s.Show(paragraphs(30))
	settle(m)
	return m, s
}

// paragraphs is n short paragraphs, n rows of prose at least.
func paragraphs(n int) string { return strings.ReplaceAll(numberedLines(n), "\n", "\n\n") }

func altKey(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code, Mod: tea.ModAlt} }

func TestAPromptIsDrawnInItsOwnBox(t *testing.T) {
	m, s := harness(t, 60, 16)
	s.PushUserForTest("hello")
	settle(m)
	row := rowOf(t, m, "hello")
	lines := strings.Split(screen(m), "\n")
	if !strings.HasPrefix(strings.TrimSpace(lines[row-1]), "╭") || !strings.HasPrefix(strings.TrimSpace(lines[row+1]), "╰") {
		t.Errorf("the prompt has no box round it:\n%s", screen(m))
	}
	if right := strings.TrimRight(lines[row], " "); !strings.HasSuffix(right, "│") {
		t.Errorf("the prompt's row has no right border: %q", right)
	}
}

func TestCopyingAPromptLeavesOutItsBox(t *testing.T) {
	m, s := harness(t, 60, 16)
	s.PushUserForTest("hello there")
	settle(m)
	p := m.content.prompts[0]
	got := m.content.selectedText(point{line: p.first}, point{line: p.last, col: 60})
	if got != "› hello there" {
		t.Errorf("copied %q, want the prompt without its border", got)
	}
}

func TestAltArrowsJumpBetweenPrompts(t *testing.T) {
	m, _ := twoTurns(t)
	first, second := m.content.prompts[0].first, m.content.prompts[1].first

	send(m, altKey(tea.KeyUp))
	if m.viewport.YOffset() != second || m.follow {
		t.Fatalf("alt+up from the tail put line %d at the top, want the last prompt at %d", m.viewport.YOffset(), second)
	}
	send(m, altKey(tea.KeyUp))
	if m.viewport.YOffset() != first {
		t.Fatalf("a second alt+up put line %d at the top, want the first prompt at %d", m.viewport.YOffset(), first)
	}
	send(m, altKey(tea.KeyDown))
	if m.viewport.YOffset() != second {
		t.Fatalf("alt+down put line %d at the top, want the next prompt at %d", m.viewport.YOffset(), second)
	}
	send(m, altKey(tea.KeyDown))
	if !m.follow || !m.viewport.AtBottom() {
		t.Errorf("alt+down past the last prompt did not go back to following the tail")
	}
}

func TestCtrlBracketsJumpBetweenPrompts(t *testing.T) {
	m, _ := twoTurns(t)
	first, second := m.content.prompts[0].first, m.content.prompts[1].first
	ctrl := func(r rune) { send(m, tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}) }

	ctrl('[')
	ctrl('[')
	if m.viewport.YOffset() != first {
		t.Fatalf("two ctrl+[ put line %d at the top, want the first prompt at %d", m.viewport.YOffset(), first)
	}
	ctrl(']')
	if m.viewport.YOffset() != second {
		t.Errorf("ctrl+] put line %d at the top, want the next prompt at %d", m.viewport.YOffset(), second)
	}
}

// The jump hint rides the scrolled pill, the only time jumping matters. ctrl+[ is esc on a terminal
// without disambiguation, so it is named only once the terminal says otherwise.
func TestTheScrolledPillNamesTheJumpKeysTheTerminalCanSend(t *testing.T) {
	m, _ := twoTurns(t)
	m = withWidth(m, 100)
	if strings.Contains(screen(m), "prompts") {
		t.Fatalf("the jump hint shows while following the tail:\n%s", screen(m))
	}
	m.scrollBy(-5)
	if s := screen(m); !strings.Contains(s, "alt+↑↓ prompts") {
		t.Fatalf("before the terminal answers the pill should name alt+arrows:\n%s", s)
	}
	send(m, tea.KeyboardEnhancementsMsg{Flags: 1})
	if s := screen(m); !strings.Contains(s, "ctrl+[ ] prompts") {
		t.Errorf("with disambiguation the pill should name ctrl+[ ]:\n%s", s)
	}
}

func withWidth(m *Model, width int) *Model {
	send(m, tea.WindowSizeMsg{Width: width, Height: m.height})
	return m
}

func TestTheAnsweredPromptPinsOnceItScrollsAway(t *testing.T) {
	m, _ := twoTurns(t)
	send(m, altKey(tea.KeyUp))
	top := m.transcriptTop()
	pinned := func() string { return strings.Split(screen(m), "\n")[top] }

	m.scrollBy(m.content.prompts[1].last - m.content.prompts[1].first)
	if strings.Contains(pinned(), "SECONDPROMPT") {
		t.Errorf("pinned while part of the prompt's box is still on screen:\n%s", screen(m))
	}
	m.scrollBy(5)
	if !strings.Contains(pinned(), "› SECONDPROMPT") {
		t.Fatalf("the prompt is not pinned once it has scrolled away:\n%s", screen(m))
	}

	click(m, top)
	if m.viewport.YOffset() != m.content.prompts[1].first {
		t.Errorf("clicking the pinned prompt put line %d at the top, want %d", m.viewport.YOffset(), m.content.prompts[1].first)
	}
}

func TestNothingIsPinnedWhileFollowing(t *testing.T) {
	m, _ := twoTurns(t)
	if row := ansi.Strip(strings.Split(m.View().Content, "\n")[m.transcriptTop()]); strings.Contains(row, "PROMPT") {
		t.Errorf("a prompt is pinned while following the tail: %q", row)
	}
}
