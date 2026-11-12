package ui

import (
	"fmt"
	"image/color"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

const (
	sidebarRail      = 5
	sidebarWide      = 32
	sidebarMinScreen = 90
	sideCountMax     = 99
	sessionTitleRows = 3
	sideTitle        = "running"
	tooNarrow        = "too narrow to open the running box; widen past 90 columns"
	nothingToOpen    = "nothing running to open"
	sideFocusHint    = "↑↓/jk move · enter opens · esc back"

	needGlyph    = "!"
	landGlyph    = "⇣"
	skillGlyph   = "▸"
	sessionGlyph = "◎"
	createdGlyph = "+"
	editedGlyph  = "~"
)

// sidebarWidth is the running box's columns: the rail unless it is open and the screen is wide enough.
func (m *Model) sidebarWidth() int {
	if m.sidebarOpen && m.width >= sidebarMinScreen {
		return sidebarWide
	}
	return sidebarRail
}

func (m *Model) sidebarDrawnOpen() bool {
	return m.sidebarWidth() == sidebarWide
}

// mainWidth is the left column the transcript, graph view and run view lay out to.
func (m *Model) mainWidth() int {
	return max(0, m.width-m.sidebarWidth())
}

// sidebarTop is the screen row the box starts on, directly under the top bar.
func (m *Model) sidebarTop() int {
	if bar := m.renderTopBar(); bar != "" {
		return lipgloss.Height(bar)
	}
	return 0
}

// sidebarCap is the rows between the top bar and the bottom section, which the box never passes.
func (m *Model) sidebarCap() int {
	return m.height - lipgloss.Height(m.renderBottom()) - m.sidebarTop()
}

// toggleSidebar flips what is drawn: an open box collapses, a rail opens unless the screen is too narrow.
func (m *Model) toggleSidebar() tea.Cmd {
	if m.sidebarDrawnOpen() {
		m.setSidebar(false)
		return nil
	}
	return m.openSidebar()
}

func (m *Model) openSidebar() tea.Cmd {
	if m.width < sidebarMinScreen {
		return m.Notify(tooNarrow)
	}
	m.setSidebar(true)
	return nil
}

func (m *Model) setSidebar(open bool) {
	m.sidebarOpen = open
	if !open {
		m.sideFocus.on = false
	}
	m.blocks = nil
	m.layout()
}

// sideFocus is the keyboard cursor on the running box; at tells apart two items that go to the same place.
type sideFocus struct {
	on     bool
	target sideTarget
	at     int
}

// sideStop is one item the cursor can rest on and the box rows it is drawn across.
type sideStop struct {
	target      sideTarget
	first, last int
}

// sideStops groups the box's rows into items, skipping headings, gaps and "+N more", which go nowhere.
func sideStops(targets []sideTarget) []sideStop {
	var out []sideStop
	for i, t := range targets {
		if t == (sideTarget{}) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].target == t && out[n-1].last == i-1 {
			out[n-1].last = i
			continue
		}
		out = append(out, sideStop{target: t, first: i, last: i})
	}
	return out
}

func (m *Model) sideFocused() bool {
	return m.sideFocus.on && m.sidebarDrawnOpen() && !m.panelOpen()
}

// focusedStop is the item the cursor is on: the one it was put on, or the nearest place once that has left the box.
func (m *Model) focusedStop(stops []sideStop) int {
	if len(stops) == 0 {
		return -1
	}
	f := m.sideFocus
	if f.at < len(stops) && stops[f.at].target == f.target {
		return f.at
	}
	for i, s := range stops {
		if s.target == f.target {
			return i
		}
	}
	return min(f.at, len(stops)-1)
}

// focusSidebar opens the box over whatever view is up and puts the cursor on its first item, NEEDS YOU when there is one.
func (m *Model) focusSidebar() tea.Cmd {
	if m.width < sidebarMinScreen {
		return m.Notify(tooNarrow)
	}
	if m.panelOpen() {
		m.closePanel()
	}
	if !m.sidebarOpen {
		m.setSidebar(true)
	}
	_, targets := m.sidebar()
	stops := sideStops(targets)
	if len(stops) == 0 {
		return m.Notify(nothingToOpen)
	}
	m.sideFocus = sideFocus{on: true, target: stops[0].target}
	return nil
}

