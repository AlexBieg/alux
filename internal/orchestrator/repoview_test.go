package orchestrator

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestViewShowsASlotsWorkAndWhetherItLanded(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	write(t, s.Dir, "b.txt", "work\n")
	must(t, s.Dir, "describe", "-m", "slot work")
	work := changeAt(t, s.Dir, "@")

	find := func(v RepoView) (WorkspaceChange, bool) {
		for _, w := range v.Workspaces {
			if w.Name != s.Name {
				continue
			}
			for _, c := range w.Changes {
				if strings.HasPrefix(work, c.Change) {
					return c, true
				}
			}
		}
		return WorkspaceChange{}, false
	}
	v, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Workspaces) < 2 || v.Workspaces[0].Name != "default" {
		t.Fatalf("workspaces are %+v, want default first and then the slot", v.Workspaces)
	}
	c, ok := find(v)
	if !ok || !c.WorkingCopy || c.Landed || c.Description != "slot work" {
		t.Fatalf("the slot's card has %+v, want its unlanded working-copy change", c)
	}
	if !strings.Contains(graphText(v), s.Name+"@") {
		t.Errorf("the graph does not tag the slot's working copy:\n%s", graphText(v))
	}
	if strings.Contains(graphText(v), graphMark) {
		t.Errorf("the graph text still holds the commit marks:\n%q", graphText(v))
	}
	if row, ok := graphRowOf(v, c.Commit); !ok || !strings.HasPrefix(row.Text[row.Indent:], c.Change) {
		t.Errorf("no graph row names commit %s with its change id where the node's text starts: %+v", c.Commit, row)
	}

	if _, err := p.Land(ctx, s, work); err != nil {
		t.Fatal(err)
	}
	v, err = p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := find(v); !ok || !c.Landed {
		t.Errorf("after landing the slot's card has %+v, want it marked landed", c)
	}
}

func graphText(v RepoView) string {
	var lines []string
	for _, r := range v.Graph {
		lines = append(lines, r.Text)
	}
	return strings.Join(lines, "\n")
}

func graphRowOf(v RepoView, commit string) (GraphRow, bool) {
	for _, r := range v.Graph {
		if r.Commit == commit {
			return r, true
		}
	}
	return GraphRow{}, false
}

func TestTheGraphJoinsAStackToTheCommitItForksFromBeyondTheDefaultCard(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	write(t, s.Dir, "b.txt", "work\n")
	must(t, s.Dir, "describe", "-m", "slot work")
	fork := must(t, s.Dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	for i := range defaultDepth + 2 {
		write(t, p.Origin, "a.txt", fmt.Sprintf("%d\n", i))
		must(t, p.Origin, "commit", "-m", fmt.Sprintf("default %d", i))
	}

	v, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := graphRowOf(v, fork); !ok {
		t.Errorf("the graph leaves out %s, the commit the slot's stack forks from:\n%s", fork, graphText(v))
	}
}

func TestViewKeepsAnEndedSessionsUnlandedWorkAfterItsSlotMovesOn(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	work := changeAt(t, s.Dir, "@")
	p.Release(ctx, s)
	next, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, next)
	if next.Name != s.Name {
		t.Fatalf("the next session took %s, want the freed %s", next.Name, s.Name)
	}

	ended := func(v RepoView) []WorkspaceChange {
		var out []WorkspaceChange
		for _, w := range v.Workspaces {
			for _, c := range w.Changes {
				if strings.HasPrefix(work, c.Change) {
					if w.Name != EndedSessions {
						t.Fatalf("the change shows in %s's card, want it only under %s", w.Name, EndedSessions)
					}
					out = append(out, c)
				}
			}
		}
		return out
	}
	v, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ended(v); len(got) != 1 || got[0].Description != "session work" || got[0].Landed {
		t.Fatalf("ended sessions shows %+v, want the unlanded change %s once", got, work)
	}
	if !strings.Contains(graphText(v), "session work") {
		t.Errorf("the graph leaves out the ended session's work:\n%s", graphText(v))
	}

	if _, err := p.Land(ctx, nil, work); err != nil {
		t.Fatal(err)
	}
	v, err = p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range v.Workspaces {
		if w.Name == EndedSessions {
			t.Errorf("after landing there is still an %s card: %+v", EndedSessions, w.Changes)
		}
	}
}

func TestADivergentEndedChangeShowsEachCopyMarkedAndRefusesToLand(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	work := changeAt(t, s.Dir, "@")
	p.Release(ctx, s)
	next, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, next)
	op := must(t, p.Origin, "op", "log", "-n1", "--no-graph", "-T", "id")
	must(t, p.Origin, "--ignore-working-copy", "describe", "-r", work, "-m", "copy a")
	must(t, p.Origin, "--ignore-working-copy", "--at-op", op, "describe", "-r", work, "-m", "copy b")

	v, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var copies []string
	for _, w := range v.Workspaces {
		for _, c := range w.Changes {
			if strings.HasPrefix(work, c.Change) {
				if !c.Divergent {
					t.Errorf("%s's row %+v is not marked divergent", w.Name, c)
				}
				copies = append(copies, c.Description)
			}
		}
	}
	if len(copies) != 2 {
		t.Fatalf("the pane shows %q for the divergent change, want both copies", copies)
	}

	_, err = p.Land(ctx, nil, work)
	var div *DivergentError
	if !errors.As(err, &div) || len(div.Commits) != 2 {
		t.Fatalf("landing a divergent change: %v, want a DivergentError naming both commits", err)
	}
	if out := must(t, p.Origin, "--ignore-working-copy", "log", "-r", "change_id("+work+") & ::default@", "--no-graph", "-T", "commit_id"); out != "" {
		t.Errorf("a refused landing still moved %s under default@", out)
	}
}

func TestASlotRebasedOntoNewerTrunkShowsAndRecordsOnlyItsOwnWork(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	write(t, s.Dir, "trunk.txt", "merged meanwhile\n")
	must(t, s.Dir, "commit", "-m", "trunk work")
	must(t, s.Dir, "bookmark", "create", "main", "-r", "@-")
	must(t, s.Dir, "config", "set", "--repo", `revset-aliases."immutable_heads()"`, "main")
	write(t, s.Dir, "b.txt", "work\n")
	must(t, s.Dir, "describe", "-m", "slot work")
	work, trunk := changeAt(t, s.Dir, "@"), changeAt(t, s.Dir, "main")

	v, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var card []string
	for _, w := range v.Workspaces {
		if w.Name == s.Name {
			for _, c := range w.Changes {
				card = append(card, c.Description)
			}
		}
	}
	if len(card) != 1 || card[0] != "slot work" {
		t.Errorf("the slot's card holds %q, want only its own work", card)
	}
	got, err := sessionChanges(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != work {
		t.Errorf("the session's changes are %q, want %s and not trunk's %s", got, work, trunk)
	}
}
