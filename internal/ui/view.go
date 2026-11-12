package ui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/awake"
	"github.com/alexbieg/alux/internal/code"
	"github.com/alexbieg/alux/internal/diff"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) contentWidth() int {
	w := m.mainWidth() - 2 // the transcript's own padding
	if w < minWidth {
		return minWidth
	}
	return w
}

// transcriptTop is the screen row the transcript starts on, directly under the top bar when there is one.
func (m *Model) transcriptTop() int {
	return m.sidebarTop()
}

const (
	inputMarker = "› "

	dot = "\u25cf"
	// A background command: a diamond against the agents' circles, so the two read apart at a glance.
	procGlyph = "\u25c6"
)

var stateColor = map[registry.State]color.Color{
	registry.Idle:    mutedColor,
	registry.Working: lipgloss.Cyan,
	registry.Waiting: pendingColor,
}

// unseenColor is a session that finished while nobody was looking at its pane.
var unseenColor = lipgloss.Green

// tasksColor is an idle session with subagents or background commands still running.
var tasksColor = lipgloss.Blue

// The top row carries the sensitive marker on the left and the PR summary in the centre. Empty
// when there is neither, and then it costs no row.
func (m *Model) renderTopBar() string {
	row := m.topRow()
	top := ""
	if row.left != "" {
		top = " " + row.left
	}
	if row.center != "" {
		top += strings.Repeat(" ", max(0, row.centerAt-ansi.StringWidth(top))) + row.center
	}
	return top
}

// topBarRow is the first row's parts; centerAt is the column the PR summary starts in.
type topBarRow struct {
	left, center string
	centerAt     int
}

// The gap the summary keeps from the marker, so it never reads as part of it.
const topBarGap = 3

const sensitiveMarker = "sensitive read · web off · local"

// The PR summary is centred, slid clear of the marker, or dropped.
func (m *Model) topRow() topBarRow {
	var row topBarRow
	if session.Effective(m.snap.Mode) != m.snap.Mode {
		row.left = lipgloss.NewStyle().Foreground(pendingColor).Render(oneRow(sensitiveMarker, max(1, m.width-2)))
	}
	summary := m.renderPRs()
	if summary == "" {
		return row
	}
	w := ansi.StringWidth(summary)
	lo, hi := 0, m.width-1-w
	if row.left != "" {
		lo = 1 + ansi.StringWidth(row.left) + topBarGap
	}
	if lo > hi {
		return row
	}
	row.center, row.centerAt = summary, min(max((m.width-w)/2, lo), hi)
	return row
}

// onPRSummary reports whether a screen cell is on the top row's PR summary.
func (m *Model) onPRSummary(x, y int) bool {
	row := m.topRow()
	if y != 0 || row.center == "" {
		return false
	}
	return x >= row.centerAt && x < row.centerAt+ansi.StringWidth(row.center)
}

// procTone is a process dot's colour, matching the box it points at.
func procTone(p procs.Info) color.Color {
	if p.Poll {
		return pollColor
	}
	return procColor
}

// rowProcs is the processes the running box lists: a foreground bash call is a running tool, not a process.
func (m *Model) rowProcs() []procs.Info {
	var out []procs.Info
	for _, p := range m.snap.Procs {
		if !p.Foreground {
			out = append(out, p)
		}
	}
	return out
}

// scrollToEntry puts the box a row stands for at the top of the transcript. Following is given
// up, or the next redraw would snap straight back to the bottom and undo the jump.
func (m *Model) scrollToEntry(e sideTarget) {
	id, ok := m.itemFor(e)
	if !ok {
		return
	}
	line, ok := m.content.first[id]
	if !ok {
		return
	}
	m.follow = false
	m.viewport.SetYOffset(line)
}

// itemFor is the transcript item a row belongs to: a subagent box carries the run, and a
// process belongs to the box of the call that started it.
func (m *Model) itemFor(e sideTarget) (int, bool) {
	for _, item := range m.snap.Items {
		switch {
		case e.run != 0 && item.Run == e.run:
			return item.ID, true
		case e.call != "" && item.Call == e.call:
			return item.ID, true
		}
	}
	return 0, false
}

