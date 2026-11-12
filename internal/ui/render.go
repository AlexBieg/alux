package ui

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/classify"
	"github.com/alexbieg/alux/internal/code"
	"github.com/alexbieg/alux/internal/diff"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/markdown"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/resolve"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/charmbracelet/x/ansi"
)

// transcript is the rendered conversation plus the map from content line to item, which is what
// makes a click land on the right box. viewport owns scrolling, so the only arithmetic here is
// "which item does line N belong to" — the rest is the library's.
type transcript struct {
	lines []string
	owner []int // parallel to lines; -1 where a line belongs to no box
	// first is each item's opening line, which is where scrolling to an item lands. Kept apart
	// from owner because it holds every item, clickable or not.
	first map[int]int
	kinds []rowKind // parallel to lines; what a copy has to leave out of each row
	// prompts is every prompt in order, which is what jumping between them and the pinned header read.
	prompts []promptSpan
	// prose is the line range of each reply and note, the only rows whose ids are clickable.
	prose [][2]int
}

func (t *transcript) inProse(line int) bool {
	for _, r := range t.prose {
		if line >= r[0] && line < r[1] {
			return true
		}
	}
	return false
}

type rowKind uint8

const (
	rowPlain rowKind = iota
	rowBarred
	rowBracketed
	rowFramed
	rowFrameEdge
)

// promptSpan is the lines a prompt was drawn across, both ends included.
type promptSpan struct {
	first, last int
	text        string
}

const noOwner = -1

// add appends a rendered block, one transcript line per line of it.
//
// Tabs are expanded here because this is the one door every line comes through, and a transcript
// line must be measurable: a tab reports zero columns to lipgloss, so one left in the text throws
// out the wrapping of its own line and makes the selection cell grid disagree with the screen.
// The code and diff renderers already expand their own via [code.ExpandTabs]; this catches the
// generic path, which is where bash output arrives.
func (t *transcript) add(block string, id int) {
	t.addLines(splitBlock(block), id)
}

// addLines is add for a block that has already been through [splitBlock], which is how a
// remembered item reaches the transcript without being expanded and split again.
func (t *transcript) addLines(lines []string, id int) {
	t.addRows(lines, id, rowPlain)
}

func (t *transcript) addRows(lines []string, id int, kind rowKind) {
	for _, line := range lines {
		t.lines = append(t.lines, line)
		t.owner = append(t.owner, id)
		t.kinds = append(t.kinds, kind)
	}
}

// addFramed is addRows for a boxed block, whose first and last rows are only border.
func (t *transcript) addFramed(lines []string, id int) {
	start := len(t.kinds)
	t.addRows(lines, id, rowFramed)
	t.kinds[start] = rowFrameEdge
	t.kinds[len(t.kinds)-1] = rowFrameEdge
}

func splitBlock(block string) []string {
	return strings.Split(code.ExpandTabs(block), "\n")
}

func (t *transcript) blank() {
	t.addRows([]string{""}, noOwner, rowPlain)
}

// mark records where the item about to be added begins.
func (t *transcript) mark(id int) {
	if t.first == nil {
		t.first = map[int]int{}
	}
	t.first[id] = len(t.lines)
}

// ownerAt maps a content line to the item occupying it.
func (t *transcript) ownerAt(line int) int {
	if line < 0 || line >= len(t.owner) {
		return noOwner
	}
	return t.owner[line]
}

// selectedText is a selection's copy without the bar columns or a box's border.
func (t *transcript) selectedText(from, to point) string {
	return clippedText(t.lines, from, to, func(row, width int) (int, int, bool) {
		if row >= len(t.kinds) {
			return 0, width, true
		}
		switch t.kinds[row] {
		case rowBarred:
			return barCols, width, true
		case rowBracketed:
			return bracketCols + barCols, width, true
		case rowFramed:
			return frameCols, width - frameCols, true
		case rowFrameEdge:
			return 0, 0, false
		}
		return 0, width, true
	})
}

// boxedPrompt reports whether item is a prompt drawn in its own box, which it is wherever one fits.
func boxedPrompt(item session.Item, width int) bool {
	return item.Kind == session.KindUser && frameInner(width) > 0
}

