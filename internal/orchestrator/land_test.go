package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inDefault(t *testing.T, p *Pool, change string) bool {
	t.Helper()
	return must(t, p.Origin, "log", "-r", "change_id("+change+") & ::@", "--no-graph", "-T", "change_id") != ""
}

func TestALeftSessionsWorkIsListedAndLandsUnderTheDefaultWorkingCopy(t *testing.T) {
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

	list, err := p.Unlanded(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !strings.HasPrefix(work, list[0].Change) || list[0].Description != "session work" {
		t.Fatalf("unlanded is %+v, want the session's change %s", list, work)
	}
	out, err := p.Land(ctx, nil, list[0].Change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Landed") || !inDefault(t, p, work) {
		t.Errorf("after %q the change is not under the default workspace's @", out)
	}
	if parent := changeAt(t, p.Origin, "@-"); parent != work {
		t.Errorf("the default @ sits on %s, want the landed change %s right under it", parent, work)
	}
	for _, f := range []string{"b.txt", "scratch.txt"} {
		if _, err := os.Stat(filepath.Join(p.Origin, f)); err != nil {
			t.Errorf("the default workspace lacks %s on disk after landing: %v", f, err)
		}
	}
	if list, _ := p.Unlanded(ctx, nil); len(list) != 0 {
		t.Errorf("after landing unlanded is still %+v", list)
	}
}

func TestLandingTheSessionsOwnWorkCarriesItOnOnTopOfIt(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	write(t, s.Dir, "b.txt", "live\n")
	must(t, s.Dir, "describe", "-m", "live work")
	work := changeAt(t, s.Dir, "@")

	out, err := p.Land(ctx, s, work)
	if err != nil {
		t.Fatal(err)
	}
	if !inDefault(t, p, work) {
		t.Fatalf("after %q the live work is not under the default workspace's @", out)
	}
	if parent := changeAt(t, s.Dir, "@-"); parent != work {
		t.Errorf("the session's @ sits on %s, want a new change on its landed work %s", parent, work)
	}
	if got, _ := os.ReadFile(filepath.Join(s.Dir, "b.txt")); string(got) != "live\n" {
		t.Errorf("the session lost its own landed work from disk: b.txt is %q", got)
	}
	if !strings.Contains(out, "jj describe") {
		t.Errorf("the result does not say the new change needs describing: %s", out)
	}
}

func TestLandingOverTheUsersSameLineSaysItConflictsAndKeepsBoth(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	write(t, p.Origin, "a.txt", "user\n")
	must(t, p.Origin, "status")
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "a.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	work := changeAt(t, s.Dir, "@")
	p.Release(ctx, s)

	out, err := p.Land(ctx, nil, work)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "conflicts with what was already there") {
		t.Errorf("Land said %q, want the conflict named", out)
	}
	got, _ := os.ReadFile(filepath.Join(p.Origin, "a.txt"))
	if !strings.Contains(string(got), "user") || !strings.Contains(string(got), "session") {
		t.Errorf("a.txt is %q, want both sides kept in the conflict", got)
	}
}

func TestAPRFlowRepoLandsNothingAndListsNothingToLand(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	p.Prepare = &Prepare{Pool: true, Flow: FlowPR}
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	work := changeAt(t, s.Dir, "@")
	p.Release(ctx, s)

	if _, err := p.Land(ctx, nil, work); !errors.Is(err, ErrPRFlow) {
		t.Errorf("landing in a PR-flow repo gave %v, want ErrPRFlow", err)
	}
	if inDefault(t, p, work) {
		t.Error("the refused landing still moved the change under the default workspace's @")
	}
	l := &Lander{Pool: p}
	if _, err := l.Command(""); !errors.Is(err, ErrPRFlow) {
		t.Errorf("bare /land gave %v, want ErrPRFlow", err)
	}
	if list, err := l.List(); len(list) != 0 || err != nil {
		t.Errorf("the pane's land list is %+v, %v; want nothing to land", list, err)
	}
	if list, err := l.Candidates(); len(list) != 0 || err != nil {
		t.Errorf("/land completions are %+v, %v; want none", list, err)
	}
}

func TestLandRefusesAChangeThatIsNotPoolWork(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	mine := changeAt(t, p.Origin, "@")
	if _, err := p.Land(ctx, nil, mine); err == nil {
		t.Error("landing the default workspace's own change was accepted")
	}
}

// Landing a change takes its unlanded ancestors along, so the list says how many go with each.
func TestUnlandedCountsTheListedChangesBeneathEach(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "first\n")
	must(t, s.Dir, "commit", "-m", "first")
	write(t, s.Dir, "c.txt", "second\n")
	must(t, s.Dir, "describe", "-m", "second")
	p.Release(ctx, s)

	list, err := p.Unlanded(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	with := map[string]int{}
	for _, u := range list {
		with[u.Description] = len(u.With)
	}
	if len(list) != 2 || with["second"] != 1 || with["first"] != 0 {
		t.Errorf("unlanded is %+v, want second with first under it", list)
	}
}

// This session's own work lands as a whole stack, so landing its lower change says the upper one goes too.
func TestLandingTheLowerOfTheSessionsOwnChangesNamesTheWholeStack(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	write(t, s.Dir, "b.txt", "first\n")
	must(t, s.Dir, "commit", "-m", "first")
	write(t, s.Dir, "c.txt", "second\n")
	must(t, s.Dir, "describe", "-m", "second")
	first, second := changeAt(t, s.Dir, "@-"), changeAt(t, s.Dir, "@")

	list, err := p.Unlanded(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	var lower Unlanded
	for _, u := range list {
		if u.Description == "first" {
			lower = u
		}
	}
	if len(lower.With) != 1 || !strings.HasPrefix(second, lower.With[0]) {
		t.Fatalf("first lands with %v, want second named", lower.With)
	}

	out, err := p.Land(ctx, s, lower.Change)
	if err != nil {
		t.Fatal(err)
	}
	if !inDefault(t, p, first) || !inDefault(t, p, second) {
		t.Fatalf("after %q both changes should be under the default @", out)
	}
	if !strings.Contains(out, "Landed 2 changes") || !strings.Contains(out, "second") {
		t.Errorf("the report does not name the change that went along: %s", out)
	}
}

// Completion reuses one listing while a change id is typed, but a landing must not leave the landed change on offer.
func TestLandCandidatesDropALandedChange(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	p.Release(ctx, s)

	l := &Lander{Pool: p}
	list, err := l.Candidates()
	if err != nil || len(list) != 1 || list[0].Description != "session work" {
		t.Fatalf("candidates are %+v, %v, want the session's change", list, err)
	}
	if _, err := l.Command(list[0].Change); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Candidates(); err != nil || len(got) != 0 {
		t.Errorf("after landing the candidates are still %+v, %v", got, err)
	}
}

func TestLandCandidatesReportAFailedListingAndDoNotCacheIt(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "b.txt", "session\n")
	must(t, s.Dir, "describe", "-m", "session work")
	p.Release(ctx, s)

	origin := p.Origin
	l := &Lander{Pool: &Pool{Origin: filepath.Join(t.TempDir(), "gone"), Root: p.Root}}
	if list, err := l.Candidates(); err == nil {
		t.Fatalf("listing from a missing repo gave %+v and no error", list)
	}
	l.Pool.Origin = origin
	if list, err := l.Candidates(); err != nil || len(list) != 1 {
		t.Errorf("after the repo is back the candidates are %+v, %v, want the session's change", list, err)
	}
}