// runColor is a run's state colour.
func runColor(state subagent.State) color.Color {
	switch state {
	case subagent.Failed:
		return errorColor
	case subagent.Waiting:
		return pendingColor
	case subagent.Idle:
		// Present but not working, and the colour is the only thing saying so: a navigator
		// between consultations is costing nothing, and showing it as live as its driver would
		// make a pair look like twice the work rather than one and a half.
		return mutedColor
	default:
		return lipgloss.Cyan
	}
}

// layout re-renders the conversation and gives the viewport whatever rows the bottom section
// does not need. Called on resize, on every session change, and after any box is toggled.
func (m *Model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	bottom := m.renderBottom()
	height := m.height - lipgloss.Height(bottom) - m.transcriptTop()
	if height < 1 {
		height = 1
	}

	// The queue list follows the bottom section's blank opening row.
	m.queueTop = -1
	if steering, queued, _ := m.shownQueue(); len(steering)+len(queued) > 0 {
		m.queueTop = m.height - lipgloss.Height(bottom) + 1
	}

	m.viewport.SetWidth(m.mainWidth())
	m.viewport.SetHeight(height)

	m.content = m.buildTranscript()
	if m.panelOpen() {
		m.layoutPanel()
	}

	// The highlight is painted on the way to the viewport, never into m.content: the selection
	// is drawn over the transcript, and the transcript is what a copy reads back. Keeping the
	// two apart means a copy can never pick up its own highlight.
	lines := m.content.lines
	if !m.sel.empty() {
		from, to := m.sel.span()
		lines = highlightLines(lines, from, to, selectionColor)
	}
	m.viewport.SetContentLines(indent(lines))
	if m.follow {
		m.viewport.GotoBottom()
	}
}

func indent(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		if line == "" {
			out[i] = ""
			continue
		}
		out[i] = " " + line
	}
	return out
}

func (m *Model) View() tea.View {
	if !m.reuseFrame || m.frame.Content == "" {
		m.frame = m.render()
	}
	return m.frame
}

func (m *Model) render() tea.View {
	view := tea.NewView("")
	view.AltScreen = true
	// One mouse mode, always on. Cell motion rather than all motion so the terminal is only
	// asked for presses, releases, the wheel and drags-with-a-button — and because all-motion is
	// the less widely supported of the two.
	view.MouseMode = tea.MouseModeCellMotion
	view.ReportFocus = true

	if m.width == 0 || m.height == 0 {
		return view
	}
	if m.quit {
		return view
	}

	sections := []string{}
	if bar := m.renderTopBar(); bar != "" {
		sections = append(sections, bar)
	}
	sections = append(sections, m.renderMiddle(), m.renderBottom())
	body := lipgloss.JoinVertical(lipgloss.Left, sections...)
	view.SetContent(m.overlayNotices(m.overlayPanel(m.overlayScrolled(m.overlayAlux(m.overlayPinned(body))))))

	// The cursor belongs to whichever text input is live, and nowhere when neither is.
	if !m.onScreen().waiting() && !m.sideFocused() && m.runView == nil && m.skillPane == nil && (m.fileGraph == nil || !m.fileGraph.chatting) {
		view.Cursor = m.inputCursor()
	}
	return view
}

// renderMiddle is the main column padded to mainWidth beside the running box, whose rail is blank while the box is expanded.
func (m *Model) renderMiddle() string {
	main := strings.Split(m.viewport.View(), "\n")
	w := m.mainWidth()
	for i, line := range main {
		line = ansi.Truncate(line, w, "")
		main[i] = line + strings.Repeat(" ", max(0, w-ansi.StringWidth(line)))
	}
	var side []string
	if box, _ := m.sidebar(); box != "" && (m.sidebarDrawnOpen() || !m.panelOpen()) {
		side = strings.Split(box, "\n")
	}
	for len(side) < len(main) {
		side = append(side, strings.Repeat(" ", m.sidebarWidth()))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(main, "\n"), strings.Join(side, "\n"))
}

