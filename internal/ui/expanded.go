package ui

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/graphview"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/charmbracelet/x/ansi"
)

const (
	// The expanded running box covers this share of the screen's width and of the rows between the top bar and the input.
	panelShareNum = 9
	panelShareDen = 10
	// skillUnbounded is the width a skill diagram is laid out to when it cannot fit the panel; the panel then cuts it.
	skillUnbounded = 1 << 12
	skillHint      = "esc closes the skill"
)

// skillPane is the open skill's diagram; view is the last run seen, so a finished run stays on screen.
type skillPane struct {
	view *session.SkillView
	// sig is what pic was drawn from and want what is being drawn, "" when nothing is.
	sig, want string
	pic       graphview.Picture
	nodes     []skills.NodeResult
	drawn     bool
}

type skillDrawnMsg struct {
	sig   string
	pic   graphview.Picture
	nodes []skills.NodeResult
	ok    bool
}

func (m *Model) panelOpen() bool {
	return m.runView != nil || m.skillPane != nil || m.wsPane != nil || m.fileGraph != nil || m.fileDiff != nil
}

// panelRect is where the expanded box sits: centred over the transcript, never over the top bar, the input
// or the open running box, though it covers the rail.
func (m *Model) panelRect() (x, y, w, h int) {
	// Counts the blank row above the input: the box keeps its own margin around itself.
	top, room, across := m.sidebarTop(), max(0, m.sidebarCap()+1), m.width
	if m.sidebarDrawnOpen() {
		across = m.mainWidth()
	}
	w, h = across*panelShareNum/panelShareDen, room*panelShareNum/panelShareDen
	return (across - w) / 2, top + (room-h)/2, w, h
}

// paneWidth is the columns the expanded box's content lays out to.
func (m *Model) paneWidth() int {
	_, _, w, _ := m.panelRect()
	return max(1, panelInner(w))
}

func (m *Model) closePanel() {
	m.runView = nil
	m.skillPane = nil
	m.wsPane = nil
	m.unfocusFindingChat()
	m.fileGraph = nil
	m.fileDiff = nil
	m.layout()
}

func (m *Model) layoutPanel() {
	_, _, w, h := m.panelRect()
	m.pane.SetWidth(max(1, panelInner(w)))
	m.pane.SetHeight(max(1, h-2-m.panelHeadRows()))
	switch {
	case m.runView != nil:
		m.layoutRun()
	case m.skillPane != nil:
		m.layoutSkill()
	case m.wsPane != nil:
		m.layoutWorkspaces()
	case m.fileGraph != nil:
		m.layoutFileGraph()
	case m.fileDiff != nil:
		m.layoutFileDiff()
	}
}

// panelHead is what the expanded box shows, wrapped to sit above its scrolling rows, and the box's frame colour.
func (m *Model) panelHead() ([]string, color.Color) {
	width := m.paneWidth()
	switch {
	case m.runView != nil:
		run, ok := m.findRun(m.runView.id)
		if !ok {
			return styledWrap("agent · no longer in the roster", dimStyle, width), mutedColor
		}
		tone := m.pillColor(run)
		return styledWrap(dot+" "+tileTitle(run)+" · "+runStatus(run), lipgloss.NewStyle().Foreground(tone).Bold(true), width), tone
	case m.wsPane != nil:
		return m.workspaceHead(), landColor
	case m.fileGraph != nil:
		return m.fileGraphHead(), pickerColor
	case m.fileDiff != nil:
		return m.fileDiffHead(), pickerColor
	case m.skillPane != nil && m.skillPane.view != nil:
		title, _ := skillProgress(m.skillPane.view)
		if m.snap.Skill == nil {
			title += " · ended"
		}
		tone := skillColor(m.skillPane.view)
		return styledWrap(title, lipgloss.NewStyle().Foreground(tone), width), tone
	}
	return styledWrap("skill", dimStyle, width), mutedColor
}

// panelHeadRows is the rows above the expanded box's scrolling ones: its head and a blank under it.
func (m *Model) panelHeadRows() int {
	head, _ := m.panelHead()
	return len(head) + 1
}

