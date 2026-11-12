package prs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/anthropics/anthropic-sdk-go"
)

const (
	pendingReviewTool  = "pr_pending_review"
	discardPendingTool = "pr_pending_review_discard"
	threadReplyTool    = "pr_thread_reply"
	reviewBotTool      = "pr_review_bot_run"
)

var conventionalLabel = regexp.MustCompile(`^\[[A-Za-z][^\]\n]*\]:\s`)

type ReviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

// pendingReview has no event and no body: without an event GitHub leaves the review PENDING.
type pendingReview struct {
	Comments []ReviewComment `json:"comments"`
}

type PendingReview struct {
	ID    int64
	URL   string
	Count int
	// Added says the comments joined a pending review that already existed.
	Added bool
}

type pendingComment struct {
	ID           int64  `json:"databaseId"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"originalLine"`
	Body         string `json:"body"`
}

type pendingState struct {
	NodeID   string
	ID       int64
	URL      string
	Comments []pendingComment
}

func validateComments(comments []ReviewComment) error {
	if len(comments) == 0 {
		return errors.New("a pending review needs at least one comment")
	}
	var problems []string
	for i, c := range comments {
		where := fmt.Sprintf("comment %d (%s:%d)", i+1, c.Path, c.Line)
		switch {
		case c.Path == "" || c.Line <= 0:
			problems = append(problems, fmt.Sprintf("comment %d needs a path and a line", i+1))
		case !conventionalLabel.MatchString(c.Body):
			problems = append(problems, where+" must start with a conventional-comment label such as [nit]: or [issue]:")
		case c.Side != "" && c.Side != "LEFT" && c.Side != "RIGHT":
			problems = append(problems, where+" has side "+c.Side+"; use LEFT or RIGHT")
		case c.StartLine != 0 && c.StartLine >= c.Line:
			problems = append(problems, where+" has start_line at or after line")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

const pendingQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) {
    reviews(states: PENDING, first: 5) { nodes {
      id databaseId url viewerDidAuthor
      comments(first: 100) { nodes { databaseId path line originalLine body } }
    } }
  } }
}`

// findPending returns the user's own pending review on the PR, or nil when there is none.
func findPending(ctx context.Context, send Send, owner, name string, number int) (*pendingState, error) {
	var resp struct {
		Repository *struct {
			PullRequest *struct {
				Reviews struct {
					Nodes []struct {
						ID              string `json:"id"`
						DatabaseID      int64  `json:"databaseId"`
						URL             string `json:"url"`
						ViewerDidAuthor bool   `json:"viewerDidAuthor"`
						Comments        struct {
							Nodes []pendingComment `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "name": name, "number": number}
	if err := graphQL(ctx, send, pendingQuery, vars, &resp); err != nil {
		return nil, err
	}
	if resp.Repository == nil || resp.Repository.PullRequest == nil {
		return nil, fmt.Errorf("no pull request %s/%s#%d", owner, name, number)
	}
	for _, r := range resp.Repository.PullRequest.Reviews.Nodes {
		if r.ViewerDidAuthor {
			return &pendingState{NodeID: r.ID, ID: r.DatabaseID, URL: r.URL, Comments: r.Comments.Nodes}, nil
		}
	}
	return nil, nil
}

const addThreadMutation = `mutation($input: AddPullRequestReviewThreadInput!) {
  addPullRequestReviewThread(input: $input) { thread { id } }
}`

