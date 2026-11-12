package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/session"
	"github.com/charmbracelet/x/ansi"
)

func topRow(m *Model) string {
	return strings.TrimRight(strings.SplitN(screen(m), "\n", 2)[0], " ")
}

func queue(review int, mine ...prs.Status) prs.Queue {
	var q prs.Queue
	for range review {
		q.Review = append(q.Review, prs.Item{})
	}
	for _, s := range mine {
		q.Mine = append(q.Mine, prs.Item{Status: s})
	}
	return q
}

func TestPRRowAbsentUntilFirstPoll(t *testing.T) {
	m, _ := harness(t, 100, 20)
	if strings.Contains(topRow(m), "PRs") {
		t.Fatalf("top row before any poll: %q", topRow(m))
	}
}

func TestPRRowSumsReposAndNamesWhatNeedsTheUser(t *testing.T) {
	m, _ := harness(t, 120, 20)
	withFlag := queue(3, prs.StatusFailing, prs.StatusConflict, prs.StatusChangesRequested, prs.StatusWaiting, prs.StatusDraft)
	withFlag.Flags = []prs.FlagHit{{Kind: prs.FlagPendingReview, Number: 7}}
	send(m, PRsMsg{States: []prs.RepoState{
		{Repo: "acme/web", Queue: withFlag},
		{Repo: "acme/api", Queue: queue(2, prs.StatusReady, prs.StatusFailing)},
	}})

	if got, want := topRow(m), "PRs ⚑1 · review 5 · mine 7 ✓1 ≠1 ✗1 ●2 ◷1 ○1"; !strings.HasSuffix(got, want) {
		t.Fatalf("top row = %q, want it to end %q", got, want)
	}
}

// A glyph a terminal draws two cells wide would shift every column and the summary's click target.
func TestStatusIconsAreOneCellWide(t *testing.T) {
	for _, g := range []string{iconReady, iconConflict, iconChanges, iconFailing, iconRunning, iconWaiting, iconDraft, "⚑"} {
		if w := ansi.StringWidth(g); w != 1 {
			t.Errorf("%q is %d cells wide", g, w)
		}
	}
}

func TestPRRowTotalCountsDraftsAndCIRuns(t *testing.T) {
	m, _ := harness(t, 100, 20)
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: queue(0, prs.StatusWaitingCI, prs.StatusWaitingCI, prs.StatusDraft)}}})
	if got, want := topRow(m), "PRs review 0 · mine 3 ↻2 ○1"; !strings.HasSuffix(got, want) {
		t.Fatalf("top row = %q, want it to end %q", got, want)
	}
}

func TestPRRowSaysStaleWhenThePollFailed(t *testing.T) {
	m, _ := harness(t, 100, 20)
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: queue(1), Err: errors.New("gh: HTTP 502")}}})
	if !strings.HasSuffix(topRow(m), "review 1 · mine 0 stale") {
		t.Fatalf("top row = %q", topRow(m))
	}
}

func TestClickingThePRSummaryOpensThePicker(t *testing.T) {
	m, s := harness(t, 120, 20)
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: queue(4)}}})

	top := topRow(m)
	start := strings.Index(top, "PRs")
	clickAt(m, ansi.StringWidth(top[:start])-1, 0)
	if m.picker != nil {
		t.Fatal("a click beside the summary opened the picker")
	}
	clickAt(m, ansi.StringWidth(top[:start])+5, 0)
	if m.picker == nil || len(s.Snapshot().Queued) != 0 {
		t.Fatalf("picker=%v queued=%v, want the picker open and nothing queued", m.picker, s.Snapshot().Queued)
	}
}

// Wired as main wires it: the poller starts before the program runs, so its first update is sent early.
func TestEndToEndPollReachesTheTopRow(t *testing.T) {
	conf, _, err := prs.Parse([]byte(`{"prs": {"repos": {"acme/web": {}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	answer := `{"data": {"viewer": {"login": "me"},
	  "review0": {"issueCount": 1, "nodes": [{"number": 3, "title": "t", "author": {"login": "kim"},
	    "createdAt": "2026-10-01T00:00:00Z", "labels": {"nodes": []}, "commits": {"nodes": []}, "reviews": {"nodes": []}}]},
	  "mine0": {"issueCount": 0, "nodes": []}}}`
	poller := prs.NewPoller(conf, func(context.Context, string) ([]byte, error) { return []byte(answer), nil }, nil)

	var program *tea.Program
	poller.OnUpdate = func() {
		if program != nil {
			program.Send(PRsMsg{States: poller.Snapshot()})
		}
	}
	in, _ := io.Pipe()
	out := &safeBuf{}
	program = tea.NewProgram(New(session.New(&agent.Agent{}, nil)), tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(100, 30))
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	poller.Start(ctx)

	finished := make(chan struct{})
	go func() {
		_, _ = program.Run()
		close(finished)
	}()
	waitFor(t, out, "PRs review 1 · mine 0", "the poll never reached the top row")
	program.Quit()
	<-finished
}
