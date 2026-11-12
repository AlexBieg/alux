package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const bodyLimit = 3000

type Detail struct {
	Viewer string
	PR
	State          string
	Base, Head     string
	Additions      int
	Deletions      int
	ChangedFiles   int
	Mergeable      string
	Requested      []string
	Threads        []Thread
	FailingChecks  []string
	PendingChecks  []string
	PassingChecks  int
	Files          []File
	FilesTruncated bool
}

type Thread struct {
	Path, Author, Excerpt string
	Outdated              bool
}

type File struct {
	Path                 string
	Additions, Deletions int
}

func detailQuery(owner, name string, number int) string {
	return fmt.Sprintf(`query {
  viewer { login }
  repository(owner: %q, name: %q) { pullRequest(number: %d) {
    number title body url isDraft state createdAt reviewDecision mergeable
    baseRefName headRefName additions deletions changedFiles
    author { login }
    labels(first: 30) { nodes { name } }
    reviewRequests(first: 30) { nodes { requestedReviewer { ... on User { login } ... on Team { combinedSlug } } } }
    reviews(first: 100) { %[4]s nodes { %[5]s } }
    reviewThreads(first: 100) { %[4]s nodes { %[6]s } }
    %[7]s
    files(first: 100) { totalCount nodes { path additions deletions } }
  } }
}`, owner, name, number, pageFields, reviewFields, detailThreadFields, headCommitFields)
}

const detailThreadFields = "isResolved isOutdated path comments(first: 1) { nodes { author { login } body } }"

type detailThread struct {
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Comments   struct {
		Nodes []prComment `json:"nodes"`
	} `json:"comments"`
}

