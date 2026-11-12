package ui

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/graphview"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/reviewgraph"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/charmbracelet/x/ansi"
)

const (
	fileGraphHint         = "↑↓ move · l or tab selects findings · enter shows the diff · d drafts the review on GitHub · esc closes"
	fileGraphFindingsHint = "↑↓ or [ ] select · c asks about it · x dismisses · d drafts the review on GitHub · enter shows the diff · h, tab or esc back to the files"
	fileGraphDiffHint     = "j/k or ↑↓ move · [ ] previous/next finding · c asks about it · x dismisses · d drafts the review on GitHub · pgup/pgdn scroll · esc back to the files"
	// fileListShare caps the file list at this fraction of the pane, leaving the rest to the graph.
	fileListShare = 3
	// minSideWidth is the narrowest the graph and findings may get before the list drops its line counts.
	minSideWidth = 40
)

// fileGraphPane is a PR's changed files on the left and the hovered one's neighbours drawn on the right.
type fileGraphPane struct {
	g     reviewgraph.Graph
	title string
	names map[string]string
	// changed indexes g.Files; cur indexes changed.
	changed []int
	cur     int
	diff    bool
	pics    map[int]neighbours
	// added and removed count each changed file's patch lines, by path.
	added, removed map[string]int
	// review is the PR's stored review, nil when it has none.
	review    *reviewstore.Review
	reviewErr error
	// inFindings moves the keys from the file list to the hovered file's findings; sel indexes those.
	inFindings bool
	sel        int
	// line indexes diffItems, the diff view's cursor; follow scrolls it into view on the next layout.
	line   int
	follow bool
	// chats are the findings' live agents, by index into review.Findings; while chatting, input is chatAt's,
	// drawn in its box.
	chats    map[int]*findingChat
	chatting bool
	chatAt   int
	input    *textarea.Model
}

type neighbours struct {
	pic   graphview.Picture
	drawn bool
}

type fileGraphMsg struct {
	item      prs.Item
	g         reviewgraph.Graph
	err       error
	review    *reviewstore.Review
	reviewErr error
}

var buildFileGraph = reviewgraph.Build

func fileGraphCmd(it prs.Item) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		g, err := buildFileGraph(ctx, it.Repo, it.Number)
		msg := fileGraphMsg{item: it, g: g, err: err}
		if r, ok, err := loadReview(it.Repo, it.Number); ok {
			msg.review = &r
		} else {
			msg.reviewErr = err
		}
		return msg
	}
}

func (m *Model) onFileGraph(msg fileGraphMsg) tea.Cmd {
	if m.picker != nil && m.picker.loading == msg.item.Number {
		m.picker.loading = 0
	}
	n := msg.item.Number
	if msg.err != nil {
		return m.Notify(fmt.Sprintf("graph #%d: %v", n, msg.err))
	}
	p := &fileGraphPane{g: msg.g, title: msg.item.Title, pics: map[int]neighbours{}, review: msg.review, reviewErr: msg.reviewErr, chats: map[int]*findingChat{}}
	paths := make([]string, len(msg.g.Files))
	for i, f := range msg.g.Files {
		paths[i] = f.Path
		if f.Changed {
			p.changed = append(p.changed, i)
		}
	}
	if len(p.changed) == 0 {
		return m.Notify(fmt.Sprintf("graph #%d: no changed files", n))
	}
	p.names = shortNames(paths)
	p.added, p.removed = lineCounts(msg.g.Diff)
	m.picker = nil
	m.closePanel()
	m.fileGraph = p
	m.pane.SetYOffset(0)
	m.pane.SetXOffset(0)
	cmd := m.clearSelection()
	m.layout()
	return cmd
}