// linesBelow is how much of the transcript sits under the viewport, zero when it shows the tail.
func (m *Model) linesBelow() int {
	return max(0, m.viewport.TotalLineCount()-m.viewport.YOffset()-m.viewport.Height())
}

// scrolledPill is the indicator and where it sits; the draw and the click hit test both read it.
func (m *Model) scrolledPill() (pill string, x, y int, ok bool) {
	below := m.linesBelow()
	if m.follow || below == 0 || m.panelOpen() {
		return "", 0, 0, false
	}
	text := fmt.Sprintf("↓ %s below · click or end to follow", plural(below, "line"))
	pill = scrolledStyle.Render(text + " · " + m.jumpKeys() + " prompts")
	if lipgloss.Width(pill) > m.mainWidth() {
		pill = scrolledStyle.Render(text)
	}
	if lipgloss.Width(pill) > m.mainWidth() {
		return "", 0, 0, false
	}
	return pill, (m.mainWidth() - lipgloss.Width(pill)) / 2, m.transcriptTop() + m.viewport.Height(), true
}

func (m *Model) onScrolledPill(x, y int) bool {
	pill, px, py, ok := m.scrolledPill()
	return ok && y == py && x >= px && x < px+lipgloss.Width(pill)
}

// overlayScrolled floats over the blank row above the input, so scrolling never resizes the layout.
func (m *Model) overlayScrolled(body string) string {
	pill, x, y, ok := m.scrolledPill()
	if !ok {
		return body
	}
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(body),
		lipgloss.NewLayer(pill).X(x).Y(y).Z(1),
	).Render()
}

