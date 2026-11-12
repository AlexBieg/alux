package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/prs"
)

// squashOnMain lands files on the remote's main as one new commit from a separate clone, as GitHub's squash merge does, and returns it.
func squashOnMain(t *testing.T, bare string, files map[string]string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "github")
	gitIn(t, filepath.Dir(clone), "clone", "-q", "-b", "main", bare, clone)
	for name, body := range files {
		write(t, clone, name, body)
		gitIn(t, clone, "add", name)
	}
	gitIn(t, clone, "commit", "-qm", "squashed (#1)")
	gitIn(t, clone, "push", "-q", "origin", "main")
	out, err := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// mergedStack is a slot holding api (bookmark f1, pushed) under ui, both described, with origin named as GitHub's acme/app.
func mergedStack(t *testing.T) (p *Pool, bare string, s *Slot, api, ui string) {
	t.Helper()
	p, bare = prPool(t)
	bare = githubRemote(t, p, bare)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "api.txt", "api\n")
	must(t, s.Dir, "commit", "-m", "api")
	must(t, s.Dir, "bookmark", "create", "f1", "-r", "@-")
	must(t, s.Dir, "git", "push", "-b", "f1")
	write(t, s.Dir, "ui.txt", "ui\n")
	must(t, s.Dir, "describe", "-m", "ui")
	return p, bare, s, changeAt(t, s.Dir, "@-"), changeAt(t, s.Dir, "@")
}

func commitAt(t *testing.T, dir, rev string) string {
	t.Helper()
	return must(t, dir, "log", "-r", rev, "--no-graph", "-T", "commit_id")
}

func shortAt(t *testing.T, p *Pool, change string) string {
	t.Helper()
	return must(t, p.Origin, "log", "-r", "change_id("+change+")", "--no-graph", "-T", "change_id.shortest(8)")
}

func lookup(found map[string][]prs.MergedPR) func(context.Context, string, []string) (map[string][]prs.MergedPR, error) {
	return func(context.Context, string, []string) (map[string][]prs.MergedPR, error) { return found, nil }
}

// githubRemote moves the bare remote to a path ending github.com/acme/app.git, which names the repo as GitHub's URL would.
func githubRemote(t *testing.T, p *Pool, bare string) string {
	t.Helper()
	moved := filepath.Join(t.TempDir(), "github.com", "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bare, moved); err != nil {
		t.Fatal(err)
	}
	must(t, p.Origin, "git", "remote", "set-url", "origin", moved)
	return moved
}

func TestSweepAbandonsASquashMergedChangeAndMovesWhatIsOnItOntoTrunk(t *testing.T) {
	t.Parallel()
	p, bare, s, api, ui := mergedStack(t)
	head := commitAt(t, s.Dir, "change_id("+api+")")
	squash := squashOnMain(t, bare, map[string]string{"api.txt": "api\n"})
	must(t, p.Origin, "git", "fetch")
	p.Merged = lookup(map[string][]prs.MergedPR{"f1": {{Number: 1, HeadSHA: head, Base: "main", MergeSHA: squash}}})

	said, held, err := p.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(said) != 1 || !strings.Contains(said[0], "(f1), merged into main") || len(held) != 0 {
		t.Fatalf("the sweep said %v and held %v", said, held)
	}
	if must(t, s.Dir, "log", "-r", "present(change_id("+api+"))", "--no-graph", "-T", `"+"`) != "" {
		t.Error("the merged change is still visible")
	}
	if parent := commitAt(t, s.Dir, "change_id("+ui+")-"); parent != squash {
		t.Errorf("ui sits on %s, want the squash commit %s", parent, squash)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "ui.txt")); err != nil {
		t.Errorf("the slot lost ui's file after the sweep: %v", err)
	}
	if must(t, s.Dir, "log", "-r", "bookmarks(exact:f1)", "--no-graph", "-T", `"+"`) != "" {
		t.Error("the local f1 bookmark survived")
	}
	gitIn(t, filepath.Dir(bare), "--git-dir", bare, "rev-parse", "--verify", "-q", "refs/heads/f1")
}

