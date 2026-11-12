package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/vcs"
)

// defaultDepth is how many commits the default workspace's card shows, its @ included.
const defaultDepth = 1

// EndedSessions names the card of ended sessions' unlanded work that no slot holds any more.
const EndedSessions = "ended sessions"

// RepoView is every workspace of a pooled repo with the changes each holds, and the same as jj's graph.
type RepoView struct {
	Name       string
	Workspaces []WorkspaceView
	// Graph is jj log's own drawing of every card's changes and the trunk commits they fork from, one row a line.
	Graph []GraphRow
	// Trunk is what PR-flow work is measured against; empty in the land flow.
	Trunk string
	// Slug is owner/name of the GitHub repo the PR-flow remote points at, empty when it is not GitHub.
	Slug string
}

// WorkspaceView is one workspace and its changes, newest first: the default's last few, or a slot's since its session began.
type WorkspaceView struct {
	Name    string
	Changes []WorkspaceChange
	// Behind counts trunk's commits the newest change lacks, in the PR flow.
	Behind int
}

// GraphRow is one line of the graph; a node's line names its commit, and its text after Indent bytes is the node's own.
type GraphRow struct {
	Text, Commit string
	Indent       int
}

// graphMark brackets the commit id the graph template puts on each node's line, so it can be read back and cut.
const graphMark = "\x1f"

func graphRows(out string) []GraphRow {
	var rows []GraphRow
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		r := GraphRow{Text: line}
		if i := strings.Index(line, graphMark); i >= 0 {
			if j := strings.Index(line[i+1:], graphMark); j >= 0 {
				r = GraphRow{Text: line[:i] + line[i+1+j+1:], Commit: line[i+1 : i+1+j], Indent: i}
			}
		}
		rows = append(rows, r)
	}
	return rows
}

type WorkspaceChange struct {
	// Commit is the change's commit id, which tells a divergent change's copies apart.
	Commit                       string
	Change, Age, Description     string
	WorkingCopy, Empty, Conflict bool
	// Divergent is a change with more than one visible commit, so it shows once per copy and cannot land.
	Divergent bool
	// Landed is a change already under the default workspace's @.
	Landed bool
	Counts vcs.Counts
	Ship   Ship
}

