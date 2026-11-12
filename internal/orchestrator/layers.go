package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/subagent"
)

// LayersRoot is where the coder workspaces of the repo whose default workspace is origin live, beside its pool slots.
func LayersRoot(home, origin string) string {
	return filepath.Join(home, paths.Workspaces, filepath.Base(origin), "layers")
}

// CoderLayers gives each dispatched coder a workspace at the last commit (@-) of the user's working copy in Repo.
type CoderLayers struct {
	// Repo is the user's own jj workspace, whose @- each workspace starts from and whose @ receives each landing.
	Repo string
	// Root holds the workspaces, one directory each.
	Root string
	// Prepare, when set, readies each workspace before the coder starts.
	Prepare *Prepare
	// Freeze, for a session in a pool workspace, first commits its work with jj new so the coder starts on it.
	Freeze bool

	next atomic.Int64
}

// lock serialises the jj operations that touch Repo's workspaces or @, across every alux process sharing the checkout.
func (c *CoderLayers) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(c.Repo, ".jj", "alux-land.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Open makes a workspace at @-, so neither the user's nor another instance's unfinished edits in @ reach the coder, and @ never moves.
// With change set the workspace instead has that existing change checked out, and the coder's edits go into it;
// with base subagent.BaseTrunk it is cut from the freshly fetched trunk instead of @-.
func (c *CoderLayers) Open(ctx context.Context, label, change, base string) (subagent.Layer, error) {
	name := fmt.Sprintf("alux-task-%d-%d", time.Now().Unix(), c.next.Add(1))
	dir := filepath.Join(c.Root, name)
	if err := os.MkdirAll(c.Root, 0o700); err != nil {
		return nil, err
	}
	l := &coderLayer{owner: c, name: name, dir: dir}
	if change == "" && base == subagent.BaseTrunk {
		l.trunk = c.Prepare.TrunkRev()
		l.fetchTrunk(ctx)
	}
	if err := l.add(ctx, change); err != nil {
		return nil, err
	}
	if change == "" {
		if strings.TrimSpace(label) == "" {
			label = "coder run"
		}
		if _, err := jj(ctx, dir, "describe", "-m", label); err != nil {
			return nil, errors.Join(err, l.discard(ctx))
		}
	}
	if c.Prepare != nil {
		if err := c.Prepare.prepare(ctx, c.Repo, dir); err != nil {
			return nil, errors.Join(err, l.discard(ctx))
		}
	}
	return l, nil
}

type coderLayer struct {
	owner     *CoderLayers
	name, dir string
	change    string
	unseen    []string
	frozen    string
	// trunk is the revision a trunk-based layer was cut from, "" for one cut from @-; fetched says how fetching it went.
	trunk, fetched string
	// existing is set when the layer edits a change it did not create; before is that change's commit when the run began.
	existing bool
	before   string
}

var changeIDShape = regexp.MustCompile(`^[k-z]+$`)

// changeRev is the revset for a change id, or for a bookmark of that name, as a flow's fix stage passes one.
func changeRev(change string) string {
	if changeIDShape.MatchString(change) {
		return "change_id(" + change + ")"
	}
	return `bookmarks(exact:"` + strings.ReplaceAll(change, `"`, `\"`) + `")`
}

// target resolves change to the one mutable commit it names that no workspace has checked out, and returns its change and commit ids.
func (l *coderLayer) target(ctx context.Context, change string) (changeID, commit string, err error) {
	rev := changeRev(change)
	out, err := jjStdout(ctx, l.owner.Repo, "log", "-r", rev, "--no-graph", "-T", `change_id ++ " " ++ commit_id ++ "\n"`)
	if err != nil {
		return "", "", err
	}
	found := lines(out)
	if len(found) != 1 {
		return "", "", fmt.Errorf("change %s names %d commits, so there is no one change to edit", change, len(found))
	}
	for _, check := range []struct{ revset, why string }{
		{rev + " ~ mutable()", "is immutable"},
		{rev + " & working_copies()", "is checked out in a workspace, where an edit from another would leave it stale"},
	} {
		bad, err := jjStdout(ctx, l.owner.Repo, "log", "-r", check.revset, "--no-graph", "-T", "commit_id")
		if err != nil {
			return "", "", err
		}
		if bad != "" {
			return "", "", fmt.Errorf("change %s %s", change, check.why)
		}
	}
	changeID, commit, _ = strings.Cut(found[0], " ")
	return changeID, commit, nil
}

func (l *coderLayer) add(ctx context.Context, change string) error {
	unlock, err := l.owner.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if change != "" {
		return l.addOn(ctx, change)
	}
	if l.trunk != "" {
		return l.addOnTrunk(ctx)
	}
	if l.owner.Freeze {
		at, err := jjStdout(ctx, l.owner.Repo, "log", "-r", "@", "--no-graph", "-T", `if(empty, "", change_id)`)
		if err != nil {
			return err
		}
		if at != "" {
			if _, err := jj(ctx, l.owner.Repo, "new"); err != nil {
				return err
			}
			l.frozen = at
		}
	}
	base, err := jjStdout(ctx, l.owner.Repo, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	if err != nil {
		return err
	}
	if base == "" || strings.Contains(base, "\n") {
		return fmt.Errorf("@- in %s is not one commit, so there is no base for a workspace", l.owner.Repo)
	}
	unseen, err := jjStdout(ctx, l.owner.Repo, "diff", "--name-only", "-r", "@")
	if err != nil {
		return err
	}
	l.unseen = lines(unseen)
	return l.addAt(ctx, base)
}

func (l *coderLayer) addAt(ctx context.Context, base string) error {
	if _, err := jj(ctx, l.owner.Repo, "workspace", "add", "--name", l.name, "-r", base, l.dir); err != nil {
		return err
	}
	var err error
	if l.change, err = jjStdout(ctx, l.dir, "log", "-r", "@", "--no-graph", "-T", "change_id"); err != nil || l.change == "" {
		return errors.Join(fmt.Errorf("reading the new workspace's change failed"), err, l.remove(ctx))
	}
	return nil
}

// fetchTrunk fetches only the trunk's bookmark, outside the land lock; a trunk that is not a plain branch@remote is not fetched.
func (l *coderLayer) fetchTrunk(ctx context.Context) {
	l.fetched = fetchTrunkBranch(ctx, l.owner.Repo, l.owner.Prepare)
}

// fetchTrunkBranch fetches only p's trunk bookmark into repo and says how it went; empty when the trunk is not a plain branch@remote.
func fetchTrunkBranch(ctx context.Context, repo string, p *Prepare) string {
	branch := p.TrunkBranch()
	if branch == "" {
		return ""
	}
	if _, err := jj(ctx, repo, "git", "fetch", "--remote", p.Remote(), "--branch", branch); err != nil {
		return "fetch failed: " + strings.TrimSpace(err.Error())
	}
	return "fetched just now"
}

// resolveTrunk is the one non-root commit trunk names in repo, with its short id and commit date; detail goes beside a resolve failure.
func resolveTrunk(ctx context.Context, repo, trunk, detail string) (commit, short, date string, err error) {
	out, err := jjStdout(ctx, repo, "log", "-r", trunk, "--no-graph",
		"-T", `commit_id ++ " " ++ if(root, "root", "-") ++ " " ++ commit_id.short() ++ " " ++ committer.timestamp().format("%Y-%m-%d %H:%M") ++ "\n"`)
	if err != nil {
		return "", "", "", fmt.Errorf("trunk %s does not resolve in %s, so there is no trunk to start from (%s): %w", trunk, repo, detail, err)
	}
	found := lines(out)
	if len(found) != 1 {
		return "", "", "", fmt.Errorf("trunk %s in %s names %d commits, so there is no one trunk commit to start from", trunk, repo, len(found))
	}
	fields := strings.SplitN(found[0], " ", 4)
	if len(fields) != 4 || fields[1] == "root" {
		return "", "", "", fmt.Errorf("trunk %s in %s is the root commit, so there is no trunk to start from", trunk, repo)
	}
	return fields[0], fields[2], fields[3], nil
}

// addOnTrunk makes the workspace on the trunk commit, which must be one commit other than the root.
func (l *coderLayer) addOnTrunk(ctx context.Context) error {
	commit, short, date, err := resolveTrunk(ctx, l.owner.Repo, l.trunk, l.Trunk())
	if err != nil {
		return err
	}
	if strings.HasPrefix(l.fetched, "fetch failed") {
		l.fetched += "; " + l.trunk + " is commit " + short + ", committed " + date
	}
	return l.addAt(ctx, commit)
}

// addOn makes the workspace and checks the existing change out in it; the empty commit workspace add made is abandoned by jj.
func (l *coderLayer) addOn(ctx context.Context, change string) error {
	changeID, commit, err := l.target(ctx, change)
	if err != nil {
		return err
	}
	if _, err := jj(ctx, l.owner.Repo, "workspace", "add", "--name", l.name, "-r", commit, l.dir); err != nil {
		return err
	}
	if _, err := jj(ctx, l.dir, "edit", "change_id("+changeID+")"); err != nil {
		return errors.Join(err, l.remove(ctx))
	}
	l.change, l.existing = changeID, true
	if l.before, err = jjStdout(ctx, l.dir, "log", "-r", "@", "--no-graph", "-T", "commit_id"); err != nil {
		return errors.Join(err, l.remove(ctx))
	}
	return nil
}

func (l *coderLayer) Dir() string      { return l.dir }
func (l *coderLayer) Origin() string   { return l.owner.Repo }
func (l *coderLayer) Unseen() []string { return l.unseen }
func (l *coderLayer) Frozen() string   { return short(l.frozen) }
func (l *coderLayer) Base() string     { return baseOf(l.trunk) }

// Trunk names the trunk a trunk-based layer was cut from and how fetching it went.
func (l *coderLayer) Trunk() string {
	if l.fetched == "" {
		return "trunk " + l.trunk
	}
	return "trunk " + l.trunk + " (" + l.fetched + ")"
}

func baseOf(trunk string) string {
	if trunk == "" {
		return subagent.BaseParent
	}
	return subagent.BaseTrunk
}

// syncOrigin snapshots the user's working copy, updating it first when the coder's edit rewrote an ancestor of its @;
// run before the coder's snapshot too, so the user's uncommitted edits are in @ when the update happens.
func (l *coderLayer) syncOrigin(ctx context.Context) error {
	unlock, err := l.owner.lock()
	if err != nil {
		return err
	}
	defer unlock()
	_, err = snapshot(ctx, l.owner.Repo)
	return err
}

// Editing is the existing change the run edits in place, "" for a run on a change of its own.
func (l *coderLayer) Editing() string {
	if !l.existing {
		return ""
	}
	return short(l.change)
}

// Close snapshots the workspace and removes it, keeping its change; a change with no files is abandoned instead.
func (l *coderLayer) Close(ctx context.Context) (subagent.Work, error) {
	if l.existing {
		if err := l.syncOrigin(ctx); err != nil {
			return nil, fmt.Errorf("its workspace %s was kept, because recording your working copy first failed: %w", l.dir, err)
		}
	}
	commit, err := snapshot(ctx, l.dir)
	if err != nil {
		return nil, fmt.Errorf("its workspace %s was kept, because recording its edits failed: %w", l.dir, err)
	}
	if l.existing {
		if err := l.syncOrigin(ctx); err != nil {
			return nil, fmt.Errorf("its workspace %s was kept, because updating your working copy failed: %w", l.dir, err)
		}
	}
	if copies, err := jjStdout(ctx, l.owner.Repo, "--ignore-working-copy", "log", "-r", "change_id("+l.change+") ~ "+commit,
		"--no-graph", "-T", `commit_id.short() ++ " "`); err != nil || copies != "" {
		return nil, errors.Join(fmt.Errorf("its workspace %s was kept, because its change %s is divergent (also %s); compare the copies before landing any",
			l.dir, short(l.change), copies), err)
	}
	diff := []string{"diff", "--name-only", "-r", commit}
	if l.existing {
		diff = []string{"diff", "--name-only", "--from", l.before, "--to", commit}
	}
	files, err := jjStdout(ctx, l.dir, append([]string{"--ignore-working-copy"}, diff...)...)
	if err != nil {
		return nil, err
	}
	if files == "" {
		return nil, l.discard(ctx)
	}
	unlock, err := l.owner.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := l.remove(ctx); err != nil {
		return nil, err
	}
	return &coderWork{repo: l.owner, change: l.change, files: lines(files), existing: l.existing, before: l.before, trunk: l.trunk}, nil
}

// discard removes the workspace and abandons its change, for a layer no run ever used; a change it did not create stays.
func (l *coderLayer) discard(ctx context.Context) error {
	unlock, err := l.owner.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return errors.Join(l.remove(ctx), l.abandon(ctx))
}

// remove forgets the workspace and deletes its directory; the change itself stays in the repo.
func (l *coderLayer) remove(ctx context.Context) error {
	if _, err := jj(ctx, l.owner.Repo, "workspace", "forget", l.name); err != nil {
		return err
	}
	if err := os.RemoveAll(l.dir); err != nil {
		return err
	}
	return JJWorkspaces{Repo: l.owner.Repo}.prune(ctx)
}

// abandon drops the layer's change, which jj may already have dropped itself when an empty working copy's workspace was forgotten.
func (l *coderLayer) abandon(ctx context.Context) error {
	if l.change == "" || l.existing {
		return nil
	}
	_, err := jj(ctx, l.owner.Repo, "abandon", "change_id("+l.change+")")
	return err
}

type coderWork struct {
	repo   *CoderLayers
	change string
	files  []string
	// existing marks a change the run edited in place; before is its commit when the run began.
	existing bool
	before   string
	trunk    string
}

func (w *coderWork) Change() string  { return short(w.change) }
func (w *coderWork) Files() []string { return w.files }
func (w *coderWork) Base() string    { return baseOf(w.trunk) }

// Editing is the existing change the run edited in place, "" for one the run created.
func (w *coderWork) Editing() string {
	if !w.existing {
		return ""
	}
	return short(w.change)
}

// Land squashes the change into the user's @ and reports any conflict jj recorded there.
// A change edited in place is not moved: Land reports its files and any conflict in it or its descendants.
func (w *coderWork) Land(ctx context.Context) (string, error) {
	unlock, err := w.repo.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if w.existing {
		return w.report(ctx), nil
	}
	if _, err := jj(ctx, w.repo.Repo, "squash", "--from", "change_id("+w.change+")", "--into", "@", "--use-destination-message"); err != nil {
		return "", err
	}
	text := "Landed in your working copy: " + strings.Join(w.files, ", ") + "."
	conflict, err := jjStdout(ctx, w.repo.Repo, "log", "-r", "@", "--no-graph", "-T", `if(conflict, "conflict")`)
	if err != nil {
		return text + " Checking @ for a conflict failed (" + err.Error() + "); run jj status.", nil
	}
	if conflict == "conflict" {
		text += " It conflicts with edits already in @: jj recorded the conflict, and the markers are in the files. Resolve them before anything else."
	}
	return text, nil
}

// report says what the run changed in place and which commits from that change down are conflicted.
func (w *coderWork) report(ctx context.Context) string {
	text := fmt.Sprintf("Change %s was edited in place: %s. Nothing was moved into your working copy.", short(w.change), strings.Join(w.files, ", "))
	conflicted, err := jjStdout(ctx, w.repo.Repo, "--ignore-working-copy", "log", "-r", "change_id("+w.change+"):: & conflicts()", "--no-graph", "-T", `change_id ++ "\n"`)
	if err != nil {
		return text + " Checking for conflicts failed (" + err.Error() + "); run jj log -r 'conflicts()'."
	}
	for _, c := range lines(conflicted) {
		if c == w.change {
			text += " The change itself is now conflicted: resolve it before building on it."
			continue
		}
		text += " Its descendant " + short(c) + " is now conflicted."
	}
	return text
}

// Drop abandons the change, or for one edited in place restores its files to how they were before the run and keeps it.
func (w *coderWork) Drop(ctx context.Context) error {
	unlock, err := w.repo.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if w.existing {
		_, err = jj(ctx, w.repo.Repo, "restore", "--from", w.before, "--into", "change_id("+w.change+")")
		return err
	}
	_, err = jj(ctx, w.repo.Repo, "abandon", "change_id("+w.change+")")
	return err
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
