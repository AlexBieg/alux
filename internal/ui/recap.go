package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/alexbieg/alux/internal/session"
)

const (
	recapIdle     = 3 * time.Minute
	recapMinTurns = 3
)

// recapOnReturn asks for a recap when focus comes back to a session idle since its last turn.
func (m *Model) recapOnReturn() tea.Cmd {
	if m.recap == nil || m.snap.Busy || m.turnDone.IsZero() || m.turnDone.Equal(m.recapFor) {
		return nil
	}
	if m.now().Sub(m.turnDone) < recapIdle {
		return nil
	}
	turns := 0
	for _, it := range m.snap.Items {
		if it.Kind == session.KindUser {
			turns++
		}
	}
	if turns < recapMinTurns {
		return nil
	}
	m.recapFor = m.turnDone
	recap := m.recap
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := recap(ctx); err != nil {
			return NoticeMsg{Text: "recap failed: " + err.Error()}
		}
		return nil
	}
}
