package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceDir is where jj runs for a card's action, so a live slot's latest edits are snapshotted first.
func (p *Pool) workspaceDir(workspace string) (string, error) {
	switch {
	case workspace == "default" || workspace == EndedSessions:
		return p.Origin, nil
	case slotName.MatchString(workspace):
		return filepath.Join(p.Root, workspace), nil
	}
	return "", fmt.Errorf("%q is not a workspace of this pool", workspace)
}

// slotRev is the slot's own changes since its session began, as its card shows them.
func (p *Pool) slotRev(name string) (string, error) {
	if !slotName.MatchString(name) {
		return "", fmt.Errorf("only a pool workspace's stack can be rebased; %s is not one", name)
	}
	rev := "present(" + name + "@)"
	if base, err := os.ReadFile(filepath.Join(p.Root, name, baseMark)); err == nil && len(base) > 0 {
		rev = "((" + string(base) + ".." + rev + ") ~ immutable())"
	}
	return rev, nil
}

// Push pushes every bookmark from change down to trunk, and gives change a bookmark of jj's naming when it has none.
func (p *Pool) Push(ctx context.Context, workspace, change string) (string, error) {
	if !p.Prepare.PRFlow() {
		return "", fmt.Errorf("pushing from the workspaces pane is for repos with \"flow\": \"pr\"")
	}
	dir, err := p.workspaceDir(workspace)
	if err != nil {
		return "", err
	}
	trunk, at := "present("+p.Prepare.TrunkRev()+")", "change_id("+change+")"
	out, err := jjStdout(ctx, dir, "log", "-r", "(::"+at+" ~ ::"+trunk+") & bookmarks()", "--no-graph", "-T", `local_bookmarks.map(|b| b.name()).join("\n") ++ "\n"`)
	if err != nil {
		return "", err
	}
	args := []string{"git", "push", "--remote", p.Prepare.Remote()}
	for _, b := range lines(out) {
		args = append(args, "-b", b)
	}
	own, err := jjStdout(ctx, dir, "--ignore-working-copy", "log", "-r", at, "--no-graph", "-T", `if(local_bookmarks, "yes")`)
	if err != nil {
		return "", err
	}
	if own == "" {
		args = append(args, "-c", at)
	}
	said, err := jj(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return "Pushed to " + p.Prepare.Remote() + ":\n" + said, nil
}

// Rebase fetches the remote and moves the slot's stack onto trunk, undoing the move when it would leave a new conflict.
func (p *Pool) Rebase(ctx context.Context, workspace string) (string, error) {
	if !p.Prepare.PRFlow() {
		return "", fmt.Errorf("rebasing from the workspaces pane is for repos with \"flow\": \"pr\"")
	}
	rev, err := p.slotRev(workspace)
	if err != nil {
		return "", err
	}
	dir, trunk, remote := filepath.Join(p.Root, workspace), "present("+p.Prepare.TrunkRev()+")", p.Prepare.Remote()
	if _, err := jj(ctx, dir, "git", "fetch", "--remote", remote); err != nil {
		return "", err
	}
	// The cleanup goes before the rebase, which would rewrite a merged change's commit so it no longer matches what merged.
	swept, _, err := p.Sweep(ctx)
	if err != nil {
		return "", fmt.Errorf("fetched %s, but cleaning up merged work failed: %w", remote, err)
	}
	fetched := "Fetched " + remote
	for _, s := range swept {
		fetched += "; " + s
	}
	trunkName := p.Prepare.TrunkRev()
	roots := "roots((" + rev + ") ~ ::" + trunk + ")"
	if n, err := count(ctx, dir, roots); err != nil || n == 0 {
		return fetched + "; " + workspace + " has nothing to rebase.", err
	}
	if n, err := count(ctx, dir, "parents("+roots+") ~ heads("+trunk+")"); err != nil || n == 0 {
		return fetched + "; " + workspace + " is already on " + trunkName + ".", err
	}
	conflicted := "(" + rev + ") & conflicts()"
	before, err := count(ctx, dir, conflicted)
	if err != nil {
		return "", err
	}
	if _, err := jj(ctx, dir, "rebase", "-s", roots, "-d", trunk); err != nil {
		return "", err
	}
	op, err := jjStdout(ctx, dir, "--ignore-working-copy", "op", "log", "-n1", "--no-graph", "-T", "id")
	if err != nil {
		return "", err
	}
	after, err := count(ctx, dir, conflicted)
	if err != nil {
		return "", err
	}
	if after > before {
		ids, _ := jjStdout(ctx, dir, "--ignore-working-copy", "log", "-r", conflicted, "--no-graph", "-T", `change_id.shortest(8) ++ "\n"`)
		var where []string
		for _, id := range lines(ids) {
			files, _ := jjStdout(ctx, dir, "--ignore-working-copy", "resolve", "--list", "-r", "change_id("+id+")")
			var paths []string
			for _, f := range lines(files) {
				paths = append(paths, strings.Fields(f)[0])
			}
			where = append(where, id+": "+strings.Join(paths, ", "))
		}
		if _, err := jj(ctx, dir, "op", "revert", op); err != nil {
			return "", fmt.Errorf("rebasing onto %s conflicts, and undoing it failed (%v); run jj op log in %s", trunkName, err, dir)
		}
		return "", fmt.Errorf("rebasing %s onto %s would conflict, so it was undone; conflicts in\n%s", workspace, trunkName, strings.Join(where, "\n"))
	}
	moved, err := count(ctx, dir, rev)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s; rebased %s's %s onto %s.", fetched, workspace, plural(moved, "change"), trunkName), nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Push is the pane's push of a change's stack.
func (l *Lander) Push(workspace, change string) (string, error) {
	return l.Pool.Push(context.Background(), workspace, change)
}

// Rebase is the pane's fetch and rebase of a slot's stack onto trunk.
func (l *Lander) Rebase(workspace string) (string, error) {
	return l.Pool.Rebase(context.Background(), workspace)
}
