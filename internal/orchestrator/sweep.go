package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Held is a change whose PR GitHub merged but that the sweep left alone, and why.
type Held struct {
	PR  int
	Why string
}

// sweepCandidate is a bookmarked change of pool work not yet in trunk.
type sweepCandidate struct {
	commit, change, bookmark, dir string
}

// Sweep abandons pool work whose PR GitHub merged into trunk, but only a change whose commit is exactly the head GitHub merged,
// whose merge commit trunk already holds, and whose stack below is all being abandoned too; work on top moves onto trunk.
// It says what it abandoned, and which merged changes it held back and why, by change id.
func (p *Pool) Sweep(ctx context.Context) ([]string, map[string]Held, error) {
	branch := p.Prepare.TrunkBranch()
	if !p.Prepare.PRFlow() || p.Merged == nil || branch == "" {
		return nil, nil, nil
	}
	slug, err := githubSlug(ctx, p.Origin, p.Prepare.Remote())
	if err != nil || slug == "" {
		return nil, nil, err
	}
	trunk := "present(" + p.Prepare.TrunkRev() + ")"
	cands, err := p.sweepCandidates(ctx, trunk)
	if err != nil || len(cands) == 0 {
		return nil, nil, err
	}
	var heads []string
	for _, c := range cands {
		heads = append(heads, c.bookmark)
	}
	merged, err := p.Merged(ctx, slug, heads)
	if err != nil {
		return nil, nil, err
	}

	held := map[string]Held{}
	var sure []sweepCandidate
	for _, c := range cands {
		var into []int
		matched := false
		for _, pr := range merged[c.bookmark] {
			if pr.Base != branch {
				continue
			}
			into = append(into, pr.Number)
			if pr.HeadSHA != c.commit {
				continue
			}
			matched = true
			n, err := count(ctx, p.Origin, "present("+pr.MergeSHA+") & ::"+trunk)
			switch {
			case err != nil:
				return nil, nil, err
			case pr.MergeSHA == "" || n == 0:
				held[c.change] = Held{pr.Number, "its merge is not in your " + branch + " yet; u fetches it"}
			default:
				sure = append(sure, c)
			}
			break
		}
		if !matched && len(into) > 0 {
			held[c.change] = Held{into[0], "this commit differs from the one that merged"}
		}
	}
	sure = wholeStacks(ctx, p.Origin, trunk, sure, held)
	if len(sure) == 0 {
		return nil, held, nil
	}

	byDir := map[string][]sweepCandidate{}
	var dirs []string
	for _, c := range sure {
		if _, ok := byDir[c.dir]; !ok {
			dirs = append(dirs, c.dir)
		}
		byDir[c.dir] = append(byDir[c.dir], c)
	}
	var said []string
	for _, dir := range dirs {
		lines, err := p.abandonMerged(ctx, dir, trunk, byDir[dir])
		if err != nil {
			return said, held, err
		}
		said = append(said, lines...)
	}
	return said, held, nil
}

// sweepCandidates is every bookmarked change of the slots' work and ended sessions' unlanded work outside trunk, with the workspace to run jj in.
func (p *Pool) sweepCandidates(ctx context.Context, trunk string) ([]sweepCandidate, error) {
	type scope struct{ rev, dir string }
	var scopes []scope
	for _, n := range p.numbers() {
		name := fmt.Sprintf("pool-%d", n)
		rev, err := p.slotRev(name)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, scope{rev, filepath.Join(p.Root, name)})
	}
	if ids, err := p.ledgerIDs(); err == nil && len(ids) > 0 {
		scopes = append(scopes, scope{unlandedRev(ids), p.Origin})
	}
	seen := map[string]bool{}
	var out []sweepCandidate
	for _, s := range scopes {
		got, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", "(("+s.rev+") ~ ::"+trunk+") & bookmarks()", "--no-graph", "-T",
			`commit_id ++ "\t" ++ change_id.shortest(8) ++ "\t" ++ local_bookmarks.filter(|b| !b.conflict()).map(|b| b.name()).join(",") ++ "\n"`)
		if err != nil {
			return nil, err
		}
		for _, line := range lines(got) {
			f := strings.Split(line, "\t")
			if len(f) != 3 || f[2] == "" || seen[f[0]] {
				continue
			}
			seen[f[0]] = true
			out = append(out, sweepCandidate{commit: f[0], change: f[1], bookmark: strings.Split(f[2], ",")[0], dir: s.dir})
		}
	}
	return out, nil
}

