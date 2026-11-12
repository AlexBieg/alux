package orchestrator

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/subagent"
)

// userRepo is the user's own workspace with base.txt committed and an edit in progress in @.
func userRepo(t *testing.T) (*CoderLayers, string) {
	t.Helper()
	repo := mainRepo(t)
	write(t, repo, "base.txt", "one\ntwo\nthree\n")
	must(t, repo, "commit", "-m", "base")
	write(t, repo, "mine.txt", "unfinished work in @\n")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &CoderLayers{Repo: repo, Root: root}, repo
}

func open(t *testing.T, c *CoderLayers) *coderLayer {
	t.Helper()
	return openOn(t, c, "")
}

func openOn(t *testing.T, c *CoderLayers, change string) *coderLayer {
	t.Helper()
	l, err := c.Open(ctx, "task", change, subagent.BaseParent)
	if err != nil {
		t.Fatal(err)
	}
	return l.(*coderLayer)
}

// twoChangeStack commits a.txt as change A and b.txt as its child B below the user's @, and returns both change ids.
func twoChangeStack(t *testing.T, repo string) (a, b string) {
	t.Helper()
	write(t, repo, "a.txt", "a one\n")
	must(t, repo, "commit", "-m", "A", "a.txt")
	write(t, repo, "b.txt", "b one\n")
	must(t, repo, "commit", "-m", "B", "b.txt")
	return changeAt(t, repo, "@--"), changeAt(t, repo, "@-")
}

func fileIn(t *testing.T, repo, rev, name string) string {
	t.Helper()
	return must(t, repo, "--ignore-working-copy", "file", "show", "-r", rev, name)
}

func closeKept(t *testing.T, l *coderLayer) subagent.Work {
	t.Helper()
	w, err := l.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil {
		t.Fatal("Close kept nothing, want the run's change")
	}
	return w
}

func TestAFreezingLayerStartsOnTheSessionsWorkAndIsDescribed(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	c.Freeze = true
	work := changeAt(t, repo, "@")
	l := open(t, c)
	defer l.discard(ctx)
	if got, _ := os.ReadFile(filepath.Join(l.Dir(), "mine.txt")); string(got) != "unfinished work in @\n" {
		t.Errorf("the layer has mine.txt %q, want the session's work", got)
	}
	if len(l.Unseen()) != 0 || l.Frozen() == "" {
		t.Errorf("unseen %v, frozen %q; want nothing unseen and the session's change named", l.Unseen(), l.Frozen())
	}
	if parent := changeAt(t, repo, "@-"); parent != work {
		t.Errorf("the session's @ sits on %s, want a new change on its work %s", parent, work)
	}
	if got := must(t, l.Dir(), "log", "-r", "@", "--no-graph", "-T", "description"); strings.TrimSpace(got) != "task" {
		t.Errorf("the layer's change reads %q, want it described from the task", got)
	}

	again := open(t, c)
	defer again.discard(ctx)
	if again.Frozen() != "" {
		t.Errorf("a dispatch on an empty working copy froze %s", again.Frozen())
	}
}

func TestALayerStartsAtTheLastCommitLeavingAtAloneAndNamesWhatItCannotSee(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	before := must(t, repo, "log", "-r", "@", "--no-graph", "-T", "commit_id")
	l := open(t, c)
	if after := must(t, repo, "log", "-r", "@", "--no-graph", "-T", "commit_id"); after != before {
		t.Errorf("@ moved from %s to %s at dispatch", before, after)
	}
	if _, err := os.Stat(filepath.Join(l.Dir(), "mine.txt")); !os.IsNotExist(err) {
		t.Error("the unfinished edit in @ reached the layer")
	}
	if got, _ := os.ReadFile(filepath.Join(l.Dir(), "base.txt")); string(got) != "one\ntwo\nthree\n" {
		t.Errorf("the layer has base.txt %q, want the committed file", got)
	}
	if !slices.Equal(l.Unseen(), []string{"mine.txt"}) {
		t.Errorf("Unseen = %v, want mine.txt", l.Unseen())
	}
}

