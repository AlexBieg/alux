package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartAtACommitPutsTheSlotOnItAndRewritesItsBase(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	write(t, p.Origin, "c.txt", "more\n")
	must(t, p.Origin, "commit", "-m", "newer")
	newer := must(t, p.Origin, "log", "-r", "@-", "--no-graph", "-T", "commit_id")

	if _, err := p.StartAt(ctx, s, newer); err != nil {
		t.Fatal(err)
	}
	if got := parentOf(t, s.Dir); got != newer {
		t.Errorf("the slot's @ is on %s, want %s", got, newer)
	}
	if mark, _ := os.ReadFile(filepath.Join(s.Dir, baseMark)); string(mark) != newer {
		t.Errorf("alux-base reads %q, want %s", mark, newer)
	}
}

func TestStartAtMainRefusesARootTrunk(t *testing.T) {
	t.Parallel()
	p, _ := poolRepo(t)
	p.Prepare = &Prepare{Pool: true, Flow: FlowPR, Trunk: "root()"}
	s, err := p.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(ctx, s)
	if _, err := p.StartAt(ctx, s, BaseMain); err == nil || !strings.Contains(err.Error(), "is the root commit") {
		t.Errorf("a root trunk gave %v", err)
	}
}
