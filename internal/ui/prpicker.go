package ui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/reviewers"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/skills"
)

// PRSource is what the picker needs from the poller.
type PRSource interface {
	Detail(ctx context.Context, repo string, number int) (string, error)
	Files(ctx context.Context, repo string, number int) ([]string, error)
	ReviewApp(ctx context.Context, repo string, number int) (prs.ReviewApp, error)
	AddLabel(ctx context.Context, repo string, number int, label string) error
	MergeReady(ctx context.Context, repo string, number int) (prs.MergeCheck, error)
	AddMergeLabel(ctx context.Context, repo string, number int) (string, error)
	Sync(ctx context.Context, force bool)
}

// e2eSkill is the skill e runs against a PR's review app.
const e2eSkill = "e2e-verify"

type pickerRow struct {
	section string
	item    prs.Item
	// ghost is a stacked PR this section does not list; the cursor skips it.
	ghost bool
	// tree is the row's stack connector, "" outside a stack.
	tree string
}

type prPicker struct {
	rows    []pickerRow
	cursor  int
	loading int // the PR whose detail is being fetched, zero when none
	syncing bool
	// reviewed maps reviewKey to the head each stored review was made at.
	reviewed map[string]string
	// offer is a label waiting on y: the deploy label for a PR with no review app, or the merge label.
	offer *labelOffer
}

type labelOffer struct {
	item prs.Item
	// merge sends y through AddMergeLabel, which rereads the PR and refuses if it no longer passes.
	merge bool
	label string
	ask   string
}

type mergeCheckMsg struct {
	item  prs.Item
	check prs.MergeCheck
	err   error
}

// e2eMsg is what e found for a PR: the skill's inputs when its review app is ready, else its state or why it could not be read.
type e2eMsg struct {
	item   prs.Item
	state  prs.EnvState
	label  string
	inputs map[string]any
	err    error
}

type labelledMsg struct {
	number int
	label  string
	merge  bool
	err    error
}

type prDetailMsg struct {
	number int
	text   string
	err    error
}

type openedMsg struct{ err error }

type prSyncedMsg struct{}

// reviewMsg carries the pr-review skill's inputs, chosen from the PR's changed files, or why they could not be.
type reviewMsg struct {
	pr     string
	inputs map[string]any
	err    error
}

var pickerColor = toolColor[agent.KindWeb]

func pickerRows(states []prs.RepoState) []pickerRow {
	var rows []pickerRow
	for _, s := range states {
		pool := append(slices.Clone(s.Queue.Review), s.Queue.Mine...)
		for _, p := range s.Stack {
			pool = append(pool, prs.Item{PR: p})
		}
		rows = append(rows, stackRows("review", s.Queue.Review, pool)...)
		rows = append(rows, stackRows("mine", s.Queue.Mine, pool)...)
	}
	return rows
}

// stackRows lists a section in its order, drawing each stack whole, top first, where its first listed member falls.
func stackRows(section string, items, pool []prs.Item) []pickerRow {
	byHead := map[string]prs.Item{}
	children := map[string][]prs.Item{}
	for _, it := range pool {
		if it.Head != "" && it.Base != "" {
			byHead[it.Head] = it
			children[it.Base] = append(children[it.Base], it)
		}
	}
	for _, cs := range children {
		slices.SortFunc(cs, func(a, b prs.Item) int { return a.Number - b.Number })
	}
	listed := map[int]bool{}
	for _, it := range items {
		listed[it.Number] = true
	}
	placed := map[int]bool{}
	var rows []pickerRow
	for _, it := range items {
		if placed[it.Number] {
			continue
		}
		root := it
		for seen := map[int]bool{it.Number: true}; ; {
			parent, ok := byHead[root.Base]
			if !ok || seen[parent.Number] {
				break
			}
			seen[parent.Number] = true
			root = parent
		}
		var members []prs.Item
		var walk func(prs.Item)
		walk = func(p prs.Item) {
			if placed[p.Number] {
				return
			}
			placed[p.Number] = true
			members = append(members, p)
			for _, c := range children[p.Head] {
				walk(c)
			}
		}
		walk(root)
		if len(members) == 1 {
			rows = append(rows, pickerRow{section: section, item: it})
			continue
		}
		for i := len(members) - 1; i >= 0; i-- {
			tree := "├"
			switch i {
			case len(members) - 1:
				tree = "┬"
			case 0:
				tree = "└"
			}
			rows = append(rows, pickerRow{section: section, item: members[i], ghost: !listed[members[i].Number], tree: tree})
		}
	}
	return rows
}