func CreatePendingReview(ctx context.Context, send Send, owner, name string, number int, comments []ReviewComment) (PendingReview, error) {
	if err := validateComments(comments); err != nil {
		return PendingReview{}, err
	}
	for i := range comments {
		if comments[i].StartLine != 0 && comments[i].StartSide == "" {
			comments[i].StartSide = comments[i].Side
		}
	}
	existing, err := findPending(ctx, send, owner, name, number)
	if err != nil {
		return PendingReview{}, err
	}
	if existing != nil {
		return addToPending(ctx, send, owner, name, number, existing, comments)
	}

	var resp struct {
		ID      int64  `json:"id"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, name, number)
	if err := sendJSON(ctx, send, "POST", path, pendingReview{Comments: comments}, &resp); err != nil {
		return PendingReview{}, err
	}
	if resp.State != "PENDING" {
		return PendingReview{}, fmt.Errorf("GitHub reports review %d as %q, not PENDING, so it may be visible to others: tell the user to check %s now",
			resp.ID, resp.State, resp.HTMLURL)
	}
	return PendingReview{ID: resp.ID, URL: resp.HTMLURL, Count: len(comments)}, nil
}

func addToPending(ctx context.Context, send Send, owner, name string, number int, review *pendingState, comments []ReviewComment) (PendingReview, error) {
	for i, c := range comments {
		input := map[string]any{"pullRequestReviewId": review.NodeID, "path": c.Path, "line": c.Line, "body": c.Body}
		if c.Side != "" {
			input["side"] = c.Side
		}
		if c.StartLine != 0 {
			input["startLine"] = c.StartLine
			input["startSide"] = c.StartSide
		}
		if err := graphQL(ctx, send, addThreadMutation, map[string]any{"input": input}, nil); err != nil {
			return PendingReview{}, fmt.Errorf("added %d of %d comments to pending review %d, then comment %d (%s:%d) failed: %w",
				i, len(comments), review.ID, i+1, c.Path, c.Line, err)
		}
	}
	after, err := findPending(ctx, send, owner, name, number)
	if err != nil {
		return PendingReview{}, fmt.Errorf("comments added, but checking the review is still pending failed: %w; tell the user to check %s", err, review.URL)
	}
	if after == nil || after.ID != review.ID {
		return PendingReview{}, fmt.Errorf("review %d is no longer pending after adding comments, so it may be visible to others: tell the user to check %s now",
			review.ID, review.URL)
	}
	return PendingReview{ID: review.ID, URL: review.URL, Count: len(comments), Added: true}, nil
}

// DraftTools are kept apart from Tools so that a read-only agent handed Tools cannot write.
func (p *Poller) DraftTools() []anthropic.ToolUnionParam {
	target := map[string]any{
		"number": map[string]any{"type": "integer", "description": "The PR number."},
		"repo":   map[string]any{"type": "string", "description": "owner/name. Optional when only one repo is configured."},
	}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{"number": target["number"], "repo": target["repo"]}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	unaskedReply := ""
	if p.conf.ReviewBot() {
		unaskedReply = ", except on a thread the review bot opened when the reply @-mentions nobody"
	}
	tools := []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name: pendingReviewTool,
		Description: anthropic.String("Leave review comments on a pull request as a PENDING review: a draft only the user " +
			"can see, which they read, edit and submit on GitHub themselves. It cannot submit, approve or request changes, " +
			"and it takes no review body — the top-level body stays empty. This is the only way to put review comments " +
			"on GitHub; shell commands that post to a pull request are refused. When the user already has a pending " +
			"review on the PR, the comments are added to it.\n\n" +
			"Every comment is anchored to a line of the diff and must start with a conventional-comment label in the " +
			"form [<type>]:, such as [nit]:, [suggestion]:, [issue]: or [question]:. line is the file's line number on " +
			"the side commented on: RIGHT (the default) for the new code, LEFT for removed lines. A line outside the " +
			"diff is rejected by GitHub. Read what is already drafted with pr_detail first, so nothing is said twice."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: with(map[string]any{
				"comments": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":       map[string]any{"type": "string", "description": "File path as the diff names it."},
							"line":       map[string]any{"type": "integer", "description": "Line the comment sits on; the last line of a range."},
							"side":       map[string]any{"type": "string", "enum": []string{"RIGHT", "LEFT"}, "description": "RIGHT for new code (default), LEFT for removed lines."},
							"start_line": map[string]any{"type": "integer", "description": "First line of a multi-line comment. Omit for one line."},
							"start_side": map[string]any{"type": "string", "enum": []string{"RIGHT", "LEFT"}, "description": "Side of start_line. Defaults to side."},
							"body":       map[string]any{"type": "string", "description": "Markdown, starting with [<type>]:."},
						},
						"required":             []string{"path", "line", "body"},
						"additionalProperties": false,
					},
				},
			}),
			Required:    []string{"number", "comments"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}, {OfTool: &anthropic.ToolParam{
		Name: discardPendingTool,
		Description: anthropic.String("Delete the user's pending review on a pull request, with every comment drafted in " +
			"it — including any the user wrote by hand. It stops for the user's approval, showing what would be lost. " +
			"Only a pending review can be deleted; a submitted one is untouched."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties:  with(nil),
			Required:    []string{"number"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}, {OfTool: &anthropic.ToolParam{
		Name: threadReplyTool,
		Description: anthropic.String("Reply to a review thread on a pull request, resolve it, or both. A reply is public " +
			"the moment it posts, so a call stops for the user's approval with the exact text shown, even in auto " +
			"mode" + unaskedReply + ". Use it to close out a finding the way the user does: say the verdict on the thread, then resolve it. " +
			"Get comment ids from pr_detail with threads: true; any comment in the thread works, and the reply lands at the end of it."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: with(map[string]any{
				"comment_id": map[string]any{"type": "integer", "description": "Id of a comment in the thread, from pr_detail with threads: true."},
				"body":       map[string]any{"type": "string", "description": "The reply, as markdown. Omit to resolve without replying."},
				"resolve":    map[string]any{"type": "boolean", "description": "Resolve the thread after replying."},
			}),
			Required:    []string{"number", "comment_id"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}, reviewLoadParam(target)}
	if p.conf.ReviewBot() {
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name: reviewBotTool,
			Description: anthropic.String("Ask the repo's review bot to review a pull request by posting its configured trigger comment on " +
				"it — that exact text and nothing else. On a draft it runs unasked; on a PR open for review it stops for the user's approval. " +
				"Its findings arrive later as review threads; read them with pr_detail and threads: true. " +
				"Name the PR by number, or by head for the one open PR on that bookmark."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: with(map[string]any{
					"head": map[string]any{"type": "string", "description": "The PR's head bookmark, instead of number."},
				}),
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}})
	}
	return append(tools, p.authorTools()...)
}

func (p *Poller) DraftHandlers() map[string]agent.Handler {
	handlers := map[string]agent.Handler{
		pendingReviewTool:  p.runPendingReview,
		discardPendingTool: p.runDiscardPending,
		threadReplyTool:    p.runThreadReply,
		reviewLoadTool:     p.runReviewLoad,
		createTool:         p.runCreate,
		editTool:           p.runEdit,
		pushTool:           p.runPush,
	}
	if p.conf.ReviewBot() {
		handlers[reviewBotTool] = p.runReviewBot
	}
	return handlers
}

type prTarget struct {
	Number int    `json:"number"`
	Repo   string `json:"repo"`
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func started(call agent.Call, cb agent.Callbacks, label string) {
	if cb.OnToolStart != nil {
		cb.OnToolStart(call.ID, label, agent.KindWeb)
	}
}

func finished(call agent.Call, cb agent.Callbacks, label, out string, err error) anthropic.ContentBlockParamUnion {
	if err != nil {
		out = err.Error()
	}
	if cb.OnToolResult != nil {
		cb.OnToolResult(call.ID, label, out, agent.KindWeb)
	}
	return anthropic.NewToolResultBlock(call.ID, out, err != nil)
}

// waiver is when a public write may skip the user's approval; every waiver lapses once PHI is read.
type waiver int

const (
	asks waiver = iota
	unasked
)

// ask puts a public write to the user through Require, which auto mode cannot answer for them.
func ask(ctx context.Context, cb agent.Callbacks, call agent.Call, w waiver, label, preview, reason string) (anthropic.ContentBlockParamUnion, bool) {
	if w == unasked && !taint.Tainted() {
		return anthropic.ContentBlockParamUnion{}, true
	}
	if cb.Require == nil {
		return anthropic.NewToolResultBlock(call.ID, "Refused: "+reason+", and there is nobody here to approve it.", true), false
	}
	if !cb.Require(ctx, label, preview, reason) {
		return anthropic.NewToolResultBlock(call.ID, "User declined this "+label+".", true), false
	}
	if ctx.Err() != nil {
		return anthropic.NewToolResultBlock(call.ID, "Interrupted.", true), false
	}
	return anthropic.ContentBlockParamUnion{}, true
}

func (p *Poller) runPendingReview(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		prTarget
		Comments []ReviewComment `json:"comments"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	label := fmt.Sprintf("PR #%d pending review", in.Number)
	started(call, cb, label)
	out, err := p.PendingReview(ctx, in.Repo, in.Number, in.Comments)
	return finished(call, cb, label, out, err)
}

func (p *Poller) PendingReview(ctx context.Context, repo string, number int, comments []ReviewComment) (string, error) {
	owner, name, err := p.resolve(repo, number)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	r, err := CreatePendingReview(ctx, p.send, owner, name, number, comments)
	if err != nil {
		return "", err
	}
	verb := "created"
	if r.Added {
		verb = "added to the existing one"
	}
	return fmt.Sprintf("Pending review %d on %s/%s#%d: %d comments, %s. Only the user can see it until they "+
		"submit it on GitHub: %s", r.ID, owner, name, number, r.Count, verb, r.URL), nil
}

func renderPending(repo string, number int, review *pendingState) string {
	if review == nil {
		return fmt.Sprintf("No pending review of the user's on %s#%d.", repo, number)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pending review %d on %s#%d, %d comments: %s\n", review.ID, repo, number, len(review.Comments), review.URL)
	for _, c := range review.Comments {
		line := c.Line
		if line == 0 {
			line = c.OriginalLine
		}
		fmt.Fprintf(&b, "\n- %s:%d (comment %d)\n%s\n", c.Path, line, c.ID, indent(c.Body))
	}
	return strings.TrimRight(b.String(), "\n")
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

func (p *Poller) runDiscardPending(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in prTarget
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	owner, name, err := p.resolve(in.Repo, in.Number)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	review, err := findPending(ctx, p.send, owner, name, in.Number)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	repo := owner + "/" + name
	if review == nil {
		return anthropic.NewToolResultBlock(call.ID, renderPending(repo, in.Number, nil), false)
	}
	label := fmt.Sprintf("discard pending review on PR #%d", in.Number)
	if res, ok := ask(ctx, cb, call, asks, label, renderPending(repo, in.Number, review),
		"it deletes every comment in the draft, including any written by hand"); !ok {
		return res
	}
	started(call, cb, label)
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d", owner, name, in.Number, review.ID)
	if err := sendJSON(ctx, p.send, "DELETE", path, nil, nil); err != nil {
		return finished(call, cb, label, "", err)
	}
	return finished(call, cb, label, fmt.Sprintf("Deleted pending review %d and its %d comments.", review.ID, len(review.Comments)), nil)
}
