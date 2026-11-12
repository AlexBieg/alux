package prs

import (
	"cmp"
	"regexp"
	"slices"
	"time"
)

type PR struct {
	Repo           string
	Number         int
	Title          string
	Body           string
	URL            string
	Author         string
	Draft          bool
	CreatedAt      time.Time
	LastCommitAt   time.Time
	Labels         []string
	ReviewDecision string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or ""
	Checks         string // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED, or "" when nothing ran
	Mergeable      string // MERGEABLE, CONFLICTING, or UNKNOWN while GitHub is still computing it
	// Head and Base are the PR's branch and the branch it merges into; HeadSHA is the head branch's commit.
	Head, Base, HeadSHA string `json:",omitempty"`
	// Unresolved counts review threads nobody has resolved, among the first hundred.
	Unresolved int `json:",omitempty"`
	Reviews    []Review
	// Requested is the users asked to review by name; team requests are not in it.
	Requested []string `json:",omitempty"`
}

type Review struct {
	Author      string
	Bot         bool   `json:",omitempty"`
	State       string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	SubmittedAt time.Time
}

func (p PR) HasLabel(name string) bool { return slices.Contains(p.Labels, name) }

func (p PR) Red() bool { return p.Checks == "FAILURE" || p.Checks == "ERROR" }

// Fetched is one poll's raw material for one repo: every query's results, before any rule runs.
type Fetched struct {
	Repo    Repo
	Viewer  string
	Team    []string
	Review  [][]PR // one slice per Review query, in config order
	Mine    [][]PR
	Partial []string // queries whose results were capped
}

type Status string

const (
	StatusReady            Status = "ready to land"
	StatusConflict         Status = "merge conflict"
	StatusChangesRequested Status = "changes requested"
	StatusFailing          Status = "CI failing"
	StatusWaitingCI        Status = "waiting on CI"
	StatusWaiting          Status = "waiting on review"
	StatusDraft            Status = "draft"
)

var statusOrder = []Status{StatusReady, StatusConflict, StatusChangesRequested, StatusFailing, StatusWaitingCI, StatusWaiting, StatusDraft}

type Item struct {
	PR
	Status Status `json:",omitempty"`
}

func (it Item) Age(now time.Time) time.Duration { return now.Sub(it.CreatedAt) }

func (it Item) Idle(now time.Time) time.Duration { return now.Sub(it.lastActivity()) }

func (it Item) lastActivity() time.Time {
	if it.LastCommitAt.IsZero() {
		return it.CreatedAt
	}
	return it.LastCommitAt
}

func longestIdleFirst(a, b Item) int { return a.lastActivity().Compare(b.lastActivity()) }

type FlagHit struct {
	Kind   FlagKind
	Label  string `json:",omitempty"`
	Repo   string
	Number int
	Title  string
}

type Queue struct {
	Review []Item
	Mine   []Item
	Flags  []FlagHit
}

// Classify applies a repo's rules to what was fetched for it.
func Classify(f Fetched) Queue {
	var q Queue
	review := union(f.Review, f.Repo.Review.Queries)
	for _, p := range review {
		if p.Author == f.Viewer || excluded(p, f.Repo.Review.Exclude, f) {
			continue
		}
		q.Review = append(q.Review, Item{PR: p})
	}
	slices.SortStableFunc(q.Review, longestIdleFirst)

	mine := union(f.Mine, f.Repo.Mine.Queries)
	for _, p := range mine {
		if excluded(p, f.Repo.Mine.Exclude, f) {
			continue
		}
		q.Mine = append(q.Mine, Item{PR: p, Status: status(p)})
	}
	slices.SortStableFunc(q.Mine, func(a, b Item) int {
		return cmp.Or(cmp.Compare(slices.Index(statusOrder, a.Status), slices.Index(statusOrder, b.Status)),
			longestIdleFirst(a, b))
	})

	// Flags see excluded PRs too: an unposted review on a teammate-approved PR still needs posting.
	all := union(append(slices.Clone(f.Review), f.Mine...), nil)
	for _, flag := range f.Repo.Flags {
		for _, p := range all {
			if hit(flag, p, f.Viewer) {
				q.Flags = append(q.Flags, FlagHit{Kind: flag.Kind, Label: flag.Label, Repo: p.Repo, Number: p.Number, Title: p.Title})
			}
		}
	}
	return q
}

// union dedupes query results and applies each query's title pattern; nil queries skips that.
func union(results [][]PR, queries []Query) []PR {
	seen := map[int]bool{}
	var out []PR
	for i, batch := range results {
		var title *regexp.Regexp
		if i < len(queries) {
			title = queries[i].TitleMatches
		}
		for _, p := range batch {
			if seen[p.Number] || (title != nil && !title.MatchString(p.Title)) {
				continue
			}
			seen[p.Number] = true
			out = append(out, p)
		}
	}
	return out
}

func excluded(p PR, rules []Exclude, f Fetched) bool {
	for _, e := range rules {
		if matches(p, e, f) {
			return true
		}
	}
	return false
}

func matches(p PR, e Exclude, f Fetched) bool {
	switch {
	case e.Draft:
		return p.Draft
	case e.ApprovedByTeam:
		return approvedByTeam(p, f.Team)
	case e.ApprovedUnlessRequested:
		return !slices.Contains(p.Requested, f.Viewer) && approvedBy(p, func(r Review) bool { return !r.Bot })
	}
	if e.UnlessLabel != "" && p.HasLabel(e.UnlessLabel) {
		return false
	}
	return (e.Author == "" || e.Author == p.Author) &&
		(e.Label == "" || p.HasLabel(e.Label)) &&
		(e.TitleMatches == nil || e.TitleMatches.MatchString(p.Title)) &&
		(e.BodyMatches == nil || e.BodyMatches.MatchString(p.Body))
}

func approvedByTeam(p PR, team []string) bool {
	return approvedBy(p, func(r Review) bool { return slices.Contains(team, r.Author) })
}

// approvedBy counts each counted non-author reviewer's latest approve, change-request or dismissal, as GitHub does.
func approvedBy(p PR, counts func(Review) bool) bool {
	latest := map[string]Review{}
	for _, r := range p.Reviews {
		if r.Author == p.Author || !counts(r) {
			continue
		}
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
			continue
		}
		if prev, ok := latest[r.Author]; !ok || r.SubmittedAt.After(prev.SubmittedAt) {
			latest[r.Author] = r
		}
	}
	for _, r := range latest {
		if r.State == "APPROVED" {
			return true
		}
	}
	return false
}

func status(p PR) Status {
	switch {
	case p.Mergeable == "CONFLICTING":
		return StatusConflict
	case p.Draft:
		return StatusDraft
	case p.ReviewDecision == "CHANGES_REQUESTED":
		return StatusChangesRequested
	case p.Red():
		return StatusFailing
	case p.ReviewDecision == "APPROVED" && p.Checks == "SUCCESS":
		return StatusReady
	case p.ReviewDecision == "APPROVED" && (p.Checks == "PENDING" || p.Checks == "EXPECTED"):
		return StatusWaitingCI
	}
	return StatusWaiting
}

func hit(flag Flag, p PR, viewer string) bool {
	switch flag.Kind {
	case FlagPendingReview:
		return slices.ContainsFunc(p.Reviews, func(r Review) bool { return r.Author == viewer && r.State == "PENDING" })
	case FlagLabelOnRed:
		return p.HasLabel(flag.Label) && p.Red()
	}
	return false
}