func TestASandboxProfileOnACoderLayerLetsItsJJBeWritten(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is macOS Seatbelt")
	}
	c, repo := userRepo(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	c.Root = LayersRoot(home, repo)
	l := open(t, c)
	defer l.discard(ctx)
	w, err := sandbox.Build(sandbox.Profile{Root: l.Dir(), Write: true})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := filepath.EvalSymlinks(filepath.Join(l.Dir(), ".jj", "working_copy"))
	if err != nil {
		t.Fatal(err)
	}
	lock = filepath.Join(lock, "working_copy.lock")
	writable := false
	for i, arg := range w.Args {
		if arg != "-D" || i+1 == len(w.Args) {
			continue
		}
		name, path, _ := strings.Cut(w.Args[i+1], "=")
		if lock != path && !strings.HasPrefix(lock, path+"/") {
			continue
		}
		if strings.HasPrefix(name, "D") {
			t.Errorf("%s is denied by %s", lock, path)
		} else {
			writable = true
		}
	}
	if !writable {
		t.Errorf("%s is not writable", lock)
	}
}

func TestClosedWorkWaitsApartUntilLandedBesideTheUsersEdits(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	l := open(t, c)
	write(t, repo, "later.txt", "user work after dispatch\n")
	must(t, repo, "status")
	write(t, l.Dir(), "coder.txt", "coder work\n")
	if out, err := jj(ctx, l.Dir(), "status"); err != nil || strings.Contains(out, "stale") {
		t.Fatalf("the layer went stale after the user edited: %v %s", err, out)
	}

	w := closeKept(t, l)
	if !slices.Equal(w.Files(), []string{"coder.txt"}) {
		t.Errorf("Files = %v, want coder.txt", w.Files())
	}
	if _, err := os.Stat(l.Dir()); !os.IsNotExist(err) {
		t.Errorf("the layer's directory is still there: %v", err)
	}
	if slices.Contains(workspaceNames(t, repo), l.name) {
		t.Errorf("the closed layer is still registered: %v", workspaceNames(t, repo))
	}
	if _, err := os.Stat(filepath.Join(repo, "coder.txt")); !os.IsNotExist(err) {
		t.Error("the coder's file reached the user's working copy before landing")
	}

	text, err := w.Land(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "coder.txt") || strings.Contains(text, "conflict") {
		t.Errorf("Land said %q, want coder.txt landed with no conflict", text)
	}
	for name, want := range map[string]string{"coder.txt": "coder work\n", "later.txt": "user work after dispatch\n", "mine.txt": "unfinished work in @\n"} {
		if got, _ := os.ReadFile(filepath.Join(repo, name)); string(got) != want {
			t.Errorf("%s in the user's working copy is %q, want %q", name, got, want)
		}
	}
	if left := must(t, repo, "log", "-r", "change_id("+l.change+")", "--no-graph", "-T", "change_id"); left != "" {
		t.Errorf("the landed change %s is still visible", left)
	}
	if divergent := must(t, repo, "log", "-r", "divergent()", "--no-graph", "-T", `change_id ++ "\n"`); divergent != "" {
		t.Errorf("landing left divergent changes: %s", divergent)
	}
}

func TestACommitMidRunLeavesTheLayerFresh(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	l := open(t, c)
	must(t, repo, "commit", "-m", "user work")
	write(t, l.Dir(), "coder.txt", "coder work\n")
	if out, err := jj(ctx, l.Dir(), "status"); err != nil || strings.Contains(out, "stale") {
		t.Fatalf("the layer went stale after the user committed: %v %s", err, out)
	}
	if _, err := closeKept(t, l).Land(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "coder.txt")); string(got) != "coder work\n" {
		t.Errorf("coder.txt is %q after landing", got)
	}
}

func TestRewritingTheBaseMidRunKeepsTheCodersEdits(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	l := open(t, c)
	write(t, l.Dir(), "coder.txt", "coder work\n")
	must(t, repo, "describe", "-r", "@-", "-m", "base, renamed")
	w := closeKept(t, l)
	if !slices.Equal(w.Files(), []string{"coder.txt"}) {
		t.Errorf("Files = %v after the base was rewritten, want coder.txt", w.Files())
	}
	if divergent := must(t, repo, "log", "-r", "divergent()", "--no-graph", "-T", `change_id ++ "\n"`); divergent != "" {
		t.Errorf("closing left divergent changes: %s", divergent)
	}
	if _, err := w.Land(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "coder.txt")); string(got) != "coder work\n" {
		t.Errorf("coder.txt is %q after landing", got)
	}
}