// The completion list sits above the frame so that typing a command grows the bottom section
// upwards into the transcript and leaves the input box — and the cursor in it — where they are.
// The queue sits above that, furthest from the box, because it is the part that is waiting.
func (m *Model) renderBottom() string {
	parts := []string{""}
	if queue := m.renderQueue(); queue != "" {
		parts = append(parts, queue)
	}
	if list := m.renderCommands(); list != "" {
		parts = append(parts, list)
	}
	parts = append(parts, m.renderFrame())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// What is lined up: messages already sent into the running turn, then those still waiting.
func (m *Model) renderQueue() string {
	lines, _ := m.queueRows()
	return strings.Join(lines, "\n")
}

// queueSpan is the rows one message was drawn across; the list and the click hit test both read it.
type queueSpan struct {
	first, last int
	id          int
	clickable   bool
}

// queueEntryRows caps one message, so a long paste waiting in the queue cannot crowd out the input.
const queueEntryRows = 3

func (m *Model) queueRows() ([]string, []queueSpan) {
	steering, queued, hidden := m.shownQueue()
	if len(steering) == 0 && len(queued) == 0 {
		return nil, nil
	}
	width := m.width - 2
	var lines []string
	var spans []queueSpan
	add := func(head string, text string, st lipgloss.Style, span queueSpan) {
		rows := clipRows(hang(dimStyle.Render(head), ansi.StringWidth(head), text, st, width), queueEntryRows, "   ")
		span.first, span.last = len(lines), len(lines)+len(rows)-1
		for _, row := range rows {
			lines = append(lines, " "+row)
		}
		spans = append(spans, span)
	}
	for _, q := range steering {
		add("→ ", oneLine(q.Text)+" · sent", dimStyle, queueSpan{id: q.ID})
	}
	for i, q := range queued {
		// A command is dim because it is not clickable, so the two kinds of row cannot be
		// confused for each other.
		st := queuedStyle
		if q.Command {
			st = dimStyle
		}
		add(fmt.Sprintf("%d. ", i+1), oneLine(q.Text), st, queueSpan{id: q.ID, clickable: !q.Command})
	}
	if hidden > 0 {
		lines = append(lines, " "+dimStyle.Render(fmt.Sprintf("  … %d more", hidden)))
	}
	return lines, spans
}

// shownQueue is the part of the queue that is drawn, and so the part a click can land on. The
// list and the hit test both read it, which is what keeps them in agreement.
func (m *Model) shownQueue() (steering, queued []session.Queued, hidden int) {
	steering, cut := clipQueue(m.snap.Steering, queueShown)
	queued, more := clipQueue(m.snap.Queued, queueShown-len(steering))
	return steering, queued, cut + more
}

// steerableCount is how many drawn rows a click could actually do something with.
func (m *Model) steerableCount() int {
	_, queued, _ := m.shownQueue()
	n := 0
	for _, q := range queued {
		if !q.Command {
			n++
		}
	}
	return n
}

// clipQueue keeps the first n, which for the waiting list is the ones running soonest.
func clipQueue(q []session.Queued, n int) ([]session.Queued, int) {
	if n < 0 {
		n = 0
	}
	if len(q) <= n {
		return q, 0
	}
	return q[:n], len(q) - n
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// The completion list only appears while a command is being typed, so it costs no rows the rest
// of the time. The first row is bold because it is what tab inserts.
func (m *Model) renderCommands() string {
	_, matches := m.completions()
	if len(matches) == 0 {
		return ""
	}
	shown, more := matches, 0
	if len(shown) > completionRows {
		shown, more = shown[:completionRows], len(shown)-completionRows
	}
	var lines []string
	for i, s := range shown {
		label := lipgloss.NewStyle().Foreground(userColor).Bold(i == 0).Render(s.Label)
		line := label + dimStyle.Render(" "+s.Description)
		lines = append(lines, "  "+oneRow(line, m.mainWidth()-2))
	}
	if more > 0 {
		lines = append(lines, "  "+dimStyle.Render(fmt.Sprintf("… %d more", more)))
	}
	return strings.Join(lines, "\n")
}

const completionRows = 8

// renderFrame is the input box, with the status line and context gauge set into its upper border.
func (m *Model) renderFrame() string {
	// The transcript's columns: one in from either edge. Width counts the border, the margin does not.
	width := m.width - 2
	inner := width - frameStyle.GetHorizontalFrameSize()
	tone, body := m.frameBody(inner)
	box := frameStyle.BorderTop(false).BorderForeground(tone).Width(width).MarginLeft(1).Render(body)

	parts := m.statusParts()
	room := width - edgeCols
	gauge := m.renderContext()
	// The gauge yields before the status's most important part would.
	if gauge != "" && room-gaugeCols-lipgloss.Width(gauge) < keptWidth(parts) {
		gauge = ""
	}
	if gauge != "" {
		room -= gaugeCols + lipgloss.Width(gauge)
	}
	sep := lipgloss.NewStyle().Foreground(tone).Render(" ─ ")
	return " " + statusEdge(fitStatus(parts, sep, room), gauge, m.snap.Topic, tone, width) + "\n" + box
}

// frameBody is what the input box holds right now, and the border colour that says so.
func (m *Model) frameBody(inner int) (color.Color, string) {
	switch p := m.onScreen(); {
	case p.question != nil:
		return askColor, m.renderAsk(inner, p)
	case p.pending != "":
		return pendingColor, m.renderApproval(inner, p)
	case m.picker != nil:
		return pickerColor, m.renderPicker(inner)
	}
	// Drawn while a turn runs too; only a question and an approval take the box away, both
	// asking for something it cannot express.
	//
	// Grows with what is typed or pasted instead of hiding it, up to a share of the pane.
	m.input.MaxHeight = max(1, m.height/3)
	m.input.SetWidth(inner)
	var tone color.Color = mutedColor
	if m.snap.Busy {
		tone = workingColor
	} else if flash, ok := m.turnFlash(); ok {
		tone = flash
	}
	return tone, m.input.View()
}

const (
	// The upper border's "╭─ " before the status and " ─╮" after it, keeping at least one dash.
	edgeCols = 6
	// What a gauge adds to the border around itself: a space before it and " ─" before the corner.
	gaugeCols = 3
	// The widest a session's topic is drawn before it is cut.
	topicCols = 30
)

// statusEdge draws the input box's upper border width wide: status after the left corner, gauge before the right.
// The topic is centred in the fill between them, and is the first thing dropped when the fill is short.
func statusEdge(status, gauge, topic string, tone color.Color, width int) string {
	edge := lipgloss.NewStyle().Foreground(tone)
	head, tail := "╭", "╮"
	if status != "" {
		head, status = "╭─ ", status+" "
	}
	if gauge != "" {
		gauge, tail = " "+gauge, " ─╮"
	}
	fill := width - lipgloss.Width(head+status+gauge+tail)
	if fill < 1 {
		return edge.Render("╭" + strings.Repeat("─", max(0, width-2)) + "╮")
	}
	rule := edge.Render(strings.Repeat("─", fill))
	if topic = oneLine(topic); topic != "" {
		topic = " " + ansi.Truncate(topic, topicCols, "…") + " "
		if w := lipgloss.Width(topic); fill >= w+2 {
			left := (fill - w) / 2
			rule = edge.Render(strings.Repeat("─", left)) + dimStyle.Render(topic) + edge.Render(strings.Repeat("─", fill-w-left))
		}
	}
	return edge.Render(head) + status + rule + gauge + edge.Render(tail)
}

// The change goes above the prompt: an edit named only by its path cannot be judged.
func (m *Model) renderApproval(width int, p onScreenPrompt) string {
	hint, tag := "allow? (y/n · esc to stop)", ""
	if p.worker != nil {
		hint, tag = "allow? (y/n · esc to set it aside)", workerTag(p.worker, pendingColor, width)
	}
	prompt := strings.Join(styledWrap(hint, dimStyle, width), "\n")
	budget := m.blockRows()
	labelBudget := budget
	if p.body != "" {
		labelBudget = max(1, budget/2)
	}
	labelRows := approvalLabel(p.pending, width, labelBudget)
	label := strings.Join(labelRows, "\n")
	if p.summary != "" {
		summary := clipRows(hang("※ ", 2, p.summary, lipgloss.NewStyle(), width), 3, "  ")
		prompt = strings.Join(summary, "\n") + "\n" + prompt
	}
	if p.body == "" {
		return tag + label + "\n" + prompt
	}
	rows := max(1, budget-len(labelRows))
	body := m.renderVerbatim(p.body, width, rows)
	if path := editedPath(p.pending); path != "" {
		body = m.renderBlock(p.body, path, width, rows)
	}
	return tag + body + "\n" + label + "\n" + prompt
}

// workerTag is the row naming the headless worker a prompt came from, kept apart from the label so a command still highlights.
func workerTag(t *workerPrompt, tone color.Color, width int) string {
	return strings.Join(styledWrap("["+t.key+"]", lipgloss.NewStyle().Foreground(tone).Bold(true), width), "\n") + "\n"
}

// editedPath is the file an approval's edit changes, through a subagent's "agent <name> — " prefix,
// and empty when the approval is not an edit and its body is therefore not a diff.
func editedPath(label string) string {
	if rest, ok := strings.CutPrefix(label, "agent "); ok {
		if _, inner, ok := strings.Cut(rest, " — "); ok {
			label = inner
		}
	}
	return tools.EditPath(label)
}

// approvalLabel draws a shell command syntax highlighted after its "$ ", and any other label plain.
func approvalLabel(label string, width, rows int) []string {
	command, ok := strings.CutPrefix(label, "$ ")
	if !ok {
		return wrapRows(label, lipgloss.NewStyle().Foreground(pendingColor), width, rows)
	}
	prefix := lipgloss.NewStyle().Foreground(pendingColor).Render("$ ")
	var out []string
	for i, line := range strings.Split(code.ExpandTabs(command), "\n") {
		lead := "  "
		if i == 0 {
			lead = prefix
		}
		out = append(out, code.Rows(lead+code.Line(line, "command.sh", nil), width)...)
	}
	return clipRows(out, rows, "")
}

func (m *Model) renderAsk(width int, p onScreenPrompt) string {
	q := *p.question
	var b strings.Builder
	if p.worker != nil {
		b.WriteString(workerTag(p.worker, askColor, width))
	}

	// Where the series has got to, and what was already decided in it. Nothing from a series
	// reaches the transcript until the last answer, so without this line the earlier answers are
	// nowhere on screen — and a flow that does not say how much is left feels unbounded.
	askStyle := lipgloss.NewStyle().Foreground(askColor)
	if meta := askMeta(q); meta != "" {
		b.WriteString(strings.Join(styledWrap(meta, dimStyle, width), "\n") + "\n")
	}
	b.WriteString(strings.Join(styledWrap(q.Question, askStyle, width), "\n"))
	if q.Context != "" {
		b.WriteString("\n" + m.renderBlock(q.Context, "", width, m.blockRows()))
	}

	open := len(q.Options) == 0
	total := len(q.Options)
	if q.AllowOther && !open {
		total++
	}
	if open {
		m.note.SetWidth(width - 6)
		b.WriteString("\n  " + askStyle.Render("› ") + m.note.View())
	}
	for i := 0; i < total; i++ {
		custom := i >= len(q.Options)
		label, detail := "Other…", ""
		if !custom {
			label, detail = q.Options[i].Label, q.Options[i].Detail
		}

		marker := "  "
		if i == m.cursor {
			marker = "❯ "
		}
		labelStyle := lipgloss.NewStyle()
		if i == m.cursor {
			labelStyle = labelStyle.Reverse(true)
		} else if custom {
			labelStyle = labelStyle.Foreground(mutedColor)
		}
		rows := hang(askStyle.Render(marker), 2, label, labelStyle, width)

		// Only the highlighted choice's detail is shown whole; moving onto one is how it is read.
		last := len(rows) - 1
		if room := width - lipgloss.Width(rows[last]) - 3; detail != "" && i != m.cursor && room >= 4 {
			rows[last] += dimStyle.Render(" — " + ansi.Truncate(detail, room, "…"))
		}
		b.WriteString("\n" + strings.Join(rows, "\n"))
		if detail != "" && i == m.cursor {
			b.WriteString("\n" + strings.Join(hang("  ", 2, detail, lipgloss.NewStyle(), width), "\n"))
		}
		if i == m.cursor && m.noting {
			m.note.SetWidth(width - 6)
			prefix := "+ "
			if custom {
				prefix = "› "
			}
			b.WriteString("\n  " + askStyle.Render(prefix) + m.note.View())
		}
	}

	hint := "↑↓/jk move · →/l add a note · enter to answer · esc cancels the turn"
	if q.Step > 1 {
		// Past the first question, esc is no longer a cancel: the answers already given survive it.
		hint = "↑↓/jk move · →/l note · enter to answer · esc keeps your answers"
	}
	if p.worker != nil {
		hint = "↑↓/jk move · →/l add a note · enter to answer · esc sets it aside"
	}
	if m.noting {
		hint = "enter to answer · esc to drop the note"
	}
	if open {
		hint = "type your answer · enter to answer · esc cancels the turn"
		if q.Step > 1 {
			hint = "type your answer · enter to answer · esc keeps your answers"
		}
		if p.worker != nil {
			hint = "type your answer · enter to answer · esc sets it aside"
		}
	}
	b.WriteString("\n" + strings.Join(styledWrap(hint, dimStyle, width), "\n"))
	return b.String()
}

// askMeta is the one dim line above a series' question: how far through it we are, and the
// answers already given. Empty for a lone question, which needs neither.
func askMeta(q ask.Question) string {
	if q.Total <= 1 {
		return ""
	}
	of := fmt.Sprintf("%d of %d", q.Step, q.Total)
	if q.Approx {
		of = fmt.Sprintf("%d of up to %d", q.Step, q.Total)
	}
	parts := []string{of}
	for _, prior := range q.Prior {
		parts = append(parts, "✓ "+prior)
	}
	return strings.Join(parts, " · ")
}

// A supporting block — a question's context, an edit's diff — shown verbatim rather than through
// the markdown renderer, so ascii diagrams keep their alignment. path names the file a diff is
// against, and is empty when the block is not one.
//
// Capped at rows so the prompt sharing the frame is never pushed off the screen.
func (m *Model) renderBlock(text, path string, width, rows int) string {
	block := diff.RenderText(text, width-2)
	if path != "" {
		block = diff.Render(text, path, width-2)
	}
	all := strings.Split(block, "\n")
	for i, line := range all {
		all[i] = "  " + line
	}
	return strings.Join(clipRows(all, rows, "  "), "\n")
}

// renderVerbatim is renderBlock for text that is not a diff, so a "- " list item stays untinted.
func (m *Model) renderVerbatim(text string, width, rows int) string {
	var all []string
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		for _, chunk := range code.Rows(code.ExpandTabs(line), max(1, width-2)) {
			all = append(all, "  "+chunk)
		}
	}
	return strings.Join(clipRows(all, rows, "  "), "\n")
}

// blockRows is how many rows a panel's wrapped text may take, leaving the transcript a few.
func (m *Model) blockRows() int {
	return max(4, m.height-14)
}

// statusPart is one piece of the status line; when the line is too wide the lowest rank goes first.
type statusPart struct {
	text string
	rank int
}

const (
	rankWorker = iota
	rankAwake
	rankVCS
	rankMode
	rankHint
	rankNow
)

// fitStatus joins parts with sep, dropping the lowest-ranked, the later of a tie first, until the line fits width.
func fitStatus(parts []statusPart, sep string, width int) string {
	join := func() string {
		texts := make([]string, len(parts))
		for i, p := range parts {
			texts[i] = p.text
		}
		return strings.Join(texts, sep)
	}
	for len(parts) > 1 && lipgloss.Width(join()) > width {
		drop := len(parts) - 1
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i].rank < parts[drop].rank {
				drop = i
			}
		}
		parts = append(parts[:drop:drop], parts[drop+1:]...)
	}
	return oneRow(join(), width)
}