// onSideKey moves the cursor, wrapping at both ends, opens its item on enter, and hands the keys back on esc.
func (m *Model) onSideKey(key string) tea.Cmd {
	_, targets := m.sidebar()
	stops := sideStops(targets)
	i := m.focusedStop(stops)
	switch key {
	case "esc", "super+e":
		m.sideFocus.on = false
	case "up", "k", "down", "j":
		if i < 0 {
			return nil
		}
		step := 1
		if key == "up" || key == "k" {
			step = -1
		}
		next := (i + step + len(stops)) % len(stops)
		m.sideFocus = sideFocus{on: true, target: stops[next].target, at: next}
	case "enter":
		if i >= 0 {
			return m.activate(stops[i].target)
		}
	case "x":
		if i >= 0 {
			return m.stopSessionKey(stops[i].target)
		}
	}
	return nil
}

// stopSessionKey arms on the first x and stops the row's session on a second x at the same row while the notice is up.
func (m *Model) stopSessionKey(t sideTarget) tea.Cmd {
	if t.stop == 0 || m.StopSession == nil {
		return nil
	}
	if m.stopArmed != t.stop || m.now().Sub(m.stopArmedAt) > noticeLife {
		m.stopArmed, m.stopArmedAt = t.stop, m.now()
		return m.Notify("x again to stop " + t.stopName)
	}
	m.stopArmed = 0
	stop, pid, name := m.StopSession, t.stop, t.stopName
	return func() tea.Msg {
		if err := stop(pid); err != nil {
			return NoticeMsg{Text: "not stopping " + name + ": " + err.Error()}
		}
		return NoticeMsg{Text: "stopped " + name}
	}
}

// sideTarget is where a click on one of the box's rows goes; the zero value goes nowhere.
type sideTarget struct {
	run        int
	call       string
	scroll     bool
	skill      bool
	workspaces bool
	file       string
	pid        int
	// stop is the session x stops from this row, named stopName in the prompt.
	stop     int
	stopName string
}

type sideRow struct {
	lines []string
	// compact is the one-line form a crowded box falls back to, empty for a row that is one line already.
	compact string
	target  sideTarget
}

type sideGroup struct {
	head string
	rows []sideRow
	// fixed is a group that is never cut.
	fixed bool
}

// sideItems is everything running, sorted into the box's groups; NEEDS YOU takes its items from the others.
type sideItems struct {
	needRuns []subagent.Run
	agents   []subagent.Run
	land     []subagent.Run
	procs    []procs.Info
	skill    *session.SkillView
	// sessions is every alux pane, this one included, while any other is running or this one has a pool to show.
	sessions     []registry.Info
	needSessions []registry.Info
	// background is every other session with no UI on it, headless workers and detached panes, not waiting on a prompt.
	background []registry.Info
	files      []session.File
}

func (m *Model) sideItems() sideItems {
	var it sideItems
	if m.Registry != nil && (len(m.others) > 0 || m.Land != nil) {
		own := m.Registry.OwnPID()
		for _, s := range m.Registry.All(m.others, m.sessionState()) {
			switch {
			case s.PID != own && s.State == registry.Waiting:
				it.needSessions = append(it.needSessions, s)
			case s.PID != own && (s.Worker != "" || s.Detached):
				it.background = append(it.background, s)
			default:
				it.sessions = append(it.sessions, s)
			}
		}
	}
	needs := false
	for _, run := range m.tileRuns() {
		// A navigator goes wherever its driver went, so the pair stays together.
		if run.Parent == 0 {
			needs = run.State == subagent.Waiting || run.State == subagent.Failed && !run.Seen
		}
		if needs {
			it.needRuns = append(it.needRuns, run)
		} else {
			it.agents = append(it.agents, run)
		}
	}
	for _, run := range m.snap.Runs {
		if len(run.Kept) > 0 {
			it.land = append(it.land, run)
		}
	}
	it.procs = m.rowProcs()
	it.skill = m.snap.Skill
	it.files = m.snap.Files
	return it
}

