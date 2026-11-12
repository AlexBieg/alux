package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/charmbracelet/x/ansi"
)

// chip is one fact on a workspace row, styled only once it is placed so a wrap never splits a style.
type chip struct {
	text string
	st   lipgloss.Style
}

// shipRow is one change of a workspace card with what the row needs from around it.
type shipRow struct {
	change orchestrator.WorkspaceChange
	// pos is the change's place in a card of n, newest first.
	pos, n int
	// prFlow picks the PR flow's chips over the land flow's.
	prFlow bool
	// going marks a change that lands along with the selected one.
	going bool
	// workspace leads the row's chips in its colour, where nothing else says which workspace holds the change.
	workspace string
	// own is a change in this session's workspace, drawn with a bold id.
	own bool
	// pr is the open PR whose head is the change's bookmark, nil for none.
	pr *prs.PR
	// base is the branch the PR should merge into: the next bookmark down the stack, else trunk.
	base string
	// held is why the cleanup kept this change though its PR merged, zero when it did not.
	held orchestrator.Held
}

// prsByHead is every open PR the poller holds for the GitHub repo slug, by head branch.
func (m *Model) prsByHead(slug string) map[string]*prs.PR {
	out := map[string]*prs.PR{}
	if slug == "" {
		return out
	}
	for _, s := range m.prStates {
		if !strings.EqualFold(s.Repo, slug) {
			continue
		}
		for _, list := range [][]prs.Item{s.Queue.Review, s.Queue.Mine} {
			for _, it := range list {
				if it.Head != "" {
					out[it.Head] = &it.PR
				}
			}
		}
	}
	return out
}

// stackBase is the branch a PR should merge into given the changes below it, newest first: the nearest bookmark, else trunk.
func stackBase(below []orchestrator.WorkspaceChange, trunk string) string {
	for _, c := range below {
		if c.Ship.Bookmark != "" && !c.Ship.OnTrunk {
			return c.Ship.Bookmark
		}
	}
	return trunk
}

// wsRow is one change of a card: its place on the stack's rail, its PR number or change id and wrapped title, then where it stands on lines of its own.
func (p *workspacePane) wsRow(r shipRow, selected bool, width int) []string {
	c, pos, n := r.change, r.pos, r.n
	land := lipgloss.NewStyle().Foreground(landColor)
	_, landable := p.landableAs(c)
	if r.prFlow {
		landable = false
	}
	marker := "  "
	switch {
	case selected:
		marker = land.Render("❯ ")
	case r.going:
		marker = land.Render("│ ")
	}
	rail, down := "┌", "│"
	switch {
	case n == 1:
		rail, down = "─", " "
	case pos == n-1:
		rail, down = "└", " "
	case pos > 0:
		rail = "├"
	}
	glyph := dimStyle.Render("·")
	if c.WorkingCopy {
		glyph = lipgloss.NewStyle().Foreground(userColor).Render("@")
	}
	idStyle := plainStyle
	switch {
	case c.Ship.OnTrunk || c.Landed:
		idStyle = dimStyle
	case landable:
		idStyle = land
	}
	idStyle = idStyle.Bold(r.own)
	id := c.Change
	if r.pr != nil {
		id = fmt.Sprintf("#%d", r.pr.Number)
	}
	prefix := marker + dimStyle.Render(rail) + " " + glyph + " " + idStyle.Render(fmt.Sprintf("%-8s", id)) + " "
	indent := "  " + dimStyle.Render(down) + strings.Repeat(" ", ansi.StringWidth(prefix)-3)
	room := width - ansi.StringWidth(prefix)

	desc, ds := c.Description, plainStyle
	if desc == "" {
		desc, ds = "(no description)", dimStyle
	}
	switch {
	case selected:
		ds = ds.Bold(true)
	case r.going:
		ds = ds.Foreground(landColor)
	}
	var rows []string
	if room < 12 {
		indent, room = "    ", max(1, width-4)
		rows = append(rows, oneRow(prefix, width))
		for _, l := range styledWrap(desc, ds, room) {
			rows = append(rows, indent+l)
		}
	} else {
		for i, l := range styledWrap(desc, ds, room) {
			lead := indent
			if i == 0 {
				lead = prefix
			}
			rows = append(rows, lead+l)
		}
	}
	for _, l := range p.rowFacts(r, selected, room) {
		rows = append(rows, indent+l)
	}
	return rows
}

