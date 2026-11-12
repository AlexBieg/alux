package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// endedWork leaves a described change in a slot whose session then ends, and returns the change.
func endedWork(t *testing.T, p *Pool) string {
	t.Helper()
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "old.txt", "earlier session\n")
	must(t, s.Dir, "describe", "-m", "earlier work")
	work := changeAt(t, s.Dir, "@")
	p.Release(ctx, s)
	return work
}

func TestResumeCarriesOnFromAnEndedSessionsWork(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	work := endedWork(t, p)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.Dir, "describe", "-m", "described but empty")
	left := changeAt(t, s.Dir, "@")

	out, err := p.Resume(ctx, s, work)
	if err != nil {
		t.Fatal(err)
	}
	if parent := changeAt(t, s.Dir, "@-"); parent != work || !strings.Contains(out, "jj describe") {
		t.Fatalf("after %q the session sits on %s, want a new change on %s", out, parent, work)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "old.txt")); err != nil {
		t.Errorf("the resumed work's file is not on disk: %v", err)
	}
	if must(t, s.Dir, "log", "-r", "present(change_id("+left+"))", "--no-graph", "-T", `"+"`) != "" {
		t.Error("the session's empty change was left behind")
	}
	if _, err := p.Resume(ctx, s, work); err == nil || !strings.Contains(err.Error(), "already in this session's workspace") {
		t.Errorf("resuming it again gave %v", err)
	}
}

func TestResumeRefusesLiveWorkAndASessionWithWorkOfItsOwn(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	other, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, other.Dir, "live.txt", "live\n")
	must(t, other.Dir, "describe", "-m", "live work")
	live := changeAt(t, other.Dir, "@")
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resume(ctx, s, live); err == nil || !strings.Contains(err.Error(), "live work in "+other.Name) {
		t.Errorf("resuming another live session's work gave %v", err)
	}
	p.Release(ctx, other)

	write(t, s.Dir, "mine.txt", "mine\n")
	before := changeAt(t, s.Dir, "@")
	if _, err := p.Resume(ctx, s, live); err == nil || !strings.Contains(err.Error(), "work of its own") {
		t.Errorf("resuming over this session's own edits gave %v", err)
	}
	if changeAt(t, s.Dir, "@") != before {
		t.Error("the refused resume moved the session")
	}
}
