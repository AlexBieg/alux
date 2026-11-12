package ui

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/diff"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/charmbracelet/x/ansi"
)

// ReviewMsg says review_load stored a review. Sent from outside the program.
type ReviewMsg struct{ Review reviewstore.Review }

var loadReview = func(repo string, number int) (reviewstore.Review, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return reviewstore.Review{}, false, err
	}
	return reviewstore.Load(home, repo, number)
}

var saveReview = func(r reviewstore.Review) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return reviewstore.Save(home, r)
}

var logDismissal = func(d reviewstore.Dismissal) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return reviewstore.AppendDismissal(home, d)
}

// outsideContext is how many lines of source the diff view shows around a finding's off-diff lines.
const outsideContext = 2

// codeRow is one patch line with its new-side line number, 0 for a header or a deleted line.
type codeRow struct {
	text string
	n    int
}

// codeRows is the patch with blocks of source spliced in, in line order, for the findings' lines
// the patch does not show, each widened by context lines.
func codeRows(patch, source string, findings []reviewstore.Finding, context int) []codeRow {
	numbers := diff.NewLines(patch)
	var chunks [][]codeRow
	var keys []int
	key := 0
	shown := map[int]bool{}
	for i, l := range strings.Split(strings.TrimSuffix(patch, "\n"), "\n") {
		if strings.HasPrefix(l, "@@") || len(chunks) == 0 {
			chunks, keys = append(chunks, nil), append(keys, -1)
		}
		last := len(chunks) - 1
		chunks[last] = append(chunks[last], codeRow{l, numbers[i]})
		if n := numbers[i]; n > 0 {
			shown[n] = true
			if keys[last] < 0 {
				keys[last] = n
			}
		}
	}
	for i := range keys {
		if keys[i] < 0 {
			keys[i] = key
		}
		key = keys[i]
	}

	src := strings.Split(strings.TrimSuffix(source, "\n"), "\n")
	want := map[int]bool{}
	for _, f := range findings {
		for n := max(1, f.StartLine-context); source != "" && f.StartLine > 0 && n <= min(len(src), f.EndLine+context); n++ {
			want[n] = !shown[n]
		}
	}
	for n := 1; n <= len(src); n++ {
		if !want[n] {
			continue
		}
		end := n
		for want[end+1] {
			end++
		}
		block := []codeRow{{fmt.Sprintf("@@ L%d-%d · outside the diff @@", n, end), 0}}
		for k := n; k <= end; k++ {
			block = append(block, codeRow{" " + src[k-1], k})
		}
		chunks, keys = append(chunks, block), append(keys, n)
		n = end
	}

	order := make([]int, len(chunks))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return keys[a] - keys[b] })
	var rows []codeRow
	for _, i := range order {
		rows = append(rows, chunks[i]...)
	}
	return rows
}

func (m *Model) onReviewLoaded(r reviewstore.Review) tea.Cmd {
	blocking := 0
	for _, f := range r.Findings {
		if f.Severity == "blocking" {
			blocking++
		}
	}
	if m.picker != nil {
		m.picker.reviewed[reviewKey(r.Repo, r.Number)] = r.Head
	}
	if p := m.fileGraph; p != nil && strings.EqualFold(p.g.Repo, r.Repo) && p.g.Number == r.Number {
		m.unfocusFindingChat()
		p.review, p.reviewErr = &r, nil
		p.inFindings, p.sel, p.chats = false, 0, map[int]*findingChat{}
		m.layout()
	}
	return m.Notify(fmt.Sprintf("review of #%d loaded · %d blocking · open with g in /prs", r.Number, blocking))
}

// currentReview is the pane's review when it was made at the PR head the graph shows.
func (p *fileGraphPane) currentReview() *reviewstore.Review {
	if p.review == nil || p.review.Head != p.g.Head {
		return nil
	}
	return p.review
}

// findingIndexes indexes the review's undismissed findings on path.
func (p *fileGraphPane) findingIndexes(path string) []int {
	r := p.currentReview()
	if r == nil {
		return nil
	}
	var out []int
	for i, f := range r.Findings {
		if f.Path == path && !f.Dismissed {
			out = append(out, i)
		}
	}
	return out
}