// step is the next row from the cursor in dir that is not a ghost, or the cursor when there is none.
func (p *prPicker) step(dir int) int {
	for i := p.cursor + dir; i >= 0 && i < len(p.rows); i += dir {
		if !p.rows[i].ghost {
			return i
		}
	}
	return p.cursor
}

func firstListed(rows []pickerRow) int {
	for i, r := range rows {
		if !r.ghost {
			return i
		}
	}
	return 0
}

func reviewKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repo), number)
}

func storedReviews(rows []pickerRow) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		if rv, ok, _ := loadReview(r.item.Repo, r.item.Number); ok {
			out[reviewKey(r.item.Repo, r.item.Number)] = rv.Head
		}
	}
	return out
}

func (m *Model) openPicker(refresh bool) tea.Cmd {
	rows := pickerRows(m.prStates)
	syncing := refresh && m.PRs != nil
	m.picker = &prPicker{rows: rows, cursor: firstListed(rows), reviewed: storedReviews(rows), syncing: syncing}
	m.layout()
	if !syncing {
		return nil
	}
	src := m.PRs
	return func() tea.Msg {
		src.Sync(context.Background(), true)
		return prSyncedMsg{}
	}
}

// refreshPicker keeps the cursor on the same PR when a poll reorders the list.
func (m *Model) refreshPicker() {
	if m.picker == nil {
		return
	}
	var keep int
	if len(m.picker.rows) > 0 {
		keep = m.picker.rows[m.picker.cursor].item.Number
	}
	m.picker.rows = pickerRows(m.prStates)
	m.picker.reviewed = storedReviews(m.picker.rows)
	m.picker.cursor = firstListed(m.picker.rows)
	for i, r := range m.picker.rows {
		if r.item.Number == keep && !r.ghost {
			m.picker.cursor = i
		}
	}
}

func (m *Model) onPickerKey(key string) tea.Cmd {
	p := m.picker
	if o := p.offer; o != nil {
		p.offer = nil
		m.layout()
		if key != "y" || m.PRs == nil {
			return nil
		}
		src, it := m.PRs, o.item
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if o.merge {
				label, err := src.AddMergeLabel(ctx, it.Repo, it.Number)
				if err == nil {
					src.Sync(ctx, true)
				}
				return labelledMsg{number: it.Number, label: label, merge: true, err: err}
			}
			return labelledMsg{number: it.Number, label: o.label, err: src.AddLabel(ctx, it.Repo, it.Number, o.label)}
		}
	}
	switch direction(key) {
	case "up":
		p.cursor = p.step(-1)
		m.layout()
		return nil
	case "down":
		p.cursor = p.step(1)
		m.layout()
		return nil
	}
	switch key {
	case "esc", "q":
		m.picker = nil
		m.layout()
		return nil
	case "enter":
		if len(p.rows) == 0 || m.PRs == nil {
			return nil
		}
		it := p.rows[p.cursor].item
		p.loading = it.Number
		m.layout()
		src := m.PRs
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			text, err := src.Detail(ctx, it.Repo, it.Number)
			return prDetailMsg{number: it.Number, text: text, err: err}
		}
	case "shift+enter", "o":
		if len(p.rows) == 0 {
			return nil
		}
		url := p.rows[p.cursor].item.URL
		return func() tea.Msg { return openedMsg{err: openURL(url)} }
	case "r":
		if len(p.rows) == 0 || m.PRs == nil {
			return nil
		}
		it := p.rows[p.cursor].item
		m.picker = nil
		m.layout()
		src := m.PRs
		return func() tea.Msg { return chooseReviewers(src, it.Repo, it.Number, it.HeadSHA) }
	case "l":
		if len(p.rows) == 0 {
			return nil
		}
		it := p.rows[p.cursor].item
		m.picker = nil
		m.input.SetValue(fmt.Sprintf("About %s#%d %q (%s): ", it.Repo, it.Number, it.Title, it.URL) + m.input.Value())
		m.input.MoveToEnd()
		m.layout()
		return nil
	case "g":
		if len(p.rows) == 0 || p.loading != 0 {
			return nil
		}
		it := p.rows[p.cursor].item
		p.loading = it.Number
		m.layout()
		return fileGraphCmd(it)
	case "e":
		if len(p.rows) == 0 || m.PRs == nil {
			return nil
		}
		it := p.rows[p.cursor].item
		p.loading = it.Number
		m.layout()
		src := m.PRs
		return func() tea.Msg { return reviewAppInputs(src, it) }
	case "m":
		if len(p.rows) == 0 || m.PRs == nil {
			return nil
		}
		it := p.rows[p.cursor].item
		p.loading = it.Number
		m.layout()
		src := m.PRs
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			c, err := src.MergeReady(ctx, it.Repo, it.Number)
			return mergeCheckMsg{item: it, check: c, err: err}
		}
	}
	return nil
}

