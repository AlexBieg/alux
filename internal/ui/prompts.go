package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// jumpPrompt puts the previous or next prompt at the top of the transcript; past the last one it
// goes back to following the tail.
func (m *Model) jumpPrompt(dir int) {
	top := m.viewport.YOffset()
	prompts := m.content.prompts
	if dir < 0 {
		for i := len(prompts) - 1; i >= 0; i-- {
			if prompts[i].first < top {
				m.follow = false
				m.viewport.SetYOffset(prompts[i].first)
				return
			}
		}
		return
	}
	for _, p := range prompts {
		if p.first > top {
			m.viewport.SetYOffset(p.first)
			m.follow = m.viewport.AtBottom()
			return
		}
	}
	m.follow = true
	m.viewport.GotoBottom()
}

// pinnedPrompt is the prompt the top of a scrolled-back view answers, once all of it is above the view.
func (m *Model) pinnedPrompt() (promptSpan, bool) {
	if m.follow || m.panelOpen() {
		return promptSpan{}, false
	}
	top := m.viewport.YOffset()
	for i := len(m.content.prompts) - 1; i >= 0; i-- {
		if p := m.content.prompts[i]; p.first < top {
			return p, p.last < top
		}
	}
	return promptSpan{}, false
}

// pinnedRow is the header and where it sits, cut to one row since it covers the transcript's top.
func (m *Model) pinnedRow() (row string, x, y int, ok bool) {
	p, ok := m.pinnedPrompt()
	width := m.contentWidth()
	if !ok || width < 4 {
		return "", 0, 0, false
	}
	text := oneRow(inputMarker+oneLine(p.text), width-2)
	text += strings.Repeat(" ", max(0, width-2-ansi.StringWidth(text)))
	return pinnedStyle.Render(text), 1, m.transcriptTop(), true
}

func (m *Model) onPinnedRow(x, y int) bool {
	row, px, py, ok := m.pinnedRow()
	return ok && y == py && x >= px && x < px+lipgloss.Width(row)
}

// overlayPinned floats over the transcript's top row, so scrolling never resizes the layout.
func (m *Model) overlayPinned(body string) string {
	row, x, y, ok := m.pinnedRow()
	if !ok {
		return body
	}
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(body),
		lipgloss.NewLayer(row).X(x).Y(y).Z(1),
	).Render()
}

// scrollToPinned opens the pinned prompt at the top of the transcript.
func (m *Model) scrollToPinned() {
	if p, ok := m.pinnedPrompt(); ok {
		m.viewport.SetYOffset(p.first)
	}
}
