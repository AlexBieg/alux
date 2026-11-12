package prs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// pageSize caps each query; past it the result is reported as partial rather than paged.
const pageSize = 50

// GraphQL names the repo in every search because gh cannot infer it inside a non-colocated jj workspace.
type GraphQL func(ctx context.Context, query string) ([]byte, error)

func GhGraphQL(ctx context.Context, query string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("gh: %s", msg)
		}
		return nil, fmt.Errorf("gh: %w", err)
	}
	return out, nil
}

const (
	reviewFields = "author { login __typename } state submittedAt"
	prFields     = `... on PullRequest {
  number title body url isDraft createdAt reviewDecision mergeable
  headRefName headRefOid baseRefName
  author { login }
  reviewThreads(first: 100) { ` + pageFields + ` nodes { isResolved } }
  labels(first: 30) { nodes { name } }
  commits(last: 1) { nodes { commit { committedDate statusCheckRollup { state } } } }
  reviews(first: 100) { ` + pageFields + ` nodes { ` + reviewFields + ` } }
  reviewRequests(first: 30) { nodes { requestedReviewer { ... on User { login } } } }
}`
)

// Fetch asks each search separately and at once, since together they run near GitHub's GraphQL time limit.
func Fetch(ctx context.Context, gql GraphQL, repo Repo) (Fetched, error) {
	queries := buildQueries(repo)
	bodies := make([][]byte, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Go(func() { bodies[i], errs[i] = askRetrying(ctx, gql, q) })
	}
	wg.Wait()
	data := map[string]json.RawMessage{}
	for i, body := range bodies {
		if errs[i] != nil {
			return Fetched{}, errs[i]
		}
		d, err := dataOf(body)
		if err != nil {
			return Fetched{}, err
		}
		maps.Copy(data, d)
	}
	return decode(ctx, gql, data, repo)
}

var retryWait = 2 * time.Second

// askRetrying retries once when GitHub gives up on a query mid-run, which it reports as a 502.
func askRetrying(ctx context.Context, gql GraphQL, query string) ([]byte, error) {
	body, err := gql(ctx, query)
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		return body, err
	}
	select {
	case <-ctx.Done():
		return nil, err
	case <-time.After(retryWait):
	}
	return gql(ctx, query)
}

func buildQueries(repo Repo) []string {
	var b strings.Builder
	b.WriteString("query {\n  viewer { login }\n")
	if repo.Team != "" {
		org, team, _ := strings.Cut(repo.Team, "/")
		fmt.Fprintf(&b, "  team: organization(login: %q) { team(slug: %q) { members(first: 100) { nodes { login } } } }\n", org, team)
	}
	b.WriteString("}\n")
	out := []string{b.String()}
	search := func(alias, q string) {
		var b strings.Builder
		b.WriteString("query {\n")
		writeSearch(&b, alias, repo, q)
		b.WriteString("}\n")
		out = append(out, b.String())
	}
	for i, q := range repo.Review.Queries {
		search(fmt.Sprintf("review%d", i), q.Search)
	}
	for i, q := range repo.Mine.Queries {
		search(fmt.Sprintf("mine%d", i), q.Search)
	}
	return out
}

func writeSearch(b *strings.Builder, alias string, repo Repo, search string) {
	full := fmt.Sprintf("repo:%s is:pr is:open %s", repo.Slug(), search)
	fmt.Fprintf(b, "  %s: search(query: %q, type: ISSUE, first: %d) {\n    issueCount\n    nodes { %s }\n  }\n",
		alias, full, pageSize, prFields)
}

type rawPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	URL            string    `json:"url"`
	IsDraft        bool      `json:"isDraft"`
	CreatedAt      time.Time `json:"createdAt"`
	ReviewDecision string    `json:"reviewDecision"`
	Mergeable      string    `json:"mergeable"`
	HeadRefName    string    `json:"headRefName"`
	HeadRefOid     string    `json:"headRefOid"`
	BaseRefName    string    `json:"baseRefName"`
	Author         *login    `json:"author"`
	ReviewThreads  connection[struct {
		IsResolved bool `json:"isResolved"`
	}] `json:"reviewThreads"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				CommittedDate     time.Time `json:"committedDate"`
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Reviews connection[struct {
		Author      *login    `json:"author"`
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submittedAt"`
	}] `json:"reviews"`
	ReviewRequests struct {
		Nodes []struct {
			Reviewer *login `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
}

type login struct {
	Login string `json:"login"`
	Type  string `json:"__typename"`
}

func (l *login) name() string {
	if l == nil {
		return "ghost"
	}
	return l.Login
}

type rawSearch struct {
	IssueCount int     `json:"issueCount"`
	Nodes      []rawPR `json:"nodes"`
}

func dataOf(body []byte) (map[string]json.RawMessage, error) {
	var resp struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("prs: reading GitHub's answer: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return nil, errors.New("prs: GitHub: " + strings.Join(msgs, "; "))
	}
	return resp.Data, nil
}

func decode(ctx context.Context, gql GraphQL, data map[string]json.RawMessage, repo Repo) (Fetched, error) {
	f := Fetched{Repo: repo}
	var viewer login
	if err := json.Unmarshal(data["viewer"], &viewer); err != nil {
		return Fetched{}, fmt.Errorf("prs: viewer: %w", err)
	}
	f.Viewer = viewer.Login

	if raw, ok := data["team"]; ok {
		var org struct {
			Team *struct {
				Members struct {
					Nodes []login `json:"nodes"`
				} `json:"members"`
			} `json:"team"`
		}
		if err := json.Unmarshal(raw, &org); err != nil {
			return Fetched{}, fmt.Errorf("prs: team: %w", err)
		}
		// Without the roster, approved_by_team would quietly match nothing.
		if org.Team == nil {
			return Fetched{}, fmt.Errorf("prs: team %s not found, or not visible to this gh login", repo.Team)
		}
		for _, m := range org.Team.Members.Nodes {
			f.Team = append(f.Team, m.Login)
		}
	}

	read := func(prefix string, queries []Query) ([][]PR, error) {
		out := make([][]PR, len(queries))
		for i, q := range queries {
			var s rawSearch
			if err := json.Unmarshal(data[fmt.Sprintf("%s%d", prefix, i)], &s); err != nil {
				return nil, fmt.Errorf("prs: %s: %w", q.Search, err)
			}
			if s.IssueCount > len(s.Nodes) {
				f.Partial = append(f.Partial, fmt.Sprintf("%q: %d of %d", q.Search, len(s.Nodes), s.IssueCount))
			}
			for _, r := range s.Nodes {
				if err := r.morePages(ctx, gql, repo.Owner, repo.Name); err != nil {
					return nil, fmt.Errorf("prs: #%d: %w", r.Number, err)
				}
				out[i] = append(out[i], r.pr(repo.Slug()))
			}
		}
		return out, nil
	}
	var err error
	if f.Review, err = read("review", repo.Review.Queries); err != nil {
		return Fetched{}, err
	}
	if f.Mine, err = read("mine", repo.Mine.Queries); err != nil {
		return Fetched{}, err
	}
	return f, nil
}

// morePages reads the reviews and review threads past the first page.
func (r *rawPR) morePages(ctx context.Context, gql GraphQL, owner, name string) error {
	var err error
	r.Reviews.Nodes, err = r.Reviews.all(ctx, gql, owner, name, "pullRequest.reviews", func(after string) string {
		return fmt.Sprintf("pullRequest(number: %d) { reviews(first: 100, after: %q) { %s nodes { %s } } }", r.Number, after, pageFields, reviewFields)
	})
	if err != nil {
		return err
	}
	r.ReviewThreads.Nodes, err = r.ReviewThreads.all(ctx, gql, owner, name, "pullRequest.reviewThreads", func(after string) string {
		return fmt.Sprintf("pullRequest(number: %d) { reviewThreads(first: 100, after: %q) { %s nodes { isResolved } } }", r.Number, after, pageFields)
	})
	return err
}

func (r rawPR) pr(repo string) PR {
	p := PR{
		Repo: repo, Number: r.Number, Title: r.Title, Body: r.Body, URL: r.URL, Author: r.Author.name(),
		Draft: r.IsDraft, CreatedAt: r.CreatedAt, ReviewDecision: r.ReviewDecision,
		Mergeable: r.Mergeable, Head: r.HeadRefName, HeadSHA: r.HeadRefOid, Base: r.BaseRefName,
	}
	for _, t := range r.ReviewThreads.Nodes {
		if !t.IsResolved {
			p.Unresolved++
		}
	}
	for _, l := range r.Labels.Nodes {
		p.Labels = append(p.Labels, l.Name)
	}
	if n := len(r.Commits.Nodes); n > 0 {
		c := r.Commits.Nodes[n-1].Commit
		p.LastCommitAt = c.CommittedDate
		if c.StatusCheckRollup != nil {
			p.Checks = c.StatusCheckRollup.State
		}
	}
	for _, rv := range r.Reviews.Nodes {
		bot := rv.Author != nil && rv.Author.Type == "Bot"
		p.Reviews = append(p.Reviews, Review{Author: rv.Author.name(), Bot: bot, State: rv.State, SubmittedAt: rv.SubmittedAt})
	}
	for _, rr := range r.ReviewRequests.Nodes {
		if rr.Reviewer != nil && rr.Reviewer.Login != "" {
			p.Requested = append(p.Requested, rr.Reviewer.Login)
		}
	}
	return p
}
