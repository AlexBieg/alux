package ui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/charmbracelet/x/ansi"
)

// LandSource lists the pool work /land can put under the default workspace's @, and the pool's workspaces.
type LandSource interface {
	List() ([]orchestrator.Unlanded, error)
	Workspaces() (orchestrator.RepoView, error)
	Stat(change string) (string, error)
	Diff(change string) (string, error)
	// PRFlow says the repo ships work through pull requests, so nothing in the pane lands.
	PRFlow() bool
	// Push pushes the change's stack from the workspace holding it; Rebase fetches and moves a slot's stack onto trunk.
	Push(workspace, change string) (string, error)
	Rebase(workspace string) (string, error)
	// Sweep abandons work GitHub merged into trunk and says what it held back, by change id.
	Sweep() ([]string, map[string]orchestrator.Held, error)
}

var landColor = readyColor

// slotColors tell pool slots apart in the graph, steering clear of the land, pending, error and user colours.
var slotColors = []color.Color{
	lipgloss.Color("#d78700"), lipgloss.Color("#af87ff"), lipgloss.Color("#5fafff"),
	lipgloss.Color("#ff87af"), lipgloss.Color("#d7af87"), lipgloss.Color("#00afaf"),
}

// workspaceColor is the workspace's colour in the graph, the same for a slot on every reload.
func workspaceColor(name string) color.Color {
	if name == "default" {
		return userColor
	}
	var n int
	if _, err := fmt.Sscanf(name, "pool-%d", &n); err == nil && n > 0 {
		return slotColors[(n-1)%len(slotColors)]
	}
	return mutedColor
}

// tintNode colours a graph row's node glyph and change id in st; the id starts at indent bytes.
func tintNode(text string, indent int, st lipgloss.Style) string {
	prefix, rest := text[:indent], text[indent:]
	if i := strings.LastIndexAny(prefix, "@○◆×"); i >= 0 {
		_, size := utf8.DecodeRuneInString(prefix[i:])
		prefix = prefix[:i] + st.Render(prefix[i:i+size]) + prefix[i+size:]
	}
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		end = len(rest)
	}
	return prefix + st.Render(rest[:end]) + rest[end:]
}

// workspacePane is the pool's workspaces in the expanded box: a card each, or jj's graph of them all.
type workspacePane struct {
	view     orchestrator.RepoView
	landable []orchestrator.Unlanded
	loaded   bool
	err      error
	graph    bool
	cursor   int
	// picks are the cards' change rows in order; pickAt maps each pane line to its pick, -1 for none.
	picks  []wsPick
	pickAt []int
	lineOf []int
	// diff is the selected change's diff, drawn over the cards until esc goes back to them.
	diff *wsDiff
	// busy names the push or rebase running now, empty when none is.
	busy string
	// held is merged work the last cleanup kept, by change id, and why.
	held map[string]orchestrator.Held
}

type wsDiff struct {
	change     string
	stat, text string
	err        error
	loaded     bool
	// expanded is the user's clicks, open is each file's default; both keyed by the file's item id.
	expanded map[int]bool
	open     map[int]bool
	owner    []int
}

type wsPick struct {
	workspace string
	change    orchestrator.WorkspaceChange
	// card and pos place the change among the view's cards, for the row's stack context.
	card, pos int
}

type workspacesMsg struct {
	view     orchestrator.RepoView
	landable []orchestrator.Unlanded
	err      error
	// sweep asks for the cleanup of merged work once these cards are drawn.
	sweep bool
}

type wsSweepMsg struct {
	said []string
	held map[string]orchestrator.Held
	err  error
}

type wsDiffMsg struct {
	change, stat, text string
	err                error
}

const (
	wsCardsHint   = "↑↓ move · ⏎ land · s resume here · d diff · t graph · r refresh · esc close"
	wsPRCardsHint = "↑↓ move · p push · u fetch and rebase · o draft PR · s resume here · d diff · t graph · r refresh · esc close"
	wsDiffHint    = "↑↓ scroll · click a file to fold it · esc back"
	// wsFoldLines is the most diff lines a file may have and still start open.
	wsFoldLines = 80
)