// frameFor is the tone of the bar an item is drawn with, or false for prose and boxed prompts.
func frameFor(item session.Item, width int) (color.Color, bool) {
	if barInner(width) == 0 || boxedPrompt(item, width) {
		return nil, false
	}
	switch item.Kind {
	case session.KindAssistant:
		return nil, false
	case session.KindUser:
		return userColor, true
	case session.KindThinking:
		return thinkingColor, true
	case session.KindTool:
		return toneFor(item.Tool), true
	case session.KindError:
		return errorColor, true
	}
	return mutedColor, true
}

// groupable reports whether item shares prev's bar: consecutive calls to the same tool.
func groupable(prev, item session.Item) bool {
	return prev.Kind == session.KindTool && item.Kind == session.KindTool && prev.Tool == item.Tool
}

// composeGroup is the compose call a tool box ran under, "" for a direct call or where the bracket cannot fit.
func composeGroup(item session.Item, width int) string {
	if item.Kind != session.KindTool || barInner(width-bracketCols) == 0 {
		return ""
	}
	return agent.ComposeParent(item.Call)
}

// items is the transcript as drawn, less pair navigators. An in-flight thought is shown as the box
// it is about to become, already carrying that box's id, so it keeps any expansion when it flushes
// into the item list.
func (m *Model) items() []session.Item {
	items := make([]session.Item, 0, len(m.snap.Items)+1)
	for _, item := range m.snap.Items {
		if m.visibleItem(item) {
			items = append(items, item)
		}
	}
	if lt := m.snap.LiveThinking; lt != nil {
		items = append(items, session.Item{ID: lt.ID, Kind: session.KindThinking, Text: lt.Text})
	}
	return items
}

func (m *Model) buildTranscript() transcript {
	var t transcript
	width := m.contentWidth()
	if m.Names != nil && m.namesStale && !m.selecting() {
		m.names, m.namesStale = m.Names.Known(), false
	}

	m.noteWelcome()
	if m.welcoming() {
		t.addRows(m.welcomeLines(width), noOwner, rowPlain)
	}

	var prev session.Item
	prevGroup := ""
	items := m.items()
	for i, item := range items {
		_, framed := frameFor(item, width)
		group := composeGroup(item, width)
		joined := framed && i > 0 && groupable(prev, item) && group == prevGroup
		switch {
		case joined:
		case group != "" && group == prevGroup:
			t.addRows([]string{bracketRow("│")}, noOwner, rowFrameEdge)
		case len(t.lines) > 0:
			t.blank()
		}
		if group != "" && group != prevGroup {
			t.addRows([]string{bracketRow("╭─")}, noOwner, rowFrameEdge)
		}
		t.mark(item.ID)
		if group != "" {
			t.addRows(bracketed(m.itemLines(item, width-bracketCols)), m.boxID(item), rowBracketed)
			if i == len(items)-1 || composeGroup(items[i+1], width) != group {
				t.addRows([]string{bracketRow("╰─")}, noOwner, rowFrameEdge)
			}
			prev, prevGroup = item, group
			continue
		}
		start := len(t.lines)
		lines := m.itemLines(item, width)
		switch {
		case boxedPrompt(item, width):
			t.addFramed(lines, m.boxID(item))
		case framed:
			t.addRows(lines, m.boxID(item), rowBarred)
		default:
			t.addRows(lines, m.boxID(item), rowPlain)
		}
		if item.Kind == session.KindAssistant || item.Kind == session.KindNote {
			t.prose = append(t.prose, [2]int{start, len(t.lines)})
		}
		if item.Kind == session.KindUser {
			t.prompts = append(t.prompts, promptSpan{first: start, last: len(t.lines) - 1, text: item.Text})
		}
		prev, prevGroup = item, group
	}

	if m.snap.Live != "" {
		// Looked up while the reply streams, so the names are usually in by the time it lands.
		if m.Names != nil {
			for _, id := range resolve.IDs(m.snap.Live) {
				m.Names.Name(id)
			}
		}
		if len(t.lines) > 0 {
			t.blank()
		}
		t.add(markdown.Render(m.snap.Live, width), noOwner)
	}
	return t
}

// blockKey is everything outside an item that its rendering depends on, so a block can never be
// served back for a width or a state it was not rendered for. Expansion earns a key of its own
// rather than evicting, so opening and closing a box repeatedly costs one render each way.
//
// Width is in here for correctness and dropped from the map on resize for size; the resize is what
// keeps this bounded, and the key is what makes it right if some other path ever changes the width.
type blockKey struct {
	id       int
	width    int
	expanded bool
	// A verdict lands after the box is drawn and retints its head, so a block remembered before
	// it arrived is not the block to draw after.
	writes      classify.Verdict
	guardian    string
	unsandboxed bool
	rev         int
	// names hashes the known names of the ids the item mentions, so a name arriving redraws only them.
	names uint64
}