func (it sideItems) runs() []subagent.Run {
	out := append([]subagent.Run{}, it.needRuns...)
	out = append(out, it.agents...)
	return append(out, it.land...)
}

// sidebar is the box as drawn and, when open, the click target of each row inside its border.
func (m *Model) sidebar() (string, []sideTarget) {
	room := m.sidebarCap()
	if room < 3 {
		return "", nil
	}
	it := m.sideItems()
	edge := dimStyle
	for _, run := range it.runs() {
		if m.viewing(run) {
			edge = lipgloss.NewStyle().Foreground(m.pillColor(run))
		}
	}
	if !m.sidebarDrawnOpen() {
		return strings.Join(framed("", clip(m.railRows(it), room-2), sidebarRail, 0, edge), "\n"), nil
	}
	width := panelInner(sidebarWide)
	rows, targets := m.openRows(it, width, room-2)
	if m.sideFocused() {
		edge = lipgloss.NewStyle().Foreground(userColor)
		stops := sideStops(targets)
		if i := m.focusedStop(stops); i >= 0 {
			for r := stops[i].first; r <= stops[i].last; r++ {
				row := ansi.Truncate(rows[r], width, "")
				rows[r] = highlightRange(row+strings.Repeat(" ", max(0, width-ansi.StringWidth(row))), 0, width, selectionColor)
			}
		}
	}
	return strings.Join(framed(sideTitle, rows, sidebarWide, 1, edge), "\n"), targets
}

func clip[T any](rows []T, n int) []T {
	if len(rows) > n {
		return rows[:max(0, n)]
	}
	return rows
}

// railRows is one glyph and count per kind running, the glyph in its most urgent member's colour.
func (m *Model) railRows(it sideItems) []string {
	var rows []string
	add := func(glyph string, c color.Color, n int) {
		if n > 0 {
			rows = append(rows, lipgloss.NewStyle().Foreground(c).Render(glyph)+strconv.Itoa(min(n, sideCountMax)))
		}
	}
	add(needGlyph, m.needColor(it), drivers(it.needRuns)+len(it.needSessions))
	if len(it.agents) > 0 {
		add(dot, m.pillColor(mostUrgent(it.agents)), drivers(it.agents))
	}
	add(landGlyph, readyColor, len(it.land))
	if len(it.procs) > 0 {
		add(procGlyph, procTone(it.procs[0]), len(it.procs))
	}
	if v := it.skill; v != nil {
		rows = append(rows, lipgloss.NewStyle().Foreground(skillColor(v)).Render(skillGlyph)+strconv.Itoa(min(skillDone(v), sideCountMax)))
	}
	if others := m.otherSessions(slices.Concat(it.sessions, it.background)); len(others) > 0 {
		add(sessionGlyph, m.sessionTone(mostUrgentSession(others)), len(others))
	}
	if len(rows) == 0 {
		rows = []string{dimStyle.Render(" ·")}
	}
	return rows
}

// drivers counts runs that are not a pair's navigator, so the rail counts a pair once.
func drivers(runs []subagent.Run) int {
	n := 0
	for _, run := range runs {
		if run.Parent == 0 {
			n++
		}
	}
	return n
}

func (m *Model) needColor(it sideItems) color.Color {
	if len(it.needSessions) > 0 {
		return pulse(m.poseTick, waitingPulse)
	}
	for _, run := range it.needRuns {
		if run.State == subagent.Waiting {
			return m.pillColor(run)
		}
	}
	return errorColor
}

// mostUrgent is the run whose state most wants a look: a failure, then a wait, then work.
func mostUrgent(runs []subagent.Run) subagent.Run {
	rank := func(s subagent.State) int {
		switch s {
		case subagent.Failed:
			return 0
		case subagent.Waiting:
			return 1
		case subagent.Running:
			return 2
		default:
			return 3
		}
	}
	top := runs[0]
	for _, run := range runs[1:] {
		if rank(run.State) < rank(top.State) {
			top = run
		}
	}
	return top
}

func skillColor(v *session.SkillView) color.Color {
	for _, n := range v.Nodes {
		if n.State == skills.OutOfOrder {
			return pendingColor
		}
	}
	if v.Complete {
		return readyColor
	}
	return workingColor
}

