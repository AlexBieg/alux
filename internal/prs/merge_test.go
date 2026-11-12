package prs

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func mergeDetail(edit func(*Detail)) Detail {
	d := Detail{Viewer: "me", State: "OPEN", Mergeable: "MERGEABLE",
		PR: PR{Author: "me", ReviewDecision: "APPROVED", Checks: "PENDING", Labels: []string{"core"}}}
	if edit != nil {
		edit(&d)
	}
	return d
}

func TestMergeBlocker(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Detail)
		want string
	}{
		{"approved, clean and still running", nil, ""},
		{"someone else's", func(d *Detail) { d.Author = "kim" }, "kim's, not yours"},
		{"merged", func(d *Detail) { d.State = "MERGED" }, "it is merged"},
		{"draft", func(d *Detail) { d.Draft = true }, "draft"},
		{"already queued", func(d *Detail) { d.Labels = append(d.Labels, "merge-queue") }, "already has merge-queue"},
		{"awaiting review", func(d *Detail) { d.ReviewDecision = "REVIEW_REQUIRED" }, "not approved"},
		{"changes requested", func(d *Detail) { d.ReviewDecision = "CHANGES_REQUESTED" }, "not approved"},
		{"conflicting", func(d *Detail) { d.Mergeable = "CONFLICTING" }, "merge conflicts"},
		{"mergeability unknown", func(d *Detail) { d.Mergeable = "UNKNOWN" }, "has not worked out"},
		{"a failing check", func(d *Detail) { d.FailingChecks = []string{"lint"} }, "failing: lint"},
		{"a red rollup", func(d *Detail) { d.Checks = "ERROR" }, "checks are failing"},
	} {
		got := mergeBlocker(mergeDetail(tc.edit), "merge-queue")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: blocker %q, want %q", tc.name, got, tc.want)
		}
	}
}

func mergeResponse(author, mergeable string) string {
	return fmt.Sprintf(`{"data": {"viewer": {"login": "me"}, "repository": {"pullRequest": {
  "number": 12, "title": "t", "isDraft": false, "state": "OPEN", "reviewDecision": "APPROVED", "mergeable": %q,
  "author": {"login": %q}, "labels": {"nodes": []},
  "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "PENDING", "contexts": {"nodes": [
    {"__typename": "CheckRun", "name": "e2e", "status": "IN_PROGRESS", "conclusion": null}
  ]}}}}]}
}}}}`, mergeable, author)
}

func TestAddMergeLabelRereadsThePRAndLabelsOnlyWhenItPasses(t *testing.T) {
	for _, tc := range []struct {
		author, mergeable string
		labelled          bool
	}{
		{"me", "MERGEABLE", true},
		{"kim", "MERGEABLE", false},
		{"me", "CONFLICTING", false},
	} {
		f := &fakeGitHub{}
		f.on("POST repos/o/r/issues/12/labels", `[]`)
		p := &Poller{conf: Config{Repos: []Repo{{Owner: "o", Name: "r", MergeLabel: "merge-queue"}}}, send: f.send,
			gql: func(context.Context, string) ([]byte, error) {
				return []byte(mergeResponse(tc.author, tc.mergeable)), nil
			}}
		label, err := p.AddMergeLabel(context.Background(), "o/r", 12)
		if got := len(f.writes()) == 1; got != tc.labelled || (err == nil) != tc.labelled {
			t.Errorf("%s/%s: labelled %v, err %v; want labelled %v", tc.author, tc.mergeable, got, err, tc.labelled)
		}
		if tc.labelled && label != "merge-queue" {
			t.Errorf("label = %q", label)
		}
	}
}

func TestMergeReadyNeedsTheRepoToNameItsLabel(t *testing.T) {
	p := &Poller{conf: Config{Repos: []Repo{{Owner: "o", Name: "r"}}},
		gql: func(context.Context, string) ([]byte, error) {
			t.Fatal("fetched with no merge label configured")
			return nil, nil
		}}
	_, err := p.MergeReady(context.Background(), "o/r", 12)
	if err == nil || !strings.Contains(err.Error(), `set "merge_label" under prs.repos.o/r`) {
		t.Errorf("err = %v", err)
	}
}
