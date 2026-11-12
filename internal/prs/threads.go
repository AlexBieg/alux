package prs

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

const maxThreadPages = 10

type ThreadComment struct {
	ID        int64
	Author    string
	Bot       bool
	Body      string
	CreatedAt time.Time
}

var mention = regexp.MustCompile(`(^|[^A-Za-z0-9_.])@[A-Za-z0-9]`)

// reviewBotReply is a reply the user lets post without approval: on a thread the review bot login opened, tagging no person or team.
func reviewBotReply(t ReviewThread, body, login string) bool {
	root := t.Comments[0]
	return login != "" && root.Bot && root.Author == login && !mention.MatchString(body)
}

type ReviewThread struct {
	NodeID     string
	Path       string
	Line       int
	IsResolved bool
	IsOutdated bool
	Comments   []ThreadComment
}

const threadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) {
    reviewThreads(first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { id path line originalLine isResolved isOutdated
        comments(first: 100) { nodes { databaseId author { login __typename } body createdAt } } }
    }
  } }
}`

func FetchThreads(ctx context.Context, send Send, owner, name string, number int) ([]ReviewThread, error) {
	var out []ReviewThread
	var after any
	for page := 0; page < maxThreadPages; page++ {
		var resp struct {
			Repository *struct {
				PullRequest *struct {
					Threads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID           string `json:"id"`
							Path         string `json:"path"`
							Line         int    `json:"line"`
							OriginalLine int    `json:"originalLine"`
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Comments     struct {
								Nodes []struct {
									ID        int64     `json:"databaseId"`
									Author    *login    `json:"author"`
									Body      string    `json:"body"`
									CreatedAt time.Time `json:"createdAt"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": name, "number": number, "after": after}
		if err := graphQL(ctx, send, threadsQuery, vars, &resp); err != nil {
			return nil, err
		}
		if resp.Repository == nil || resp.Repository.PullRequest == nil {
			return nil, fmt.Errorf("no pull request %s/%s#%d", owner, name, number)
		}
		ts := resp.Repository.PullRequest.Threads
		for _, n := range ts.Nodes {
			t := ReviewThread{NodeID: n.ID, Path: n.Path, Line: n.Line, IsResolved: n.IsResolved, IsOutdated: n.IsOutdated}
			if t.Line == 0 {
				t.Line = n.OriginalLine
			}
			for _, c := range n.Comments.Nodes {
				author, bot := "ghost", false
				if c.Author != nil {
					author, bot = c.Author.Login, c.Author.Type == "Bot"
				}
				t.Comments = append(t.Comments, ThreadComment{ID: c.ID, Author: author, Bot: bot, Body: c.Body, CreatedAt: c.CreatedAt})
			}
			out = append(out, t)
		}
		if !ts.PageInfo.HasNextPage {
			return out, nil
		}
		after = ts.PageInfo.EndCursor
	}
	return out, nil
}