func TestABaseRewrittenAfterASnapshotStillLandsOnlyTheLatestEdits(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	l := open(t, c)
	write(t, l.Dir(), "coder.txt", "first draft\n")
	must(t, l.Dir(), "status")
	must(t, repo, "describe", "-r", "@-", "-m", "base, renamed")
	write(t, l.Dir(), "coder.txt", "final\n")
	if _, err := closeKept(t, l).Land(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "coder.txt")); string(got) != "final\n" {
		t.Errorf("coder.txt is %q after landing, want the coder's last edit", got)
	}
	if divergent := must(t, repo, "log", "-r", "divergent()", "--no-graph", "-T", `change_id ++ "\n"`); divergent != "" {
		t.Errorf("landing left divergent changes: %s", divergent)
	}
}

func TestLandingOverTheUsersSameLineRecordsAConflictAndKeepsBoth(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	l := open(t, c)
	write(t, repo, "base.txt", "one\nuser\nthree\n")
	must(t, repo, "status")
	write(t, l.Dir(), "base.txt", "one\ncoder\nthree\n")

	text, err := closeKept(t, l).Land(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "conflicts with edits already in @") {
		t.Errorf("Land said %q, want the conflict named", text)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "base.txt"))
	if !strings.Contains(string(got), "user") || !strings.Contains(string(got), "coder") {
		t.Errorf("base.txt is %q, want both sides kept in the conflict", got)
	}
}

func TestDroppedWorkNeverReachesTheUser(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	l := open(t, c)
	write(t, l.Dir(), "half.txt", "half done\n")
	if err := closeKept(t, l).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if left := must(t, repo, "log", "-r", "change_id("+l.change+")", "--no-graph", "-T", "change_id"); left != "" {
		t.Errorf("the dropped change %s is still visible", left)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "mine.txt")); string(got) != "unfinished work in @\n" {
		t.Errorf("mine.txt is %q, want the user's edit untouched", got)
	}
}

func TestARunThatChangedNothingLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	l := open(t, c)
	if w, err := l.Close(ctx); err != nil || w != nil {
		t.Fatalf("Close gave %v %v, want nothing kept", w, err)
	}
	if left := must(t, repo, "log", "-r", "change_id("+l.change+")", "--no-graph", "-T", "change_id"); left != "" {
		t.Errorf("the empty change %s is still visible", left)
	}
}

func TestLandingWaitsForAnotherInstanceHoldingTheLock(t *testing.T) {
	t.Parallel()
	c, _ := userRepo(t)
	l := open(t, c)
	write(t, l.Dir(), "coder.txt", "coder work\n")
	w := closeKept(t, l)

	other := &CoderLayers{Repo: c.Repo}
	unlock, err := other.lock()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := w.Land(ctx); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("Land finished (%v) while another instance held the lock", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Land never finished after the lock was released")
	}
}

func TestAFailedPreparationRemovesTheLayerAndLeavesTheUserAlone(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	c.Prepare = &Prepare{Run: []string{"exit 3"}}
	if _, err := c.Open(ctx, "task", "", subagent.BaseParent); err == nil || !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("Open gave %v, want the failed command named", err)
	}
	for _, name := range workspaceNames(t, repo) {
		if strings.HasPrefix(name, "alux-task-") {
			t.Errorf("workspace %s is still registered after a failed preparation", name)
		}
	}
	if entries, _ := os.ReadDir(c.Root); len(entries) != 0 {
		t.Errorf("%s still holds %d entries", c.Root, len(entries))
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "mine.txt")); string(got) != "unfinished work in @\n" {
		t.Errorf("mine.txt is %q, want the user's edit untouched", got)
	}
}