func (p *fileGraphPane) findingsOn(path string) []reviewstore.Finding {
	var out []reviewstore.Finding
	for _, i := range p.findingIndexes(path) {
		out = append(out, p.review.Findings[i])
	}
	return out
}

func (m *Model) dismissFinding(i int) tea.Cmd {
	p := m.fileGraph
	r := *p.review
	r.Findings = append([]reviewstore.Finding{}, r.Findings...)
	r.Findings[i].Dismissed = true
	if err := saveReview(r); err != nil {
		return m.Notify("dismiss: " + err.Error())
	}
	p.review = &r
	var cmd tea.Cmd
	d := reviewstore.Dismissal{Repo: r.Repo, Number: r.Number, Head: r.Head, DismissedAt: time.Now(), Finding: r.Findings[i]}
	if err := logDismissal(d); err != nil {
		cmd = m.Notify("dismiss: " + err.Error())
	}
	if p.diff {
		items := m.diffItems()
		p.line = max(0, min(p.line, len(items)-1))
		for p.line > 0 && !items[p.line].selectable() {
			p.line--
		}
		p.follow = true
	} else {
		left := len(p.findingIndexes(p.hovered().Path))
		p.sel = min(p.sel, left-1)
		p.inFindings = left > 0
	}
	m.layout()
	return cmd
}

// draftReview asks the main agent to put the review's undismissed findings on GitHub as a pending review.
func (m *Model) draftReview() tea.Cmd {
	p := m.fileGraph
	r := p.currentReview()
	if r == nil {
		return m.Notify("no review at the PR's head to draft from")
	}
	var b strings.Builder
	n := 0
	for _, f := range r.Findings {
		if f.Dismissed {
			continue
		}
		n++
		lines := "whole file"
		switch {
		case f.StartLine > 0 && f.EndLine > f.StartLine:
			lines = fmt.Sprintf("lines %d-%d", f.StartLine, f.EndLine)
		case f.StartLine > 0:
			lines = fmt.Sprintf("line %d", f.StartLine)
		}
		if f.OffDiff {
			lines += ", outside the diff"
		}
		fmt.Fprintf(&b, "\n%d. `%s` %s · %s · %s\n", n, f.Path, lines, f.Severity, f.Class)
		for _, s := range f.Sections() {
			fmt.Fprintf(&b, "   %s\n", sectionText(s))
		}
		for _, msg := range f.Thread {
			fmt.Fprintf(&b, "   > %s: %s\n", msg.Role, msg.Text)
		}
	}
	if n == 0 {
		return m.Notify("every finding is dismissed; nothing to draft")
	}
	m.closePanel()
	m.follow = true
	m.session.Submit(fmt.Sprintf("Draft a pending review on %s#%d with pr_pending_review, one comment per finding below, "+
		"worded by the review-comment rules. Where a finding has a thread, word the comment as the thread settled it. "+
		"Anchor a whole-file or off-diff finding to the closest line in the diff. Read what is already drafted with pr_detail first.\n%s",
		r.Repo, r.Number, b.String()))
	m.layout()
	return nil
}

func sectionText(s reviewstore.Section) string {
	if s.Label == "" {
		return s.Text
	}
	return s.Label + ": " + s.Text
}

// findingDeco is what the pane adds to a file's finding boxes: asking parallels the findings, and the
// selected box carries hint and, when non-nil, input.
type findingDeco struct {
	asking []string
	hint   string
	input  *textarea.Model
}

// fileFindingRows draws each finding under the patch lines it sits on, highlighting sel (-1 for none);
// it also returns the first row of sel's box and the last row sel takes.
func fileFindingRows(file, patch, source string, findings []reviewstore.Finding, deco findingDeco, sel, width int) (rows []string, boxFrom, selTo int) {
	lines := codeRows(patch, source, findings, 0)
	head := plural(len(findings), "finding") + " on this file"
	if sel < 0 {
		head += " · l or tab selects one"
	}
	rows = styledWrap(head, dimStyle, width)
	for i, f := range findings {
		rows = append(rows, "")
		var code []string
		for _, l := range lines {
			if f.StartLine > 0 && l.n >= f.StartLine && l.n <= f.EndLine {
				code = append(code, strings.Split(diff.Render(l.text, file, width), "\n")...)
			}
		}
		rows = append(rows, code...)
		if i == sel {
			boxFrom = len(rows)
		}
		rows = append(rows, findingRows(f, deco.at(i, i == sel), width)...)
		if i == sel {
			selTo = len(rows) - 1
		}
	}
	return rows, boxFrom, selTo
}