func RenderThreads(repo string, number int, threads []ReviewThread, unresolvedOnly bool) string {
	var b strings.Builder
	open := 0
	for _, t := range threads {
		if !t.IsResolved {
			open++
		}
	}
	fmt.Fprintf(&b, "%s#%d: %d review threads, %d unresolved.\n", repo, number, len(threads), open)
	for _, t := range threads {
		if unresolvedOnly && t.IsResolved {
			continue
		}
		status := "unresolved"
		if t.IsResolved {
			status = "resolved"
		}
		if t.IsOutdated {
			status += ", outdated"
		}
		fmt.Fprintf(&b, "\n### %s:%d — %s — thread %s\n", t.Path, t.Line, status, t.NodeID)
		for _, c := range t.Comments {
			fmt.Fprintf(&b, "- comment %d by %s, %s\n%s\n", c.ID, c.Author, c.CreatedAt.Local().Format("2006-01-02 15:04"), indent(c.Body))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func threadOf(threads []ReviewThread, commentID int64) (ReviewThread, bool) {
	for _, t := range threads {
		for _, c := range t.Comments {
			if c.ID == commentID {
				return t, true
			}
		}
	}
	return ReviewThread{}, false
}

const resolveMutation = `mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { isResolved } } }`

func (p *Poller) runThreadReply(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		prTarget
		CommentID int64  `json:"comment_id"`
		Body      string `json:"body"`
		Resolve   bool   `json:"resolve"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" && !in.Resolve {
		return anthropic.NewToolResultBlock(call.ID, "give a body to reply, resolve: true to resolve, or both", true)
	}
	owner, name, err := p.resolve(in.Repo, in.Number)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	threads, err := FetchThreads(ctx, p.send, owner, name, in.Number)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	thread, found := threadOf(threads, in.CommentID)
	if !found {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("no review thread on %s/%s#%d holds comment %d; read the ids with pr_detail and threads: true",
			owner, name, in.Number, in.CommentID), true)
	}
	resolve := in.Resolve && !thread.IsResolved

	root := thread.Comments[0]
	var preview strings.Builder
	fmt.Fprintf(&preview, "%s/%s#%d · %s:%d\nThread started by %s:\n%s\n", owner, name, in.Number, thread.Path, thread.Line, root.Author, indent(clip(root.Body, 400)))
	if in.Body != "" {
		fmt.Fprintf(&preview, "\nReply, posted publicly as you:\n%s\n", indent(in.Body))
	}
	if resolve {
		preview.WriteString("\nThen resolve the thread.")
	}
	label := fmt.Sprintf("PR #%d: reply on %s:%d", in.Number, thread.Path, thread.Line)
	if in.Body == "" {
		label = fmt.Sprintf("PR #%d: resolve %s:%d", in.Number, thread.Path, thread.Line)
	}
	if in.Body == "" && !resolve {
		return anthropic.NewToolResultBlock(call.ID, "That thread is already resolved; nothing to do.", false)
	}
	w := asks
	var botLogin string
	if r, _ := p.repoConfig(owner + "/" + name); r.ReviewBot != nil {
		botLogin = r.ReviewBot.Login
	}
	if reviewBotReply(thread, in.Body, botLogin) {
		w = unasked
	}
	if res, ok := ask(ctx, cb, call, w, label, strings.TrimRight(preview.String(), "\n"), "it changes the pull request publicly, as you"); !ok {
		return res
	}
	started(call, cb, label)

	var done []string
	if in.Body != "" {
		var reply struct {
			ID      int64  `json:"id"`
			HTMLURL string `json:"html_url"`
		}
		path := fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%d/replies", owner, name, in.Number, root.ID)
		if err := sendJSON(ctx, p.send, "POST", path, map[string]string{"body": in.Body}, &reply); err != nil {
			return finished(call, cb, label, "", fmt.Errorf("the reply was not posted: %w", err))
		}
		done = append(done, fmt.Sprintf("Replied (comment %d): %s", reply.ID, reply.HTMLURL))
	}
	if resolve {
		if err := graphQL(ctx, p.send, resolveMutation, map[string]any{"id": thread.NodeID}, nil); err != nil {
			done = append(done, "Resolving the thread failed: "+err.Error())
			return finished(call, cb, label, strings.Join(done, "\n"), nil)
		}
		done = append(done, "Resolved thread "+thread.NodeID+".")
	}
	return finished(call, cb, label, strings.Join(done, "\n"), nil)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (p *Poller) runReviewBot(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		prTarget
		Head string `json:"head"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if (in.Number > 0) == (in.Head != "") {
		return anthropic.NewToolResultBlock(call.ID, "give the PR's number or its head bookmark, not both", true)
	}
	if in.Head != "" {
		n, err := p.openPRFor(ctx, in.Repo, in.Head)
		if err != nil {
			return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
		}
		in.Number = n
	}
	owner, name, err := p.resolve(in.Repo, in.Number)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	r, _ := p.repoConfig(owner + "/" + name)
	if r.ReviewBot == nil {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf(`%s/%s does not run a review bot; set "bugbot" under prs.repos.%s/%s in settings.json`, owner, name, owner, name), true)
	}
	trigger := r.ReviewBot.Trigger
	label := fmt.Sprintf("PR #%d: run the review bot", in.Number)
	preview := fmt.Sprintf("Posts the comment %q on %s/%s#%d, publicly as you.", trigger, owner, name, in.Number)
	var pr struct {
		Draft bool   `json:"draft"`
		State string `json:"state"`
	}
	if err := sendJSON(ctx, p.send, "GET", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, in.Number), nil, &pr); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	w := asks
	if pr.Draft && pr.State == "open" {
		w = unasked
	}
	if res, ok := ask(ctx, cb, call, w, label, preview, "it posts a comment on the pull request"); !ok {
		return res
	}
	started(call, cb, label)
	var resp struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, in.Number)
	if err := sendJSON(ctx, p.send, "POST", path, map[string]string{"body": trigger}, &resp); err != nil {
		return finished(call, cb, label, "", err)
	}
	return finished(call, cb, label, "Asked the review bot to run: "+resp.HTMLURL+". Its findings arrive as review threads; read them with pr_detail and threads: true.", nil)
}

// openPRFor is the number of the one open PR whose head is the bookmark.
func (p *Poller) openPRFor(ctx context.Context, repo, head string) (int, error) {
	owner, name, err := p.resolveRepo(repo)
	if err != nil {
		return 0, err
	}
	var prs []struct {
		Number int `json:"number"`
	}
	path := fmt.Sprintf("repos/%s/%s/pulls?head=%s:%s&state=open", owner, name, owner, url.QueryEscape(head))
	if err := sendJSON(ctx, p.send, "GET", path, nil, &prs); err != nil {
		return 0, err
	}
	if len(prs) != 1 {
		return 0, fmt.Errorf("%d open PRs on %s/%s have head %s; give the number instead", len(prs), owner, name, head)
	}
	return prs[0].Number, nil
}