func TestSweepHoldsBackEveryMergedChangeItCannotBeSureOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pr   func(head, squash string) prs.MergedPR
		why  string
	}{
		{"amended after its last push", func(_, squash string) prs.MergedPR {
			return prs.MergedPR{Number: 1, HeadSHA: "0123456789abcdef0123456789abcdef01234567", Base: "main", MergeSHA: squash}
		}, "differs from the one that merged"},
		{"merge not fetched yet", func(head, _ string) prs.MergedPR {
			return prs.MergedPR{Number: 1, HeadSHA: head, Base: "main", MergeSHA: "89abcdef0123456789abcdef0123456789abcdef"}
		}, "not in your main yet"},
		{"merged into another branch", func(head, squash string) prs.MergedPR {
			return prs.MergedPR{Number: 1, HeadSHA: head, Base: "abc-1-api", MergeSHA: squash}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, bare, s, api, _ := mergedStack(t)
			head := commitAt(t, s.Dir, "change_id("+api+")")
			squash := squashOnMain(t, bare, map[string]string{"api.txt": "api\n"})
			must(t, p.Origin, "git", "fetch")
			p.Merged = lookup(map[string][]prs.MergedPR{"f1": {c.pr(head, squash)}})

			said, held, err := p.Sweep(ctx)
			if err != nil || len(said) != 0 {
				t.Fatalf("the sweep said %v, %v; want nothing abandoned", said, err)
			}
			if commitAt(t, s.Dir, "change_id("+api+")") != head {
				t.Error("the held change was rewritten")
			}
			got, ok := held[shortAt(t, p, api)]
			if ok != (c.why != "") || !strings.Contains(got.Why, c.why) {
				t.Errorf("held %+v, want a reason containing %q", held, c.why)
			}
		})
	}
}

func TestSweepKeepsAMergedChangeWhoseWorkBelowHasNotMerged(t *testing.T) {
	t.Parallel()
	p, bare, s, _, ui := mergedStack(t)
	must(t, s.Dir, "bookmark", "create", "f2", "-r", "@")
	must(t, s.Dir, "git", "push", "-b", "f2")
	head := commitAt(t, s.Dir, "change_id("+ui+")")
	squash := squashOnMain(t, bare, map[string]string{"ui.txt": "ui\n"})
	must(t, p.Origin, "git", "fetch")
	p.Merged = lookup(map[string][]prs.MergedPR{"f2": {{Number: 2, HeadSHA: head, Base: "main", MergeSHA: squash}}})

	said, held, err := p.Sweep(ctx)
	if err != nil || len(said) != 0 {
		t.Fatalf("the sweep said %v, %v; want ui kept while api under it is unmerged", said, err)
	}
	if !strings.Contains(held[shortAt(t, p, ui)].Why, "below it has not merged") {
		t.Errorf("held is %+v", held)
	}
}

func TestSweepThatWouldConflictIsUndone(t *testing.T) {
	t.Parallel()
	p, bare, s, api, ui := mergedStack(t)
	write(t, s.Dir, "api.txt", "api changed on top\n")
	head := commitAt(t, s.Dir, "change_id("+api+")")
	squash := squashOnMain(t, bare, map[string]string{"api.txt": "api reworded in review\n"})
	must(t, p.Origin, "git", "fetch")
	p.Merged = lookup(map[string][]prs.MergedPR{"f1": {{Number: 1, HeadSHA: head, Base: "main", MergeSHA: squash}}})
	beforeUI := commitAt(t, s.Dir, "change_id("+ui+")")

	if _, _, err := p.Sweep(ctx); err == nil || !strings.Contains(err.Error(), "would conflict, so it was undone") {
		t.Fatalf("a conflicting sweep gave %v", err)
	}
	if commitAt(t, s.Dir, "change_id("+api+")") != head || commitAt(t, s.Dir, "change_id("+ui+")") != beforeUI {
		t.Error("the undone sweep left the stack rewritten")
	}
}
