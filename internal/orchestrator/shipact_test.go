package orchestrator

import (
	"strings"
	"testing"
)

// remoteHas is the commit the bare remote's branch points at, empty for none.
func remoteHas(t *testing.T, p *Pool, branch string) string {
	t.Helper()
	must(t, p.Origin, "git", "fetch")
	return must(t, p.Origin, "log", "-r", "present("+branch+"@origin)", "--no-graph", "-T", "commit_id")
}

func TestPushSendsTheStackBelowAndNamesABookmarklessChange(t *testing.T) {
	t.Parallel()
	p, _ := prPool(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "api.txt", "api\n")
	must(t, s.Dir, "commit", "-m", "api")
	must(t, s.Dir, "bookmark", "create", "f1", "-r", "@-")
	write(t, s.Dir, "ui.txt", "ui\n")
	must(t, s.Dir, "describe", "-m", "ui")
	ui := changeAt(t, s.Dir, "@")

	out, err := p.Push(ctx, s.Name, ui)
	if err != nil {
		t.Fatal(err)
	}
	if remoteHas(t, p, "f1") != must(t, s.Dir, "log", "-r", "f1", "--no-graph", "-T", "commit_id") {
		t.Errorf("f1 under the pushed change did not reach the remote: %s", out)
	}
	named := must(t, s.Dir, "log", "-r", "change_id("+ui+")", "--no-graph", "-T", `local_bookmarks.map(|b| b.name()).join(",")`)
	if !strings.HasPrefix(named, "push-") {
		t.Fatalf("the bookmarkless change got bookmark %q, want one jj named push-…", named)
	}
	if remoteHas(t, p, named) == "" {
		t.Errorf("the remote lacks %s after the push", named)
	}
}

func TestRebaseMovesTheSlotsStackOntoTheFetchedTrunk(t *testing.T) {
	t.Parallel()
	p, bare := prPool(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "mine\n")
	must(t, s.Dir, "describe", "-m", "mine")
	pushFromElsewhere(t, bare, "main", "theirs.txt")

	out, err := p.Rebase(ctx, s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if parent := must(t, s.Dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id"); parent != remoteHas(t, p, "main") {
		t.Errorf("after %q the slot's change sits on %s, not on the fetched main", out, parent)
	}
	if again, err := p.Rebase(ctx, s.Name); err != nil || !strings.Contains(again, "already on main@origin") {
		t.Errorf("a second rebase said %q, %v", again, err)
	}
}

func TestRebaseThatWouldConflictIsUndoneAndNamesTheFile(t *testing.T) {
	t.Parallel()
	p, bare := prPool(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "theirs.txt", "mine\n")
	must(t, s.Dir, "describe", "-m", "mine")
	before := must(t, s.Dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	pushFromElsewhere(t, bare, "main", "theirs.txt")

	_, err = p.Rebase(ctx, s.Name)
	if err == nil || !strings.Contains(err.Error(), "theirs.txt") {
		t.Fatalf("a conflicting rebase gave %v, want a refusal naming theirs.txt", err)
	}
	if after := must(t, s.Dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id"); after != before {
		t.Errorf("the refused rebase left the change on %s, want it back on %s", after, before)
	}
	if n := must(t, s.Dir, "log", "-r", "conflicts()", "--no-graph", "-T", `"+"`); n != "" {
		t.Errorf("the refused rebase left conflicts behind")
	}
}