// openRows is the open box's groups, fitted to room rows: agents go to one line first, then the largest group is cut.
func (m *Model) openRows(it sideItems, width, room int) ([]string, []sideTarget) {
	var groups []sideGroup
	add := func(head string, fixed bool, rows []sideRow) {
		if len(rows) > 0 {
			groups = append(groups, sideGroup{head: head, rows: rows, fixed: fixed})
		}
	}
	var need, agents, land, commands, skill, background, sessions, files []sideRow
	for _, s := range it.needSessions {
		if s.Worker != "" {
			need = append(need, m.workerSideRow(s, width))
		} else {
			need = append(need, m.sessionSideRow(s, width))
		}
	}
	for _, s := range it.background {
		if s.Worker != "" {
			background = append(background, m.workerSideRow(s, width))
		} else {
			background = append(background, m.sessionSideRow(s, width))
		}
	}
	for _, s := range it.sessions {
		sessions = append(sessions, m.sessionSideRow(s, width))
	}
	need = append(need, m.agentSideRows(it.needRuns, width)...)
	agents = m.agentSideRows(it.agents, width)
	for _, run := range it.land {
		glyph := lipgloss.NewStyle().Foreground(readyColor).Render(landGlyph)
		right := dimStyle.Render(plural(len(run.Kept), "file") + " " + tileAge(m.landAge(run)))
		land = append(land, sideRow{lines: []string{glyphRow(glyph, tileTitle(run), right, width)},
			target: sideTarget{run: run.ID, scroll: true}})
	}
	for _, p := range it.procs {
		glyph := lipgloss.NewStyle().Foreground(procTone(p)).Render(procGlyph)
		commands = append(commands, sideRow{lines: []string{glyphRow(glyph, oneLine(p.Command), dimStyle.Render(tileAge(p.Elapsed)), width)},
			target: sideTarget{call: p.Call, scroll: true}})
	}
	if v := it.skill; v != nil {
		glyph := lipgloss.NewStyle().Foreground(skillColor(v)).Render(skillGlyph)
		count := dimStyle.Render(strconv.Itoa(skillDone(v)) + "/" + strconv.Itoa(len(v.Nodes)))
		skill = append(skill, sideRow{lines: []string{glyphRow(glyph, v.Name, count, width)}, target: sideTarget{skill: true}})
	}
	add("NEEDS YOU", true, need)
	add("AGENTS", false, agents)
	add("TO LAND", false, land)
	add("COMMANDS", false, commands)
	add("SKILL", false, skill)
	// Newest first, so a crowded box cuts the files touched longest ago.
	paths := make([]string, len(it.files))
	for i, f := range it.files {
		paths[i] = f.Path
	}
	names := shortNames(paths)
	for i := len(it.files) - 1; i >= 0; i-- {
		files = append(files, sideRow{lines: []string{fileRow(it.files[i], names[it.files[i].Path], width)}, target: sideTarget{file: it.files[i].Path}})
	}
	add("BACKGROUND", false, background)
	add("SESSIONS", false, sessions)
	add("FILES", false, files)
	if len(groups) == 0 {
		return []string{dimStyle.Render("nothing running")}, []sideTarget{{}}
	}

	height := func() int {
		n := 2*len(groups) - 1
		for _, g := range groups {
			for _, r := range g.rows {
				n += len(r.lines)
			}
		}
		return n
	}
	if height() > room {
		for gi := range groups {
			for ri, r := range groups[gi].rows {
				if r.compact != "" {
					groups[gi].rows[ri].lines = []string{r.compact}
				}
			}
		}
	}
	for over := height() - room; over > 0; over = height() - room {
		largest := -1
		for gi, g := range groups {
			if !g.fixed && len(g.rows) > 1 && (largest < 0 || len(g.rows) > len(groups[largest].rows)) {
				largest = gi
			}
		}
		if largest < 0 {
			break
		}
		g := &groups[largest]
		keep := max(0, len(g.rows)-over-1)
		more := sideRow{lines: []string{dimStyle.Render(fmt.Sprintf("+%d more", len(g.rows)-keep))}}
		g.rows = append(g.rows[:keep:keep], more)
		g.fixed = true
	}

	var rows []string
	var targets []sideTarget
	for gi, g := range groups {
		if gi > 0 {
			rows, targets = append(rows, ""), append(targets, sideTarget{})
		}
		rows, targets = append(rows, dimStyle.Render(g.head)), append(targets, sideTarget{})
		for _, r := range g.rows {
			for _, line := range r.lines {
				rows, targets = append(rows, line), append(targets, r.target)
			}
		}
	}
	return clip(rows, room), clip(targets, room)
}

