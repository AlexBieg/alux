package ui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/vcs"
	"github.com/charmbracelet/x/ansi"
)

type fakeLand struct {
	rows  []orchestrator.Unlanded
	view  orchestrator.RepoView
	calls int
	stats []string
	diffs map[string]string
	pr    bool
	acts  []string
	said  []string
	held  map[string]orchestrator.Held
}

func (f *fakeLand) Sweep() ([]string, map[string]orchestrator.Held, error) {
	f.acts = append(f.acts, "sweep")
	said := f.said
	f.said = nil
	return said, f.held, nil
}

func (f *fakeLand) PRFlow() bool { return f.pr }

func (f *fakeLand) Push(workspace, change string) (string, error) {
	f.acts = append(f.acts, "push "+workspace+" "+change)
	return "Pushed to origin:\n  bookmark: f1 [add to abc]", nil
}

func (f *fakeLand) Rebase(workspace string) (string, error) {
	f.acts = append(f.acts, "rebase "+workspace)
	return "Fetched origin and rebased " + workspace + "'s 2 changes onto main@origin.", nil
}

func (f *fakeLand) List() ([]orchestrator.Unlanded, error) {
	f.calls++
	return f.rows, nil
}

func (f *fakeLand) Workspaces() (orchestrator.RepoView, error) { return f.view, nil }

func (f *fakeLand) Stat(change string) (string, error) {
	f.stats = append(f.stats, change)
	return "STAT for " + change + "\nb.txt | 1 +", nil
}

func (f *fakeLand) Diff(change string) (string, error) { return f.diffs[change], nil }

// gitDiff is a git diff of one file whose single hunk adds n lines.
func gitDiff(path string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, path, n)
	for i := range n {
		fmt.Fprintf(&b, "+%s line %d\n", path, i)
	}
	return b.String()
}

func workspaceModel(t *testing.T) (*Model, *fakeLand) {
	t.Helper()
	m, _ := pickerModel(t)
	src := &fakeLand{
		rows: []orchestrator.Unlanded{{Change: "leftover", Age: "1 hour ago", Description: "left by an ended session"}},
		view: orchestrator.RepoView{Name: "alux", Workspaces: []orchestrator.WorkspaceView{
			{Name: "default", Changes: []orchestrator.WorkspaceChange{{Change: "defaultwc", WorkingCopy: true, Empty: true, Age: "1m"}}},
			{Name: "pool-1", Changes: []orchestrator.WorkspaceChange{
				{Commit: "c-live", Change: "livework", WorkingCopy: true, Description: "being worked right now", Age: "2m"},
			}},
			{Name: "pool-2", Changes: []orchestrator.WorkspaceChange{
				{Change: "freshwc", WorkingCopy: true, Empty: true, Age: "3m"},
				{Commit: "c-left", Change: "leftover", Description: "left by an ended session", Age: "1h"},
			}},
		}, Graph: []orchestrator.GraphRow{
			{Text: "@  livework pool-1@ being worked right now", Commit: "c-live", Indent: 3},
			{Text: "│ ○  leftover left by an ended session", Commit: "c-left", Indent: len("│ ○  ")},
			{Text: "├─╯"},
			{Text: "◆  trunkfork merged upstream", Commit: "c-trunk", Indent: len("◆  ")},
		}},
	}
	m.Land = src
	m.Registry = registry.New(t.TempDir(), "alux", "/tmp/alux")
	ws := "pool-1"
	m.Registry.Publish(registry.Patch{Workspace: &ws})
	settle(m)
	return m, src
}

func openWorkspacesByClick(t *testing.T, m *Model) {
	t.Helper()
	row := rowOf(t, m, dot+" alux")
	clickAt(m, screenXOf(t, m, row, "alux"), row)
	if m.wsPane == nil {
		t.Fatalf("clicking the session row did not open the workspace view:\n%s", screen(m))
	}
	runCmd(m, m.loadWorkspaces())
}

func TestASessionRowOpensTheWorkspaceCards(t *testing.T) {
	m, _ := workspaceModel(t)
	openWorkspacesByClick(t, m)
	out := screen(m)
	for _, want := range []string{"default", "your working copy", "pool-1", "● alux (this) · idle", "pool-2", "free",
		"livework", "being worked right now", "⇣ land", "1 to land"} {
		if !strings.Contains(out, want) {
			t.Errorf("workspace view is missing %q:\n%s", want, out)
		}
	}
}