func (m *Model) onFileGraphKey(key string) tea.Cmd {
	p := m.fileGraph
	if key == "d" {
		return m.draftReview()
	}
	if p.diff {
		if key == "esc" || key == "q" {
			p.diff = false
			m.pane.SetYOffset(0)
			m.layout()
			return nil
		}
		items := m.diffItems()
		if key == "c" {
			m.focusFindingChat()
			return nil
		}
		if key == "x" {
			if p.line < len(items) && items[p.line].finding >= 0 {
				return m.dismissFinding(p.findingIndexes(p.hovered().Path)[items[p.line].finding])
			}
			return nil
		}
		step := map[string]int{"up": -1, "down": 1}[direction(key)]
		jump := map[string]int{"[": -1, "]": 1}[key]
		if step == 0 && jump == 0 {
			m.onPaneKey(key)
			return nil
		}
		for i := p.line + step + jump; i >= 0 && i < len(items); i += step + jump {
			if items[i].selectable() && (jump == 0 || items[i].finding >= 0) {
				p.line, p.follow = i, true
				break
			}
		}
		m.layout()
		return nil
	}
	if p.inFindings {
		return m.onFindingsKey(key)
	}
	switch direction(key) {
	case "up":
		p.cur = max(0, p.cur-1)
	case "down":
		p.cur = min(len(p.changed)-1, p.cur+1)
	case "right":
		if len(p.findingIndexes(p.hovered().Path)) == 0 {
			return nil
		}
		p.inFindings, p.sel = true, 0
	default:
		switch key {
		case "enter":
			m.openDiff(-1)
		case "tab":
			return m.onFileGraphKey("right")
		case "esc", "q":
			m.closePanel()
			return nil
		default:
			return nil
		}
	}
	m.layout()
	return nil
}

func (m *Model) onFindingsKey(key string) tea.Cmd {
	p := m.fileGraph
	idx := p.findingIndexes(p.hovered().Path)
	switch key {
	case "[":
		key = "up"
	case "]":
		key = "down"
	}
	switch direction(key) {
	case "up":
		p.sel = max(0, p.sel-1)
	case "down":
		p.sel = min(len(idx)-1, p.sel+1)
	case "left":
		p.inFindings = false
	default:
		switch key {
		case "c":
			m.focusFindingChat()
			return nil
		case "x":
			return m.dismissFinding(idx[p.sel])
		case "enter":
			p.inFindings = false
			m.openDiff(p.sel)
		case "tab", "esc", "q":
			p.inFindings = false
		default:
			return nil
		}
	}
	m.layout()
	return nil
}

// openDiff shows the hovered file's diff with the cursor on finding sel, or on its first line when sel is -1.
func (m *Model) openDiff(sel int) {
	p := m.fileGraph
	p.diff, p.line, p.follow = true, 0, sel >= 0
	m.pane.SetYOffset(0)
	items := m.diffItems()
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].selectable() && (sel < 0 || items[i].finding == sel) {
			p.line = i
		}
	}
}

func (m *Model) diffItems() []diffItem {
	path := m.fileGraph.hovered().Path
	return reviewedItems(m.patchOf(path), m.fileGraph.g.Sources[path], m.fileGraph.findingsOn(path), m.paneWidth())
}

func (p *fileGraphPane) hovered() reviewgraph.File { return p.g.Files[p.changed[p.cur]] }

func (m *Model) fileGraphHead() []string {
	p := m.fileGraph
	g := p.g
	head := fmt.Sprintf("%s#%d · %s changed · %d impacted",
		g.Repo, g.Number, plural(len(p.changed), "file"), len(g.Files)-len(p.changed))
	width := m.paneWidth()
	var lines []string
	if p.title != "" {
		lines = styledWrap(p.title, lipgloss.NewStyle().Foreground(pickerColor).Bold(true), width)
	}
	lines = append(lines, styledWrap(head, dimStyle, width)...)
	lines = append(lines, p.reviewLine(width)...)
	return append(lines, styledWrap(p.hovered().Path, lipgloss.NewStyle().Bold(true), width)...)
}

