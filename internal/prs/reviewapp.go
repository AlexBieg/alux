package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

type EnvState string

const (
	EnvNone      EnvState = "none"      // the PR lacks the review app label
	EnvDeploying EnvState = "deploying" // labelled, but the health check has not passed on the head commit
	EnvFailed    EnvState = "failed"    // the health check failed on the head commit
	EnvReady     EnvState = "ready"
)

type ReviewApp struct {
	State EnvState
	Label string            // the label that asks CI to deploy it
	Slug  string            // e.g. 4242-brave-otter
	Apps  map[string]string // app key -> origin (scheme://host, no path)
	// Config is the repo's review_app settings, which Pick and the e2e login read.
	Config ReviewAppConfig
}

type prComment struct {
	Author *login `json:"author"`
	Body   string `json:"body"`
}

func reviewAppQuery(owner, name string, number int) string {
	return fmt.Sprintf(`query {
  repository(owner: %q, name: %q) { pullRequest(number: %d) {
    labels(first: 50) { nodes { name } }
    comments(first: 100) { %s nodes { author { login } body } }
    %s
  } }
}`, owner, name, number, pageFields, headCommitFields)
}

// FetchReviewApp reads a PR's review app: whether it is deployed and the origin of each app in it.
func FetchReviewApp(ctx context.Context, gql GraphQL, conf ReviewAppConfig, owner, name string, number int) (ReviewApp, error) {
	body, err := gql(ctx, reviewAppQuery(owner, name, number))
	if err != nil {
		return ReviewApp{}, err
	}
	var resp struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					Comments connection[prComment] `json:"comments"`
					Commits  headCommit            `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ReviewApp{}, fmt.Errorf("prs: reading GitHub's answer: %w", err)
	}
	if len(resp.Errors) > 0 {
		return ReviewApp{}, errors.New("prs: GitHub: " + resp.Errors[0].Message)
	}
	if resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
		return ReviewApp{}, fmt.Errorf("prs: no pull request #%d in %s/%s", number, owner, name)
	}
	raw := resp.Data.Repository.PullRequest

	labelled := false
	for _, l := range raw.Labels.Nodes {
		labelled = labelled || l.Name == conf.Label
	}
	if !labelled {
		return ReviewApp{State: EnvNone, Label: conf.Label}, nil
	}

	_, checks, err := raw.Commits.checks(ctx, gql, owner, name)
	if err != nil {
		return ReviewApp{}, err
	}
	healthy, running, seen := false, false, false
	for _, c := range checks {
		if c.Type != "CheckRun" || !strings.HasSuffix(c.Name, conf.HealthCheck) {
			continue
		}
		seen = true
		if c.Status != "COMPLETED" {
			running = true
		} else if c.Conclusion == "SUCCESS" {
			healthy = true
		}
	}
	if !healthy {
		if running || !seen {
			return ReviewApp{State: EnvDeploying, Label: conf.Label}, nil
		}
		return ReviewApp{State: EnvFailed, Label: conf.Label}, nil
	}

	comments, err := raw.Comments.all(ctx, gql, owner, name, "pullRequest.comments", func(after string) string {
		return fmt.Sprintf("pullRequest(number: %d) { comments(first: 100, after: %q) { %s nodes { author { login } body } } }", number, after, pageFields)
	})
	if err != nil {
		return ReviewApp{}, err
	}
	for i := len(comments) - 1; i >= 0; i-- {
		c := comments[i]
		if c.Author == nil || c.Author.Login != conf.Bot || !strings.Contains(c.Body, conf.Heading) {
			continue
		}
		if apps := parseEnvApps(c.Body, conf.HostSuffix); len(apps) > 0 {
			return ReviewApp{State: EnvReady, Label: conf.Label, Slug: envSlug(apps, conf.SlugApp), Apps: apps, Config: conf}, nil
		}
	}
	return ReviewApp{}, errors.New("prs: the PR environment is healthy, but the deploy bot's comment with the URLs was not found")
}

var envLine = regexp.MustCompile(`(?m)^\s*[*-]\s+\*\*(.+?)\*\*:\s*(\S+)`)

func parseEnvApps(body, hostSuffix string) map[string]string {
	apps := map[string]string{}
	for _, m := range envLine.FindAllStringSubmatch(body, -1) {
		u, err := url.Parse(m[2])
		if err != nil || u.Scheme != "https" || u.User != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), hostSuffix) {
			continue
		}
		key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(m[1])), " ", "-")
		apps[key] = "https://" + strings.ToLower(u.Host)
	}
	return apps
}

func envSlug(apps map[string]string, first string) string {
	keys := make([]string, 0, len(apps))
	for k := range apps {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if i := slices.Index(keys, first); first != "" && i > 0 {
		keys = slices.Insert(slices.Delete(keys, i, i+1), 0, first)
	}
	for _, k := range keys {
		host := strings.TrimPrefix(apps[k], "https://")
		label, _, _ := strings.Cut(host, ".")
		if slug, ok := strings.CutPrefix(label, k+"-"); ok {
			return slug
		}
	}
	return ""
}

// Pick chooses the app to test from a PR's changed paths: the one under apps_dir with the most files, else rank's first.
func (a ReviewApp) Pick(files []string) (key, origin string) {
	conf := a.Config
	if conf.AppsDir == "" {
		return "", ""
	}
	counts := map[string]int{}
	for _, f := range files {
		rest, ok := strings.CutPrefix(f, conf.AppsDir)
		if !ok {
			continue
		}
		dir, _, ok := strings.Cut(rest, "/")
		if !ok || slices.Contains(conf.Skip, dir) {
			continue
		}
		if _, has := a.Apps[dir]; has {
			counts[dir]++
		}
	}
	if len(counts) == 0 {
		if len(conf.Rank) == 0 {
			return "", ""
		}
		if origin, ok := a.Apps[conf.Rank[0]]; ok {
			return conf.Rank[0], origin
		}
		return "", ""
	}
	rank := func(k string) int {
		if i := slices.Index(conf.Rank, k); i >= 0 {
			return i
		}
		return len(conf.Rank)
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int {
		if counts[x] != counts[y] {
			return counts[y] - counts[x]
		}
		if rank(x) != rank(y) {
			return rank(x) - rank(y)
		}
		return strings.Compare(x, y)
	})
	return keys[0], a.Apps[keys[0]]
}

// ReviewApp reads one PR's review app; the repo must opt in with "review_app" in its config.
func (p *Poller) ReviewApp(ctx context.Context, repo string, number int) (ReviewApp, error) {
	owner, name, err := p.resolve(repo, number)
	if err != nil {
		return ReviewApp{}, err
	}
	slug := owner + "/" + name
	r, ok := p.repoConfig(slug)
	if !ok || r.ReviewApp == nil {
		return ReviewApp{}, fmt.Errorf(`%s has no review apps; set "review_app" under prs.repos.%s in settings.json`, slug, slug)
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	return FetchReviewApp(ctx, p.gql, *r.ReviewApp, owner, name, number)
}

// AddLabel adds a label to a PR.
func (p *Poller) AddLabel(ctx context.Context, repo string, number int, label string) error {
	owner, name, err := p.resolve(repo, number)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	path := fmt.Sprintf("repos/%s/%s/issues/%d/labels", owner, name, number)
	return sendJSON(ctx, p.send, "POST", path, map[string][]string{"labels": {label}}, nil)
}

func (p *Poller) repoConfig(slug string) (Repo, bool) {
	i := slices.IndexFunc(p.conf.Repos, func(r Repo) bool { return r.Slug() == slug })
	if i < 0 {
		return Repo{}, false
	}
	return p.conf.Repos[i], true
}