// said is whether a queued notice holds every part, read from the queue because the bubble wraps it.
func said(m *Model, parts ...string) bool {
	for _, n := range m.notices {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(n.text, p)
		}
		if all {
			return true
		}
	}
	return false
}

func TestEnterLandsAnEndedSessionsChangeButNotLiveWork(t *testing.T) {
	m, _ := workspaceModel(t)
	openWorkspacesByClick(t, m)
	if pick, _ := m.wsPane.selected(); pick.change.Change != "leftover" {
		t.Fatalf("the cursor starts on %q, want the first change that can land", pick.change.Change)
	}

	press(m, "up")
	press(m, "up")
	press(m, "enter")
	if m.wsPane == nil || len(m.session.Snapshot().Queued) != 0 {
		t.Fatal("enter on another workspace's live work queued a landing")
	}
	if !said(m, "livework is live work in pool-1") {
		t.Errorf("no notice says why live work cannot land:\n%s", screen(m))
	}

	press(m, "down")
	press(m, "down")
	press(m, "enter")
	q := m.session.Snapshot().Queued
	if m.wsPane != nil || len(q) != 1 || q[0].Text != "/land leftover" {
		t.Fatalf("enter on the ended session's change queued %+v, want /land leftover and the view closed", q)
	}
}

func TestTheGraphSelectsCardChangesShowsEachOnesChipsAndLandsIt(t *testing.T) {
	m, _ := workspaceModel(t)
	openWorkspacesByClick(t, m)
	press(m, "t")
	out := screen(m)
	if !strings.Contains(out, "❯ │ ○  leftover") || !strings.Contains(out, "trunkfork merged upstream") {
		t.Fatalf("the graph should open on leftover, the change that can land, and draw the fork point:\n%s", out)
	}
	if !strings.Contains(out, "  │ │  pool-2 · ⇣ land · 1h") || !strings.Contains(out, "  │  pool-1 (this) · 2m") {
		t.Errorf("every selectable row should show its chips under it, inside the graph's rail, and this session's as (this):\n%s", out)
	}
	if strings.Contains(out, "pool-2 (this)") || !m.ownWorkspace("pool-1") || m.ownWorkspace("pool-2") {
		t.Errorf("only pool-1, which this session holds, should be marked as its own:\n%s", out)
	}
	if !strings.Contains(out, "t cards") {
		t.Errorf("the graph's hint does not offer the way back to the cards:\n%s", out)
	}

	press(m, "down")
	if pick, _ := m.wsPane.selected(); pick.change.Change != "leftover" {
		t.Errorf("down from the last card change moved to %q; the connector and trunk rows are not picks", pick.change.Change)
	}
	press(m, "up")
	if pick, _ := m.wsPane.selected(); pick.change.Change != "livework" {
		t.Fatalf("up from leftover selected %q, want livework, the row above it", pick.change.Change)
	}

	press(m, "down")
	press(m, "enter")
	q := m.session.Snapshot().Queued
	if m.wsPane != nil || len(q) != 1 || q[0].Text != "/land leftover" {
		t.Fatalf("enter on leftover in the graph queued %+v, want /land leftover", q)
	}
}

func TestGoingBackUpAfterScrollingShowsTheTopCardAgainInBothViews(t *testing.T) {
	for _, graph := range []bool{false, true} {
		m, src := workspaceModel(t)
		src.view.Workspaces[0].Changes[0].Commit = "c-default"
		src.view.Graph = append([]orchestrator.GraphRow{{Text: "@  defaultwc default@ (empty)", Commit: "c-default", Indent: len("@  ")}}, src.view.Graph...)
		for i := range 30 {
			c := orchestrator.WorkspaceChange{Commit: fmt.Sprintf("c-%d", i), Change: fmt.Sprintf("deep%04d", i), Description: "stacked work", Age: "1h"}
			src.view.Workspaces[2].Changes = append(src.view.Workspaces[2].Changes, c)
			src.view.Graph = append(src.view.Graph, orchestrator.GraphRow{Text: "○  " + c.Change + " stacked work", Commit: c.Commit, Indent: len("○  ")})
		}
		openWorkspacesByClick(t, m)
		if graph {
			press(m, "t")
		}
		for range 40 {
			press(m, "down")
		}
		if strings.Contains(screen(m), "defaultwc") {
			t.Fatalf("graph=%v: the pane never scrolled, so the test proves nothing:\n%s", graph, screen(m))
		}
		for range 40 {
			press(m, "up")
		}
		out := screen(m)
		if pick, _ := m.wsPane.selected(); pick.change.Change != "defaultwc" {
			t.Errorf("graph=%v: up to the top selected %q, want defaultwc", graph, pick.change.Change)
		}
		want := "defaultwc"
		if !graph {
			want = "your working copy"
		}
		if !strings.Contains(out, want) {
			t.Errorf("graph=%v: back at the top, %q is still scrolled out of view:\n%s", graph, want, out)
		}
	}
}

