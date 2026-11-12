package vcs

import (
	"errors"
	"os"
	"strings"
)

// Mark is the working copy as Snapshot found it, for Diff to compare against.
type Mark struct {
	dir, tool, rev string
}

// Snapshot records the working copy under dir, or reports false outside a jj or git repository.
// It snapshots jj's working copy, so call it only between agent steps; see Read.
func Snapshot(dir string) (Mark, bool) {
	if dir == "" {
		return Mark{}, false
	}
	if out, err := command(dir, nil, "jj", "log", "-r", "@", "--no-graph", "-T", "commit_id"); err == nil {
		if rev := strings.TrimSpace(out); rev != "" {
			return Mark{dir: dir, tool: "jj", rev: rev}, true
		}
	}
	if tree, err := gitTree(dir); err == nil && tree != "" {
		return Mark{dir: dir, tool: "git", rev: tree}, true
	}
	return Mark{}, false
}

// Diff is every change since the mark, as a git-format patch, new files included.
func (m Mark) Diff() (string, error) {
	switch m.tool {
	case "jj":
		return command(m.dir, nil, "jj", "diff", "--git", "--from", m.rev)
	case "git":
		tree, err := gitTree(m.dir)
		if err != nil {
			return "", err
		}
		return command(m.dir, nil, "git", "diff", m.rev, tree)
	}
	return "", errors.New("no repository was snapshotted")
}

// gitTree writes the working tree, untracked files included, through a throwaway index so the
// user's own index is never touched.
func gitTree(dir string) (string, error) {
	f, err := os.CreateTemp("", "alux-index-*")
	if err != nil {
		return "", err
	}
	index := f.Name()
	f.Close()
	// git refuses an empty file as an index, so it has to create the file itself.
	os.Remove(index)
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := command(dir, env, "git", "add", "-A"); err != nil {
		return "", err
	}
	out, err := command(dir, env, "git", "write-tree")
	return strings.TrimSpace(out), err
}