func (m *Model) namesKey(item session.Item) uint64 {
	if m.Names == nil || (item.Kind != session.KindAssistant && item.Kind != session.KindNote) {
		return 0
	}
	h := fnv.New64a()
	for _, id := range resolve.IDs(item.Text) {
		if e := m.names[id]; e.State == resolve.Known {
			h.Write([]byte(id + "\x00" + e.Name + "\x00"))
		}
	}
	return h.Sum64()
}

// nameFor reads only the applied copy; a miss there is queued with the resolver for a later NamesMsg.
func (m *Model) nameFor(id string) (string, bool) {
	if e := m.names[id]; e.State == resolve.Known {
		return e.Name, true
	}
	m.Names.Name(id)
	return "", false
}

// underlineNames marks every id with a known name, leaving the layout markdown chose untouched.
func (m *Model) underlineNames(rendered string) string {
	if m.Names == nil {
		return rendered
	}
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		if spans := m.namedSpans(ansi.Strip(line)); len(spans) > 0 {
			lines[i] = underlineCells(line, spans)
		}
	}
	return strings.Join(lines, "\n")
}

// underlineCells keeps the line's own escapes, re-asserting the underline after any SGR inside a span.
func underlineCells(line string, spans []namedSpan) string {
	var b strings.Builder
	var state byte
	col, on := 0, false
	for len(line) > 0 {
		seq, width, n, next := ansi.DecodeSequence(line, state, nil)
		line, state = line[n:], next
		if width == 0 {
			b.WriteString(seq)
			if on && strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") {
				b.WriteString("\x1b[4m")
			}
			continue
		}
		if !on && len(spans) > 0 && col == spans[0].from {
			b.WriteString("\x1b[4m")
			on = true
		}
		b.WriteString(seq)
		col += width
		if on && col >= spans[0].to {
			b.WriteString("\x1b[24m")
			on, spans = false, spans[1:]
		}
	}
	return b.String()
}

type namedSpan struct {
	from, to int // cell columns
	id, name string
}

func (m *Model) namedSpans(plain string) []namedSpan {
	var out []namedSpan
	for _, s := range resolve.Find(plain) {
		id := plain[s[0]:s[1]]
		if name, ok := m.nameFor(id); ok {
			from := ansi.StringWidth(plain[:s[0]])
			out = append(out, namedSpan{from: from, to: from + len(id), id: id, name: name})
		}
	}
	return out
}

// nameAt is the named id drawn under a transcript point, when it sits in prose.
func (m *Model) nameAt(pt point) (namedSpan, bool) {
	if m.Names == nil || !m.content.inProse(pt.line) {
		return namedSpan{}, false
	}
	for _, s := range m.namedSpans(ansi.Strip(m.content.lines[pt.line])) {
		if pt.col >= s.from && pt.col < s.to {
			return s, true
		}
	}
	return namedSpan{}, false
}

// itemLines is an item's rendered block, remembered where it can be.
//
// Laying out re-renders the whole conversation, off-screen items included, and it has to: the
// viewport needs a total line count to scroll against, a click needs the line-to-item map, and a
// copy reads the lines back. Rendering an item is dominated by markdown, so without this a single
// streamed token or spinner frame re-renders every item above it — which a few hundred items in is
// more than a frame's worth of work for one line of change.
func (m *Model) itemLines(item session.Item, width int) []string {
	if volatile(item, m.snap.LiveThinking, m.snap.Procs) {
		return m.framedLines(item, width)
	}
	expanded := false
	if box, ok := session.Collapsible(item, m.snap.Runs, m.snap.Procs); ok {
		expanded = m.isExpanded(item, box)
	}
	key := blockKey{id: item.ID, width: width, expanded: expanded, writes: item.Writes, guardian: guardianTag(item.Guardian), unsandboxed: item.Unsandboxed, rev: item.Rev, names: m.namesKey(item)}
	if lines, ok := m.blocks[key]; ok {
		return lines
	}
	lines := m.framedLines(item, width)
	if m.blocks == nil {
		m.blocks = map[blockKey][]string{}
	}
	m.blocks[key] = lines
	return lines
}