func (m *Model) onMergeCheck(msg mergeCheckMsg) tea.Cmd {
	if m.picker != nil && m.picker.loading == msg.item.Number {
		m.picker.loading = 0
	}
	n := msg.item.Number
	if msg.err != nil {
		return m.Notify(fmt.Sprintf("merge #%d: %v", n, msg.err))
	}
	if m.picker == nil {
		return nil
	}
	ask := fmt.Sprintf("#%d is yours, approved and merges cleanly", n)
	if k := len(msg.check.Pending); k > 0 {
		ask += fmt.Sprintf(", with %s still running", plural(k, "check"))
	}
	ask += fmt.Sprintf(". y adds %s · any other key cancels", msg.check.Label)
	m.picker.offer = &labelOffer{item: msg.item, merge: true, ask: ask}
	m.layout()
	return nil
}

// reviewAppInputs reads a PR's review app and, once it is ready, picks the app its changed files touch.
func reviewAppInputs(src PRSource, it prs.Item) e2eMsg {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	app, err := src.ReviewApp(ctx, it.Repo, it.Number)
	if err != nil || app.State != prs.EnvReady {
		return e2eMsg{item: it, state: app.State, label: app.Label, err: err}
	}
	login := app.Config.LoginApp
	if login == "" {
		return e2eMsg{item: it, err: fmt.Errorf(`no app to log in through; set "login_app" under prs.repos.%s.review_app in settings.json`, it.Repo)}
	}
	api, ok := app.Apps[login]
	if !ok {
		return e2eMsg{item: it, err: fmt.Errorf("the deploy comment lists no URL for login_app %q to log in through", login)}
	}
	files, err := src.Files(ctx, it.Repo, it.Number)
	if err != nil {
		return e2eMsg{item: it, err: err}
	}
	key, origin := app.Pick(files)
	if origin == "" {
		return e2eMsg{item: it, err: errors.New("no app in the review app matches the changed files")}
	}
	pr := fmt.Sprintf("%s#%d", it.Repo, it.Number)
	return e2eMsg{item: it, state: app.State, inputs: map[string]any{
		"change":  fmt.Sprintf("%s, %q, tested in its review app's %s; read its description with pr_detail for what should now be true", pr, it.Title, key),
		"app_url": origin,
		"api_url": api,
		"diff":    "pr_diff on " + pr + "; the review app runs the PR's pushed head, not the working copy",
	}}
}

func (m *Model) onE2E(msg e2eMsg) tea.Cmd {
	if m.picker != nil && m.picker.loading == msg.item.Number {
		m.picker.loading = 0
	}
	n := msg.item.Number
	switch {
	case msg.err != nil:
		return m.Notify(fmt.Sprintf("e2e #%d: %v", n, msg.err))
	case msg.state == prs.EnvNone:
		if m.picker == nil {
			return m.Notify(fmt.Sprintf("e2e #%d: it has no review app", n))
		}
		ask := fmt.Sprintf("#%d has no review app. y adds %s to deploy one · any other key cancels", n, msg.label)
		m.picker.offer = &labelOffer{item: msg.item, label: msg.label, ask: ask}
		m.layout()
		return nil
	case msg.state == prs.EnvDeploying:
		return m.Notify(fmt.Sprintf("e2e #%d: its review app is still deploying; try again once its health check passes", n))
	case msg.state == prs.EnvFailed:
		return m.Notify(fmt.Sprintf("e2e #%d: its review app failed its health check; rerun the deploy first", n))
	}
	m.picker = nil
	m.layout()
	m.follow = true
	m.session.RunSkill(e2eSkill, msg.inputs)
	return nil
}