// wholeStacks keeps only the sure changes with nothing below them outside trunk that is not also being abandoned, holding back the rest.
func wholeStacks(ctx context.Context, dir, trunk string, sure []sweepCandidate, held map[string]Held) []sweepCandidate {
	for {
		set := commitSet(sure)
		var kept []sweepCandidate
		for _, c := range sure {
			n, err := count(ctx, dir, "(::"+c.commit+" ~ ::"+trunk+") ~ ("+set+")")
			if err == nil && n == 0 {
				kept = append(kept, c)
				continue
			}
			held[c.change] = Held{Why: "work below it has not merged"}
		}
		if len(kept) == len(sure) {
			return kept
		}
		sure = kept
		if len(sure) == 0 {
			return nil
		}
	}
}

func commitSet(cs []sweepCandidate) string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.commit
	}
	return strings.Join(ids, " | ")
}

// abandonMerged moves what sits on the merged changes onto trunk and abandons them, from dir so its working copy follows,
// undoing both when that adds a conflict.
func (p *Pool) abandonMerged(ctx context.Context, dir, trunk string, cs []sweepCandidate) ([]string, error) {
	set := commitSet(cs)
	moving, err := jjStdout(ctx, dir, "--ignore-working-copy", "log", "-r", "("+set+"):: ~ ("+set+")", "--no-graph", "-T", `change_id ++ "\n"`)
	if err != nil {
		return nil, err
	}
	conflicted := "none()"
	for _, id := range lines(moving) {
		conflicted += " | present(change_id(" + id + "))"
	}
	conflicted = "(" + conflicted + ") & conflicts()"
	before, err := count(ctx, dir, conflicted)
	if err != nil {
		return nil, err
	}
	atMerged, err := count(ctx, dir, "@ & ("+set+")")
	if err != nil {
		return nil, err
	}
	var ops []string
	op := func() error {
		id, err := jjStdout(ctx, dir, "--ignore-working-copy", "op", "log", "-n1", "--no-graph", "-T", "id")
		ops = append(ops, id)
		return err
	}
	if n, err := count(ctx, dir, "children("+set+") ~ ("+set+")"); err != nil {
		return nil, err
	} else if n > 0 {
		if _, err := jj(ctx, dir, "rebase", "-s", "children("+set+") ~ ("+set+")", "-d", trunk); err != nil {
			return nil, err
		}
		if err := op(); err != nil {
			return nil, err
		}
	}
	if _, err := jj(ctx, dir, "abandon", set); err != nil {
		return nil, err
	}
	if err := op(); err != nil {
		return nil, err
	}
	if atMerged > 0 && dir != p.Origin {
		if _, err := jj(ctx, dir, "rebase", "-r", "@", "-d", trunk); err != nil {
			return nil, err
		}
		if err := op(); err != nil {
			return nil, err
		}
	}
	after, err := count(ctx, dir, conflicted)
	if err != nil {
		return nil, err
	}
	if after > before {
		for i := len(ops) - 1; i >= 0; i-- {
			if _, err := jj(ctx, dir, "op", "revert", ops[i]); err != nil {
				return nil, fmt.Errorf("cleaning up merged work conflicted, and undoing it failed (%v); run jj op log in %s", err, dir)
			}
		}
		return nil, fmt.Errorf("cleaning up merged work in %s would conflict, so it was undone", filepath.Base(dir))
	}
	var said []string
	for _, c := range cs {
		said = append(said, fmt.Sprintf("abandoned %s (%s), merged into %s", c.change, c.bookmark, p.Prepare.TrunkBranch()))
	}
	return said, nil
}
