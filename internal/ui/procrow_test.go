package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/session"
	"github.com/charmbracelet/x/ansi"
)

// waitForUI polls a condition that a process exiting will satisfy, since the exit is reaped on
// its own goroutine.
func waitForUI(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// withShells gives the session a registry wired the way main wires it.
func withShells(t *testing.T, s *session.Session) {
	t.Helper()
	if s.Procs != nil {
		return
	}
	s.Procs = procs.NewRegistry()
	s.Procs.OnFinish = s.ProcFinished
	t.Cleanup(func() { s.Procs.KillAll() })
}

// startProc puts a real background command on the session, under the tool call that owns its box.
func startProc(t *testing.T, s *session.Session, call, command string) {
	t.Helper()
	withShells(t, s)
	p, err := procs.Start(context.Background(), command, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Procs.Adopt(p, call)
}

// crowdOfProcs starts n background commands, each under a tool box of its own, call_0 first.
func crowdOfProcs(t *testing.T, s *session.Session, n int) {
	t.Helper()
	for i := range n {
		call := fmt.Sprintf("call_%d", i)
		s.StartToolForTest(call, fmt.Sprintf("$ sleep 30 # p%d &", i), agent.KindBash)
		startProc(t, s, call, "sleep 30")
	}
}

func TestTheBoxGlyphsAreOneCellWide(t *testing.T) {
	for _, glyph := range []string{dot, procGlyph, landGlyph, needGlyph, skillGlyph, sessionGlyph} {
		if got := ansi.StringWidth(glyph); got != 1 {
			t.Errorf("%q is %d cells wide, which breaks the box's columns", glyph, got)
		}
	}
}

func TestARunningCommandIsADiamondRowUnderCommands(t *testing.T) {
	m, s := harness(t, 140, 30)
	startProc(t, s, "call_1", "sleep 30")
	m.sidebarOpen = true
	settle(m)
	box := plainBox(m)
	if !strings.Contains(box, "COMMANDS") || !strings.Contains(box, procGlyph+" sleep 30") {
		t.Fatalf("the box has no command row:\n%s", box)
	}
	if strings.Contains(box, dot) {
		t.Errorf("the command was drawn as an agent dot:\n%s", box)
	}
}

func TestAnExitedCommandLeavesTheBox(t *testing.T) {
	m, s := harness(t, 140, 30)
	startProc(t, s, "call_1", "sleep 30")
	m.sidebarOpen = true
	settle(m)
	if !strings.Contains(plainBox(m), procGlyph) {
		t.Fatal("the command never reached the box")
	}
	s.Procs.KillAll()
	waitForUI(t, "the row to go", func() bool {
		settle(m)
		return !strings.Contains(plainBox(m), procGlyph)
	})
}

// The transcript is made long enough that the target is off screen to begin with, or the
// assertion would pass without any scrolling.
func TestClickingACommandRowScrollsToItsBox(t *testing.T) {
	m, s := harness(t, 140, 24)
	s.StartToolForTest("call_1", "$ sleep 30 &", agent.KindBash)
	for range 60 {
		s.Show("filler")
	}
	startProc(t, s, "call_1", "sleep 30")
	m.sidebarOpen = true
	settle(m)
	if strings.Contains(ansi.Strip(m.viewport.View()), "sleep 30 &") {
		t.Fatal("the target box is already on screen, so the test proves nothing")
	}

	_, row := boxLine(t, m, sideTarget{call: "call_1", scroll: true})
	clickAt(m, m.mainWidth()+4, m.sidebarTop()+1+row)

	if !strings.Contains(ansi.Strip(m.viewport.View()), "sleep 30 &") {
		t.Errorf("clicking the row did not scroll to the command's box:\n%s", screen(m))
	}
	id, _ := m.itemFor(sideTarget{call: "call_1"})
	if m.follow || m.viewport.YOffset() != m.content.first[id] {
		t.Errorf("the pane is at %d following=%v, want the box's line %d", m.viewport.YOffset(), m.follow, m.content.first[id])
	}
}

func TestAClickInTheBoxKeepsTheSelection(t *testing.T) {
	m, s := harness(t, 140, 24)
	crowdOfProcs(t, s, 2)
	s.Show("some words to select")
	m.sidebarOpen = true
	settle(m)
	_, row := boxLine(t, m, sideTarget{call: "call_0", scroll: true})
	for _, click := range []struct {
		what string
		x, y int
	}{
		{"on a command row", m.mainWidth() + 4, m.sidebarTop() + 1 + row},
		{"on a heading", m.mainWidth() + 4, m.sidebarTop() + 1},
		{"on the border", m.mainWidth(), m.sidebarTop() + 2},
	} {
		dragSelect(m, 3, m.transcriptTop()+1, 10, m.transcriptTop()+1)
		if m.sel.empty() {
			t.Fatal("nothing was selected, so this proves nothing")
		}
		clickAt(m, click.x, click.y)
		if m.sel.empty() {
			t.Errorf("a click %s dropped the selection", click.what)
		}
	}
}
