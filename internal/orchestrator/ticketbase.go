package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// BaseMain is the base that means the trunk, fetched first.
const BaseMain = "main"

// CheckTrunk refuses a main base in repo before any fetch, when the trunk does not resolve there or is the root commit.
func (p *Pool) CheckTrunk(ctx context.Context, repo string) error {
	_, _, _, err := resolveTrunk(ctx, repo, p.Prepare.TrunkRev(), "checked before fetching; does the repo have a remote?")
	return err
}

// StartAt moves a claimed slot onto base, BaseMain or a commit id, and rewrites its base mark; note reports a failed fetch.
func (p *Pool) StartAt(ctx context.Context, s *Slot, base string) (note string, err error) {
	unlock, err := p.landLock()
	if err != nil {
		return "", err
	}
	defer unlock()
	var commit string
	if base == BaseMain {
		fetched := fetchTrunkBranch(ctx, s.Dir, p.Prepare)
		trunk := p.Prepare.TrunkRev()
		c, short, date, err := resolveTrunk(ctx, s.Dir, trunk, fetched)
		if err != nil {
			return "", err
		}
		commit = c
		if strings.HasPrefix(fetched, "fetch failed") {
			note = fetched + "; " + trunk + " is commit " + short + ", committed " + date
		}
	} else {
		commits, err := JJ{}.Resolve(ctx, s.Dir, base)
		if err != nil {
			return "", err
		}
		if len(commits) != 1 {
			return "", fmt.Errorf("base %s names %d commits in %s, not one", base, len(commits), s.Dir)
		}
		commit = commits[0]
	}
	if _, err := jj(ctx, s.Dir, "new", commit); err != nil {
		return "", err
	}
	return note, writeStamp(filepath.Join(s.Dir, baseMark), commit)
}