func (m *Model) layoutFileGraph() {
	p := m.fileGraph
	width := m.paneWidth()
	m.pane.SetXOffset(0)
	if i, ok := m.selectedFinding(); ok && p.chats[i] != nil {
		p.chats[i].unread = false
	}
	if p.diff {
		path := p.hovered().Path
		title := styledWrap(path, lipgloss.NewStyle().Bold(true), width)
		rows, from, to := renderItems(path, m.diffItems(), p.findingsOn(path), p.deco(path), p.line, width)
		m.pane.SetContentLines(append(title, rows...))
		if p.follow {
			p.follow = false
			from, to = from+len(title), to+len(title)
			y, h := m.pane.YOffset(), m.pane.Height()
			if to >= y+h {
				y = to - h + 1
			}
			m.pane.SetYOffset(max(0, min(y, from)))
		}
		return
	}
	m.pane.SetYOffset(0)
	height := m.pane.Height()
	markW := 0
	for _, i := range p.changed {
		mark, _ := findingMark(p.findingsOn(p.g.Files[i].Path))
		markW = max(markW, ansi.StringWidth(mark))
	}
	markCol := 0
	if markW > 0 {
		markCol = markW + 1
	}
	listWidth := func(counts bool) int {
		w := 0
		for _, i := range p.changed {
			path := p.g.Files[i].Path
			w = max(w, ansi.StringWidth(p.names[path]+m.fileSuffix(path, counts))+2+markCol)
		}
		return min(w, max(8, width/fileListShare))
	}
	counts := true
	listW := listWidth(counts)
	if width-listW-3 < minSideWidth {
		counts = false
		listW = listWidth(counts)
	}
	sideW := max(1, width-listW-3)

	first := min(max(0, p.cur-height/2), max(0, len(p.changed)-height))
	var left []string
	for k := first; k < len(p.changed) && len(left) < height; k++ {
		path := p.g.Files[p.changed[k]].Path
		suffix := m.fileSuffix(path, counts)
		name := oneRow(p.names[path], max(1, listW-2-markCol-ansi.StringWidth(suffix)))
		pad := strings.Repeat(" ", max(0, listW-2-markCol-ansi.StringWidth(name+suffix)))
		if k == p.cur {
			name = lipgloss.NewStyle().Bold(true).Foreground(pickerColor).Background(selectionColor).Render("› " + name)
		} else {
			name = "  " + name
		}
		prefix := ""
		if markCol > 0 {
			mark, markStyle := findingMark(p.findingsOn(path))
			prefix = strings.Repeat(" ", markW-ansi.StringWidth(mark)) + markStyle.Render(mark) + " "
		}
		left = append(left, prefix+name+pad+m.styledFileSuffix(path, counts))
	}
	right := m.neighbourRows(sideW)
	boxFrom, selTo := 0, 0
	if path := p.hovered().Path; len(p.findingIndexes(path)) > 0 {
		sel := -1
		if p.inFindings {
			sel = p.sel
		}
		rows, a, b := fileFindingRows(path, m.patchOf(path), p.g.Sources[path], p.findingsOn(path), p.deco(path), sel, sideW)
		right = append(right, "")
		boxFrom, selTo = len(right)+a, len(right)+b
		right = append(right, rows...)
	}
	right = append(right, gapLines(p.g, sideW)...)
	if p.inFindings && len(right) > height && selTo > height-2 {
		// A range taller than the pane gives up its top lines so the finding's box stays on screen.
		right = right[max(0, min(boxFrom, selTo+2-height)):]
	}
	right = clipRows(right, height, "")

	rows := make([]string, max(len(left), len(right)))
	for y := range rows {
		var l, r string
		if y < len(left) {
			l = left[y]
		}
		if y < len(right) {
			r = right[y]
		}
		rows[y] = l + strings.Repeat(" ", max(0, listW-ansi.StringWidth(l))) + dimStyle.Render(" │ ") + r
	}
	m.pane.SetContentLines(rows)
}

type styledPart struct {
	text  string
	style lipgloss.Style
}

// fileSuffixParts is what follows a file's name in the list: its line counts when counts is set, and
// chat mark.
func (m *Model) fileSuffixParts(path string, counts bool) []styledPart {
	p := m.fileGraph
	chat, chatStyle := m.chatMark(path)
	parts := []styledPart{
		{fmt.Sprintf(" +%d", p.added[path]), lipgloss.NewStyle().Foreground(readyColor)},
		{fmt.Sprintf(" -%d", p.removed[path]), errorStyle},
		{chat, chatStyle},
	}
	if !counts {
		parts = parts[2:]
	}
	return parts
}

func joinParts(parts []styledPart) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.text)
	}
	return b.String()
}

func (m *Model) fileSuffix(path string, counts bool) string {
	return joinParts(m.fileSuffixParts(path, counts))
}

func (m *Model) styledFileSuffix(path string, counts bool) string {
	var b strings.Builder
	for _, part := range m.fileSuffixParts(path, counts) {
		if part.text != "" {
			b.WriteString(part.style.Render(part.text))
		}
	}
	return b.String()
}

// lineCounts is each file's added and removed lines in a git diff, by path.
func lineCounts(gitDiff string) (added, removed map[string]int) {
	added, removed = map[string]int{}, map[string]int{}
	for _, part := range splitGitDiff(gitDiff) {
		for _, line := range strings.Split(part.body, "\n") {
			switch {
			case strings.HasPrefix(line, "+"):
				added[part.path]++
			case strings.HasPrefix(line, "-"):
				removed[part.path]++
			}
		}
	}
	return added, removed
}