func (m *Model) framedLines(item session.Item, width int) []string {
	if boxedPrompt(item, width) {
		return promptBox(splitBlock(m.renderItem(item, frameInner(width))), width, userColor)
	}
	tone, framed := frameFor(item, width)
	if !framed {
		return splitBlock(m.renderItem(item, width))
	}
	return barred(splitBlock(m.renderItem(item, barInner(width))), width, tone)
}

// volatile reports whether an item can render differently without being replaced, which is what
// makes it unsafe to remember. Four can: a subagent box is drawn from the live roster, a running
// tool box shimmers and is about to be resolved in place, a resolved bash box with
// a process still running behind it is drawn from that process's output, and an in-flight thought
// grows with every delta. Everything else is fixed once it reaches the transcript, since the
// session appends items and never edits one.
func volatile(item session.Item, thinking *session.LiveThinking, live []procs.Info) bool {
	if item.Run != 0 || item.Running {
		return true
	}
	if _, ok := session.LiveProc(item, live); ok {
		return true
	}
	return thinking != nil && item.ID == thinking.ID
}

// boxID is the item's id when it is clickable, and noOwner otherwise, so a click on prose does
// nothing rather than toggling whatever happens to be nearby.
func (m *Model) boxID(item session.Item) int {
	if _, ok := session.Collapsible(item, m.snap.Runs, m.snap.Procs); ok {
		return item.ID
	}
	return noOwner
}

// hasLiveBox reports whether any box redraws on its own — a tool still shimmering, or a
// background command writing into a box already resolved — which is what makes a tick worth a
// relayout. The tick is the clock all of them ride, since none has its own.
func (m *Model) hasLiveBox() bool {
	for _, item := range m.snap.Items {
		if item.Running {
			return true
		}
		if _, ok := session.LiveProc(item, m.snap.Procs); ok {
			return true
		}
	}
	return false
}

// noteFinishes times only runs seen live first, so a pane opened late celebrates nothing.
func (m *Model) noteFinishes(was, now []subagent.Run) {
	live := map[int]bool{}
	for _, run := range was {
		live[run.ID] = run.Live()
	}
	for _, run := range now {
		if (run.State == subagent.Done || run.State == subagent.Failed) && live[run.ID] {
			m.finished[run.ID] = m.now()
		}
	}
}

func (m *Model) pruneFinished() bool {
	pruned := false
	for id, at := range m.finished {
		if m.now().Sub(at) >= flourishFor {
			delete(m.finished, id)
			pruned = true
		}
	}
	return pruned
}

func (m *Model) flourishing(id int) (time.Duration, bool) {
	at, ok := m.finished[id]
	if !ok {
		return 0, false
	}
	elapsed := m.now().Sub(at)
	return elapsed, elapsed < flourishFor
}

const (
	flourishFor  = 1800 * time.Millisecond
	flourishStep = 300 * time.Millisecond
	// gazeHold is a second and a half of MiniDot's twelve ticks a second.
	gazeHold = 18
)

var runGazes = []string{aluxEyesUp, aluxEyesUpLeft, aluxEyesUpRight}

// runPose is a run's hat and face rows, nil when the run has none to show.
func (m *Model) runPose(run subagent.Run) []string {
	hat, eyes := runHat(run.ID), aluxEyesUp
	switch run.State {
	case subagent.Running:
		eyes = gaze(run.ID, m.poseTick)
	case subagent.Waiting:
		eyes = aluxWaveUp
		if (m.poseTick/aluxBeat)%2 == 1 {
			eyes = aluxWaveDown
		}
	case subagent.Idle:
		hat, eyes = tipHat(hat, 1), aluxEyesShut
		if (m.poseTick/(2*aluxHold))%2 == 1 {
			eyes = aluxEyesDoze
		}
	case subagent.Done:
		elapsed, ok := m.flourishing(run.ID)
		if !ok {
			return nil
		}
		if int(elapsed/flourishStep)%2 == 1 {
			hat = aluxHatLifted
		}
	case subagent.Failed:
		if _, ok := m.flourishing(run.ID); !ok {
			return nil
		}
		hat, eyes = tipHat(hat, -1), aluxEyesSad
	default:
		return nil
	}
	return []string{hat, eyes}
}

// gaze offsets each run's hold by a hash of the run, so runs side by side never glance in step.
func gaze(run, tick int) string {
	h := mix(uint32(run))
	hold := (uint32(tick) + h%gazeHold) / gazeHold
	return runGazes[mix(h^hold)%uint32(len(runGazes))]
}

