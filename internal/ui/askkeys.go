package ui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/ask"
)

// direction lets hjkl stand in for the arrows, but only where a modal owns the keyboard. The
// main prompt is a live text input, so a bare "j" typed there has to stay a "j" — which is why
// this is reached from the question panel and not from the transcript's own scroll keys.
func direction(key string) string {
	switch key {
	case "up", "k":
		return "up"
	case "down", "j":
		return "down"
	case "left", "h":
		return "left"
	case "right", "l":
		return "right"
	}
	return ""
}

func (m *Model) onQuestionKey(msg tea.KeyPressMsg, key string, p onScreenPrompt) tea.Cmd {
	q := p.question
	rows := len(q.Options)
	if q.AllowOther {
		rows++
	}

	if m.typingAnswer() {
		switch key {
		case "esc":
			// An open question has no choices to fall back to, so its esc is the panel's own.
			if len(q.Options) > 0 {
				m.noting = false
				m.note.SetValue("")
				return nil
			}
		case "enter":
			return m.answer(m.note.Value())
		default:
			focus := m.note.Focus()
			var cmd tea.Cmd
			m.note, cmd = m.note.Update(msg)
			return tea.Batch(focus, cmd)
		}
	}

	switch dir := direction(key); dir {
	case "up":
		m.cursor = max(0, m.cursor-1)
		m.layout()
		return nil
	case "down":
		m.cursor = min(rows-1, m.cursor+1)
		m.layout()
		return nil
	case "right":
		return m.startNote()
	}

	switch key {
	case "enter":
		// A free-text choice has nothing to submit yet, so enter opens the field rather than
		// answering with nothing.
		if m.cursor >= len(q.Options) {
			return m.startNote()
		}
		return m.answer("")
	case "esc":
		if p.worker != nil {
			return m.setAsideWorker(p.worker)
		}
		// Cancelling the turn would discard the answers already given in a series, so past the
		// first question esc only stops the asking: the turn resumes with what it has. On the
		// first question nothing has been answered, so esc is the cancel it has always been.
		if q.Step > 1 {
			m.session.AnswerID(m.snap.QuestionSeq, nil)
			m.cursor = 0
			return nil
		}
		m.session.Cancel()
		return nil
	case "pgup":
		m.viewport.ScrollUp(max(1, m.viewport.Height()-2))
		m.follow = false
		return nil
	case "pgdown":
		m.viewport.ScrollDown(max(1, m.viewport.Height()-2))
		m.follow = m.viewport.AtBottom()
		return nil
	}
	return nil
}

// typingAnswer reports whether keys on the question go to the text field: a note is open, or
// the question has no options and the field is all there is.
func (m *Model) typingAnswer() bool {
	q := m.onScreen().question
	return q != nil && (m.noting || len(q.Options) == 0)
}

func (m *Model) startNote() tea.Cmd {
	m.noting = true
	m.note.SetValue("")
	m.layout()
	return m.note.Focus()
}

func (m *Model) answer(text string) tea.Cmd {
	p := m.onScreen()
	q := p.question
	if q == nil {
		return nil
	}
	custom := m.cursor >= len(q.Options)
	// The free-text choice is the answer, so an empty one is nothing to send; a note on a
	// labelled choice is optional.
	if custom && text == "" {
		return nil
	}
	label := "Other"
	if !custom {
		label = q.Options[m.cursor].Label
	}
	a := &ask.Answer{Label: label, Note: text, Custom: custom}
	if p.worker != nil {
		return m.respondWorker(p.worker, false, a)
	}
	m.session.AnswerID(m.snap.QuestionSeq, a)

	m.cursor = 0
	m.noting = false
	m.note.SetValue("")
	return nil
}
