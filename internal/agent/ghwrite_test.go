package agent

import (
	"context"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
)

func TestGitHubWritesWithNoHarnessToolAreSingledOut(t *testing.T) {
	a := &Agent{}
	writes := []string{
		"gh run rerun 1 -R acme/app --failed",
		"gh -R o/r run cancel 1",
		"gh pr merge 12 --squash",
		"gh pr close 12",
		"gh workflow run deploy.yml",
		"gh issue comment 4 -b hi",
		"gh release create v1",
		"gh api -X POST repos/o/r/pulls/1/requested_reviewers -f reviewers[]=x",
		"gh api --method=PATCH repos/o/r/pulls/comments/9 -f body=x",
		"gh api -XDELETE repos/o/r/git/refs/heads/x",
		"gh api repos/o/r/labels -f name=x",
		"gh api repos/o/r/dispatches --input body.json",
		`gh api graphql -f query='mutation { enablePullRequestAutoMerge(input: {}) { clientMutationId } }'`,
		"jj log && gh run rerun 1",
		`sh -c "gh run rerun 1"`,
	}
	for _, command := range writes {
		if writesToGitHub(a.intent(bashCall(command)), command) == "" {
			t.Errorf("not singled out: %s", command)
		}
	}
	reads := []string{
		"gh run view 1 --log-failed",
		"gh run watch 1 --exit-status",
		"gh run list --workflow ci.yml",
		"gh pr view 12 --json reviews",
		"gh pr checks 12",
		"gh api repos/o/r/actions/runs/1 --jq .status",
		`gh api -X GET repos/o/r/actions/workflows/1/runs -f created=2026-10-16 -f per_page=100`,
		`gh api graphql -f query='query { repository(owner:"o", name:"r") { id } }'`,
		"gh search prs --repo o/r is:open",
		`jj describe -m "ask before gh run rerun"`,
	}
	for _, command := range reads {
		if why := writesToGitHub(a.intent(bashCall(command)), command); why != "" {
			t.Errorf("singled out %s: %s", command, why)
		}
	}
}

func TestAGitHubWriteAsksEvenSandboxedWithAPassingScreen(t *testing.T) {
	for _, approve := range []bool{true, false} {
		a, _, r := sandboxedAgent(t, shellTurn("bash", "gh run rerun 1 --failed"), mockapi.Turn{Text: "done"})
		requires := 0
		cb := (&recorder{}).callbacks()
		cb.Require = func(context.Context, string, string, string) bool { requires++; return approve }
		a.RunTurn(context.Background(), user("go"), cb)
		if requires != 1 {
			t.Errorf("approve=%v: asked %d times, want once", approve, requires)
		}
		if _, ran := r["t1"]; ran != approve {
			t.Errorf("approve=%v: ran=%v", approve, ran)
		}
	}
}

func TestAGitHubWriteIsRefusedWithNobodyToAsk(t *testing.T) {
	a, _, r := sandboxedAgent(t, shellTurn("bash", "gh pr merge 3"), mockapi.Turn{Text: "done"})
	cb := (&recorder{}).callbacks()
	cb.Require = nil
	a.RunTurn(context.Background(), user("go"), cb)
	if _, ran := r["t1"]; ran {
		t.Error("a merge ran with nobody to ask")
	}
}