func (m *Model) onLabelled(msg labelledMsg) tea.Cmd {
	if msg.merge {
		if msg.err != nil {
			return m.Notify(fmt.Sprintf("merge #%d: %v", msg.number, msg.err))
		}
		return m.Notify(fmt.Sprintf("added %s to #%d", msg.label, msg.number))
	}
	if msg.err != nil {
		return m.Notify(fmt.Sprintf("e2e #%d: adding %s failed: %v", msg.number, msg.label, msg.err))
	}
	return m.Notify(fmt.Sprintf("added %s to #%d; press e again once its review app is deployed", msg.label, msg.number))
}

// chooseReviewers flags a re-review when the stored review is of another head.
func chooseReviewers(src PRSource, repo string, number int, head string) reviewMsg {
	pr := fmt.Sprintf("%s#%d", repo, number)
	dir, err := skills.Dir()
	if err != nil {
		return reviewMsg{pr: pr, err: err}
	}
	corpus, err := reviewers.Load(reviewers.Path(dir))
	if err != nil {
		return reviewMsg{pr: pr, err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	files, err := src.Files(ctx, repo, number)
	if err != nil {
		return reviewMsg{pr: pr, err: err}
	}
	inputs := corpus.Select(files).Inputs(pr)
	inputs["rereview"], inputs["prior_head"], inputs["prior"] = false, "", ""
	if stored, ok, _ := loadReview(repo, number); ok && stored.Head != "" && stored.Head != head {
		home, err := os.UserHomeDir()
		if err != nil {
			return reviewMsg{pr: pr, err: err}
		}
		file, err := reviewstore.Path(home, repo, number)
		if err != nil {
			return reviewMsg{pr: pr, err: err}
		}
		inputs["rereview"], inputs["prior_head"], inputs["prior"] = true, stored.Head, file
	}
	return reviewMsg{pr: pr, inputs: inputs}
}

func (m *Model) onReview(msg reviewMsg) tea.Cmd {
	if msg.err != nil {
		return m.Notify(fmt.Sprintf("review %s: %v", msg.pr, msg.err))
	}
	m.follow = true
	m.session.RunSkill(reviewers.Skill, msg.inputs)
	return nil
}

func (m *Model) onPRDetail(msg prDetailMsg) tea.Cmd {
	if m.picker != nil && m.picker.loading == msg.number {
		m.picker.loading = 0
	}
	if msg.err != nil {
		return m.Notify(fmt.Sprintf("PR #%d: %v", msg.number, msg.err))
	}
	m.follow = true
	m.session.Show(msg.text)
	return nil
}

var openURL = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Run()
}

// pickerHeight caps the list so the transcript keeps most of the pane.
func (m *Model) pickerHeight() int { return max(3, m.height/2-4) }

func (m *Model) renderPicker(width int) string {
	p := m.picker
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(pickerColor).Render("Pull requests"))
	if p.syncing {
		b.WriteString(dimStyle.Render(" · refreshing from GitHub…"))
	}
	if len(p.rows) == 0 {
		b.WriteString("\n" + dimStyle.Render("Nothing to show yet: the first poll has not landed, or the lists are empty."))
	}

	height := m.pickerHeight()
	from := max(0, min(p.cursor-height/2, len(p.rows)-height))
	to := min(len(p.rows), from+height)
	section := ""
	if from > 0 {
		section = p.rows[from-1].section
	}
	authorWidth := 0
	for _, r := range p.rows[from:to] {
		if r.section != "mine" {
			authorWidth = max(authorWidth, min(maxAuthorWidth, lipgloss.Width(r.item.Author)))
		}
	}
	tree := slices.ContainsFunc(p.rows, func(r pickerRow) bool { return r.tree != "" })
	now := m.now()
	for i := from; i < to; i++ {
		r := p.rows[i]
		if r.section != section {
			section = r.section
			b.WriteString("\n" + dimStyle.Render(section))
		}
		b.WriteString("\n" + m.pickerRow(r, i == p.cursor, p.loading == r.item.Number, tree, authorWidth, width, now))
	}
	if to < len(p.rows) || from > 0 {
		b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("%d–%d of %d", from+1, to, len(p.rows))))
	}
	b.WriteString("\n" + dimStyle.Render(ellipsize(iconLegend, width)))
	if o := p.offer; o != nil {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(pendingColor).Render(strings.Join(wrapPlain(o.ask, width), "\n")))
		return b.String()
	}
	hint := "↑↓ move · ⏎ details · r review · l ask · g graph · e e2e · m merge · ⇧⏎ or o browser · esc close"
	b.WriteString("\n" + dimStyle.Render(ellipsize(hint, width)))
	return b.String()
}