// overlayPanel draws the expanded box over the transcript.
func (m *Model) overlayPanel(body string) string {
	if !m.panelOpen() {
		return body
	}
	x, y, w, h := m.panelRect()
	if w < 8 || h < 3 {
		return body
	}
	head, tone := m.panelHead()
	rows := clip(append(append(head, ""), strings.Split(m.pane.View(), "\n")...), h-2)
	for len(rows) < h-2 {
		rows = append(rows, "")
	}
	box := strings.Join(framed(sideTitle, rows, w, 1, lipgloss.NewStyle().Foreground(tone)), "\n")
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(body),
		lipgloss.NewLayer(box).X(x).Y(y).Z(2),
	).Render()
}

// onPanelClick takes every press while the box is expanded: its title collapses it, a row goes to
// what it shows, a press on the transcript around it collapses it too, and the open running box keeps its own.
func (m *Model) onPanelClick(x, y int) tea.Cmd {
	if m.sidebarDrawnOpen() && m.inSidebarColumn(x, y) {
		return m.onSidebarClick(x, y)
	}
	px, py, w, h := m.panelRect()
	if x < px || x >= px+w || y < py || y >= py+h {
		if top := m.sidebarTop(); y >= top && y < top+m.sidebarCap() {
			m.closePanel()
		}
		return nil
	}
	if y == py {
		from := px + ansi.StringWidth(panelLead)
		if x >= from && x < from+ansi.StringWidth(panelHeadTitle(sideTitle, w)) {
			m.closePanel()
		}
		return nil
	}
	row := y - py - 1 - m.panelHeadRows()
	if row < 0 || row >= m.pane.Height() {
		return nil
	}
	switch {
	case m.runView != nil:
		return m.onRunClick(row)
	case m.wsPane != nil:
		return m.onWorkspaceClick(row)
	}
	return nil
}

// onPaneKey scrolls the expanded box, or collapses it on esc or q; it reports whether the box is still open.
func (m *Model) onPaneKey(key string) bool {
	switch key {
	case "esc", "q":
		m.closePanel()
		return false
	case "up", "k":
		m.pane.ScrollUp(1)
	case "down", "j":
		m.pane.ScrollDown(1)
	case "pgup":
		m.pane.ScrollUp(max(1, m.pane.Height()-2))
	case "pgdown", "space":
		m.pane.ScrollDown(max(1, m.pane.Height()-2))
	case "home", "g":
		m.pane.GotoTop()
	case "end", "G":
		m.pane.GotoBottom()
	}
	return true
}

func (m *Model) openSkill() tea.Cmd {
	if m.skillPane == nil {
		m.closePanel()
		m.skillPane = &skillPane{}
		m.sel = selection{}
	}
	m.pane.SetYOffset(0)
	m.layout()
	return m.loadSkill()
}

func (m *Model) onSkillKey(key string) tea.Cmd {
	if key == "enter" {
		m.closePanel()
		return nil
	}
	m.onPaneKey(key)
	return nil
}

// loadSkill lays the open skill's diagram out off the update goroutine whenever its nodes or the panel's width have moved.
func (m *Model) loadSkill() tea.Cmd {
	p := m.skillPane
	if p == nil {
		return nil
	}
	if v := m.snap.Skill; v != nil {
		p.view = v
	}
	if p.view == nil || p.want != "" {
		return nil
	}
	width := m.paneWidth()
	d := skillDiagram(p.view)
	sig := fmt.Sprint(width, d)
	if sig == p.sig {
		return nil
	}
	p.want = sig
	nodes := append([]skills.NodeResult{}, p.view.Nodes...)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pic, ok := graphview.FitPicture(ctx, d, width)
		if !ok {
			pic, ok = graphview.FitPicture(ctx, d, skillUnbounded)
		}
		return skillDrawnMsg{sig: sig, pic: pic, nodes: nodes, ok: ok}
	}
}

