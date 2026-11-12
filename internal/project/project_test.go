package project

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoOfFollowsAWorkspaceBackToItsMainCheckout(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(base, "Code", "alux")
	if err := os.MkdirAll(filepath.Join(main, ".jj", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(base, "pool", "alux", "pool-2")
	rel, err := filepath.Rel(filepath.Join(slot, ".jj"), filepath.Join(main, ".jj", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(slot, ".jj", "repo"), rel)
	worktree := filepath.Join(base, "wt")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n")

	for _, c := range []struct{ name, path, root, origin string }{
		{"main checkout", filepath.Join(main, "internal", "ui", "ui.go"), main, main},
		{"jj workspace", filepath.Join(slot, "internal", "ui", "new.go"), slot, main},
		{"git worktree", filepath.Join(worktree, "a.go"), worktree, main},
		{"outside any repo", filepath.Join(base, "loose", "notes.md"), "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, origin := RepoOf(c.path)
			if root != c.root || origin != c.origin {
				t.Fatalf("RepoOf = %q, %q; want %q, %q", root, origin, c.root, c.origin)
			}
		})
	}
}