// rowFacts is where a row's change stands, as chips wrapped to room, and for the selected landable one what lands with it.
func (p *workspacePane) rowFacts(r shipRow, selected bool, room int) []string {
	u, landable := p.landableAs(r.change)
	if r.prFlow {
		landable = false
	}
	chips := landChips(u, landable, r.change)
	if r.prFlow {
		chips = shipChips(r, trunkName(p.view.Trunk))
	}
	if r.workspace != "" {
		name, st := r.workspace, lipgloss.NewStyle().Foreground(workspaceColor(r.workspace))
		if r.own {
			name, st = name+" (this)", st.Bold(true)
		}
		chips = append([]chip{{name, st}}, chips...)
	}
	rows := wrapChips(chips, room)
	if landable && selected && len(u.With) > 0 {
		say := fmt.Sprintf("⏎ also lands %s: %s", plural(len(u.With), "change"), strings.Join(u.With, ", "))
		rows = append(rows, styledWrap(say, lipgloss.NewStyle().Foreground(pendingColor), room)...)
	}
	return rows
}

// landChips says whether a change can land, and with how many others, or already has, then the facts every row shows.
func landChips(u orchestrator.Unlanded, landable bool, c orchestrator.WorkspaceChange) []chip {
	var chips []chip
	if landable {
		tag := landGlyph + " land"
		if len(u.With) > 0 {
			tag += fmt.Sprintf(" +%d", len(u.With))
		}
		chips = append(chips, chip{tag, lipgloss.NewStyle().Foreground(landColor)})
	}
	if c.Landed {
		chips = append(chips, chip{"landed", dimStyle})
	}
	return append(chips, changeChips(c)...)
}

// changeChips are the facts every row shows in either flow: conflict, divergence, emptiness, size and age.
func changeChips(c orchestrator.WorkspaceChange) []chip {
	var chips []chip
	if c.Conflict {
		chips = append(chips, chip{"conflict", errorStyle})
	}
	if c.Divergent {
		chips = append(chips, chip{"divergent", errorStyle})
	}
	if c.Empty {
		chips = append(chips, chip{"empty", dimStyle})
	}
	if counts := c.Counts.Render(); counts != "" {
		chips = append(chips, chip{text: counts})
	}
	return append(chips, chip{c.Age, dimStyle})
}