func (m *Model) openWorkspaces() tea.Cmd {
	if m.wsPane == nil {
		m.closePanel()
		m.wsPane = &workspacePane{}
		m.sel = selection{}
	}
	m.pane.SetYOffset(0)
	m.layout()
	return m.loadWorkspaces()
}

// loadWorkspaces reads the cards, then has the PR flow's cleanup run behind them.
func (m *Model) loadWorkspaces() tea.Cmd { return m.readWorkspaces(true) }

func (m *Model) readWorkspaces(sweep bool) tea.Cmd {
	src := m.Land
	if src == nil {
		return nil
	}
	return func() tea.Msg {
		view, err := src.Workspaces()
		if err != nil {
			return workspacesMsg{err: err}
		}
		landable, err := src.List()
		return workspacesMsg{view: view, landable: landable, err: err, sweep: sweep && src.PRFlow()}
	}
}

func (m *Model) sweepWorkspaces() tea.Cmd {
	src := m.Land
	return func() tea.Msg {
		said, held, err := src.Sweep()
		return wsSweepMsg{said: said, held: held, err: err}
	}
}

// onWsSweep marks what the cleanup kept, says what it abandoned, and rereads the cards when it changed them.
func (m *Model) onWsSweep(msg wsSweepMsg) tea.Cmd {
	if m.wsPane == nil {
		return nil
	}
	m.wsPane.held = msg.held
	m.layout()
	say := msg.said
	if msg.err != nil {
		say = append(say, "cleaning up merged work failed: "+msg.err.Error())
	}
	var cmds []tea.Cmd
	if len(say) > 0 {
		cmds = append(cmds, m.Notify(strings.Join(say, " · ")))
	}
	if len(msg.said) > 0 {
		cmds = append(cmds, m.readWorkspaces(false))
	}
	return tea.Batch(cmds...)
}

// onWorkspaces starts the cursor on the first change that can land, and keeps it on its change when a reload reorders the cards.
func (m *Model) onWorkspaces(msg workspacesMsg) {
	p := m.wsPane
	if p == nil {
		return
	}
	keep := ""
	if pick, ok := p.selected(); ok {
		keep = pick.change.Change
	}
	p.loaded, p.err = true, msg.err
	if msg.err == nil {
		p.view, p.landable = msg.view, msg.landable
	}
	p.picks = wsPicks(p.view)
	p.cursor = min(p.cursor, max(0, len(p.picks)-1))
	for i, pick := range p.picks {
		if _, ok := p.landableAs(pick.change); keep == "" && ok {
			p.cursor = i
			break
		}
		if pick.change.Change == keep {
			p.cursor = i
		}
	}
}

func wsPicks(v orchestrator.RepoView) []wsPick {
	var out []wsPick
	for wi, w := range v.Workspaces {
		for ci, c := range w.Changes {
			out = append(out, wsPick{workspace: w.Name, change: c, card: wi, pos: ci})
		}
	}
	return out
}

// shipRow is pick i's row with what the flow needs from its card: PR, cleanup verdict and stack base, or whether it lands with the selection.
func (m *Model) shipRow(i int, with []string, byHead map[string]*prs.PR) shipRow {
	p := m.wsPane
	pick := p.picks[i]
	changes := p.view.Workspaces[pick.card].Changes
	c := pick.change
	r := shipRow{change: c, pos: pick.pos, n: len(changes), prFlow: m.Land != nil && m.Land.PRFlow(), own: m.ownWorkspace(pick.workspace)}
	if r.prFlow {
		r.pr, r.held, r.base = byHead[c.Ship.Bookmark], p.held[c.Change], stackBase(changes[pick.pos+1:], trunkName(p.view.Trunk))
	} else {
		r.going = i != p.cursor && slices.ContainsFunc(with, func(id string) bool { return sameChange(id, c.Change) })
	}
	return r
}