// View reads the default workspace and every slot, slots in number order, then any unlanded ledger work no slot holds.
func (p *Pool) View(ctx context.Context) (RepoView, error) {
	view := RepoView{Name: filepath.Base(p.Origin)}
	revs := map[string]string{"default": fmt.Sprintf("ancestors(default@, %d)", defaultDepth)}
	names := []string{"default"}
	for _, n := range p.numbers() {
		name := fmt.Sprintf("pool-%d", n)
		rev, err := p.slotRev(name)
		if err != nil {
			return RepoView{}, err
		}
		revs[name] = rev
		names = append(names, name)
	}
	// Every read here is a jj read of a snapshot, so the cards are read side by side.
	pr := p.Prepare.PRFlow()
	cards := make([]WorkspaceView, len(names))
	cardErrs := make([]error, len(names))
	var onRemote map[string]bool
	var slugErr, remoteErr error
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			cards[i].Name = name
			cards[i].Changes, cardErrs[i] = workspaceChanges(ctx, p.Origin, revs[name])
		})
	}
	if pr {
		view.Trunk = p.Prepare.TrunkRev()
		wg.Go(func() { view.Slug, slugErr = githubSlug(ctx, p.Origin, p.Prepare.Remote()) })
		wg.Go(func() { onRemote, remoteErr = remoteBookmarks(ctx, p.Origin, p.Prepare.Remote()) })
	}
	wg.Wait()
	if cardErrs[0] != nil {
		return RepoView{}, cardErrs[0]
	}
	if err := errors.Join(slugErr, remoteErr); err != nil {
		return RepoView{}, err
	}
	var all []string
	for i, name := range names {
		if cardErrs[i] == nil {
			view.Workspaces = append(view.Workspaces, cards[i])
			all = append(all, revs[name])
		}
	}

	var ended *WorkspaceView
	endedRev := ""
	if ids, err := p.ledgerIDs(); err == nil && len(ids) > 0 {
		endedRev = "(" + unlandedRev(ids) + ") ~ (" + strings.Join(all, " | ") + ")"
	}
	shipErrs := make([]error, len(view.Workspaces)+1)
	var graph string
	var graphErr error
	wg.Go(func() {
		rev := strings.Join(all, " | ")
		if endedRev != "" {
			rev += " | " + endedRev
		}
		rev = "(" + rev + ") | parents(roots((" + rev + ") ~ ::default@))"
		graph, graphErr = jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "--color", "never", "-r", rev, "-T",
			`"`+graphMark+`" ++ commit_id ++ "`+graphMark+`" ++ change_id.shortest(8) ++ " " ++ if(working_copies, working_copies ++ " ") ++ if(empty, "(empty) ") ++ if(conflict, "(conflict) ") ++ if(divergent, "(divergent) ") ++ description.first_line() ++ "\n"`)
	})
	if pr {
		for i := range view.Workspaces {
			wg.Go(func() { shipErrs[i] = p.ship(ctx, all[i], &view.Workspaces[i], onRemote) })
		}
	}
	if endedRev != "" {
		wg.Go(func() {
			changes, err := workspaceChanges(ctx, p.Origin, endedRev)
			if err != nil || len(changes) == 0 {
				return
			}
			ended = &WorkspaceView{Name: EndedSessions, Changes: changes}
			if pr {
				shipErrs[len(shipErrs)-1] = p.ship(ctx, endedRev, ended, onRemote)
			}
		})
	}
	wg.Wait()
	if err := errors.Join(append(shipErrs, graphErr)...); err != nil {
		return RepoView{}, err
	}
	if ended != nil {
		view.Workspaces = append(view.Workspaces, *ended)
	}
	view.Graph = graphRows(graph)
	return view, nil
}

func workspaceChanges(ctx context.Context, origin, rev string) ([]WorkspaceChange, error) {
	out, err := jjStdout(ctx, origin, "--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T",
		`commit_id ++ "\t" ++ change_id.shortest(8) ++ "\t" ++ if(working_copies, "wc") ++ "\t" ++ if(empty, "empty") ++ "\t" ++ if(conflict, "conflict") ++ "\t" ++ if(divergent, "divergent") ++ "\t" ++ if(self.contained_in("::default@"), "landed") ++ "\t" ++ committer.timestamp().ago() ++ "\t" ++ `+vcs.CountsTemplate(`\t`)+` ++ "\t" ++ description.first_line() ++ "\n"`)
	if err != nil {
		return nil, err
	}
	var list []WorkspaceChange
	for _, line := range lines(out) {
		f := strings.SplitN(line, "\t", 12)
		if len(f) != 12 {
			continue
		}
		list = append(list, WorkspaceChange{Commit: f[0], Change: f[1], WorkingCopy: f[2] != "", Empty: f[3] != "", Conflict: f[4] != "",
			Divergent: f[5] != "", Landed: f[6] != "", Age: f[7], Counts: vcs.ParseCounts(f[8], f[9], f[10]), Description: f[11]})
	}
	return list, nil
}

// Workspaces is the pool's view, for the workspace pane.
func (l *Lander) Workspaces() (RepoView, error) {
	return l.Pool.View(context.Background())
}

// Sweep is the PR flow's cleanup of merged work, which the pane runs after drawing the cards since it asks GitHub.
func (l *Lander) Sweep() ([]string, map[string]Held, error) {
	return l.Pool.Sweep(context.Background())
}

// Stat is jj show --stat for one change: its header and the files it touches.
func (l *Lander) Stat(change string) (string, error) {
	return jjStdout(context.Background(), l.Pool.Origin, "--ignore-working-copy", "show", "--color", "never", "--stat", "-r", "change_id("+change+")")
}

// Diff is one change's git-format diff.
func (l *Lander) Diff(change string) (string, error) {
	return ChangeDiff(context.Background(), l.Pool.Origin, "change_id("+change+")")
}