// shipChips says where a change stands: its PR or bookmark, whether the remote has it, what moved past it, then the facts every row shows and, with a PR, its change id.
func shipChips(r shipRow, trunk string) []chip {
	c, s := r.change, r.change.Ship
	if s.OnTrunk {
		return []chip{{"on " + trunk, dimStyle}, {c.Age, dimStyle}}
	}
	pending, ready := lipgloss.NewStyle().Foreground(pendingColor), lipgloss.NewStyle().Foreground(readyColor)
	var chips []chip
	if r.pr != nil {
		chips = append(chips, prChips(r.pr, r.base)...)
	}
	switch {
	case r.pr != nil && s.BookmarkConflict:
		chips = append(chips, chip{"bookmark conflict", errorStyle})
	case r.pr != nil && s.Ahead > 0:
		chips = append(chips, chip{fmt.Sprintf("↑%d unpushed", s.Ahead), pending})
	case r.pr != nil:
	case s.Bookmark == "" && s.Pushed:
		chips = append(chips, chip{"pushed", dimStyle})
	case s.Bookmark == "":
		chips = append(chips, chip{"local only", dimStyle})
	case s.BookmarkConflict:
		chips = append(chips, chip{s.Bookmark, plainStyle}, chip{"bookmark conflict", errorStyle})
	case s.NotOnRemote:
		chips = append(chips, chip{s.Bookmark, plainStyle}, chip{"↑ not pushed", pending})
	case s.Ahead > 0:
		chips = append(chips, chip{s.Bookmark, plainStyle}, chip{fmt.Sprintf("↑%d unpushed", s.Ahead), pending})
	default:
		chips = append(chips, chip{s.Bookmark, plainStyle}, chip{"✓ pushed", ready})
	}
	if h := r.held; h.PR > 0 {
		chips = append(chips, chip{fmt.Sprintf("#%d merged, kept: %s", h.PR, h.Why), pending})
	} else if h.Why != "" {
		chips = append(chips, chip{"kept: " + h.Why, pending})
	}
	if s.Moved != "" {
		chips = append(chips, chip{fmt.Sprintf("%s moved ↓%d past this", s.Moved, s.MovedBy), errorStyle})
	}
	chips = append(chips, changeChips(c)...)
	if r.pr != nil {
		chips = append(chips, chip{c.Change, dimStyle})
	}
	return chips
}

// prChips is the PR's own state: draft or ready, review, CI, conflicts, open threads, and a base that is not where the stack says.
func prChips(pr *prs.PR, base string) []chip {
	pending, ready := lipgloss.NewStyle().Foreground(pendingColor), lipgloss.NewStyle().Foreground(readyColor)
	chips := []chip{{"ready", plainStyle}}
	if pr.Draft {
		chips[0] = chip{"draft", dimStyle}
	}
	switch pr.ReviewDecision {
	case "APPROVED":
		chips = append(chips, chip{"approved", ready})
	case "CHANGES_REQUESTED":
		chips = append(chips, chip{"changes requested", errorStyle})
	}
	switch {
	case pr.Red():
		chips = append(chips, chip{"CI ✗", errorStyle})
	case pr.Checks == "PENDING" || pr.Checks == "EXPECTED":
		chips = append(chips, chip{"CI …", pending})
	case pr.Checks == "SUCCESS":
		chips = append(chips, chip{"CI ✓", ready})
	}
	if pr.Mergeable == "CONFLICTING" {
		chips = append(chips, chip{"merge conflict", errorStyle})
	}
	if pr.Unresolved > 0 {
		chips = append(chips, chip{plural(pr.Unresolved, "thread"), pending})
	}
	if pr.Base != "" && base != "" && pr.Base != base {
		chips = append(chips, chip{"base " + pr.Base + ", stack says " + base, errorStyle})
	}
	return chips
}

// wrapChips joins chips with dots, breaking only between them, and wraps a chip wider than room on rows of its own.
func wrapChips(chips []chip, room int) []string {
	sep := dimStyle.Render(" · ")
	var rows []string
	line, w := "", 0
	for _, c := range chips {
		cw := ansi.StringWidth(c.text)
		switch {
		case cw > room:
			if line != "" {
				rows = append(rows, line)
			}
			parts := styledWrap(c.text, c.st, room)
			rows = append(rows, parts[:len(parts)-1]...)
			line, w = parts[len(parts)-1], ansi.StringWidth(parts[len(parts)-1])
		case line == "":
			line, w = c.st.Render(c.text), cw
		case w+3+cw <= room:
			line, w = line+sep+c.st.Render(c.text), w+3+cw
		default:
			rows = append(rows, line)
			line, w = c.st.Render(c.text), cw
		}
	}
	if line != "" {
		rows = append(rows, line)
	}
	return rows
}

// trunkName is trunk as a person names it: main for main@origin.
func trunkName(trunk string) string {
	if i := strings.LastIndex(trunk, "@"); i > 0 {
		return trunk[:i]
	}
	return trunk
}
