package agent

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestShellCommandsThatPostToAPullRequestAreRefused(t *testing.T) {
	a := &Agent{}
	refused := []string{
		"gh pr review 12 --comment -b looks-good",
		"gh pr review 12 --approve",
		"gh pr comment 12 -b hi",
		"gh -R o/r pr review 12 --request-changes -b no",
		"gh api repos/o/r/pulls/12/reviews -f event=COMMENT -f body=x",
		"gh api --method POST /repos/o/r/pulls/12/reviews/99/events -f event=APPROVE",
		"gh api repos/o/r/pulls/12/comments -f body=x",
		"gh api repos/o/r/issues/12/comments -f body=x",
		`gh api graphql -f query='mutation { submitPullRequestReview(input: {}) { clientMutationId } }'`,
		"curl -X POST https://api.github.com/repos/o/r/pulls/1/reviews -d '{}'",
		"jj log && gh pr review 3 --approve",
		"gh pr review 1 --approve > /dev/null",
		`echo "$(gh api repos/o/r/pulls/1/reviews -f event=APPROVE)"`,
		"gh api -X POST repos/o/r/pulls/1/comments/9/replies -f body=x",
		`gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: "PRRT_1"}) { thread { isResolved } } }'`,
		`gh pr comment 4 --body "bugbot run"`,
		"gh pr create --draft --title t --body-file b.md --attach /tmp/demo.mp4",
		"gh pr new -t t -b b",
		"gh pr edit 12 --add-label ready",
		"gh -R o/r pr ready 12",
		`sh -c "gh pr edit 3 --body x"`,
	}
	for _, command := range refused {
		if postsToPullRequest(a.intent(bashCall(command)), command) == "" {
			t.Errorf("not refused: %s", command)
		}
	}
	allowed := []string{
		"gh pr view 12",
		"gh pr diff 12",
		"gh pr checks 12",
		"gh pr list --author @me",
		"gh api repos/o/r/pulls/12",
		`jj describe -m "refuse gh pr review from agents"`,
		"go test ./internal/prs/...",
	}
	for _, command := range allowed {
		if why := postsToPullRequest(a.intent(bashCall(command)), command); why != "" {
			t.Errorf("refused %s: %s", command, why)
		}
	}
}

func TestTheMainLoopRefusesPostingEvenWithAPassingScreen(t *testing.T) {
	ran := false
	a := &Agent{Handlers: map[string]Handler{"bash": func(context.Context, Call, Callbacks) anthropic.ContentBlockParamUnion {
		ran = true
		return ok("t1", "")
	}}, Screen: &fakeScreen{verdict: passing}}
	res := a.runTool(context.Background(), bashCall("gh pr review 12 --comment -b x"), Callbacks{
		Approve:      func(context.Context, string, string) bool { return true },
		OnToolResult: func(string, string, string, ToolKind) {},
	})
	if ran {
		t.Error("a review was posted through the shell")
	}
	if res.OfToolResult == nil || !res.OfToolResult.IsError.Value {
		t.Error("the refusal was not reported as an error")
	}
}