const (
	maxAuthorWidth    = 16
	selectedTitleRows = 3
	// minTitleWidth is what the title keeps before the author column is dropped to make room.
	minTitleWidth = 30
)

var (
	numberStyle = dimStyle
	idleStyle   = dimStyle
	authorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#87afd7"))
	titleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#e4e4e4"))
)

// pickerRow lays one PR out in fixed columns; the title takes what is left, and the selected row wraps it in full.
func (m *Model) pickerRow(r pickerRow, selected, loading, tree bool, authorWidth, width int, now time.Time) string {
	bg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(selectionColor)
		}
		return s
	}
	marker := "  "
	if selected {
		marker = "❯ "
	}
	glyph, glyphStyle := statusIcon(r.item.Status)
	if r.section != "mine" {
		glyph, glyphStyle = checksIcon(r.item.PR)
	}
	idle, aStyle, tStyle := prDays(r.item.Idle(now)), authorStyle, titleStyle
	if r.ghost {
		glyph, idle, aStyle, tStyle = " ", "", dimStyle, dimStyle
	}

	mark, markStyle := " ", lipgloss.NewStyle()
	if head, ok := m.picker.reviewed[reviewKey(r.item.Repo, r.item.Number)]; ok {
		mark, markStyle = "R", lipgloss.NewStyle().Foreground(pickerColor).Bold(true)
		if r.item.HeadSHA != "" && head != r.item.HeadSHA {
			markStyle = dimStyle
		}
	}

	sp := bg(lipgloss.NewStyle()).Render(" ")
	prefix := lipgloss.NewStyle().Foreground(pickerColor).Render(marker) +
		bg(glyphStyle).Render(glyph) + bg(markStyle).Render(mark) + sp
	if tree {
		prefix += bg(dimStyle).Render(cmp.Or(r.tree, " ")) + sp
	}
	prefix += bg(numberStyle).Render(fmt.Sprintf("#%-7d", r.item.Number)) + sp +
		bg(idleStyle).Render(fmt.Sprintf("%-5s", idle)) + sp
	if r.section != "mine" && authorWidth > 0 && width-lipgloss.Width(prefix)-authorWidth-1 >= minTitleWidth {
		author := ellipsize(r.item.Author, authorWidth)
		prefix += bg(aStyle).Render(author+strings.Repeat(" ", authorWidth-lipgloss.Width(author))) + sp
	}

	room := width - lipgloss.Width(prefix)
	title := r.item.Title
	if loading {
		title += " — loading…"
	}
	ts := bg(tStyle)
	if selected {
		ts = ts.Bold(true)
	}
	if room < 8 {
		return oneRow(prefix, width)
	}
	if !selected || lipgloss.Width(title) <= room {
		return prefix + ts.Render(ellipsize(title, room))
	}
	lines := wrapPlain(title, room)
	if len(lines) > selectedTitleRows {
		rest := strings.Join(lines[selectedTitleRows-1:], " ")
		lines = append(lines[:selectedTitleRows-1], ellipsize(strings.Join(strings.Fields(rest), " "), room))
	}
	indent := strings.Repeat(" ", width-room)
	out := prefix + ts.Render(lines[0])
	for _, l := range lines[1:] {
		out += "\n" + indent + ts.Render(l)
	}
	return out
}

// ellipsize fits s into width cells, preferring to cut at a word and marking the cut with "…".
func ellipsize(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	cut := string(runes)
	if i := strings.LastIndexByte(cut, ' '); i > len(cut)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ·—-:,") + "…"
}

func prDays(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d < 10*24*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d < 1000*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return "999d+"
}