func TestALayerOnAnExistingChangeEditsItInPlaceAndRebasesItsChild(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	a, b := twoChangeStack(t, repo)
	childBefore := must(t, repo, "log", "-r", "change_id("+b+")", "--no-graph", "-T", "commit_id")

	l := openOn(t, c, a)
	if l.Editing() == "" || l.Unseen() != nil {
		t.Errorf("Editing %q, Unseen %v; want the change named and nothing unseen", l.Editing(), l.Unseen())
	}
	if got := fileIn(t, l.Dir(), "@", "a.txt"); got != "a one" {
		t.Fatalf("the layer has a.txt %q, want the change's own file", got)
	}
	write(t, l.Dir(), "a.txt", "a two\n")
	w := closeKept(t, l)
	if !slices.Equal(w.Files(), []string{"a.txt"}) {
		t.Errorf("Files = %v, want only a.txt", w.Files())
	}
	text, err := w.Land(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "a.txt") || strings.Contains(text, "conflicted") {
		t.Errorf("Land said %q, want a.txt edited in place and no conflict", text)
	}

	if got := fileIn(t, repo, "change_id("+a+")", "a.txt"); got != "a two" {
		t.Errorf("change A holds a.txt %q, want the coder's edit", got)
	}
	if got := must(t, repo, "--ignore-working-copy", "log", "-r", "change_id("+b+")- & change_id("+a+")", "--no-graph", "-T", "change_id"); got != a {
		t.Errorf("B's parent is %q, want A", got)
	}
	if now := must(t, repo, "--ignore-working-copy", "log", "-r", "change_id("+b+")", "--no-graph", "-T", "commit_id"); now == childBefore {
		t.Error("B was not rebased onto A's new commit")
	}
	if got := fileIn(t, repo, "change_id("+b+")", "b.txt"); got != "b one" {
		t.Errorf("B holds b.txt %q after the rebase", got)
	}
	if got := must(t, repo, "--ignore-working-copy", "log", "-r", "change_id("+a+")", "--no-graph", "-T", "description"); got != "A" {
		t.Errorf("A is described %q, want it left alone", got)
	}
	if got := must(t, repo, "--ignore-working-copy", "diff", "--name-only", "-r", "@"); got != "mine.txt" {
		t.Errorf("@ changes %q, want only the user's mine.txt: nothing was squashed into it", got)
	}
	if kids := must(t, repo, "--ignore-working-copy", "log", "-r", "children(change_id("+a+"))", "--no-graph", "-T", `change_id ++ "\n"`); kids != b {
		t.Errorf("A's children are %q, want only B: the workspace's empty change is gone", kids)
	}
}

func TestLandingOnAnExistingChangeNamesAConflictItLeavesInItsChild(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	write(t, repo, "a.txt", "one\ntwo\nthree\n")
	must(t, repo, "commit", "-m", "A", "a.txt")
	write(t, repo, "a.txt", "one\nchild\nthree\n")
	must(t, repo, "commit", "-m", "B", "a.txt")
	a, b := changeAt(t, repo, "@--"), changeAt(t, repo, "@-")

	l := openOn(t, c, a)
	write(t, l.Dir(), "a.txt", "one\ncoder\nthree\n")
	text, err := closeKept(t, l).Land(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "descendant "+short(b)+" is now conflicted") || strings.Contains(text, "change itself") {
		t.Errorf("Land said %q, want B named as the conflicted descendant", text)
	}
}

func TestDroppingWorkOnAnExistingChangeRestoresItAndKeepsTheChange(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	a, b := twoChangeStack(t, repo)

	l := openOn(t, c, a)
	write(t, l.Dir(), "a.txt", "a two\n")
	write(t, l.Dir(), "extra.txt", "new\n")
	if err := closeKept(t, l).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fileIn(t, repo, "change_id("+a+")", "a.txt"); got != "a one" {
		t.Errorf("change A holds a.txt %q, want it restored", got)
	}
	if files := must(t, repo, "--ignore-working-copy", "diff", "--name-only", "-r", "change_id("+a+")"); files != "a.txt" {
		t.Errorf("change A has files %q, want only a.txt: the coder's new file is gone", files)
	}
	if got := must(t, repo, "--ignore-working-copy", "log", "-r", "change_id("+a+") | change_id("+b+")", "--no-graph", "-T", `description.first_line() ++ " "`); got != "B A" {
		t.Errorf("changes A and B read %q, want both kept with their descriptions", got)
	}
}