// agentSideRow is the run's dot, its kind's hat and name over his face, kind and age.
// agentSideRows is each run's row, a pair's two joined by a bracket in a gutter the group's other rows leave blank.
func (m *Model) agentSideRows(runs []subagent.Run, width int) []sideRow {
	paired := slices.ContainsFunc(runs, func(run subagent.Run) bool { return run.Parent != 0 })
	if !paired {
		var out []sideRow
		for _, run := range runs {
			out = append(out, m.agentSideRow(run, width))
		}
		return out
	}
	out := make([]sideRow, len(runs))
	for i, run := range runs {
		top, rest := " ", " "
		switch {
		case run.Parent != 0:
			top = "╰"
		case i+1 < len(runs) && runs[i+1].Parent == run.ID:
			top, rest = "╭", "│"
		}
		row := m.agentSideRow(run, width-1)
		for j, line := range row.lines {
			g := rest
			if j == 0 {
				g = top
			}
			row.lines[j] = dimStyle.Render(g) + line
		}
		row.compact = dimStyle.Render(top) + row.compact
		out[i] = row
	}
	return out
}

// navigatorWord is what a pair's navigator is doing, which its row says where a run's agent would go.
func navigatorWord(state subagent.State) string {
	switch state {
	case subagent.Idle:
		return "idle"
	case subagent.Running:
		return "consulting"
	case subagent.Failed:
		return "failed"
	default:
		return "done"
	}
}

func (m *Model) agentSideRow(run subagent.Run, width int) sideRow {
	tone, pose, paint, _ := m.runInk(run)
	name, kind, title := run.Label(), run.Agent, tileTitle(run)
	if name == "" {
		name = run.Agent
	}
	age := dimStyle.Render(tileAge(m.runElapsed(run)))
	// A navigator shares its driver's name and time, so it shows its kind and the consultations it has had.
	if run.Parent != 0 {
		name, kind, title = run.Agent, navigatorWord(run.State), run.Agent
		age = dimStyle.Render(stepCount(run))
	}
	hat, face, short := "", strings.Repeat(" ", aluxWide), ""
	if pose != nil {
		hat, face, short = pose[0], pose[1], pose[2]
	}
	left := "  " + face + " " + dimStyle.Render(kind)
	second := left + strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-ansi.StringWidth(age))) + age
	return sideRow{
		lines:   []string{statusRow(tone, hat, name, paint, width), second},
		compact: statusRow(tone, short, title, paint, width, age),
		target:  sideTarget{run: run.ID},
	}
}

// sessionSideRow is a pane's state dot and topic across the full width, its project and doing under it, this pane marked; a click switches to a
// served session, or else opens the workspace view when there is a pool.
func (m *Model) sessionSideRow(s registry.Info, width int) sideRow {
	target := sideTarget{workspaces: m.Land != nil}
	if s.PID != m.Registry.OwnPID() && m.CanAttach != nil && m.CanAttach(s.PID) {
		target = sideTarget{pid: s.PID}
	}
	if s.PID != m.Registry.OwnPID() {
		target.stop, target.stopName = s.PID, s.Name
	}
	glyph := lipgloss.NewStyle().Foreground(m.sessionTone(s)).Render(dot)
	name, ink := s.Name, plainStyle
	if s.PID == m.Registry.OwnPID() {
		ink = thisSessionStyle
	}
	place := ""
	if s.Topic != "" {
		name, place = s.Topic, dimStyle.Render(s.Name)
	}
	word := dimStyle.Render(sessionWord(s))
	head := inkedGlyphRow(glyph, name, ink, word, width)
	var under []string
	if place != "" {
		under = append(under, place)
	}
	if len(under) == 0 {
		lines := wrappedGlyphRows(glyph, name, ink, word, width, sessionTitleRows)
		if len(lines) == 1 {
			return sideRow{lines: lines, target: target}
		}
		return sideRow{lines: lines, compact: head, target: target}
	}
	lines := wrappedGlyphRows(glyph, name, ink, "", width, sessionTitleRows)
	wordW := ansi.StringWidth(word)
	left := ansi.Truncate("  "+strings.Join(under, " "), max(0, width-wordW-1), "…")
	lines = append(lines, left+strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-wordW))+word)
	compact := head
	if place != "" {
		compact = inkedGlyphRow(glyph, name, ink, place+" "+word, width)
	}
	return sideRow{lines: lines, compact: compact, target: target}
}