func mix(x uint32) uint32 {
	x *= 0x9E3779B1
	x ^= x >> 15
	x *= 0x2C1B3C6D
	x ^= x >> 12
	return x
}

// heldProcs holds every live box's tail where it already was for as long as a selection is up.
// A selection is absolute line indices and the copy happens on release, so a box that grows
// mid-drag slides the selection onto text nobody dragged across.
func (m *Model) heldProcs(live []procs.Info) []procs.Info {
	if !m.selecting() {
		return live
	}
	for i, p := range live {
		for _, was := range m.snap.Procs {
			if was.Call == p.Call {
				live[i].Tail = was.Tail
				break
			}
		}
	}
	return live
}

// selecting counts a press that has not moved yet: the anchor is already fixed to a line.
func (m *Model) selecting() bool { return m.sel.dragging || !m.sel.empty() }

func (m *Model) renderItem(item session.Item, width int) string {
	if item.Run != 0 {
		return m.renderRunItem(item, width)
	}
	if box, ok := session.Collapsible(item, m.snap.Runs, m.snap.Procs); ok {
		return m.renderBox(item, box, width)
	}
	switch item.Kind {
	case session.KindUser:
		// Joined rather than concatenated so a prompt that wraps keeps its continuation lines
		// under the text, not under the marker.
		return lipgloss.JoinHorizontal(lipgloss.Top,
			userStyle.Render("› "), prose(item.Text, width-2))
	case session.KindAssistant, session.KindNote:
		return m.underlineNames(markdown.Render(item.Text, width))
	case session.KindReport:
		// Already in columns, so a row too wide is cut rather than rewrapped.
		lines := strings.Split(item.Text, "\n")
		for i, line := range lines {
			lines[i] = oneRow(line, width)
		}
		return strings.Join(lines, "\n")
	case session.KindTable:
		if item.Table != nil {
			return renderTable(item.Table, width)
		}
		return ""
	case session.KindError:
		return strings.Join(styledWrap(item.Text, errorStyle, width), "\n")
	default:
		return strings.Join(styledWrap(item.Text, dimStyle, width), "\n")
	}
}

// prose is markdown at width, wrapped plain where width is too narrow for markdown to honour.
func prose(md string, width int) string {
	if width < markdown.MinWidth {
		return strings.Join(styledWrap(md, plainStyle, width), "\n")
	}
	return markdown.Render(md, width)
}

func (m *Model) renderBox(item session.Item, box session.Box, width int) string {
	var tone color.Color
	if item.Kind == session.KindThinking {
		tone = thinkingColor
	} else {
		tone = toneFor(item.Tool)
	}

	expanded := m.isExpanded(item, box)
	// A tool that has started and not answered has a head and no body, so the marker offers
	// nothing to open and the shimmering head says the call is still going.
	running := item.Kind == session.KindTool && item.Running
	// A background command still writing draws its tail under the head whatever the expansion
	// says, so the marker points down with it rather than offering to open what is already open.
	_, live := session.LiveProc(item, m.snap.Procs)
	marker := "▸"
	switch {
	case running:
		marker = "•"
	case expanded || live:
		marker = "▾"
	}

	markerStyle := lipgloss.NewStyle()
	headStyle := lipgloss.NewStyle()
	if tone != nil {
		markerStyle = markerStyle.Foreground(tone)
		headStyle = headStyle.Foreground(tone)
	}

	// Everything left of the head's text: the marker, and the caret a collapsed prompt keeps.
	// Its width is also the hanging indent, so a wrapped head sits under its own first line.
	lead := markerStyle.Render(marker) + " "
	leadWidth := 2
	if item.Kind == session.KindUser {
		lead += userStyle.Render("› ")
		leadWidth += 2
	}
	// A command the model reads as writing to disk, in the box's own tone rather than a warning
	// colour: the prompt forbids editing through bash, and this says which command to look at
	// without making bash boxes two different colours. One cell wide, like every marker here.
	if item.Writes == classify.Writes {
		lead += markerStyle.Render("✎ ")
		leadWidth += 2
	}
	// nf-md-shield_off, one cell in Monaspace Neon NF.
	if item.Unsandboxed {
		lead += unsandboxedStyle.Render(unsandboxedMark + " ")
		leadWidth += 2
	}

	// No count on a live box: there is no hidden body to measure, and the number would be the
	// tail's rather than the command's — changing every frame while saying nothing the lines
	// under the head do not already say.
	var tail []string
	if tag := guardianTag(item.Guardian); tag != "" {
		tail = append(tail, tag)
	}
	if !expanded && !running && !live {
		n := box.Lines()
		plural := "s"
		if n == 1 {
			plural = ""
		}
		tail = append(tail, dimStyle.Render(fmt.Sprintf("%d line%s", n, plural)))
	}
	count := ""
	if len(tail) > 0 {
		count = " " + dimStyle.Render("· ") + strings.Join(tail, dimStyle.Render(" · "))
	}

	paint := func(s string) string { return headStyle.Render(s) }
	if running {
		ramp := headShimmer(tone)
		paint = func(s string) string { return shimmer(s, m.poseTick, headShimmerSpeed, headStyle, ramp) }
	}
	if item.Tool == agent.KindPoll {
		if h, ok := parsePollHead(box.Head); ok {
			return renderPollBox(h, box.Body, lead, tail, leadWidth, paint, running, live, expanded || live, width)
		}
	}
	head := wrapHead(box.Head, count, paint, width-leadWidth)
	for i := range head {
		if i == 0 {
			head[i] = lead + head[i]
			continue
		}
		head[i] = strings.Repeat(" ", leadWidth) + head[i]
	}
	headBlock := strings.Join(head, "\n")

	// Still running, so there is nothing to open yet — but the expansion is remembered, and the
	// output appears under this head the moment the call answers. A live command, in the foreground or
	// not, is the one box that opens itself: a tail that has to be clicked for is not being watched.
	if (!expanded || running) && !live || live && box.Body == "" {
		return headBlock
	}
	if body, ok := renderCode(item, box, width); ok {
		return headBlock + "\n" + body
	}
	bodyStyle := plainStyle
	if item.Kind == session.KindTool || item.Kind == session.KindThinking {
		bodyStyle = dimStyle
	}
	body := styledWrap(box.Body, bodyStyle, width)
	return headBlock + "\n" + strings.Join(body, "\n")
}

