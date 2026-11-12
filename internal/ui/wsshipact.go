package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/orchestrator"
)

type wsActionMsg struct {
	verb, text string
	err        error
}

// selectedStack is the selected change's card and its place in it.
func (p *workspacePane) selectedStack() (wsPick, []orchestrator.WorkspaceChange, int, bool) {
	pick, ok := p.selected()
	if !ok {
		return wsPick{}, nil, 0, false
	}
	for _, w := range p.view.Workspaces {
		if w.Name != pick.workspace {
			continue
		}
		if i := slices.IndexFunc(w.Changes, func(c orchestrator.WorkspaceChange) bool { return c.Change == pick.change.Change }); i >= 0 {
			return pick, w.Changes, i, true
		}
	}
	return wsPick{}, nil, 0, false
}

// remoteTrouble is why changes cannot go to the remote as they stand, empty when they can.
func remoteTrouble(changes []orchestrator.WorkspaceChange) string {
	for _, c := range changes {
		switch s := c.Ship; {
		case s.Moved != "":
			return fmt.Sprintf("%s has someone else's commits on top of %s; bring them into the stack with jj rebase first", s.Moved, c.Change)
		case s.BookmarkConflict:
			return fmt.Sprintf("%s moved both here and on the remote; settle it with jj bookmark set first", s.Bookmark)
		}
	}
	return ""
}

// shipAction runs p, u or o on the selected change, or says why it cannot.
func (m *Model) shipAction(key string) tea.Cmd {
	p := m.wsPane
	pick, card, i, ok := p.selectedStack()
	if !ok {
		return nil
	}
	if p.busy != "" {
		return m.Notify("still " + p.busy + "; wait for it to finish")
	}
	c, trunk := pick.change, trunkName(p.view.Trunk)
	switch key {
	case "p":
		switch {
		case c.Ship.OnTrunk:
			return m.Notify(c.Change + " is already on " + trunk)
		case c.Description == "":
			return m.Notify(c.Change + " needs a description before it can be pushed")
		case remoteTrouble(card[i:]) != "":
			return m.Notify("not pushed: " + remoteTrouble(card[i:]))
		}
		src, change := m.Land, c.Change
		return m.runShip("pushing "+change, func() (string, error) { return src.Push(pick.workspace, change) })
	case "u":
		if why := remoteTrouble(card); why != "" {
			return m.Notify("not rebased: " + why)
		}
		src, ws := m.Land, pick.workspace
		return m.runShip("fetching and rebasing "+ws, func() (string, error) { return src.Rebase(ws) })
	}
	if pr := m.prsByHead(p.view.Slug)[c.Ship.Bookmark]; c.Ship.Bookmark != "" && pr != nil {
		return m.Notify(fmt.Sprintf("#%d is already open for %s", pr.Number, c.Ship.Bookmark))
	}
	if s := c.Ship; s.OnTrunk || s.Bookmark == "" || s.NotOnRemote || s.Ahead > 0 || s.BookmarkConflict {
		return m.Notify(c.Change + " needs its bookmark pushed before a PR can open; press p first")
	}
	base := stackBase(card[i+1:], trunk)
	m.closePanel()
	m.follow = true
	m.session.Submit(fmt.Sprintf("Open a draft PR with pr_create for bookmark `%s` (change %s, %q), with base `%s`.",
		c.Ship.Bookmark, c.Change, c.Description, base))
	m.layout()
	return nil
}

func (m *Model) runShip(verb string, run func() (string, error)) tea.Cmd {
	m.wsPane.busy = verb
	m.layout()
	return func() tea.Msg {
		text, err := run()
		return wsActionMsg{verb: verb, text: text, err: err}
	}
}

// onWsAction says how a push or rebase went and rereads the workspaces, whose chips it has changed.
func (m *Model) onWsAction(msg wsActionMsg) tea.Cmd {
	if m.wsPane != nil {
		m.wsPane.busy = ""
		m.layout()
	}
	say := msg.text
	if msg.err != nil {
		say = msg.verb + " failed: " + msg.err.Error()
	}
	var rows []string
	for _, l := range strings.Split(strings.TrimSpace(say), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			rows = append(rows, l)
		}
	}
	return tea.Batch(m.Notify(strings.Join(rows, " · ")), m.readWorkspaces(false))
}
