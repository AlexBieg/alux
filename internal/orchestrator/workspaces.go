package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type JJWorkspaces struct {
	Repo string
}

func (w JJWorkspaces) prune(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(w.Repo, ".git")); err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "prune")
	cmd.Dir = w.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree prune in %s: %w\n%s", w.Repo, err, tail(string(out), outputCap))
	}
	return nil
}

func (w JJWorkspaces) registered(ctx context.Context, name string) (bool, error) {
	out, err := jj(ctx, w.Repo, "--ignore-working-copy", "workspace", "list", "-T", `name ++ "\n"`)
	if err != nil {
		return false, err
	}
	return slices.Contains(strings.Split(out, "\n"), name), nil
}