func (p *fileGraphPane) reviewLine(width int) []string {
	switch {
	case p.reviewErr != nil:
		return styledWrap("review: "+p.reviewErr.Error(), errorStyle, width)
	case p.review == nil:
		return styledWrap("no review of this PR yet", dimStyle, width)
	case p.currentReview() == nil:
		return styledWrap(fmt.Sprintf("review was of %s; the PR has moved, run the review again", shortSHA(p.review.Head)),
			lipgloss.NewStyle().Foreground(pendingColor), width)
	}
	blocking, other, dismissed := 0, 0, 0
	for _, f := range p.review.Findings {
		switch {
		case f.Dismissed:
			dismissed++
		case f.Severity == "blocking":
			blocking++
		default:
			other++
		}
	}
	text := fmt.Sprintf("review · %s · %d blocking · %d other", p.review.Verdict, blocking, other)
	if dismissed > 0 {
		text += fmt.Sprintf(" · %d dismissed", dismissed)
	}
	return styledWrap(text, lipgloss.NewStyle().Foreground(pickerColor), width)
}

// findingMark is the list's count of a file's findings, red when any blocks.
func findingMark(fs []reviewstore.Finding) (string, lipgloss.Style) {
	if len(fs) == 0 {
		return "", dimStyle
	}
	for _, f := range fs {
		if f.Severity == "blocking" {
			return fmt.Sprintf("%d", len(fs)), errorStyle
		}
	}
	return fmt.Sprintf("%d", len(fs)), lipgloss.NewStyle().Foreground(pendingColor)
}

// diffItem is one row of the diff view the cursor can stop on: a patch line, or a finding (index into
// the file's findings); headings and hunk headers are skipped.
type diffItem struct {
	line    codeRow
	finding int
	heading []string
}

func (it diffItem) selectable() bool {
	return it.heading == nil && (it.finding >= 0 || !strings.HasPrefix(it.line.text, "@@"))
}

// reviewedItems lays a file's patch out with each finding after the last of its lines the patch shows,
// then the file's findings that sit on no shown line.
func reviewedItems(patch, source string, findings []reviewstore.Finding, width int) []diffItem {
	rows := codeRows(patch, source, findings, outsideContext)
	shown := map[int]bool{}
	for _, r := range rows {
		shown[r.n] = r.n > 0
	}
	after := map[int][]int{}
	var loose []int
	for i, f := range findings {
		anchor := 0
		for n := f.EndLine; f.StartLine > 0 && n >= f.StartLine; n-- {
			if shown[n] {
				anchor = n
				break
			}
		}
		if anchor == 0 {
			loose = append(loose, i)
			continue
		}
		after[anchor] = append(after[anchor], i)
	}
	var items []diffItem
	for _, r := range rows {
		items = append(items, diffItem{line: r, finding: -1})
		if r.n > 0 {
			for _, i := range after[r.n] {
				items = append(items, diffItem{finding: i})
			}
		}
	}
	if len(loose) > 0 {
		items = append(items, diffItem{finding: -1, heading: append([]string{""}, styledWrap("findings on this file beside no line shown:", dimStyle, width)...)})
		for _, i := range loose {
			items = append(items, diffItem{finding: i})
		}
	}
	return items
}

// renderItems draws items under a one-column gutter: › at the cursor's line, and the severity's bar
// beside every line of the finding under the cursor. It returns the rows the cursor takes.
func renderItems(file string, items []diffItem, findings []reviewstore.Finding, deco findingDeco, cur, width int) (out []string, curFrom, curTo int) {
	var mark *reviewstore.Finding
	if cur >= 0 && cur < len(items) && items[cur].finding >= 0 {
		mark = &findings[items[cur].finding]
	}
	markSt := lipgloss.NewStyle().Foreground(pendingColor)
	if mark != nil && mark.Severity == "blocking" {
		markSt = errorStyle
	}
	for i, it := range items {
		if i == cur {
			curFrom = len(out)
		}
		switch {
		case it.heading != nil:
			out = append(out, it.heading...)
		case it.finding >= 0:
			for _, r := range findingRows(findings[it.finding], deco.at(it.finding, i == cur), width-1) {
				out = append(out, " "+r)
			}
		default:
			gutter := " "
			switch {
			case i == cur:
				gutter = lipgloss.NewStyle().Foreground(pickerColor).Bold(true).Render("›")
			case mark != nil && mark.StartLine > 0 && it.line.n >= mark.StartLine && it.line.n <= mark.EndLine:
				gutter = markSt.Render("┃")
			}
			for k, r := range strings.Split(diff.Render(it.line.text, file, width-1), "\n") {
				if k > 0 && i == cur {
					gutter = " "
				}
				out = append(out, gutter+r)
			}
		}
		if i == cur {
			curTo = len(out) - 1
		}
	}
	return out, curFrom, curTo
}

