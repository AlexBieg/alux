package prs

import (
	"regexp"
	"slices"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func numbers(items []Item) []int {
	var out []int
	for _, it := range items {
		out = append(out, it.Number)
	}
	return out
}

func review(author, state string, at time.Duration) Review {
	return Review{Author: author, State: state, SubmittedAt: ago(at)}
}

func TestApprovedByTeamCountsOnlyCurrentTeammateApprovals(t *testing.T) {
	team := []string{"me", "sam", "jen"}
	cases := []struct {
		name    string
		author  string
		reviews []Review
		want    bool
	}{
		{"teammate approved", "kim", []Review{review("sam", "APPROVED", time.Hour)}, true},
		{"bot approved", "kim", []Review{review("cursor", "APPROVED", time.Hour)}, false},
		{"outsider approved", "kim", []Review{review("lee", "APPROVED", time.Hour)}, false},
		{"author is a teammate approving own", "sam", []Review{review("sam", "APPROVED", time.Hour)}, false},
		{"later changes requested withdraws", "kim", []Review{review("sam", "APPROVED", 2*time.Hour), review("sam", "CHANGES_REQUESTED", time.Hour)}, false},
		{"later comment keeps approval", "kim", []Review{review("sam", "APPROVED", 2*time.Hour), review("sam", "COMMENTED", time.Hour)}, true},
		{"dismissed", "kim", []Review{review("jen", "APPROVED", 2*time.Hour), review("jen", "DISMISSED", time.Hour)}, false},
		{"only commented", "kim", []Review{review("jen", "COMMENTED", time.Hour)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := approvedByTeam(PR{Author: c.author, Reviews: c.reviews}, team); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestApprovedUnlessRequestedKeepsOnlyWhatIWasAskedFor(t *testing.T) {
	approved := []Review{review("lee", "APPROVED", time.Hour)}
	cases := []struct {
		name      string
		reviews   []Review
		requested []string
		kept      bool
	}{
		{"anyone approved, not asked", approved, nil, false},
		{"approved, asked by name", approved, []string{"sam", "me"}, true},
		{"approved, others asked", approved, []string{"sam"}, false},
		{"only a bot approved", []Review{{Author: "cursor", Bot: true, State: "APPROVED", SubmittedAt: ago(time.Hour)}}, nil, true},
		{"approval withdrawn", []Review{review("lee", "APPROVED", 2*time.Hour), review("lee", "DISMISSED", time.Hour)}, nil, true},
		{"author approving own", []Review{review("kim", "APPROVED", time.Hour)}, nil, true},
		{"unreviewed", nil, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := PR{Number: 1, Author: "kim", Reviews: c.reviews, Requested: c.requested}
			q := Classify(Fetched{
				Repo:   Repo{Review: Kind{Queries: []Query{{Search: "label:core"}}, Exclude: []Exclude{{ApprovedUnlessRequested: true}}}},
				Viewer: "me", Review: [][]PR{{p}},
			})
			if kept := len(q.Review) == 1; kept != c.kept {
				t.Errorf("kept = %v, want %v", kept, c.kept)
			}
		})
	}
}

func TestClassifyReviewQueue(t *testing.T) {
	repo := Repo{
		Owner: "acme", Name: "web", Team: "acme/core",
		Review: Kind{
			Queries: []Query{{Search: "review-requested:@me"}, {Search: "label:core"}, {Search: "CORE in:title", TitleMatches: regexp.MustCompile(`\bCORE-\d+`)}},
			Exclude: []Exclude{
				{Draft: true},
				{Author: "jen", BodyMatches: regexp.MustCompile(`\bWIP\b`), UnlessLabel: "core"},
				{ApprovedByTeam: true},
			},
		},
	}
	routed := PR{Number: 1, Author: "kim", CreatedAt: ago(48 * time.Hour), LastCommitAt: ago(2 * time.Hour)}
	labelled := PR{Number: 2, Author: "lee", Labels: []string{"core"}, CreatedAt: ago(96 * time.Hour), LastCommitAt: ago(30 * time.Hour)}
	draft := PR{Number: 3, Author: "kim", Draft: true, Labels: []string{"core"}}
	jenWIP := PR{Number: 4, Author: "jen", Title: "[CORE-9] x", Body: "WIP do not review"}
	jenWIPLabelled := PR{Number: 5, Author: "jen", Body: "WIP", Labels: []string{"core"}, CreatedAt: ago(10 * time.Hour), LastCommitAt: ago(time.Hour)}
	teamApproved := PR{Number: 6, Author: "kim", Labels: []string{"core"}, Reviews: []Review{review("sam", "APPROVED", time.Hour)}}
	looseTitle := PR{Number: 7, Author: "kim", Title: "Score the core metric"}
	mineInQueue := PR{Number: 8, Author: "me", Labels: []string{"core"}}

	q := Classify(Fetched{
		Repo: repo, Viewer: "me", Team: []string{"me", "sam"},
		Review: [][]PR{{routed, labelled}, {labelled, draft, jenWIPLabelled, teamApproved, mineInQueue}, {jenWIP, looseTitle}},
	})

	if got, want := numbers(q.Review), []int{2, 1, 5}; !slices.Equal(got, want) {
		t.Fatalf("review = %v, want %v (longest idle first, nothing excluded leaking)", got, want)
	}
	if age, idle := q.Review[1].Age(now), q.Review[1].Idle(now); age != 48*time.Hour || idle != 2*time.Hour {
		t.Errorf("age/idle = %v/%v", age, idle)
	}
}

func TestClassifyMineByNextAction(t *testing.T) {
	repo := Repo{Mine: Kind{Queries: []Query{{Search: "author:@me"}}}}
	q := Classify(Fetched{Repo: repo, Viewer: "me", Mine: [][]PR{{
		{Number: 1, Draft: true},
		{Number: 2, ReviewDecision: "REVIEW_REQUIRED", Checks: "SUCCESS"},
		{Number: 3, ReviewDecision: "APPROVED", Checks: "FAILURE"},
		{Number: 4, ReviewDecision: "APPROVED", Checks: "SUCCESS"},
		{Number: 5, ReviewDecision: "CHANGES_REQUESTED", Checks: "FAILURE"},
		{Number: 6, ReviewDecision: "APPROVED", Checks: "PENDING"},
		{Number: 7, ReviewDecision: "REVIEW_REQUIRED", Checks: "PENDING"},
		{Number: 8, ReviewDecision: "CHANGES_REQUESTED", Checks: "FAILURE", Mergeable: "CONFLICTING"},
		{Number: 9, ReviewDecision: "APPROVED", Checks: "SUCCESS", Mergeable: "UNKNOWN"},
		{Number: 10, Draft: true, Mergeable: "CONFLICTING"},
	}}})

	want := map[int]Status{1: StatusDraft, 2: StatusWaiting, 3: StatusFailing, 4: StatusReady, 5: StatusChangesRequested, 6: StatusWaitingCI, 7: StatusWaiting,
		8: StatusConflict, 9: StatusReady, 10: StatusConflict}
	for _, it := range q.Mine {
		if it.Status != want[it.Number] {
			t.Errorf("#%d = %q, want %q", it.Number, it.Status, want[it.Number])
		}
	}
	if got := numbers(q.Mine); got[0] != 4 || got[len(got)-1] != 1 {
		t.Errorf("order = %v, want ready first and drafts last", got)
	}
}

func TestFlagsSeePRsTheQueueExcluded(t *testing.T) {
	repo := Repo{
		Team:   "acme/core",
		Review: Kind{Queries: []Query{{Search: "label:core"}}, Exclude: []Exclude{{ApprovedByTeam: true}}},
		Mine:   Kind{Queries: []Query{{Search: "author:@me"}}},
		Flags:  []Flag{{Kind: FlagPendingReview}, {Kind: FlagLabelOnRed, Label: "merge-queue"}},
	}
	approvedWithMyDraft := PR{Number: 1, Author: "kim", Reviews: []Review{
		review("sam", "APPROVED", time.Hour), {Author: "me", State: "PENDING"},
	}}
	someoneElsesDraft := PR{Number: 2, Author: "kim", Reviews: []Review{{Author: "sam", State: "PENDING"}}}
	queuedRed := PR{Number: 3, Author: "me", Labels: []string{"merge-queue"}, Checks: "FAILURE"}
	queuedGreen := PR{Number: 4, Author: "me", Labels: []string{"merge-queue"}, Checks: "SUCCESS"}

	q := Classify(Fetched{
		Repo: repo, Viewer: "me", Team: []string{"sam"},
		Review: [][]PR{{approvedWithMyDraft, someoneElsesDraft}},
		Mine:   [][]PR{{queuedRed, queuedGreen}},
	})

	if len(q.Review) != 1 || q.Review[0].Number != 2 {
		t.Fatalf("review = %v, want only #2", numbers(q.Review))
	}
	var got []int
	for _, f := range q.Flags {
		got = append(got, f.Number)
	}
	if !slices.Equal(got, []int{1, 3}) {
		t.Errorf("flagged %v, want #1 (my pending review) and #3 (merge-queue on red)", got)
	}
}