func TestARunThatChangedNothingOnAnExistingChangeLeavesTheChange(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	a, _ := twoChangeStack(t, repo)
	if w, err := openOn(t, c, a).Close(ctx); err != nil || w != nil {
		t.Fatalf("Close gave %v %v, want nothing kept", w, err)
	}
	if got := changeAt(t, repo, "change_id("+a+")"); got != a {
		t.Errorf("change A reads %q after an empty run, want it kept", got)
	}
}

func TestALayerOnABookmarkEditsTheChangeItNames(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	a, _ := twoChangeStack(t, repo)
	must(t, repo, "bookmark", "create", "abc-919-invite-plan-dash", "-r", "change_id("+a+")")
	l := openOn(t, c, "abc-919-invite-plan-dash")
	if got := fileIn(t, l.Dir(), "@", "a.txt"); got != "a one" {
		t.Fatalf("the layer on the bookmark has a.txt %q, want change A's file", got)
	}
	write(t, l.Dir(), "a.txt", "a two\n")
	if _, err := closeKept(t, l).Land(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fileIn(t, repo, "change_id("+a+")", "a.txt"); got != "a two" {
		t.Errorf("change A holds a.txt %q, want the coder's edit", got)
	}
}

func TestALayerRefusesAChangeCheckedOutInAWorkspace(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	at := changeAt(t, repo, "@")
	if _, err := c.Open(ctx, "task", at, subagent.BaseParent); err == nil || !strings.Contains(err.Error(), "checked out in a workspace") {
		t.Fatalf("Open on the user's own @ gave %v, want it refused", err)
	}
	for _, name := range workspaceNames(t, repo) {
		if strings.HasPrefix(name, "alux-task-") {
			t.Errorf("workspace %s was left behind by the refusal", name)
		}
	}
}

func TestATrunkLayerIsCutFromTheFetchedTrunkNotTheWorkingCopy(t *testing.T) {
	t.Parallel()
	c, repo := userRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	must(t, repo, "git", "remote", "add", "origin", bare)
	must(t, repo, "bookmark", "create", "main", "-r", "@-")
	must(t, repo, "git", "push", "-b", "main")
	pushFromElsewhere(t, bare, "main", "trunk.txt")

	l, err := c.Open(ctx, "task", "", subagent.BaseTrunk)
	if err != nil {
		t.Fatal(err)
	}
	layer := l.(*coderLayer)
	defer layer.discard(ctx)
	trunk := must(t, repo, "log", "-r", "main@origin", "--no-graph", "-T", "commit_id")
	if got := must(t, layer.Dir(), "log", "-r", "@-", "--no-graph", "-T", "commit_id"); got != trunk {
		t.Errorf("the layer sits on %s, want the fetched trunk %s", got, trunk)
	}
	if _, err := os.Stat(filepath.Join(layer.Dir(), "trunk.txt")); err != nil {
		t.Error("the layer lacks the commit pushed to trunk since the last fetch")
	}
	if l.Base() != subagent.BaseTrunk || len(l.Unseen()) != 0 || !strings.Contains(layer.Trunk(), "fetched") {
		t.Errorf("base %q, unseen %v, trunk %q; want a fetched trunk base with nothing unseen", l.Base(), l.Unseen(), layer.Trunk())
	}
}

func TestATrunkLayerIsRefusedWhenTheTrunkIsMissingOrTheRoot(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		prepare *Prepare
		want    string
	}{
		"missing": {nil, "does not resolve"},
		"root":    {&Prepare{Flow: FlowPR, Trunk: "root()"}, "is the root commit"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			layers, repo := userRepo(t)
			layers.Prepare = c.prepare
			if _, err := layers.Open(ctx, "task", "", subagent.BaseTrunk); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Open gave %v, want %q", err, c.want)
			}
			for _, ws := range workspaceNames(t, repo) {
				if strings.HasPrefix(ws, "alux-task-") {
					t.Errorf("the refused layer left workspace %s", ws)
				}
			}
		})
	}
}