func (m *Model) statusParts() []statusPart {
	var parts []statusPart
	add := func(text string, rank int) { parts = append(parts, statusPart{text, rank}) }

	// Opens the line, being the one part of it that is about right now.
	if m.snap.Busy {
		lead, text := shimmer("working…", m.poseTick, 1, dimStyle, workingShimmer), ""
		// Named only while there is something to click, since a hint for a gesture with no
		// target is one nobody can act on.
		if m.snap.Steerable && m.steerableCount() > 0 {
			text = "click a queued message to send it now"
		}
		if m.snap.Cancelling {
			lead, text = shimmer("cancelling…", m.poseTick, 2, dimStyle, cancellingShimmer), "esc again to give up on it"
		}
		if text != "" {
			lead += dimStyle.Render(" " + text)
		}
		add(lead, rankNow)
	}

	switch m.snap.Awake {
	case awake.Holding:
		add(dimStyle.Render(awakeMark), rankAwake)
	case awake.LowBattery:
		add(lipgloss.NewStyle().Foreground(pendingColor).Render(
			fmt.Sprintf("battery under %d%%, letting the Mac sleep", awake.Floor)), rankHint)
	}

	if m.vcs != nil {
		pr := 0
		if p := m.prsByHead(m.VCSSlug)[m.vcs.Bookmark]; p != nil {
			pr = p.Number
		}
		add(m.vcs.Render(pr), rankVCS)
	}

	mode := session.Effective(m.snap.Mode)
	modeStyle := lipgloss.NewStyle().Foreground(modeColor[mode])
	add(modeStyle.Render(string(mode)+" mode"), rankMode)

	// Only the keys that mean something in the current context; the always-true gestures are learnt once.
	switch {
	case m.sideFocused():
		add(dimStyle.Render(sideFocusHint), rankHint)
	case m.runView != nil:
		add(dimStyle.Render(runViewHint), rankHint)
	case m.skillPane != nil:
		add(dimStyle.Render(skillHint), rankHint)
	case m.fileDiff != nil:
		add(dimStyle.Render(fileDiffHint), rankHint)
	case m.fileGraph != nil && m.fileGraph.chatting:
		add(dimStyle.Render(fileGraphChatHint), rankHint)
	case m.fileGraph != nil && m.fileGraph.diff:
		add(dimStyle.Render(fileGraphDiffHint), rankHint)
	case m.fileGraph != nil && m.fileGraph.inFindings:
		add(dimStyle.Render(fileGraphFindingsHint), rankHint)
	case m.fileGraph != nil:
		add(dimStyle.Render(fileGraphHint), rankHint)
	case !m.sel.empty():
		add(dimStyle.Render("copied on release · esc clears"), rankHint)
	}

	for _, o := range m.others {
		if o.Worker == "" || o.Origin != m.Origin {
			continue
		}
		text := strings.TrimSpace(o.Worker + " " + o.Stage)
		if o.PromptID != "" {
			text += " ⚑"
		}
		add(dimStyle.Render(text), rankWorker)
	}

	return parts
}

