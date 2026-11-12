package prs

import (
	"context"
	"fmt"
	"strings"
)

// MergeCheck is a PR that may take its repo's merge label, read fresh from GitHub.
type MergeCheck struct {
	Label   string
	Pending []string // checks still running, which the merge queue waits on
}

// mergeBlocker says why a PR may not take the merge label, or "" when it may.
func mergeBlocker(d Detail, label string) string {
	switch {
	case d.State != "OPEN":
		return "it is " + strings.ToLower(d.State)
	case d.Author != d.Viewer:
		return fmt.Sprintf("it is %s's, not yours", d.Author)
	case d.Draft:
		return "it is a draft"
	case d.HasLabel(label):
		return "it already has " + label
	case d.ReviewDecision != "APPROVED":
		return "it is not approved"
	case d.Mergeable == "CONFLICTING":
		return "it has merge conflicts"
	case d.Mergeable != "MERGEABLE":
		return "GitHub has not worked out yet whether it merges cleanly; try again in a moment"
	case len(d.FailingChecks) > 0:
		return "checks are failing: " + strings.Join(d.FailingChecks, ", ")
	case d.Red():
		return "its checks are failing"
	}
	return ""
}

// MergeReady reads the PR from GitHub and passes it only when it is the viewer's, approved, mergeable and not failing.
func (p *Poller) MergeReady(ctx context.Context, repo string, number int) (MergeCheck, error) {
	owner, name, err := p.resolve(repo, number)
	if err != nil {
		return MergeCheck{}, err
	}
	slug := owner + "/" + name
	r, ok := p.repoConfig(slug)
	if !ok || r.MergeLabel == "" {
		return MergeCheck{}, fmt.Errorf(`%s has no merge label; set "merge_label" under prs.repos.%s in settings.json`, slug, slug)
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	d, err := FetchDetail(ctx, p.gql, owner, name, number)
	if err != nil {
		return MergeCheck{}, err
	}
	if why := mergeBlocker(d, r.MergeLabel); why != "" {
		return MergeCheck{}, fmt.Errorf("not adding %s: %s", r.MergeLabel, why)
	}
	return MergeCheck{Label: r.MergeLabel, Pending: d.PendingChecks}, nil
}

// AddMergeLabel rechecks the PR with MergeReady and labels it only if it still passes.
func (p *Poller) AddMergeLabel(ctx context.Context, repo string, number int) (string, error) {
	c, err := p.MergeReady(ctx, repo, number)
	if err != nil {
		return "", err
	}
	return c.Label, p.AddLabel(ctx, repo, number, c.Label)
}