// workerSideRow is a headless session's dot, worker key and worker state, its stage and workspace under it.
func (m *Model) workerSideRow(s registry.Info, width int) sideRow {
	target := sideTarget{workspaces: m.Land != nil, stop: s.PID, stopName: "worker " + s.Worker}
	glyph := lipgloss.NewStyle().Foreground(m.sessionTone(s)).Render(dot)
	word := s.WorkerState
	if word == "" {
		word = sessionWord(s)
	}
	head := glyphRow(glyph, s.Worker, dimStyle.Render(word), width)
	var under []string
	if s.Stage != "" {
		under = append(under, s.Stage)
	}
	if s.Workspace != "" {
		under = append(under, s.Workspace+"@")
	}
	if len(under) == 0 {
		return sideRow{lines: []string{head}, target: target}
	}
	return sideRow{lines: []string{head, dimStyle.Render(oneRow("  "+strings.Join(under, " "), width))}, compact: head, target: target}
}

// sessionTone is a pane's dot colour: another pane waiting pulses, and one that finished unseen is green.
func (m *Model) sessionTone(s registry.Info) color.Color {
	switch {
	case s.PID != m.Registry.OwnPID() && s.State == registry.Waiting:
		return pulse(m.poseTick, waitingPulse)
	case s.PID != m.Registry.OwnPID() && s.State == registry.Idle && s.Unseen:
		return unseenColor
	case s.State == registry.Idle && s.Tasks > 0:
		return tasksColor
	default:
		return stateColor[s.State]
	}
}

func sessionWord(s registry.Info) string {
	switch {
	case s.State == registry.Idle && s.Unseen:
		return "done"
	case s.State == registry.Idle && s.Tasks > 0:
		return plural(s.Tasks, "task")
	}
	return string(s.State)
}

func (m *Model) otherSessions(all []registry.Info) []registry.Info {
	var out []registry.Info
	for _, s := range all {
		if s.PID != m.Registry.OwnPID() {
			out = append(out, s)
		}
	}
	return out
}

// mostUrgentSession is the pane that most wants a look: one finished unseen, then one working.
func mostUrgentSession(all []registry.Info) registry.Info {
	rank := func(s registry.Info) int {
		switch {
		case s.State == registry.Idle && s.Unseen:
			return 0
		case s.State == registry.Working:
			return 1
		default:
			return 2
		}
	}
	top := all[0]
	for _, s := range all[1:] {
		if rank(s) < rank(top) {
			top = s
		}
	}
	return top
}

// glyphRow is a one-cell glyph, a name cut to fit, and right flush with the edge when there is room for it.
func glyphRow(glyph, name, right string, width int) string {
	return inkedGlyphRow(glyph, name, plainStyle, right, width)
}

// inkedGlyphRow is glyphRow with the name drawn in ink once it is cut to fit.
func inkedGlyphRow(glyph, name string, ink lipgloss.Style, right string, width int) string {
	rightW := ansi.StringWidth(right)
	room := width - 3 - rightW
	if right == "" || room < 4 {
		right, rightW, room = "", 0, width-2
	}
	name = ellipsize(name, max(1, room))
	row := glyph + " " + ink.Render(name)
	if right != "" {
		row += strings.Repeat(" ", max(1, width-2-ansi.StringWidth(name)-rightW)) + right
	}
	return row
}