func TestEachSlotKeepsItsOwnGraphColourAndTintingKeepsTheText(t *testing.T) {
	seen := map[any]string{}
	for _, name := range []string{"default", "pool-1", "pool-2", "pool-3", "pool-4", "pool-5"} {
		c := workspaceColor(name)
		if other, ok := seen[c]; ok {
			t.Errorf("%s and %s share a colour", name, other)
		}
		seen[c] = name
		if workspaceColor(name) != c {
			t.Errorf("%s's colour changes between calls", name)
		}
	}
	text := "│ ○  leftover left by an ended session"
	tinted := tintNode(text, len("│ ○  "), lipgloss.NewStyle().Foreground(workspaceColor("pool-2")))
	if ansi.Strip(tinted) != text {
		t.Errorf("tinting changed the row's text: %q", ansi.Strip(tinted))
	}
}

func TestEnterOnADivergentChangeSaysSoInsteadOfLanding(t *testing.T) {
	m, src := workspaceModel(t)
	src.view.Workspaces[2].Changes[1].Divergent = true
	openWorkspacesByClick(t, m)
	if out := screen(m); !strings.Contains(out, "divergent") || strings.Contains(out, "⇣ land") {
		t.Fatalf("the divergent row should be tagged divergent with no land tag:\n%s", out)
	}
	for range 5 {
		if pick, _ := m.wsPane.selected(); pick.change.Change == "leftover" {
			break
		}
		press(m, "down")
	}
	if pick, _ := m.wsPane.selected(); pick.change.Change != "leftover" {
		t.Fatalf("could not select leftover; cursor is on %q", pick.change.Change)
	}
	press(m, "enter")
	if m.wsPane == nil || len(m.session.Snapshot().Queued) != 0 {
		t.Fatal("enter on a divergent change queued a landing")
	}
	if !strings.Contains(screen(m), "leftover is divergent") {
		t.Errorf("no notice says why a divergent change cannot land:\n%s", screen(m))
	}
}

func TestBareLandOpensTheWorkspaceCardsOnWhatCanLand(t *testing.T) {
	m, src := workspaceModel(t)
	src.rows[0].With = []string{"beneath"}
	runCmd(m, typeLine(m, "/land"))
	if m.wsPane == nil || len(m.session.Snapshot().Queued) != 0 || src.calls != 1 {
		t.Fatal("/land should open the workspace view and read the land list, without queueing a command")
	}
	if pick, _ := m.wsPane.selected(); pick.change.Change != "leftover" {
		t.Fatalf("the cursor starts on %q, want the first change that can land", pick.change.Change)
	}
	if out := screen(m); !strings.Contains(out, "⏎ also lands 1 change: beneath") {
		t.Errorf("the selected change does not say what lands with it:\n%s", out)
	}

	press(m, "enter")
	q := m.session.Snapshot().Queued
	if m.wsPane != nil || len(q) != 1 || q[0].Text != "/land leftover" {
		t.Fatalf("enter queued %+v, want /land leftover and the view closed", q)
	}
}

