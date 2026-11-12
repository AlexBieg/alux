package prs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

type request struct {
	method, path string
	body         map[string]any
}

// fakeGitHub answers each request with the first route whose key is in "METHOD path" or, for GraphQL, in the query.
type fakeGitHub struct {
	routes []route
	sent   []request
}

type route struct {
	key    string
	answer string
	err    error
}

func (f *fakeGitHub) on(key, answer string) *fakeGitHub {
	f.routes = append(f.routes, route{key: key, answer: answer})
	return f
}

func (f *fakeGitHub) fail(key, answer string, err error) *fakeGitHub {
	f.routes = append(f.routes, route{key: key, answer: answer, err: err})
	return f
}

func (f *fakeGitHub) send(_ context.Context, method, path string, body []byte) ([]byte, error) {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	f.sent = append(f.sent, request{method, path, m})
	subject := method + " " + path
	if q, ok := m["query"].(string); ok {
		subject = q
	}
	for _, r := range f.routes {
		if strings.Contains(subject, r.key) {
			return []byte(r.answer), r.err
		}
	}
	return nil, errors.New("fake: no route for " + subject)
}

func (f *fakeGitHub) writes() []request {
	var out []request
	for _, r := range f.sent {
		q, _ := r.body["query"].(string)
		if r.method != "GET" && (r.path != "graphql" || strings.HasPrefix(strings.TrimSpace(q), "mutation")) {
			out = append(out, r)
		}
	}
	return out
}

const noPending = `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}}`
const onePending = `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[
  {"id":"PRR_1","databaseId":5,"url":"https://x/5","viewerDidAuthor":true,
   "comments":{"nodes":[{"databaseId":50,"path":"a.go","line":3,"body":"[nit]: mine, by hand"}]}}]}}}}}`

func poller(f *fakeGitHub) *Poller {
	return &Poller{conf: Config{Repos: []Repo{{Owner: "o", Name: "r", ReviewBot: &ReviewBot{Trigger: "bugbot run", Login: "cursor"}}}}, send: f.send}
}

func call(input string) agent.Call {
	return agent.Call{ID: "1", Input: json.RawMessage(input)}
}

func isError(res anthropic.ContentBlockParamUnion) bool {
	return res.OfToolResult != nil && res.OfToolResult.IsError.Value
}