// keptWidth is the width of the part fitStatus drops last, zero when there are none.
func keptWidth(parts []statusPart) int {
	kept := -1
	for i, p := range parts {
		if kept < 0 || p.rank > parts[kept].rank {
			kept = i
		}
	}
	if kept < 0 {
		return 0
	}
	return lipgloss.Width(parts[kept].text)
}

// jumpKeys names the prompt-jump keys this terminal can deliver.
func (m *Model) jumpKeys() string {
	if m.disambiguated {
		return "ctrl+[ ]"
	}
	return "alt+↑↓"
}

// How full the model's context is, per the last answered request: the whole prompt it was sent,
// not what this conversation's text adds up to. Absent until then, because "0%" would claim a
// measurement nobody has taken.
func (m *Model) renderContext() string {
	u := m.snap.Usage
	if !u.Known() {
		return ""
	}
	// Floored at 1%, since a rounded-down zero reads as "not measured".
	percent := max(int(u.Fraction()*100+0.5), 1)

	style := dimStyle
	switch {
	case percent >= 90:
		style = lipgloss.NewStyle().Foreground(errorColor)
	case percent >= 70:
		style = lipgloss.NewStyle().Foreground(pendingColor)
	}
	return style.Render(fmt.Sprintf("ctx %d%%", percent)) +
		dimStyle.Render(fmt.Sprintf(" (%s/%s)", compactTokens(u.Total()), compactTokens(u.Window))) +
		renderCache(u)
}