// wrappedGlyphRows is inkedGlyphRow with the name wrapped under itself over at most rows lines, the last cut to fit.
func wrappedGlyphRows(glyph, name string, ink lipgloss.Style, right string, width, rows int) []string {
	room := width - 3 - ansi.StringWidth(right)
	if right == "" || room < 4 {
		room = width - 2
	}
	first := wrapPlain(name, max(1, room))
	if len(first) <= 1 || rows <= 1 {
		return []string{inkedGlyphRow(glyph, name, ink, right, width)}
	}
	out := []string{inkedGlyphRow(glyph, first[0], ink, right, width)}
	rest := strings.TrimSpace(strings.TrimPrefix(name, first[0]))
	more := wrapPlain(rest, max(1, width-2))
	if len(more) > rows-1 {
		more = append(more[:rows-2:rows-2], strings.Join(more[rows-2:], " "))
	}
	for _, line := range more {
		out = append(out, "  "+ink.Render(ellipsize(line, max(1, width-2))))
	}
	return out
}

// fileRow is the file's glyph and its name cut from the left, so the file's own name survives a narrow box.
func fileRow(f session.File, name string, width int) string {
	glyph := lipgloss.NewStyle().Foreground(pendingColor).Render(editedGlyph)
	if f.Created {
		glyph = lipgloss.NewStyle().Foreground(readyColor).Render(createdGlyph)
	}
	path, room := name, width-2
	if over := ansi.StringWidth(path) - room; over > 0 {
		path = "…" + ansi.TruncateLeft(path, over+1, "")
	}
	return glyph + " " + path
}

// landAge is how long a run's change has waited to be landed.
func (m *Model) landAge(run subagent.Run) time.Duration {
	if run.Ended.IsZero() {
		return m.runElapsed(run)
	}
	return m.now().Sub(run.Ended)
}

type sideHit int

const (
	sideMiss sideHit = iota
	sideOnRail
	sideOnTitle
	sideOnRow
)

// inSidebarColumn reports whether a cell is right of the main column, between the top bar and the bottom section.
func (m *Model) inSidebarColumn(x, y int) bool {
	return x >= m.mainWidth() && y >= m.sidebarTop() && y < m.height-lipgloss.Height(m.renderBottom())
}

// sidebarAt is what of the box a cell lands on, read from the same rows the box was drawn from.
func (m *Model) sidebarAt(x, y int) (sideHit, sideTarget) {
	box, targets := m.sidebar()
	top, left := m.sidebarTop(), m.mainWidth()
	if box == "" || x < left || y < top || y >= top+lipgloss.Height(box) {
		return sideMiss, sideTarget{}
	}
	if !m.sidebarDrawnOpen() {
		return sideOnRail, sideTarget{}
	}
	if y == top {
		from := left + ansi.StringWidth(panelLead)
		if x >= from && x < from+ansi.StringWidth(panelHeadTitle(sideTitle, sidebarWide)) {
			return sideOnTitle, sideTarget{}
		}
		return sideMiss, sideTarget{}
	}
	if row := y - top - 1; row < len(targets) {
		return sideOnRow, targets[row]
	}
	return sideMiss, sideTarget{}
}

// onSidebarClick opens the rail, collapses on the title, and sends a row's click where its target says.
func (m *Model) onSidebarClick(x, y int) tea.Cmd {
	hit, t := m.sidebarAt(x, y)
	switch hit {
	case sideOnRail:
		return m.openSidebar()
	case sideOnTitle:
		m.setSidebar(false)
	case sideOnRow:
		return m.activate(t)
	}
	return nil
}

// activate opens what a row stands for, from a click or from enter on the cursor, and gives the keys back to the prompt.
func (m *Model) activate(t sideTarget) tea.Cmd {
	m.sideFocus.on = false
	switch {
	case t.pid != 0:
		m.SwitchTo, m.quit = t.pid, true
		return tea.Quit
	case t.skill:
		return m.openSkill()
	case t.workspaces:
		return m.openWorkspaces()
	case t.file != "":
		return m.openFileDiff(t.file)
	case t.run != 0 && !t.scroll:
		return m.openRun(t.run)
	case t.run != 0 || t.call != "":
		if m.panelOpen() {
			m.closePanel()
		}
		m.scrollToEntry(t)
	}
	return nil
}
