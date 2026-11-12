package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
)

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// snapshot records dir's edits into its working-copy commit and returns it; update-stale runs only when jj calls it stale, because run on a fresh workspace it can move the edits off @ and delete them from disk.
func snapshot(ctx context.Context, dir string) (string, error) {
	commit, err := jjStdout(ctx, dir, "log", "-r", "@", "--no-graph", "-T", "commit_id")
	if err == nil || !strings.Contains(err.Error(), "stale") {
		return commit, err
	}
	if _, err := jj(ctx, dir, "workspace", "update-stale"); err != nil {
		return "", err
	}
	return jjStdout(ctx, dir, "log", "-r", "@", "--no-graph", "-T", "commit_id")
}