func TestThePendingReviewRequestCarriesNoEventAndNoBody(t *testing.T) {
	f := (&fakeGitHub{}).on("reviews(states: PENDING", noPending).
		on("POST repos/o/r/pulls/12/reviews", `{"id":7,"state":"PENDING","html_url":"https://x/7"}`)
	r, err := CreatePendingReview(context.Background(), f.send, "o", "r", 12, []ReviewComment{
		{Path: "a.go", Line: 4, Body: "[nit]: rename"},
		{Path: "b.go", Line: 9, StartLine: 6, Side: "RIGHT", Body: "[issue]: leaks"},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 1 || w[0].path != "repos/o/r/pulls/12/reviews" {
		t.Fatalf("wrote %+v", w)
	}
	for _, key := range []string{"event", "body"} {
		if _, ok := w[0].body[key]; ok {
			t.Errorf("the request carries %q, which would submit or fill the review", key)
		}
	}
	comments := w[0].body["comments"].([]any)
	if got := comments[1].(map[string]any)["start_side"]; got != "RIGHT" {
		t.Errorf("start_side = %v, want it to follow side", got)
	}
	if r.ID != 7 || r.Count != 2 || r.Added {
		t.Errorf("got %+v", r)
	}
}

func TestCommentsJoinAnExistingPendingReview(t *testing.T) {
	f := (&fakeGitHub{}).on("reviews(states: PENDING", onePending).on("addPullRequestReviewThread", `{"data":{}}`)
	r, err := CreatePendingReview(context.Background(), f.send, "o", "r", 12, []ReviewComment{{Path: "b.go", Line: 2, Body: "[nit]: x"}})
	if err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 1 {
		t.Fatalf("wrote %+v", w)
	}
	input := w[0].body["variables"].(map[string]any)["input"].(map[string]any)
	if input["pullRequestReviewId"] != "PRR_1" || input["body"] != "[nit]: x" {
		t.Errorf("input = %v", input)
	}
	if !r.Added || r.ID != 5 {
		t.Errorf("got %+v", r)
	}
}

func TestAnUnlabelledCommentIsRefusedBeforeAnythingIsSent(t *testing.T) {
	f := &fakeGitHub{}
	_, err := CreatePendingReview(context.Background(), f.send, "o", "r", 1, []ReviewComment{
		{Path: "a.go", Line: 1, Body: "[nit]: fine"},
		{Path: "a.go", Line: 2, Body: "this one has no label"},
	})
	if err == nil || !strings.Contains(err.Error(), "comment 2") {
		t.Fatalf("err = %v", err)
	}
	if len(f.sent) != 0 {
		t.Error("an invalid review reached GitHub")
	}
}

func TestAReviewGitHubDidNotLeavePendingIsReportedAsAnError(t *testing.T) {
	f := (&fakeGitHub{}).on("reviews(states: PENDING", noPending).
		on("POST repos/o/r/pulls/1/reviews", `{"id":7,"state":"COMMENTED","html_url":"https://x/7"}`)
	_, err := CreatePendingReview(context.Background(), f.send, "o", "r", 1, []ReviewComment{{Path: "a.go", Line: 1, Body: "[nit]: x"}})
	if err == nil || !strings.Contains(err.Error(), "COMMENTED") || !strings.Contains(err.Error(), "https://x/7") {
		t.Fatalf("err = %v", err)
	}
}

func TestGitHubsRefusalReasonIsPassedOn(t *testing.T) {
	f := (&fakeGitHub{}).on("reviews(states: PENDING", noPending).
		fail("POST repos/o/r/pulls/1/reviews", `{"message":"Unprocessable Entity","errors":["line must be part of the diff"]}`, errors.New("gh: HTTP 422"))
	_, err := CreatePendingReview(context.Background(), f.send, "o", "r", 1, []ReviewComment{{Path: "a.go", Line: 900, Body: "[nit]: x"}})
	if err == nil || !strings.Contains(err.Error(), "part of the diff") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheToolRefusesAnEventField(t *testing.T) {
	f := &fakeGitHub{}
	res := poller(f).runPendingReview(context.Background(),
		call(`{"number":1,"event":"APPROVE","comments":[{"path":"a.go","line":1,"body":"[nit]: x"}]}`), agent.Callbacks{})
	if !isError(res) || len(f.sent) != 0 {
		t.Error("an input carrying event was accepted")
	}
}

func TestDiscardingShowsTheDraftAndDeletesOnlyOnYes(t *testing.T) {
	for _, yes := range []bool{false, true} {
		f := (&fakeGitHub{}).on("reviews(states: PENDING", onePending).on("DELETE repos/o/r/pulls/1/reviews/5", `{}`)
		var shown string
		cb := agent.Callbacks{Require: func(_ context.Context, _, body, _ string) bool { shown = body; return yes }}
		poller(f).runDiscardPending(context.Background(), call(`{"number":1}`), cb)
		if !strings.Contains(shown, "mine, by hand") {
			t.Errorf("the approval did not show the drafted comment: %q", shown)
		}
		if deleted := len(f.writes()) == 1; deleted != yes {
			t.Errorf("approved=%v but deleted=%v", yes, deleted)
		}
	}
}

func TestPublicWritesAreRefusedWithNobodyToAsk(t *testing.T) {
	threads := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
	  {"id":"PRRT_1","path":"a.go","line":3,"isResolved":false,"comments":{"nodes":[{"databaseId":9,"author":{"login":"sam"},"body":"bug"}]}}]}}}}}`
	f := (&fakeGitHub{}).on("reviewThreads", threads).on("reviews(states: PENDING", onePending)
	p := poller(f)
	for name, run := range map[string]func() anthropic.ContentBlockParamUnion{
		"reply": func() anthropic.ContentBlockParamUnion {
			return p.runThreadReply(context.Background(), call(`{"number":1,"comment_id":9,"body":"ok"}`), agent.Callbacks{})
		},
		"bugbot": func() anthropic.ContentBlockParamUnion {
			return p.runReviewBot(context.Background(), call(`{"number":1}`), agent.Callbacks{})
		},
		"discard": func() anthropic.ContentBlockParamUnion {
			return p.runDiscardPending(context.Background(), call(`{"number":1}`), agent.Callbacks{})
		},
	} {
		if !isError(run()) {
			t.Errorf("%s ran without an approval channel", name)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("wrote %+v", w)
	}
}

func TestAReviewBotThreadTakesAReplyUnaskedOnlyWhenItTagsNobody(t *testing.T) {
	thread := func(author string) string {
		return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
	  {"id":"PRRT_1","path":"a.go","line":3,"isResolved":false,"comments":{"nodes":[{"databaseId":9,"author":` + author + `,"body":"bug"}]}}]}}}}}`
	}
	for _, c := range []struct {
		name, author, body string
		posts              bool
	}{
		{"bugbot, no mention", `{"login":"cursor","__typename":"Bot"}`, "Fixed in abc123.", true},
		{"bugbot, an email is no mention", `{"login":"cursor","__typename":"Bot"}`, "Asked a@b.com.", true},
		{"bugbot, tags a person", `{"login":"cursor","__typename":"Bot"}`, "Fixed, cc @sam", false},
		{"bugbot, tags a team", `{"login":"cursor","__typename":"Bot"}`, "@org/team please look", false},
		{"a human named cursor", `{"login":"cursor","__typename":"User"}`, "Fixed.", false},
		{"another bot", `{"login":"ci","__typename":"Bot"}`, "Fixed.", false},
	} {
		f := (&fakeGitHub{}).on("reviewThreads", thread(c.author)).on("POST repos/o/r/pulls/1/comments/9/replies", `{"id":10}`)
		poller(f).runThreadReply(context.Background(), call(`{"number":1,"comment_id":9,"body":"`+c.body+`"}`), agent.Callbacks{})
		if posted := len(f.writes()) > 0; posted != c.posts {
			t.Errorf("%s: posted=%v without an approval channel, want %v", c.name, posted, c.posts)
		}
	}
}

