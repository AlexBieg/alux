package prs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var testRepo = Repo{
	Owner: "acme", Name: "web", Team: "acme/core",
	Review: Kind{Queries: []Query{{Search: "label:core"}}},
	Mine:   Kind{Queries: []Query{{Search: "author:@me"}}},
}

// A deleted author, no checks, the viewer's unsubmitted review, and a capped search.
const response = `{"data": {
  "viewer": {"login": "me"},
  "team": {"team": {"members": {"nodes": [{"login": "me"}, {"login": "sam"}]}}},
  "review0": {"issueCount": 60, "nodes": [{
    "number": 7, "title": "[CORE-1] x", "body": "b", "url": "https://github.com/acme/web/pull/7",
    "isDraft": false, "createdAt": "2026-10-01T10:00:00Z", "reviewDecision": "REVIEW_REQUIRED",
    "author": null,
    "labels": {"nodes": [{"name": "core"}]},
    "commits": {"nodes": [{"commit": {"committedDate": "2026-10-05T10:00:00Z", "statusCheckRollup": null}}]},
    "reviews": {"nodes": [{"author": {"login": "me"}, "state": "PENDING", "submittedAt": null},
      {"author": {"login": "cursor", "__typename": "Bot"}, "state": "APPROVED", "submittedAt": "2026-10-04T10:00:00Z"}]},
    "reviewRequests": {"nodes": [{"requestedReviewer": {"login": "me"}}, {"requestedReviewer": {}}]}
  }]},
  "mine0": {"issueCount": 1, "nodes": [{
    "number": 9, "title": "mine", "body": "", "url": "u", "isDraft": true, "createdAt": "2026-10-02T10:00:00Z",
    "reviewDecision": null, "mergeable": "CONFLICTING", "author": {"login": "me"}, "labels": {"nodes": []},
    "headRefName": "abc-1-ui", "headRefOid": "abc123", "baseRefName": "abc-1-api",
    "reviewThreads": {"nodes": [{"isResolved": false}, {"isResolved": true}, {"isResolved": false}]},
    "commits": {"nodes": [{"commit": {"committedDate": "2026-10-03T10:00:00Z", "statusCheckRollup": {"state": "FAILURE"}}}]},
    "reviews": {"nodes": []}
  }]}
}}`

func TestFetchDecodesGitHubAnswer(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	f, err := Fetch(context.Background(), func(_ context.Context, q string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, q)
		return []byte(response), nil
	}, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(asked, "\n")
	if !strings.Contains(all, `"repo:acme/web is:pr is:open label:core"`) || !strings.Contains(all, `team(slug: "core")`) {
		t.Errorf("queries did not scope the search or ask for the team:\n%s", all)
	}
	if len(asked) != 3 {
		t.Errorf("sent %d requests, want viewer and team plus one per search:\n%s", len(asked), all)
	}
	if f.Viewer != "me" || len(f.Team) != 2 || len(f.Partial) != 1 {
		t.Fatalf("viewer=%q team=%v partial=%v", f.Viewer, f.Team, f.Partial)
	}
	pr := f.Review[0][0]
	if pr.Author != "ghost" || pr.Checks != "" || pr.Repo != "acme/web" || pr.LastCommitAt.Day() != 5 {
		t.Errorf("review PR decoded as %+v", pr)
	}
	if r := pr.Reviews[0]; r.State != "PENDING" || !r.SubmittedAt.IsZero() {
		t.Errorf("pending review decoded as %+v", r)
	}
	if !pr.Reviews[1].Bot || pr.Reviews[0].Bot {
		t.Errorf("bot reviews not told apart: %+v", pr.Reviews)
	}
	if len(pr.Requested) != 1 || pr.Requested[0] != "me" {
		t.Errorf("requested = %v, want only the user asked by name (a team request decodes empty)", pr.Requested)
	}
	if mine := f.Mine[0][0]; !mine.Draft || mine.Checks != "FAILURE" || mine.Mergeable != "CONFLICTING" {
		t.Errorf("mine decoded as %+v", mine)
	}
	if mine := f.Mine[0][0]; mine.Head != "abc-1-ui" || mine.Base != "abc-1-api" || mine.HeadSHA != "abc123" || mine.Unresolved != 2 {
		t.Errorf("mine's branches and threads decoded as head %q base %q sha %q unresolved %d", mine.Head, mine.Base, mine.HeadSHA, mine.Unresolved)
	}
}

