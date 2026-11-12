package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// poolRepo is a repo whose default workspace has a described commit with scratch edits on top, and a pool beside it.
func poolRepo(t *testing.T) (*Pool, string) {
	t.Helper()
	origin := mainRepo(t)
	write(t, origin, "a.txt", "base\n")
	must(t, origin, "commit", "-m", "base work")
	write(t, origin, "scratch.txt", "not described\n")
	base := must(t, origin, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	return &Pool{Origin: origin, Root: filepath.Join(filepath.Dir(origin), "pool")}, base
}

func TestPoolForNeedsTheDefaultWorkspaceOfARepoWithPoolOn(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	origin := mainRepo(t)
	settings := func(body string) {
		dir := filepath.Join(home, ".config", "alux")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings(`{"workspaces": {"` + origin + `": {"coders": true}}}`)
	if p, err := PoolForIn(ctx, home, origin); p != nil || err != nil {
		t.Errorf("a repo without pool on gave %+v, %v", p, err)
	}
	settings(`{"workspaces": {"` + origin + `": {"pool": true}}}`)
	p, err := PoolForIn(ctx, home, origin)
	if err != nil || p == nil {
		t.Fatalf("a repo with pool on gave %+v, %v", p, err)
	}
	if want := filepath.Join(home, ".local", "state", "alux-workspaces", "repo"); p.Root != want || p.Origin != origin {
		t.Errorf("pool at %s for %s, want %s for %s", p.Root, p.Origin, want, origin)
	}
	if p, _ := PoolForIn(ctx, home, filepath.Join(filepath.Dir(origin), "ws1")); p != nil {
		t.Errorf("a launch from another workspace got a pool: %+v", p)
	}
}

func TestWorkspaceFlowDefaultsToLandAndRejectsWhatItCannotUse(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "alux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	load := func(entry string) (*Prepare, error) {
		body := `{"workspaces": {"` + repo + `": ` + entry + `}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadPrepareIn(home, repo)
	}
	p, err := load(`{"pool": true}`)
	if err != nil || p == nil || p.PRFlow() {
		t.Errorf("no flow gave %+v, %v; want the land flow", p, err)
	}
	p, err = load(`{"pool": true, "flow": "pr"}`)
	if err != nil || !p.PRFlow() || p.TrunkRev() != "main@origin" {
		t.Errorf(`"flow": "pr" gave PR flow %v on trunk %q, %v; want main@origin`, p.PRFlow(), p.TrunkRev(), err)
	}
	p, err = load(`{"pool": true, "flow": "pr", "trunk": "trunk@upstream"}`)
	if err != nil || p.TrunkRev() != "trunk@upstream" {
		t.Errorf("a set trunk read as %q, %v", p.TrunkRev(), err)
	}
	for _, bad := range []string{`{"flow": "github"}`, `{"trunk": "main@origin"}`, `{"flow": "land", "trunk": "main@origin"}`} {
		if p, err := load(bad); err == nil {
			t.Errorf("%s was accepted as %+v", bad, p)
		}
	}
}

func parentOf(t *testing.T, dir string) string {
	t.Helper()
	return must(t, dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
}

func TestClaimPreparesASlotOnTheLatestDescribedCommitAndAnotherWhileItIsHeld(t *testing.T) {
	t.Parallel()
	p, base := poolRepo(t)
	first, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, first)
	if first.Name != "pool-1" || parentOf(t, first.Dir) != base {
		t.Errorf("claimed %s on %s, want pool-1 on the described commit %s", first.Name, parentOf(t, first.Dir), base)
	}
	if _, err := os.Stat(filepath.Join(first.Dir, "scratch.txt")); err == nil {
		t.Error("the default workspace's undescribed edits reached the slot")
	}
	second, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, second)
	if second.Name != "pool-2" {
		t.Errorf("a second claim while pool-1 is held got %s, want pool-2", second.Name)
	}
}

func TestAReleasedSlotIsHandedOutAgainOnTheNewBaseKeepingDescribedWork(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	work := changeAt(t, s.Dir, "@")
	p.Release(ctx, s)

	write(t, p.Origin, "c.txt", "more\n")
	must(t, p.Origin, "commit", "-m", "newer base")
	newer := must(t, p.Origin, "log", "-r", "@-", "--no-graph", "-T", "commit_id")

	again, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, again)
	if again.Name != "pool-1" || parentOf(t, again.Dir) != newer {
		t.Errorf("reclaimed %s on %s, want pool-1 on the newer base %s", again.Name, parentOf(t, again.Dir), newer)
	}
	if got := must(t, p.Origin, "log", "-r", "change_id("+work+")", "--no-graph", "-T", "description"); strings.TrimSpace(got) != "session work" {
		t.Errorf("the last session's described work reads %q, want it kept", got)
	}
	if _, err := os.Stat(filepath.Join(again.Dir, "b.txt")); err == nil {
		t.Error("the last session's work is still in the slot's working copy")
	}
}

func TestReleaseAbandonsAnEmptyChangeEvenWhenDescribed(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.Dir, "describe", "-m", "looked around")
	gone := changeAt(t, s.Dir, "@")
	p.Release(ctx, s)
	if got := must(t, p.Origin, "log", "-r", "change_id("+gone+")", "--no-graph", "-T", "change_id"); got != "" {
		t.Errorf("the empty change %s survived release", gone)
	}
}

func TestRestartLeavesTheSlotAsARelaunchWouldWhileStillHoldingIt(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	write(t, s.Dir, "b.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	work := changeAt(t, s.Dir, "@")

	write(t, p.Origin, "c.txt", "more\n")
	must(t, p.Origin, "commit", "-m", "newer base")
	newer := must(t, p.Origin, "log", "-r", "@-", "--no-graph", "-T", "commit_id")

	if err := p.Restart(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := parentOf(t, s.Dir); got != newer {
		t.Errorf("restarted on %s, want the newer base %s", got, newer)
	}
	if got := must(t, s.Dir, "log", "-r", "@", "--no-graph", "-T", `if(empty, "empty", "work") ++ if(description, " described", "")`); got != "empty" {
		t.Errorf("the restarted @ is %q, want an empty change with no description", got)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "b.txt")); err == nil {
		t.Error("the last session's work is still in the slot's working copy")
	}
	unlanded, err := p.Unlanded(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(unlanded) != 1 || !strings.HasPrefix(work, unlanded[0].Change) {
		t.Errorf("unlanded work after restart is %+v, want the last session's change %s", unlanded, work)
	}
	other, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, other)
	if other.Name == s.Name {
		t.Errorf("another session claimed %s while this one still held it", s.Name)
	}
}

func TestSparePreparesOneFreeSlotAndNoMore(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	for range 2 {
		if err := p.Spare(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.numbers(); len(got) != 2 {
		t.Errorf("slots after two spares with one held: %v, want pool-1 and one spare", got)
	}
	next, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, next)
	if next.Name != "pool-2" {
		t.Errorf("the next claim got %s, want the spare pool-2", next.Name)
	}
}

func TestSpareAfterAKilledSpareWithAnEmptyPrepare(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	p.Prepare = &Prepare{Pool: true}
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	if err := os.WriteFile(filepath.Join(p.Root, "pool-2.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Spare(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Spare hung")
	}
	if got := p.numbers(); len(got) != 2 {
		t.Errorf("slots after the spare: %v, want pool-1 and pool-2", got)
	}
}

func TestRefreshReinstallsOnlyWhenALockfileChanged(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	write(t, p.Origin, "go.sum", "v1\n")
	must(t, p.Origin, "commit", "-m", "deps v1")
	marker := filepath.Join(t.TempDir(), "installs")
	p.Prepare = &Prepare{Refresh: []string{"echo x >> " + marker}}
	installs := func() int {
		data, _ := os.ReadFile(marker)
		return strings.Count(string(data), "x")
	}

	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(ctx, s)
	if n := installs(); n != 0 {
		t.Errorf("a new slot cloned from matching lockfiles reinstalled %d times", n)
	}
	s, _ = p.Claim(ctx)
	p.Release(ctx, s)
	if n := installs(); n != 0 {
		t.Errorf("a handout with unchanged lockfiles reinstalled %d times", n)
	}
	write(t, p.Origin, "go.sum", "v2\n")
	must(t, p.Origin, "commit", "-m", "deps v2")
	s, err = p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	if n := installs(); n != 1 {
		t.Errorf("a handout after a lockfile changed reinstalled %d times, want once", n)
	}
}