func TestNoRepoWithAReviewBotMeansNoReviewBotTool(t *testing.T) {
	f := &fakeGitHub{}
	p := poller(f)
	p.conf.Repos[0].ReviewBot = nil
	for _, tool := range p.DraftTools() {
		if *tool.GetName() == reviewBotTool {
			t.Error("pr_review_bot_run is offered with no repo running a review bot")
		}
		if desc := tool.GetDescription(); desc != nil && strings.Contains(*desc, "review bot") {
			t.Errorf("%s still mentions the review bot", *tool.GetName())
		}
	}
	if _, ok := p.DraftHandlers()[reviewBotTool]; ok {
		t.Error("pr_review_bot_run is handled with no repo running a review bot")
	}
	p.conf.Repos = append(p.conf.Repos, Repo{Owner: "o", Name: "bot", ReviewBot: &ReviewBot{Trigger: "t", Login: "l"}})
	if _, ok := p.DraftHandlers()[reviewBotTool]; !ok {
		t.Fatal("pr_review_bot_run is not handled though a repo runs a review bot")
	}
	cb := agent.Callbacks{Require: func(context.Context, string, string, string) bool { return true }}
	if res := p.runReviewBot(context.Background(), call(`{"repo":"o/r","number":4}`), cb); !isError(res) || len(f.writes()) != 0 {
		t.Error("the review bot was asked to run on a repo that does not run it")
	}
}

func TestTheReadOnlySetHoldsNoWriteTool(t *testing.T) {
	p := poller(&fakeGitHub{})
	drafts := p.DraftHandlers()
	for _, tool := range p.Tools() {
		if _, ok := drafts[*tool.GetName()]; ok {
			t.Errorf("%s is offered to read-only agents", *tool.GetName())
		}
	}
	for name := range p.Handlers() {
		if _, ok := drafts[name]; ok {
			t.Errorf("%s is handled for read-only agents", name)
		}
	}
}