func TestFetchReadsReviewsAndThreadsPastTheFirstPage(t *testing.T) {
	first := `{"data": {"viewer": {"login": "me"}, "team": {"team": {"members": {"nodes": []}}},
  "review0": {"issueCount": 1, "nodes": [{"number": 7, "title": "t", "url": "u", "createdAt": "2026-10-01T10:00:00Z",
    "labels": {"nodes": []}, "commits": {"nodes": []},
    "reviewThreads": {"pageInfo": {"hasNextPage": true, "endCursor": "t1"}, "nodes": [{"isResolved": true}]},
    "reviews": {"pageInfo": {"hasNextPage": true, "endCursor": "r1"}, "nodes": [{"author": {"login": "ci", "__typename": "Bot"}, "state": "COMMENTED", "submittedAt": "2026-10-02T10:00:00Z"}]}}]},
  "mine0": {"issueCount": 0, "nodes": []}}}`
	f, err := Fetch(context.Background(), func(_ context.Context, q string) ([]byte, error) {
		switch {
		case strings.Contains(q, `pullRequest(number: 7) { reviews(first: 100, after: "r1")`):
			return []byte(`{"data": {"repository": {"pullRequest": {"reviews": {"pageInfo": {"hasNextPage": false}, "nodes": [
  {"author": {"login": "sam"}, "state": "APPROVED", "submittedAt": "2026-10-03T10:00:00Z"}]}}}}}`), nil
		case strings.Contains(q, `pullRequest(number: 7) { reviewThreads(first: 100, after: "t1")`):
			return []byte(`{"data": {"repository": {"pullRequest": {"reviewThreads": {"pageInfo": {"hasNextPage": false}, "nodes": [{"isResolved": false}]}}}}}`), nil
		}
		return []byte(first), nil
	}, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	pr := f.Review[0][0]
	if len(pr.Reviews) != 2 || pr.Reviews[1].Author != "sam" || pr.Reviews[1].State != "APPROVED" || pr.Unresolved != 1 {
		t.Errorf("reviews=%+v unresolved=%d; want sam's approval and the thread from page two", pr.Reviews, pr.Unresolved)
	}
}

func TestFetchRefusesUnresolvedTeam(t *testing.T) {
	_, err := Fetch(context.Background(), func(context.Context, string) ([]byte, error) {
		return []byte(`{"data": {"viewer": {"login": "me"}, "team": {"team": null}}}`), nil
	}, testRepo)
	if err == nil || !strings.Contains(err.Error(), "acme/core") {
		t.Fatalf("err = %v, want a refusal naming the team", err)
	}
}

func TestFetchRetriesASearchGitHubGaveUpOnOnce(t *testing.T) {
	old := retryWait
	retryWait = 0
	t.Cleanup(func() { retryWait = old })
	var tries atomic.Int32
	f, err := Fetch(context.Background(), func(_ context.Context, q string) ([]byte, error) {
		if strings.Contains(q, "mine0:") && tries.Add(1) == 1 {
			return nil, errors.New("gh: gh: HTTP 502")
		}
		return []byte(response), nil
	}, testRepo)
	if err != nil || len(f.Mine[0]) != 1 {
		t.Fatalf("err = %v, mine = %v; want the 502 retried and the search read", err, f.Mine)
	}
}

func TestFetchSurfacesGraphQLErrors(t *testing.T) {
	_, err := Fetch(context.Background(), func(context.Context, string) ([]byte, error) {
		return []byte(`{"data": null, "errors": [{"message": "Could not resolve to an Organization"}]}`), nil
	}, testRepo)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Fatalf("err = %v", err)
	}
}
