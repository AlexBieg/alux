package ui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/alexbieg/alux/internal/session"
)

func recapHarness(t *testing.T, turns int) (*Model, *int, func(time.Duration)) {
	t.Helper()
	m, _, _, advance := vcsHarness(t)
	asked := 0
	m.recap = func(context.Context) error { asked++; return nil }
	for range turns {
		m.snap.Items = append(m.snap.Items, session.Item{Kind: session.KindUser, Text: "do it"})
	}
	m.turnDone = m.now()
	return m, &asked, advance
}

func TestRefocusAfterIdleAsksForOneRecap(t *testing.T) {
	m, asked, advance := recapHarness(t, 3)

	advance(recapIdle - time.Second)
	drive(m, tea.FocusMsg{})
	if *asked != 0 {
		t.Fatal("a recap was asked for before the session had been idle long enough")
	}

	advance(time.Second)
	drive(m, tea.FocusMsg{})
	drive(m, tea.FocusMsg{})
	if *asked != 1 {
		t.Fatalf("asked %d times, want one recap per idle stretch", *asked)
	}

	m.turnDone = m.now()
	advance(recapIdle)
	drive(m, tea.FocusMsg{})
	if *asked != 2 {
		t.Fatal("a later turn followed by another idle stretch got no recap")
	}
}

func TestShortSessionGetsNoRecap(t *testing.T) {
	m, asked, advance := recapHarness(t, recapMinTurns-1)
	advance(time.Hour)
	drive(m, tea.FocusMsg{})
	if *asked != 0 {
		t.Fatal("a session under the turn minimum was recapped")
	}
}