func FetchDetail(ctx context.Context, gql GraphQL, owner, name string, number int) (Detail, error) {
	body, err := gql(ctx, detailQuery(owner, name, number))
	if err != nil {
		return Detail{}, err
	}
	var resp struct {
		Data struct {
			Viewer     login `json:"viewer"`
			Repository *struct {
				PullRequest *struct {
					rawPR
					State        string `json:"state"`
					Mergeable    string `json:"mergeable"`
					BaseRefName  string `json:"baseRefName"`
					HeadRefName  string `json:"headRefName"`
					Additions    int    `json:"additions"`
					Deletions    int    `json:"deletions"`
					ChangedFiles int    `json:"changedFiles"`
					Requests     struct {
						Nodes []struct {
							Reviewer struct {
								Login string `json:"login"`
								Team  string `json:"combinedSlug"`
							} `json:"requestedReviewer"`
						} `json:"nodes"`
					} `json:"reviewRequests"`
					Threads connection[detailThread] `json:"reviewThreads"`
					Commits headCommit               `json:"commits"`
					Files   struct {
						TotalCount int    `json:"totalCount"`
						Nodes      []File `json:"nodes"`
					} `json:"files"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Detail{}, fmt.Errorf("prs: reading GitHub's answer: %w", err)
	}
	if len(resp.Errors) > 0 {
		return Detail{}, errors.New("prs: GitHub: " + resp.Errors[0].Message)
	}
	if resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
		return Detail{}, fmt.Errorf("prs: no pull request #%d in %s/%s", number, owner, name)
	}
	raw := resp.Data.Repository.PullRequest
	if err := raw.rawPR.morePages(ctx, gql, owner, name); err != nil {
		return Detail{}, err
	}
	threads, err := raw.Threads.all(ctx, gql, owner, name, "pullRequest.reviewThreads", func(after string) string {
		return fmt.Sprintf("pullRequest(number: %d) { reviewThreads(first: 100, after: %q) { %s nodes { %s } } }", number, after, pageFields, detailThreadFields)
	})
	if err != nil {
		return Detail{}, err
	}
	checkState, checks, err := raw.Commits.checks(ctx, gql, owner, name)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{
		Viewer: resp.Data.Viewer.Login, PR: raw.rawPR.pr(owner + "/" + name),
		State: raw.State, Mergeable: raw.Mergeable, Base: raw.BaseRefName, Head: raw.HeadRefName,
		Additions: raw.Additions, Deletions: raw.Deletions, ChangedFiles: raw.ChangedFiles,
		Files: raw.Files.Nodes, FilesTruncated: raw.Files.TotalCount > len(raw.Files.Nodes),
	}
	for _, r := range raw.Requests.Nodes {
		if r.Reviewer.Login != "" {
			d.Requested = append(d.Requested, r.Reviewer.Login)
		} else if r.Reviewer.Team != "" {
			d.Requested = append(d.Requested, r.Reviewer.Team)
		}
	}
	for _, t := range threads {
		if t.IsResolved {
			continue
		}
		th := Thread{Path: t.Path, Outdated: t.IsOutdated, Author: "ghost"}
		if len(t.Comments.Nodes) > 0 {
			c := t.Comments.Nodes[0]
			th.Author, th.Excerpt = c.Author.name(), excerpt(c.Body, 140)
		}
		d.Threads = append(d.Threads, th)
	}
	if n := len(raw.Commits.Nodes); n > 0 {
		d.LastCommitAt = raw.Commits.Nodes[n-1].Commit.CommittedDate
	}
	d.Checks = checkState
	for _, c := range checks {
		name, state := c.Name, c.Conclusion
		if c.Type == "StatusContext" {
			name, state = c.Context, c.State
		} else if c.Status != "COMPLETED" {
			state = "PENDING"
		}
		switch state {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			d.PassingChecks++
		case "PENDING", "EXPECTED", "":
			d.PendingChecks = append(d.PendingChecks, name)
		default:
			d.FailingChecks = append(d.FailingChecks, name)
		}
	}
	return d, nil
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// latestReviews is each reviewer's most recent submitted verdict; comments count only when nothing stronger came before.
func latestReviews(reviews []Review) []Review {
	latest := map[string]Review{}
	var order []string
	for _, r := range reviews {
		if r.State == "PENDING" {
			continue
		}
		prev, ok := latest[r.Author]
		if !ok {
			order = append(order, r.Author)
		}
		if !ok || (r.State != "COMMENTED" || prev.State == "COMMENTED") && !r.SubmittedAt.Before(prev.SubmittedAt) {
			latest[r.Author] = r
		}
	}
	out := make([]Review, 0, len(order))
	for _, a := range order {
		out = append(out, latest[a])
	}
	return out
}

func RenderDetail(d Detail, now time.Time) string {
	var b strings.Builder
	it := Item{PR: d.PR}
	fmt.Fprintf(&b, "### #%d %s\n\n%s\n\n", d.Number, d.Title, d.URL)
	state := strings.ToLower(d.State)
	if d.Draft {
		state = "draft"
	}
	fmt.Fprintf(&b, "**%s** by %s · %s ← %s · +%d/−%d in %d files · age %s · idle %s\n\n",
		state, d.Author, d.Base, d.Head, d.Additions, d.Deletions, d.ChangedFiles, days(it.Age(now)), days(it.Idle(now)))
	if slices.ContainsFunc(d.Reviews, func(r Review) bool { return r.Author == d.Viewer && r.State == "PENDING" }) {
		b.WriteString("⚑ **You have a review drafted on this PR and not submitted.**\n\n")
	}
	if d.ReviewDecision != "" {
		fmt.Fprintf(&b, "- **Review decision:** %s", strings.ToLower(strings.ReplaceAll(d.ReviewDecision, "_", " ")))
		if d.Mergeable == "CONFLICTING" {
			b.WriteString(" · has merge conflicts")
		}
		b.WriteString("\n")
	}
	if len(d.Labels) > 0 {
		fmt.Fprintf(&b, "- **Labels:** %s\n", strings.Join(d.Labels, ", "))
	}
	if len(d.Requested) > 0 {
		fmt.Fprintf(&b, "- **Awaiting:** %s\n", strings.Join(d.Requested, ", "))
	}

	b.WriteString("\n#### Reviews\n\n")
	reviews := latestReviews(d.Reviews)
	if len(reviews) == 0 {
		b.WriteString("None.\n")
	}
	for _, r := range reviews {
		fmt.Fprintf(&b, "- %s %s, %s ago\n", r.Author, strings.ToLower(strings.ReplaceAll(r.State, "_", " ")), days(now.Sub(r.SubmittedAt)))
	}

	fmt.Fprintf(&b, "\n#### Checks: %d failing, %d pending, %d passing\n\n", len(d.FailingChecks), len(d.PendingChecks), d.PassingChecks)
	seen := map[string]int{}
	var names []string
	for _, c := range d.FailingChecks {
		if seen[c] == 0 {
			names = append(names, c)
		}
		seen[c]++
	}
	for _, c := range names {
		if seen[c] > 1 {
			fmt.Fprintf(&b, "- failing: %s (×%d)\n", c, seen[c])
		} else {
			fmt.Fprintf(&b, "- failing: %s\n", c)
		}
	}

	fmt.Fprintf(&b, "\n#### Unresolved threads (%d)\n\n", len(d.Threads))
	for _, t := range d.Threads {
		outdated := ""
		if t.Outdated {
			outdated = " (outdated)"
		}
		fmt.Fprintf(&b, "- `%s`%s — %s: %s\n", t.Path, outdated, t.Author, t.Excerpt)
	}

	fmt.Fprintf(&b, "\n#### Files (%d)\n\n", d.ChangedFiles)
	for _, f := range d.Files {
		fmt.Fprintf(&b, "- `%s` +%d/−%d\n", f.Path, f.Additions, f.Deletions)
	}
	if d.FilesTruncated {
		fmt.Fprintf(&b, "- … and %d more\n", d.ChangedFiles-len(d.Files))
	}

	b.WriteString("\n#### Description\n\n" + description(d.Body))
	return b.String()
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// description is the author's markdown, minus HTML comments, cut to bodyLimit without leaving a fence open.
func description(body string) string {
	body = strings.TrimSpace(htmlComment.ReplaceAllString(body, ""))
	if body == "" {
		return "(empty)"
	}
	cut := false
	if r := []rune(body); len(r) > bodyLimit {
		body, cut = string(r[:bodyLimit]), true
	}
	fences := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fences++
		}
	}
	if fences%2 == 1 {
		body += "\n```"
	}
	if cut {
		body += "\n\n*[… description truncated]*"
	}
	return body
}