// graphPicks maps each graph line to the pick it draws, -1 for a connector or a commit no card holds.
func (p *workspacePane) graphPicks() []int {
	byCommit := map[string]int{}
	for i, pick := range p.picks {
		if _, ok := byCommit[pick.change.Commit]; !ok && pick.change.Commit != "" {
			byCommit[pick.change.Commit] = i
		}
	}
	out := make([]int, len(p.view.Graph))
	for li, row := range p.view.Graph {
		out[li] = -1
		if i, ok := byCommit[row.Commit]; ok && row.Commit != "" {
			out[li] = i
		}
	}
	return out
}

// graphStep moves the cursor to the next pick the graph draws in direction d, or onto the first when it is on none.
func (p *workspacePane) graphStep(d int) {
	var order []int
	for _, i := range p.graphPicks() {
		if i >= 0 {
			order = append(order, i)
		}
	}
	if len(order) == 0 {
		return
	}
	at := slices.Index(order, p.cursor)
	if at < 0 {
		p.cursor = order[0]
		return
	}
	p.cursor = order[max(0, min(len(order)-1, at+d))]
}

// graphIndent continues the graph past the node row cur in its first w columns: a rail where a line passes through cur, and under the node when the row below reaches up to it.
func graphIndent(cur, next string, w int) string {
	var b strings.Builder
	at, below := []rune(ansi.Strip(cur)), []rune(ansi.Strip(next))
	for i := range w {
		passes := i < len(at) && at[i] == '│'
		node := i < len(at) && strings.ContainsRune("@○◆×", at[i])
		up := i < len(below) && strings.ContainsRune("│├┤┼┴╯╰@○◆×~", below[i])
		if passes || node && up {
			b.WriteString(dimStyle.Render("│"))
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func (p *workspacePane) selected() (wsPick, bool) {
	if p.cursor < 0 || p.cursor >= len(p.picks) {
		return wsPick{}, false
	}
	return p.picks[p.cursor], true
}

// landableAs is the change's entry in the land list, which is what /land will take.
func (p *workspacePane) landableAs(c orchestrator.WorkspaceChange) (orchestrator.Unlanded, bool) {
	if c.Divergent {
		return orchestrator.Unlanded{}, false
	}
	for _, u := range p.landable {
		if sameChange(u.Change, c.Change) {
			return u, true
		}
	}
	return orchestrator.Unlanded{}, false
}

func (m *Model) workspaceHead() []string {
	p, width := m.wsPane, m.paneWidth()
	title := "workspaces"
	if p.view.Name != "" {
		title += " · " + p.view.Name + " · " + plural(len(p.view.Workspaces), "workspace")
		if n := len(p.landable); n > 0 {
			title += fmt.Sprintf(" · %d to land", n)
		}
		if p.busy != "" {
			title += " · " + p.busy + "…"
		}
	}
	hint := wsCardsHint
	if m.Land != nil && m.Land.PRFlow() {
		hint = wsPRCardsHint
	}
	switch {
	case p.diff != nil:
		title = "diff of " + p.diff.change
		hint = wsDiffHint
	case p.graph:
		hint = strings.Replace(hint, "t graph", "t cards", 1)
	}
	return append(styledWrap(title, lipgloss.NewStyle().Foreground(landColor).Bold(true), width), styledWrap(hint, dimStyle, width)...)
}

func (m *Model) layoutWorkspaces() {
	p, width := m.wsPane, m.paneWidth()
	if p.diff != nil {
		o := m.wsDiffLines(p.diff, width)
		p.diff.owner = o.owner
		m.pane.SetContentLines(o.lines)
		m.pane.SetXOffset(0)
		return
	}
	var lines []string
	p.pickAt, p.lineOf = nil, make([]int, len(p.picks))
	add := func(rows []string, pick int) {
		for range rows {
			p.pickAt = append(p.pickAt, pick)
		}
		lines = append(lines, rows...)
	}
	switch {
	case !p.loaded:
		add(styledWrap("reading the workspaces…", dimStyle, width), -1)
	case p.err != nil:
		add(styledWrap("workspaces: "+p.err.Error(), errorStyle, width), -1)
	case p.graph:
		with, byHead := p.landsWithSelected(), m.prsByHead(p.view.Slug)
		land := lipgloss.NewStyle().Foreground(landColor)
		for li, i := range p.graphPicks() {
			row := p.view.Graph[li]
			if i < 0 {
				add([]string{"  " + ansi.Truncate(row.Text, max(1, width-2), "…")}, -1)
				continue
			}
			r := m.shipRow(i, with, byHead)
			r.workspace = p.picks[i].workspace
			marker := "  "
			switch {
			case i == p.cursor:
				marker = land.Render("❯ ")
			case r.going:
				marker = land.Render("│ ")
			}
			p.lineOf[i] = len(lines)
			tint := lipgloss.NewStyle().Foreground(workspaceColor(r.workspace)).Bold(r.own)
			rows := []string{marker + ansi.Truncate(tintNode(row.Text, row.Indent, tint), max(1, width-2), "…")}
			w := ansi.StringWidth(row.Text[:row.Indent])
			next := ""
			if li+1 < len(p.view.Graph) {
				next = p.view.Graph[li+1].Text
			}
			indent := "  " + graphIndent(row.Text, next, w)
			for _, l := range p.rowFacts(r, i == p.cursor, max(1, width-2-w)) {
				rows = append(rows, indent+l)
			}
			add(rows, i)
		}
	default:
		with, byHead := p.landsWithSelected(), m.prsByHead(p.view.Slug)
		i := 0
		for wi, w := range p.view.Workspaces {
			if wi > 0 {
				add([]string{""}, -1)
			}
			add(m.wsCardHead(w, width), -1)
			for range w.Changes {
				p.lineOf[i] = len(lines)
				add(p.wsRow(m.shipRow(i, with, byHead), i == p.cursor, width), i)
				i++
			}
		}
	}
	m.pane.SetContentLines(lines)
	m.pane.SetXOffset(0)
}

// landsWithSelected is the other changes ⏎ would land along with the selected one.
func (p *workspacePane) landsWithSelected() []string {
	pick, ok := p.selected()
	if !ok {
		return nil
	}
	u, _ := p.landableAs(pick.change)
	return u.With
}

func sameChange(a, b string) bool { return strings.HasPrefix(a, b) || strings.HasPrefix(b, a) }

// wsDiffLines is jj show --stat's header and file list, then a foldable edit box per file, long ones folded.
func (m *Model) wsDiffLines(d *wsDiff, width int) owned {
	var o owned
	switch {
	case !d.loaded:
		o.add(styledWrap("reading the diff of "+d.change+"…", dimStyle, width), noOwner)
		return o
	case d.err != nil:
		o.add(styledWrap("diff of "+d.change+": "+d.err.Error(), errorStyle, width), noOwner)
		return o
	}
	for _, line := range strings.Split(strings.TrimRight(d.stat, "\n"), "\n") {
		if line == "" {
			o.add([]string{""}, noOwner)
			continue
		}
		o.add(styledWrap(line, plainStyle, width), noOwner)
	}
	files := diffItems(d.text)
	if len(files) == 0 {
		return o
	}
	for _, f := range files {
		d.open[f.ID] = strings.Count(f.Body, "\n")+1 <= wsFoldLines
	}
	o.add([]string{""}, noOwner)
	m.drawItems(&o, files, width)
	return o
}

// wsBoxOpen is isExpanded for a file in the workspace pane's diff: the user's click, or its default.
func (m *Model) wsBoxOpen(id int) (open, ok bool) {
	if m.wsPane == nil || m.wsPane.diff == nil {
		return false, false
	}
	d := m.wsPane.diff
	if _, mine := d.open[id]; !mine {
		return false, false
	}
	if open, clicked := d.expanded[id]; clicked {
		return open, true
	}
	return d.open[id], true
}

// wsCardHead is the workspace's name, how far behind trunk it is in the PR flow, then each session holding it, or free.
func (m *Model) wsCardHead(w orchestrator.WorkspaceView, width int) []string {
	type part struct {
		text  string
		style lipgloss.Style
	}
	name := w.Name
	parts := []part{{name, lipgloss.NewStyle().Bold(true)}}
	if w.Behind > 0 && m.wsPane.view.Trunk != "" {
		parts = append(parts, part{fmt.Sprintf("%s ↓%d", trunkName(m.wsPane.view.Trunk), w.Behind), lipgloss.NewStyle().Foreground(pendingColor)})
	}
	var holders []registry.Info
	switch name {
	case "default":
		parts = append(parts, part{"your working copy", dimStyle})
	case orchestrator.EndedSessions:
		parts = append(parts, part{"no workspace holds these", dimStyle})
	default:
		holders = m.wsHolders(name)
	}
	for _, s := range holders {
		label := s.Name
		if m.Registry != nil && s.PID == m.Registry.OwnPID() {
			label += " (this)"
		}
		parts = append(parts, part{dot + " " + label + " · " + sessionWord(s), lipgloss.NewStyle().Foreground(m.sessionTone(s))})
	}
	if len(holders) == 0 && name != "default" && name != orchestrator.EndedSessions {
		parts = append(parts, part{"free", dimStyle})
	}
	total := 0
	for _, p := range parts {
		total += ansi.StringWidth(p.text) + 2
	}
	if total-2 <= width {
		var row []string
		for _, p := range parts {
			row = append(row, p.style.Render(p.text))
		}
		return []string{strings.Join(row, "  ")}
	}
	var rows []string
	for i, p := range parts {
		indent := ""
		if i > 0 {
			indent = "  "
		}
		for _, l := range styledWrap(p.text, p.style, max(1, width-len(indent))) {
			rows = append(rows, indent+l)
		}
	}
	return rows
}

// wsHolders is every running session of this repo whose workspace is name.
func (m *Model) wsHolders(name string) []registry.Info {
	if m.Registry == nil {
		return nil
	}
	var out []registry.Info
	for _, s := range m.Registry.All(m.others, m.sessionState()) {
		if s.Workspace == name && s.Name == m.wsPane.view.Name {
			out = append(out, s)
		}
	}
	return out
}

// ownWorkspace reports whether this session holds the workspace named name.
func (m *Model) ownWorkspace(name string) bool {
	for _, s := range m.wsHolders(name) {
		if s.PID == m.Registry.OwnPID() {
			return true
		}
	}
	return false
}

// keepCursorInView scrolls the pane just enough to show the selected row whole, with the headers and connectors above it.
func (m *Model) keepCursorInView() {
	p := m.wsPane
	if p.diff != nil || p.cursor >= len(p.lineOf) {
		return
	}
	start, end := p.lineOf[p.cursor], p.lineOf[p.cursor]
	for start > 0 && start-1 < len(p.pickAt) && p.pickAt[start-1] < 0 {
		start--
	}
	for end+1 < len(p.pickAt) && p.pickAt[end+1] == p.cursor {
		end++
	}
	switch {
	case start < m.pane.YOffset():
		m.pane.SetYOffset(start)
	case end >= m.pane.YOffset()+m.pane.Height():
		m.pane.SetYOffset(end - m.pane.Height() + 1)
	}
}

func (m *Model) onWorkspaceKey(key string) tea.Cmd {
	p := m.wsPane
	if p.diff != nil {
		if key == "esc" || key == "q" {
			p.diff = nil
			m.layout()
			m.keepCursorInView()
			return nil
		}
		m.onPaneKey(key)
		return nil
	}
	if d := map[string]int{"up": -1, "down": 1}[direction(key)]; d != 0 {
		if p.graph {
			p.graphStep(d)
		} else {
			p.cursor = max(0, min(len(p.picks)-1, p.cursor+d))
		}
		m.layout()
		m.keepCursorInView()
		return nil
	}
	switch key {
	case "t":
		p.graph = !p.graph
		if p.graph && !slices.Contains(p.graphPicks(), p.cursor) {
			p.graphStep(0)
		}
		m.pane.SetYOffset(0)
		m.layout()
		m.keepCursorInView()
		return nil
	case "r":
		return m.loadWorkspaces()
	case "s":
		if m.Land == nil {
			break
		}
		return m.resumePick()
	case "p", "u", "o":
		if m.Land == nil || !m.Land.PRFlow() {
			break
		}
		return m.shipAction(key)
	case "enter":
		return m.landPick()
	case "d":
		pick, ok := p.selected()
		if !ok || m.Land == nil {
			return nil
		}
		src, change := m.Land, pick.change.Change
		p.diff = &wsDiff{change: change, expanded: map[int]bool{}, open: map[int]bool{}}
		m.pane.SetYOffset(0)
		m.layout()
		return func() tea.Msg {
			stat, err := src.Stat(change)
			if err != nil {
				return wsDiffMsg{change: change, err: err}
			}
			text, err := src.Diff(change)
			return wsDiffMsg{change: change, stat: stat, text: text, err: err}
		}
	}
	m.onPaneKey(key)
	return nil
}

// landPick lands the selected change the way /land does, or says why it cannot be landed.
func (m *Model) landPick() tea.Cmd {
	p := m.wsPane
	pick, ok := p.selected()
	if !ok || m.Land == nil || m.Land.PRFlow() {
		return nil
	}
	c := pick.change
	if u, ok := p.landableAs(c); ok {
		m.closePanel()
		m.follow = true
		m.session.Submit("/land " + u.Change)
		m.layout()
		return nil
	}
	why := fmt.Sprintf("is live work in %s; it can land once that session ends, or from inside it", pick.workspace)
	switch {
	case c.Divergent:
		why = "is divergent: it has more than one visible commit; keep one in jj before landing"
	case c.Landed:
		why = "is already under your working copy"
	case pick.workspace == "default":
		why = "is in the default workspace already"
	case c.Empty:
		why = "is empty; there is nothing to land"
	case c.Description == "":
		why = "needs a description before it can land"
	}
	return m.Notify(c.Change + " " + why)
}

// resumePick carries on from the selected change in this session's workspace the way /resume does, or says why it cannot.
func (m *Model) resumePick() tea.Cmd {
	pick, ok := m.wsPane.selected()
	if !ok {
		return nil
	}
	c := pick.change
	var why string
	for _, s := range m.wsHolders(pick.workspace) {
		why = "is live work in " + pick.workspace + "; it can be resumed once that session ends"
		if m.Registry != nil && s.PID == m.Registry.OwnPID() {
			why = "is already in this session's workspace"
			break
		}
	}
	switch {
	case why != "":
	case pick.workspace == "default" || c.Landed:
		why = "is already under your working copy, which every new session starts from"
	case c.Ship.OnTrunk:
		why = "is already on " + trunkName(m.wsPane.view.Trunk)
	case c.Empty && c.Description == "":
		why = "is empty; there is no work to resume"
	}
	if why != "" {
		return m.Notify(c.Change + " " + why)
	}
	m.closePanel()
	m.follow = true
	m.session.Submit("/resume " + c.Change)
	m.layout()
	return nil
}

func (m *Model) onWsDiff(msg wsDiffMsg) {
	if m.wsPane == nil || m.wsPane.diff == nil || m.wsPane.diff.change != msg.change {
		return
	}
	d := m.wsPane.diff
	d.stat, d.text, d.err, d.loaded = msg.stat, msg.text, msg.err, true
	m.layout()
}

func (m *Model) onWorkspaceClick(row int) tea.Cmd {
	p := m.wsPane
	line := m.pane.YOffset() + row
	if d := p.diff; d != nil {
		if line < 0 || line >= len(d.owner) || d.owner[line] == noOwner {
			return nil
		}
		id := d.owner[line]
		open, _ := m.wsBoxOpen(id)
		d.expanded[id] = !open
		offset := m.pane.YOffset()
		m.layout()
		m.pane.SetYOffset(offset)
		return nil
	}
	if line < 0 || line >= len(p.pickAt) || p.pickAt[line] < 0 {
		return nil
	}
	p.cursor = p.pickAt[line]
	m.layout()
	return nil
}