func TestSResumesAnEndedSessionsChangeButNotLiveOrOwnWork(t *testing.T) {
	m, _ := workspaceModel(t)
	runCmd(m, typeLine(m, "/land"))
	selectChange(t, m, "livework")
	press(m, "s")
	if !said(m, "livework is already in this session's workspace") || len(m.session.Snapshot().Queued) != 0 {
		t.Errorf("s on this session's own work did not say so:\n%s", screen(m))
	}
	selectChange(t, m, "defaultwc")
	press(m, "s")
	if len(m.session.Snapshot().Queued) != 0 {
		t.Error("s on the default workspace's change queued a resume")
	}
	selectChange(t, m, "leftover")
	press(m, "s")
	q := m.session.Snapshot().Queued
	if m.wsPane != nil || len(q) != 1 || q[0].Text != "/resume leftover" {
		t.Fatalf("s on the ended session's change queued %+v, want /resume leftover and the view closed", q)
	}
}

func TestAPRFlowRepoOpensTheCardsWithoutOfferingToLand(t *testing.T) {
	m, src := workspaceModel(t)
	src.pr, src.rows = true, nil
	runCmd(m, typeLine(m, "/land"))
	if m.wsPane == nil {
		t.Fatal("/land in a PR-flow repo did not open the workspace view")
	}
	out := screen(m)
	if strings.Contains(out, "⏎ land") || strings.Contains(out, "to land") {
		t.Errorf("a PR-flow repo's cards still offer landing:\n%s", out)
	}
	press(m, "enter")
	if m.wsPane == nil || len(m.session.Snapshot().Queued) != 0 {
		t.Fatalf("enter in a PR-flow repo queued %+v or closed the view", m.session.Snapshot().Queued)
	}
}