// Below this a prompt may be under the model's cacheable minimum, so caching nothing is no fault.
const cacheableFloor = 4096

// A cacheable prompt that neither read nor wrote the cache was never asked to be cached.
func renderCache(u agent.Usage) string {
	if u.Prompt <= 0 || u.Counted {
		return ""
	}
	if u.CacheRead == 0 && u.CacheWrite == 0 && u.Prompt >= cacheableFloor {
		return dimStyle.Render(" · ") + lipgloss.NewStyle().Foreground(errorColor).Render("cache off")
	}
	return dimStyle.Render(fmt.Sprintf(" · cache %d%%", int(float64(u.CacheRead)/float64(u.Prompt)*100+0.5)))
}

// Token counts are read at a glance, so the exact figure is noise past a thousand.
func compactTokens(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 10_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1000, 'f', 1, 64), ".0") + "k"
	default:
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	}
}

// The cursor is the textarea's own, shifted by where the frame put it: the margin, border and
// padding on the left, and the upper border, which carries the status line, above.
func (m *Model) inputCursor() *tea.Cursor {
	c := m.input.Cursor()
	if c == nil {
		return nil
	}
	frameTop := m.height - 1 - m.input.Height() - 1
	c.X += 3
	c.Y += frameTop + 1
	return c
}