// reviewedPatch is the diff view of a file with no cursor.
func reviewedPatch(file, patch, source string, findings []reviewstore.Finding, width int) []string {
	out, _, _ := renderItems(file, reviewedItems(patch, source, findings, width), findings, findingDeco{}, -1, width)
	return append(styledWrap(file, lipgloss.NewStyle().Bold(true), width), out...)
}

// findingBox is one finding's share of a findingDeco.
type findingBox struct {
	asking, hint string
	input        *textarea.Model
	selected     bool
}

func (d findingDeco) at(i int, selected bool) findingBox {
	b := findingBox{selected: selected}
	if i < len(d.asking) {
		b.asking = d.asking[i]
	}
	if selected {
		b.hint, b.input = d.hint, d.input
	}
	return b
}

// findingRows boxes a finding, with its thread and any question waiting on a reply under its text;
// a selected one closes with its input, if any, and hint.
func findingRows(f reviewstore.Finding, fb findingBox, width int) []string {
	asking, hint, selected := fb.asking, fb.hint, fb.selected
	st := lipgloss.NewStyle().Foreground(pendingColor)
	if f.Severity == "blocking" {
		st = errorStyle
	}
	head := "review · " + f.Severity + " · " + f.Class
	if f.Open {
		head += " · open"
	}
	if f.StartLine > 0 {
		head += fmt.Sprintf(" · L%d", f.StartLine)
		if f.EndLine > f.StartLine {
			head += fmt.Sprintf("-%d", f.EndLine)
		}
	}
	if len(f.Personas) > 0 {
		head += " · " + strings.Join(f.Personas, ", ")
	}
	if width < 12 {
		bar := st.Render("┃") + " "
		rows := hang(bar, 2, head, st.Bold(true), width)
		for _, s := range f.Sections() {
			rows = append(rows, hang(bar, 2, sectionText(s), plainStyle, width)...)
		}
		return rows
	}
	fill, border := lipgloss.NewStyle(), lipgloss.RoundedBorder()
	if selected {
		fill, border = fill.Background(selectionColor), lipgloss.ThickBorder()
	}
	inner := width - 4
	var lines []string
	add := func(text string, ts lipgloss.Style) {
		for _, l := range styledWrap(strings.TrimSpace(text), ts.Inherit(fill), inner) {
			lines = append(lines, fill.Render(" ")+l+fill.Render(strings.Repeat(" ", max(0, inner-ansi.StringWidth(l))+1)))
		}
	}
	add(head, st.Bold(true))
	for _, s := range f.Sections() {
		if s.Label != "" {
			add(s.Label, dimStyle)
		}
		add(s.Text, plainStyle)
	}
	if len(f.Thread) > 0 || asking != "" {
		add(strings.Repeat("─", inner), dimStyle)
	}
	for _, t := range f.Thread {
		if t.Role == "user" {
			add("› "+t.Text, userStyle)
		} else {
			add(t.Text, plainStyle)
		}
	}
	if asking != "" {
		add("› "+asking, userStyle)
		add("thinking…", dimStyle)
	}
	if fb.input != nil {
		fb.input.SetWidth(inner)
		for _, l := range strings.Split(fb.input.View(), "\n") {
			lines = append(lines, fill.Render(" ")+l+fill.Render(strings.Repeat(" ", max(0, inner-ansi.StringWidth(l))+1)))
		}
	}
	if selected && hint != "" {
		add(hint, dimStyle)
	}
	box := lipgloss.NewStyle().Border(border).BorderForeground(st.GetForeground())
	return strings.Split(box.Render(strings.Join(lines, "\n")), "\n")
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