func TestAPRFlowCardDrawsTheStackWithWhereEachChangeStands(t *testing.T) {
	m, src := workspaceModel(t)
	src.pr, src.rows = true, nil
	src.view.Trunk = "main@origin"
	src.view.Workspaces[1] = orchestrator.WorkspaceView{Name: "pool-1", Behind: 14, Changes: []orchestrator.WorkspaceChange{
		{Change: "wipwipwi", WorkingCopy: true, Description: "wip", Age: "1m"},
		{Change: "uiuiuiui", Description: "Search filter UI", Age: "2m", Ship: orchestrator.Ship{Bookmark: "abc-612-ui", Ahead: 1}},
		{Change: "apiapiap", Description: "Search filter API", Age: "3m", Divergent: true, Ship: orchestrator.Ship{Pushed: true, Moved: "abc-612-api", MovedBy: 2}},
	}}
	runCmd(m, typeLine(m, "/land"))
	out := screen(m)
	for _, want := range []string{"pool-1", "main ↓14", "┌ @ wipwipwi", "local only", "├ · uiuiuiui", "abc-612-ui · ↑1 unpushed",
		"└ · apiapiap", "pushed · abc-612-api moved ↓2 past this · divergent"} {
		if !strings.Contains(out, want) {
			t.Errorf("the PR-flow card is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "⇣ land") {
		t.Errorf("a PR-flow row still offers to land:\n%s", out)
	}
}

func TestAPRFlowRowShowsTheOpenPRItsBookmarkHeads(t *testing.T) {
	m, src := workspaceModel(t)
	src.pr, src.rows = true, nil
	src.view.Trunk, src.view.Slug = "main@origin", "acme/app"
	src.view.Workspaces[1] = orchestrator.WorkspaceView{Name: "pool-1", Changes: []orchestrator.WorkspaceChange{
		{Change: "uiuiuiui", Description: "Search filter UI", Age: "2m", Ship: orchestrator.Ship{Bookmark: "abc-612-ui", Ahead: 1}},
		{Change: "apiapiap", Description: "Search filter API", Age: "3m", Ship: orchestrator.Ship{Bookmark: "abc-612-api"}},
	}}
	pr := func(n int, head, base string, f func(*prs.PR)) prs.Item {
		p := prs.PR{Repo: "acme/app", Number: n, Head: head, Base: base}
		f(&p)
		return prs.Item{PR: p}
	}
	m.Update(PRsMsg{States: []prs.RepoState{
		{Repo: "acme/app", Queue: prs.Queue{Mine: []prs.Item{
			pr(8813, "abc-612-ui", "main", func(p *prs.PR) { p.Draft, p.Checks, p.Unresolved = true, "FAILURE", 2 }),
			pr(8812, "abc-612-api", "main", func(p *prs.PR) { p.Checks, p.ReviewDecision = "SUCCESS", "APPROVED" }),
		}}},
		{Repo: "someone/else", Queue: prs.Queue{Mine: []prs.Item{pr(1, "abc-612-api", "main", func(*prs.PR) {})}}},
	}})
	runCmd(m, typeLine(m, "/land"))
	out := screen(m)
	for _, want := range []string{"┌ · #8813", "draft · CI ✗ · 2 threads · base main, stack says abc-612-api", "↑1 unpushed · 2m · uiuiuiui",
		"└ · #8812", "ready · approved · CI ✓ · 3m · apiapiap"} {
		if !strings.Contains(out, want) {
			t.Errorf("the PR-flow card is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#1 ") {
		t.Errorf("a PR from another repo with the same branch name was joined:\n%s", out)
	}
}

// shipModel is the workspace pane open on a PR-flow pool-1 stack of ui over api, api with an open PR.
func shipModel(t *testing.T, api orchestrator.Ship) (*Model, *fakeLand) {
	t.Helper()
	m, src := workspaceModel(t)
	src.pr, src.rows = true, nil
	src.view.Trunk, src.view.Slug = "main@origin", "acme/app"
	src.view.Workspaces[1] = orchestrator.WorkspaceView{Name: "pool-1", Changes: []orchestrator.WorkspaceChange{
		{Change: "uiuiuiui", Description: "Search filter UI", Age: "2m", Ship: orchestrator.Ship{Bookmark: "abc-612-ui"}},
		{Change: "apiapiap", Description: "Search filter API", Age: "3m", Ship: api},
	}}
	m.Update(PRsMsg{States: []prs.RepoState{{Repo: "acme/app", Queue: prs.Queue{Mine: []prs.Item{
		{PR: prs.PR{Repo: "acme/app", Number: 8812, Head: "abc-612-api", Base: "main"}}}}}}})
	runCmd(m, typeLine(m, "/land"))
	return m, src
}

func selectChange(t *testing.T, m *Model, change string) {
	t.Helper()
	for i, pick := range m.wsPane.picks {
		if pick.change.Change == change {
			m.wsPane.cursor = i
			return
		}
	}
	t.Fatalf("no row for %s", change)
}

func TestPPushesTheSelectedStackAndSaysWhatWent(t *testing.T) {
	m, src := shipModel(t, orchestrator.Ship{Bookmark: "abc-612-api"})
	selectChange(t, m, "uiuiuiui")
	runCmd(m, press(m, "p"))
	if len(src.acts) != 1 || src.acts[0] != "push pool-1 uiuiuiui" {
		t.Fatalf("p ran %v, want one push of pool-1's uiuiuiui", src.acts)
	}
	if !said(m, "Pushed to origin: · bookmark: f1") {
		t.Errorf("no notice says what the push sent: %+v", m.notices)
	}
}

func TestPAndURefuseAStackWhoseBookmarkSomeoneElseMoved(t *testing.T) {
	m, src := shipModel(t, orchestrator.Ship{Pushed: true, Moved: "abc-612-api", MovedBy: 1})
	selectChange(t, m, "uiuiuiui")
	press(m, "p")
	if !said(m, "not pushed: abc-612-api", "on top of apiapiap") {
		t.Errorf("no notice says why the push was refused: %+v", m.notices)
	}
	press(m, "u")
	if len(src.acts) != 0 {
		t.Errorf("a stack someone else moved was still sent to jj: %v", src.acts)
	}
}

func TestURebasesTheSelectedSlot(t *testing.T) {
	m, src := shipModel(t, orchestrator.Ship{Bookmark: "abc-612-api"})
	selectChange(t, m, "apiapiap")
	runCmd(m, press(m, "u"))
	if len(src.acts) != 1 || src.acts[0] != "rebase pool-1" {
		t.Fatalf("u ran %v, want a rebase of pool-1", src.acts)
	}
}

func TestOAsksTheAgentForADraftPROnlyForAPushedBookmarkWithoutOne(t *testing.T) {
	m, _ := shipModel(t, orchestrator.Ship{Bookmark: "abc-612-api"})
	selectChange(t, m, "apiapiap")
	press(m, "o")
	if !said(m, "#8812 is already open for abc-612-api") || len(m.session.Snapshot().Queued) != 0 {
		t.Errorf("o on a bookmark with an open PR did not say so:\n%s", screen(m))
	}

	m, _ = shipModel(t, orchestrator.Ship{Bookmark: "abc-612-api"})
	m.wsPane.view.Workspaces[1].Changes[0].Ship.NotOnRemote = true
	m.wsPane.picks = wsPicks(m.wsPane.view)
	selectChange(t, m, "uiuiuiui")
	press(m, "o")
	if !said(m, "press p first") || len(m.session.Snapshot().Queued) != 0 {
		t.Errorf("o on an unpushed bookmark did not ask for a push first:\n%s", screen(m))
	}

	m, _ = shipModel(t, orchestrator.Ship{Bookmark: "abc-612-api"})
	selectChange(t, m, "uiuiuiui")
	press(m, "o")
	q := m.session.Snapshot().Queued
	if m.wsPane != nil || len(q) != 1 || !strings.Contains(q[0].Text, "bookmark `abc-612-ui`") || !strings.Contains(q[0].Text, "base `abc-612-api`") {
		t.Fatalf("o queued %+v, want a draft-PR ask for abc-612-ui onto abc-612-api and the pane closed", q)
	}
}

func TestTheCleanupRunsAfterTheCardsAreDrawnAndRereadsThemOnlyWhenItAbandoned(t *testing.T) {
	m, src := workspaceModel(t)
	src.pr, src.rows = true, nil
	src.view.Trunk = "main@origin"
	src.view.Workspaces[1].Changes = []orchestrator.WorkspaceChange{
		{Change: "apiapiap", Description: "Search filter API", Age: "3m", Ship: orchestrator.Ship{Bookmark: "abc-612-api"}}}
	src.said = []string{"abandoned oldoldol (abc-500), merged into main"}
	src.held = map[string]orchestrator.Held{"apiapiap": {PR: 8812, Why: "this commit differs from the one that merged"}}
	runCmd(m, typeLine(m, "/land"))
	if len(src.acts) != 0 || !strings.Contains(screen(m), "apiapiap") {
		t.Fatalf("the cards were not drawn before the cleanup ran: acts %v\n%s", src.acts, screen(m))
	}

	sweep := m.sweepWorkspaces()
	reread := m.onWsSweep(sweep().(wsSweepMsg))
	if !said(m, "abandoned oldoldol (abc-500), merged into main") {
		t.Errorf("no notice says what the cleanup abandoned: %+v", m.notices)
	}
	if out := screen(m); !strings.Contains(out, "#8812 merged, kept: this commit differs") {
		t.Errorf("the pane is missing why apiapiap was kept:\n%s", out)
	}
	if reread == nil {
		t.Fatal("a cleanup that abandoned work did not reread the cards")
	}
	before := src.calls
	runCmd(m, m.readWorkspaces(false))
	if src.calls != before+1 || len(src.acts) != 1 {
		t.Errorf("the reread after a cleanup ran another cleanup: acts %v", src.acts)
	}
	if m.onWsSweep(sweep().(wsSweepMsg)) != nil && len(src.said) == 0 {
		t.Error("a cleanup that abandoned nothing still asked for a reread")
	}
}

func TestWorkNoSlotHoldsGetsItsOwnCardAndLands(t *testing.T) {
	m, src := workspaceModel(t)
	src.view.Workspaces[2].Changes = src.view.Workspaces[2].Changes[:1]
	src.view.Workspaces = append(src.view.Workspaces, orchestrator.WorkspaceView{Name: orchestrator.EndedSessions,
		Changes: []orchestrator.WorkspaceChange{{Change: "leftover", Description: "left by an ended session", Age: "1h"}}})
	runCmd(m, typeLine(m, "/land"))
	out := screen(m)
	row := rowOf(t, m, orchestrator.EndedSessions)
	if head := strings.Split(out, "\n")[row]; !strings.Contains(head, "no workspace holds these") || strings.Contains(head, "free") {
		t.Errorf("the ended sessions card head reads %q, want it to say no workspace holds these", head)
	}
	if pick, _ := m.wsPane.selected(); pick.change.Change != "leftover" || pick.workspace != orchestrator.EndedSessions {
		t.Fatalf("the cursor starts on %+v, want leftover in the ended sessions card", pick)
	}
	press(m, "enter")
	if q := m.session.Snapshot().Queued; m.wsPane != nil || len(q) != 1 || q[0].Text != "/land leftover" {
		t.Fatalf("enter queued %+v, want /land leftover and the view closed", q)
	}
}

func TestLandWithAChangeSkipsTheWorkspaceCards(t *testing.T) {
	m, _ := workspaceModel(t)
	typeLine(m, "/land abc")
	if m.wsPane != nil || len(m.session.Snapshot().Queued) != 1 {
		t.Fatal("/land abc should go to the session as a command, not open the workspace view")
	}
}

func TestTSwitchesToTheGraph(t *testing.T) {
	m, _ := workspaceModel(t)
	openWorkspacesByClick(t, m)
	press(m, "t")
	if !strings.Contains(screen(m), "├─╯") {
		t.Errorf("t did not show jj's graph:\n%s", screen(m))
	}
}

func TestDShowsTheChangesStatAndDiffInThePaneWithLongFilesFolded(t *testing.T) {
	m, src := workspaceModel(t)
	src.diffs = map[string]string{"livework": gitDiff("short.go", 3) + gitDiff("long.go", wsFoldLines+1)}
	openWorkspacesByClick(t, m)
	press(m, "up")
	press(m, "up")
	runCmd(m, press(m, "d"))
	if len(src.stats) != 1 || src.stats[0] != "livework" {
		t.Fatalf("d asked for stats of %v, want livework", src.stats)
	}
	out := screen(m)
	if m.wsPane == nil || m.wsPane.diff == nil {
		t.Fatalf("d closed the pane instead of showing the diff in it:\n%s", out)
	}
	for _, want := range []string{"diff of livework", "STAT for livework", "short.go line 2", "long.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diff view is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "long.go line 0") {
		t.Errorf("a file over %d lines starts open:\n%s", wsFoldLines, out)
	}
	if strings.Contains(transcriptText(m), "STAT for livework") {
		t.Error("the stat was printed to the transcript too")
	}

	row := rowOf(t, m, "long.go")
	clickAt(m, screenXOf(t, m, row, "long.go"), row)
	if !strings.Contains(screen(m), "long.go line 0") {
		t.Errorf("clicking the folded file did not open it:\n%s", screen(m))
	}

	press(m, "esc")
	if m.wsPane == nil || m.wsPane.diff != nil || !strings.Contains(screen(m), "being worked right now") {
		t.Fatalf("esc did not go back to the cards:\n%s", screen(m))
	}
	if pick, _ := m.wsPane.selected(); pick.change.Change != "livework" {
		t.Errorf("going back moved the cursor to %q", pick.change.Change)
	}
}

func TestEachRowShowsItsCountsAndWhatLandsWithTheSelectedOneIsMarked(t *testing.T) {
	m, src := workspaceModel(t)
	src.rows = []orchestrator.Unlanded{
		{Change: "top", Description: "second of a stack", With: []string{"bottom"}},
		{Change: "bottom", Description: "first of a stack"},
	}
	src.view.Workspaces[2].Changes = []orchestrator.WorkspaceChange{
		{Change: "top", Description: "second of a stack", Age: "1h", Counts: vcs.Counts{Modified: 2, LinesAdded: 85, LinesRemoved: 249}},
		{Change: "bottom", Description: "first of a stack", Age: "2h", Counts: vcs.Counts{Added: 1, LinesAdded: 12}},
	}
	openWorkspacesByClick(t, m)
	if pick, _ := m.wsPane.selected(); pick.change.Change != "top" {
		t.Fatalf("the cursor starts on %q, want top", pick.change.Change)
	}
	out := screen(m)
	for _, want := range []string{"[~2] +85/-249", "[+1] +12/-0"} {
		if !strings.Contains(out, want) {
			t.Errorf("no row shows %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "│ └ · bottom") {
		t.Errorf("bottom, which lands with top, is not marked:\n%s", out)
	}

	press(m, "down")
	if out := screen(m); strings.Contains(out, "│ ┌ · top") || strings.Contains(out, "│ └ · bottom") {
		t.Errorf("selecting bottom, which lands alone, still marks a row:\n%s", out)
	}
}