// neighbourRows draws the hovered file with the files leading into it and out of it, or lists them when the picture is too wide.
func (m *Model) neighbourRows(width int) []string {
	p := m.fileGraph
	at := p.changed[p.cur]
	f := p.g.Files[at]
	var in, out []string
	ids := []string{f.Path}
	index := map[string]int{f.Path: 0}
	var gr graphview.Diagram
	add := func(other string) {
		if _, seen := index[other]; !seen {
			index[other] = len(ids)
			ids = append(ids, other)
		}
	}
	link := func(from, to string) {
		gr.Edges = append(gr.Edges, graphview.Edge{From: index[from], To: index[to]})
	}
	for _, e := range p.g.Edges {
		switch f.Path {
		case e.To:
			in = append(in, e.From)
			add(e.From)
			link(e.From, e.To)
		case e.From:
			out = append(out, e.To)
			add(e.To)
			link(e.From, e.To)
		}
	}
	for _, id := range ids {
		name := p.names[id]
		if name == "" {
			name = id
		}
		gr.Nodes = append(gr.Nodes, graphview.Node{Lines: []string{name}})
	}
	if len(in)+len(out) == 0 {
		return styledWrap("nothing in the PR leads into this file, and nothing found mentions what it defines", dimStyle, width)
	}
	nb, ok := p.pics[at]
	if !ok {
		nb.pic, nb.drawn = graphview.SketchPicture(gr)
		p.pics[at] = nb
	}
	if nb.drawn && pictureWidth(nb.pic) <= width {
		changed := map[string]bool{}
		for _, i := range p.changed {
			changed[p.g.Files[i].Path] = true
		}
		styles := make([]lipgloss.Style, len(ids))
		for i, id := range ids {
			styles[i] = lipgloss.NewStyle().Foreground(mutedColor)
			if changed[id] {
				styles[i] = lipgloss.NewStyle().Foreground(pickerColor)
			}
		}
		styles[0] = styles[0].Bold(true)
		return pictureRows(nb.pic, styles, lipgloss.NewStyle().Foreground(mutedColor))
	}
	var lines []string
	section := func(title string, paths []string) {
		if len(paths) == 0 {
			return
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, styledWrap(fmt.Sprintf("%s (%d)", title, len(paths)), dimStyle, width)...)
		for _, q := range paths {
			lines = append(lines, hang("  ", 2, p.names[q], plainStyle, width)...)
		}
	}
	section("leads in", in)
	section("leads out", out)
	return lines
}

func pictureWidth(pic graphview.Picture) int {
	w := 0
	for _, row := range pic.Cells {
		w = max(w, len(row))
	}
	return w
}

func (m *Model) patchOf(file string) string {
	for _, part := range splitGitDiff(m.fileGraph.g.Diff) {
		if part.path == file {
			return part.body
		}
	}
	return ""
}

// shortNames is each path's base name, with as many parent directories as it takes to tell it from every other path.
func shortNames(paths []string) map[string]string {
	depth := map[string]int{}
	for _, p := range paths {
		depth[p] = 1
	}
	suffix := func(p string) string {
		parts := strings.Split(path.Clean(p), "/")
		return strings.Join(parts[max(0, len(parts)-depth[p]):], "/")
	}
	for {
		by := map[string][]string{}
		for _, p := range paths {
			by[suffix(p)] = append(by[suffix(p)], p)
		}
		grew := false
		for _, same := range by {
			if len(same) < 2 {
				continue
			}
			for _, p := range same {
				if depth[p] < strings.Count(path.Clean(p), "/")+1 {
					depth[p]++
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}
	names := map[string]string{}
	for _, p := range paths {
		names[p] = suffix(p)
	}
	return names
}

// gapLines lists what the graph did not follow: unread changed files and too-common names.
func gapLines(g reviewgraph.Graph, width int) []string {
	var lines []string
	section := func(head string, items []string) {
		if len(items) == 0 {
			return
		}
		lines = append(append(lines, ""), styledWrap(head, lipgloss.NewStyle().Foreground(pendingColor), width)...)
		for _, it := range items {
			lines = append(lines, hang("  ", 2, it, dimStyle, width)...)
		}
	}
	section(fmt.Sprintf("%d changed files had no entity sem could read, so nothing they define is followed:", len(g.Unexamined)), g.Unexamined)
	section(fmt.Sprintf("%d changed names appear in too many files to trace, so they draw no edge:", len(g.TooCommon)), g.TooCommon)
	return lines
}
