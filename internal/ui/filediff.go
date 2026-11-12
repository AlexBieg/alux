package ui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const fileDiffHint = "↑↓ scroll · esc back"

// fileDiffPane is one touched file's change since the session first edited it.
type fileDiffPane struct {
	path   string
	loaded bool
	patch  string
	err    error
}

type fileDiffMsg struct {
	path, patch string
	err         error
}

func (m *Model) openFileDiff(path string) tea.Cmd {
	m.closePanel()
	m.fileDiff = &fileDiffPane{path: path}
	m.sel = selection{}
	m.pane.SetYOffset(0)
	m.layout()
	s := m.session
	return func() tea.Msg {
		patch, err := s.FileDiff(path)
		return fileDiffMsg{path: path, patch: patch, err: err}
	}
}

func (m *Model) onFileDiff(msg fileDiffMsg) {
	p := m.fileDiff
	if p == nil || p.path != msg.path {
		return
	}
	p.loaded, p.patch, p.err = true, msg.patch, msg.err
	m.layout()
}

func (m *Model) fileDiffHead() []string {
	return styledWrap(m.fileDiff.path+" · since the session first edited it", lipgloss.NewStyle().Foreground(pickerColor).Bold(true), m.paneWidth())
}

func (m *Model) layoutFileDiff() {
	p, width := m.fileDiff, m.paneWidth()
	var lines []string
	switch {
	case !p.loaded:
		lines = styledWrap("reading the diff…", dimStyle, width)
	case p.err != nil:
		lines = styledWrap(p.err.Error(), errorStyle, width)
	case p.patch == "":
		lines = styledWrap("no changes since the session started", dimStyle, width)
	default:
		lines, _, _ = renderItems(p.path, reviewedItems(p.patch, "", nil, width), nil, findingDeco{}, -1, width)
	}
	m.pane.SetContentLines(lines)
}
