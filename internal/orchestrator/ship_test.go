package orchestrator

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Other", "-c", "user.email=other@example.com"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// pushFromElsewhere commits file on top of branch in a separate git clone and pushes it, as a teammate would.
func pushFromElsewhere(t *testing.T, bare, branch, file string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "other")
	gitIn(t, filepath.Dir(clone), "clone", "-q", "-b", branch, bare, clone)
	write(t, clone, file, "theirs\n")
	gitIn(t, clone, "add", file)
	gitIn(t, clone, "commit", "-qm", "from elsewhere on "+branch)
	gitIn(t, clone, "push", "-q", "origin", branch)
}

func shipOf(t *testing.T, w WorkspaceView, desc string) Ship {
	t.Helper()
	for _, c := range w.Changes {
		if c.Description == desc {
			return c.Ship
		}
	}
	t.Fatalf("%s has no change %q: %+v", w.Name, desc, w.Changes)
	return Ship{}
}

// prPool is a PR-flow pool whose repo has a bare git remote holding main at the default workspace's described commit.
func prPool(t *testing.T) (*Pool, string) {
	t.Helper()
	p, _ := poolRepo(t)
	p.Prepare = &Prepare{Pool: true, Flow: FlowPR}
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	must(t, p.Origin, "git", "remote", "add", "origin", bare)
	must(t, p.Origin, "bookmark", "create", "main", "-r", "@-")
	must(t, p.Origin, "git", "push", "-b", "main")
	return p, bare
}

func TestAPRFlowCardSaysWhatEachChangeLacksAgainstTheRemoteAndTrunk(t *testing.T) {
	t.Parallel()
	p, bare := prPool(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ file, desc, bookmark string }{{"api.txt", "api", "f1"}, {"ui.txt", "ui", "f2"}, {"docs.txt", "docs", "f3"}} {
		write(t, s.Dir, c.file, c.desc+"\n")
		must(t, s.Dir, "commit", "-m", c.desc)
		must(t, s.Dir, "bookmark", "create", c.bookmark, "-r", "@-")
	}
	must(t, s.Dir, "git", "push", "-b", "f1", "-b", "f2", "-b", "f3")
	write(t, s.Dir, "wip.txt", "wip\n")
	must(t, s.Dir, "describe", "-m", "wip")
	must(t, s.Dir, "bookmark", "create", "f4", "-r", "@")
	must(t, s.Dir, "describe", "-r", "f2", "-m", "ui v2")

	pushFromElsewhere(t, bare, "f1", "theirs-on-api.txt")
	pushFromElsewhere(t, bare, "f3", "theirs-on-docs.txt")
	pushFromElsewhere(t, bare, "main", "theirs-on-main.txt")
	must(t, p.Origin, "git", "fetch")

	view, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Trunk != "main@origin" {
		t.Errorf("trunk is %q, want main@origin", view.Trunk)
	}
	var w WorkspaceView
	for _, ws := range view.Workspaces {
		if ws.Name == s.Name {
			w = ws
		}
	}
	if w.Behind != 1 {
		t.Errorf("the stack is %d behind trunk, want 1 for the commit pushed to main from elsewhere", w.Behind)
	}
	if got := shipOf(t, w, "wip"); got.Bookmark != "f4" || !got.NotOnRemote || got.Pushed {
		t.Errorf("the never-pushed bookmark reads %+v", got)
	}
	if got := shipOf(t, w, "docs"); got.Bookmark != "f3" || !got.BookmarkConflict {
		t.Errorf("the bookmark moved both here and on the remote reads %+v, want a bookmark conflict", got)
	}
	if got := shipOf(t, w, "ui v2"); got.Bookmark != "f2" || got.Ahead != 1 || got.BookmarkConflict || got.Moved != "" {
		t.Errorf("the amended pushed change reads %+v, want 1 ahead and nothing from the remote", got)
	}
	if got := shipOf(t, w, "api"); got.Bookmark != "" || got.Moved != "f1" || got.MovedBy != 1 || !got.Pushed {
		t.Errorf("the change whose bookmark a fetch moved onto someone else's commit reads %+v", got)
	}
	for _, c := range view.Workspaces[0].Changes {
		if c.Description == "base work" && !c.Ship.OnTrunk {
			t.Errorf("the default card's trunk commit is not marked on trunk: %+v", c)
		}
	}
}

func TestGithubSlugReadsTheRemotesURLInEitherForm(t *testing.T) {
	t.Parallel()
	dir := mainRepo(t)
	must(t, dir, "git", "remote", "add", "origin", "git@github.com:acme/app.git")
	must(t, dir, "git", "remote", "add", "fork", "https://github.com/alex/app")
	must(t, dir, "git", "remote", "add", "gitlab", "https://gitlab.com/x/y.git")
	for remote, want := range map[string]string{"origin": "acme/app", "fork": "alex/app", "gitlab": "", "missing": ""} {
		if got, err := githubSlug(ctx, dir, remote); got != want || err != nil {
			t.Errorf("%s reads as %q, %v; want %q", remote, got, err, want)
		}
	}
}

func TestALandFlowViewCarriesNoShipState(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "x\n")
	must(t, s.Dir, "describe", "-m", "work")
	view, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Trunk != "" {
		t.Errorf("a land-flow view has trunk %q", view.Trunk)
	}
	for _, w := range view.Workspaces {
		for _, c := range w.Changes {
			if c.Ship != (Ship{}) {
				t.Errorf("%s in %s carries ship state %+v", c.Change, w.Name, c.Ship)
			}
		}
	}
}