func (m *Model) onSkillDrawn(msg skillDrawnMsg) tea.Cmd {
	p := m.skillPane
	if p == nil || msg.sig != p.want {
		return nil
	}
	p.want, p.sig = "", msg.sig
	p.pic, p.nodes, p.drawn = msg.pic, msg.nodes, msg.ok
	m.layout()
	return m.loadSkill()
}

// skillDiagram is the run's nodes, each boxed with its name, any dispatched skill and its state, joined by order edges.
func skillDiagram(v *session.SkillView) graphview.Diagram {
	d := graphview.Diagram{Flow: graphview.LR}
	index := map[string]int{}
	for i, n := range v.Nodes {
		index[n.Name] = i
		state := string(n.State)
		if n.Optional {
			state += " · optional"
		}
		if n.Waived > 0 && n.State != skills.Waived {
			state += fmt.Sprintf(" · %d waived", n.Waived)
		}
		lines := []string{n.Name, state}
		if note := nodeNote(n); note != "" {
			lines = append(lines, note)
		}
		d.Nodes = append(d.Nodes, graphview.Node{Lines: lines})
	}
	for _, e := range v.Edges {
		from, okFrom := index[e.From]
		to, okTo := index[e.To]
		if okFrom && okTo {
			d.Edges = append(d.Edges, graphview.Edge{From: from, To: to, Label: e.Label, Dashed: e.Dashed})
		}
	}
	return d
}

// nodeNote is a stage's progress, failures and stop in a few words, or "" when it has none.
func nodeNote(n skills.NodeResult) string {
	var parts []string
	if n.Items > 0 {
		parts = append(parts, fmt.Sprintf("item %d/%d", n.Item, n.Items))
	}
	if n.Rounds > 0 {
		parts = append(parts, fmt.Sprintf("round %d/%d", n.Round, n.Rounds))
	}
	if n.Fails > 0 {
		parts = append(parts, fmt.Sprintf("failed %d×", n.Fails))
	}
	if n.Stopped != "" {
		parts = append(parts, "stopped")
	}
	return strings.Join(parts, " · ")
}

// skillNotes is the run's stop reasons and waivers, each under its heading, with a blank row above each heading.
func skillNotes(v *session.SkillView, width int) []string {
	var lines []string
	heading := lipgloss.NewStyle().Bold(true)
	entry := func(name, text string) {
		lines = append(lines, styledWrap(name, lipgloss.NewStyle().Foreground(pendingColor), width)...)
		lines = append(lines, hang("  ", 2, text, lipgloss.NewStyle(), width)...)
	}
	for _, n := range v.Nodes {
		if n.Stopped != "" {
			lines = append(lines, "", heading.Render("Stopped"))
			entry(n.Name, n.Stopped)
		}
	}
	if len(v.Waivers) > 0 {
		lines = append(lines, "", heading.Render("Waived"))
	}
	for _, w := range v.Waivers {
		entry(w.Step, w.Reason)
	}
	return lines
}

func (m *Model) layoutSkill() {
	p := m.skillPane
	if v := m.snap.Skill; v != nil {
		p.view = v
	}
	width := m.paneWidth()
	var lines []string
	switch {
	case p.view == nil:
		lines = styledWrap("No skill is running.", dimStyle, width)
	case p.sig == "":
		lines = styledWrap("laying out the diagram…", dimStyle, width)
	case p.drawn:
		styles := make([]lipgloss.Style, len(p.nodes))
		for i, n := range p.nodes {
			styles[i] = nodeStyle(n)
		}
		lines = pictureRows(p.pic, styles, lipgloss.NewStyle().Foreground(mutedColor))
	default:
		_, styles := skillProgress(p.view)
		lines = append(styledWrap("the diagram could not be laid out; the steps in order:", dimStyle, width), "")
		lines = append(lines, chainRows(p.view, styles, width)...)
	}
	if p.view != nil && p.sig != "" {
		lines = append(lines, skillNotes(p.view, width)...)
	}
	m.pane.SetContentLines(lines)
}