// wrapHead lays a head out in width columns and returns it line by line, with the collapsed
// box's line count riding along: on the last line when it fits there, on a line of its own when
// it does not. Nothing is truncated: a long bash command or a long dispatch task must not run
// off the right edge and take the line count with it, which is the one thing the collapsed form
// is there to say.
//
// Wrapped before styling, like the body below: lipgloss styles a multi-line string line by
// line, and an ANSI-aware wrap would instead leave the colour open across the newline, where
// the cell grid loses it.
func wrapHead(text, count string, paint func(string) string, width int) []string {
	if width < 1 {
		width = 1
	}
	lines := wrapPlain(text, width)
	for i, line := range lines {
		lines[i] = paint(line)
	}
	if count == "" {
		return lines
	}
	last := len(lines) - 1
	if lipgloss.Width(lines[last])+lipgloss.Width(count) <= width {
		lines[last] += count
		return lines
	}
	return append(lines, strings.TrimLeft(count, " "))
}

// guardianTag is guardian's verdict as it reads on a box's head, in the error colour when it flags the command.
func guardianTag(v guardian.Verdict) string {
	if !v.Known() {
		return ""
	}
	// A flagged command only gets a box once the person has let it run.
	if v.Flagged {
		return errorStyle.Render(fmt.Sprintf("guardian ✗ %s · %s risk · you allowed it", v.Does(), v.Risk()))
	}
	return dimStyle.Render(fmt.Sprintf("guardian ✓ %s · %s risk", v.Does(), v.Risk()))
}

// Three tools show source rather than prose, and grey on grey is the hardest way to read any of
// them: the editor writing a change, the editor viewing a file, and a search reporting matches.
// Each has its own shape, and the shape is what decides the drawing — a view's line numbers are
// not a change, so they are not painted like one.
//
// Record-store boxes carry markdown written for the user, so they are drawn as markdown.
// Everything else — bash, the vault tools, a failure message from any of them — is not source and
// comes back as the plain dim text it was.
func renderCode(item session.Item, box session.Box, width int) (string, bool) {
	switch item.Tool {
	case agent.KindEdit:
		path := tools.EditPath(box.Head)
		if tools.EditVerb(box.Head) == "view" {
			return code.Listing(box.Body, path, width), true
		}
		return diff.Render(box.Body, path, width), true
	case agent.KindSearch:
		// No path argument: a search crosses files, so each line names its own.
		return code.Results(box.Body, width), true
	case agent.KindRecord:
		return markdown.Render(box.Body, width), true
	}
	return "", false
}
