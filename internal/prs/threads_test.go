package prs

import (
	"context"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
)

const twoThreads = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
  {"id":"PRRT_1","path":"a.go","line":3,"isResolved":false,"comments":{"nodes":[
    {"databaseId":9,"author":{"login":"cursor"},"body":"possible nil","createdAt":"2026-10-01T10:00:00Z"},
    {"databaseId":10,"author":{"login":"alex"},"body":"checking","createdAt":"2026-10-01T11:00:00Z"}]}},
  {"id":"PRRT_2","path":"b.go","line":0,"originalLine":8,"isResolved":true,"isOutdated":true,"comments":{"nodes":[
    {"databaseId":20,"author":null,"body":"old","createdAt":"2026-10-01T10:00:00Z"}]}}]}}}}}`

func TestThreadsShowEveryCommentAndTheIdsTheWriteToolsTake(t *testing.T) {
	f := (&fakeGitHub{}).on("reviewThreads", twoThreads)
	threads, err := FetchThreads(context.Background(), f.send, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderThreads("o/r", 1, threads, false)
	for _, want := range []string{"2 review threads, 1 unresolved", "PRRT_1", "comment 9 by cursor", "comment 10 by alex", "checking",
		"b.go:8 — resolved, outdated", "by ghost"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(RenderThreads("o/r", 1, threads, true), "PRRT_2") {
		t.Error("unresolved_only kept a resolved thread")
	}
}

func TestThreadsArePaged(t *testing.T) {
	calls := 0
	send := func(_ context.Context, _, _ string, body []byte) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},
			  "nodes":[{"id":"PRRT_1","comments":{"nodes":[]}}]}}}}}`), nil
		}
		if !strings.Contains(string(body), `"after":"c1"`) {
			t.Errorf("second page did not pass the cursor: %s", body)
		}
		return []byte(twoThreads), nil
	}
	threads, err := FetchThreads(context.Background(), send, "o", "r", 1)
	if err != nil || len(threads) != 3 {
		t.Fatalf("threads=%d err=%v", len(threads), err)
	}
}

func TestAReplyPostsToTheThreadsFirstCommentAndResolvesAfterApproval(t *testing.T) {
	f := (&fakeGitHub{}).on("reviewThreads", twoThreads).
		on("POST repos/o/r/pulls/1/comments/9/replies", `{"id":77,"html_url":"https://x/77"}`).
		on("resolveReviewThread", `{"data":{"resolveReviewThread":{"thread":{"isResolved":true}}}}`)
	var shown string
	cb := agent.Callbacks{Require: func(_ context.Context, _, body, _ string) bool { shown = body; return true }}
	res := poller(f).runThreadReply(context.Background(), call(`{"number":1,"comment_id":10,"body":"Not a bug: guarded above.","resolve":true}`), cb)
	if isError(res) {
		t.Fatalf("failed: %+v", res.OfToolResult)
	}
	for _, want := range []string{"possible nil", "Not a bug: guarded above.", "resolve the thread"} {
		if !strings.Contains(shown, want) {
			t.Errorf("approval preview lacks %q:\n%s", want, shown)
		}
	}
	w := f.writes()
	if len(w) != 2 || w[0].body["body"] != "Not a bug: guarded above." {
		t.Fatalf("wrote %+v", w)
	}
	if id := w[1].body["variables"].(map[string]any)["id"]; id != "PRRT_1" {
		t.Errorf("resolved %v", id)
	}
}

func TestADeclinedReplySendsNothing(t *testing.T) {
	f := (&fakeGitHub{}).on("reviewThreads", twoThreads)
	cb := agent.Callbacks{Require: func(context.Context, string, string, string) bool { return false }}
	res := poller(f).runThreadReply(context.Background(), call(`{"number":1,"comment_id":9,"body":"x","resolve":true}`), cb)
	if !isError(res) || len(f.writes()) != 0 {
		t.Error("a declined reply still wrote")
	}
}

func TestAReplyToACommentOutsideThePRIsRefused(t *testing.T) {
	f := (&fakeGitHub{}).on("reviewThreads", twoThreads)
	cb := agent.Callbacks{Require: func(context.Context, string, string, string) bool {
		t.Error("asked about a comment that is not on the PR")
		return true
	}}
	if res := poller(f).runThreadReply(context.Background(), call(`{"number":1,"comment_id":999,"body":"x"}`), cb); !isError(res) {
		t.Error("accepted an unknown comment id")
	}
}

func TestTheReviewBotToolPostsOnlyTheConfiguredTrigger(t *testing.T) {
	f := (&fakeGitHub{}).on("GET repos/o/r/pulls/4", openPR).on("POST repos/o/r/issues/4/comments", `{"html_url":"https://x/c"}`)
	cb := agent.Callbacks{Require: func(context.Context, string, string, string) bool { return true }}
	p := poller(f)
	p.conf.Repos[0].ReviewBot = &ReviewBot{Trigger: "review please", Login: "reviewer"}
	if res := p.runReviewBot(context.Background(), call(`{"number":4,"body":"something else"}`), cb); !isError(res) {
		t.Error("accepted a body")
	}
	p.runReviewBot(context.Background(), call(`{"number":4}`), cb)
	w := f.writes()
	if len(w) != 1 || w[0].body["body"] != "review please" || len(w[0].body) != 1 {
		t.Fatalf("wrote %+v", w)
	}
}
