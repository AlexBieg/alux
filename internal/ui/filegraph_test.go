package ui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/reviewgraph"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

type fakeChat struct {
	reply string
}

func (f *fakeChat) Send(context.Context, string) (string, error) {
	return f.reply, nil
}

func (f *fakeChat) Model() string { return "MODEL-X" }

func stubFileGraph(t *testing.T, g reviewgraph.Graph) *[]string {
	t.Helper()
	var asked []string
	old := buildFileGraph
	buildFileGraph = func(_ context.Context, repo string, number int) (reviewgraph.Graph, error) {
		asked = append(asked, fmt.Sprintf("%s#%d", repo, number))
		g.Repo, g.Number = repo, number
		return g, nil
	}
	oldLoad, oldSave := loadReview, saveReview
	loadReview = func(string, int) (reviewstore.Review, bool, error) { return reviewstore.Review{}, false, nil }
	saveReview = func(reviewstore.Review) error { return nil }
	t.Cleanup(func() { buildFileGraph, loadReview, saveReview = old, oldLoad, oldSave })
	return &asked
}

func stubReview(r reviewstore.Review) {
	loadReview = func(string, int) (reviewstore.Review, bool, error) { return r, true, nil }
}

var sampleFileGraph = reviewgraph.Graph{
	Files: []reviewgraph.File{
		{Path: "src/parse.go", Changed: true, Status: "M"},
		{Path: "src/load.go", Changed: true, Status: "A"},
		{Path: "web/run.go"},
	},
	Edges: []reviewgraph.Edge{
		{From: "src/parse.go", To: "src/load.go", Via: []string{"Parse"}},
		{From: "src/parse.go", To: "web/run.go", Via: []string{"Lex"}},
	},
	TooCommon: []string{"Config"},
	Diff: "diff --git a/src/parse.go b/src/parse.go\n--- a/src/parse.go\n+++ b/src/parse.go\n@@ -1 +1 @@\n-old\n+PARSELINE\n" +
		"diff --git a/src/load.go b/src/load.go\nnew file mode 100644\n--- /dev/null\n+++ b/src/load.go\n@@ -0,0 +1,9 @@\n" +
		strings.Repeat("+line\n", 8) + "+LOADLINE\n",
}

func TestShortNamesKeepParentsOnlyWhereBaseNamesClash(t *testing.T) {
	got := shortNames([]string{"a/x/util.go", "b/x/util.go", "c/util.py", "src/parse.go", "mod.go", "pkg/mod.go"})
	want := map[string]string{
		"a/x/util.go":  "a/x/util.go",
		"b/x/util.go":  "b/x/util.go",
		"c/util.py":    "util.py",
		"src/parse.go": "parse.go",
		"mod.go":       "mod.go",
		"pkg/mod.go":   "pkg/mod.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestGWhileAGraphIsLoadingStartsNoSecondBuild(t *testing.T) {
	stubFileGraph(t, sampleFileGraph)
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	if press(m, "g") == nil {
		t.Fatal("the first g should start a build")
	}
	if press(m, "g") != nil {
		t.Fatal("a second g while the first build is loading started another")
	}
}

func TestTheHoveredFileDrawsOnlyItsNeighboursAndEnterShowsItsDiff(t *testing.T) {
	asked := stubFileGraph(t, sampleFileGraph)
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	if len(*asked) != 1 || (*asked)[0] != "acme/web#2" || m.picker != nil || m.fileGraph == nil {
		t.Fatalf("g should close the picker and open acme/web#2's files; built %v", *asked)
	}
	out := screen(m)
	if head := ansi.Strip(strings.Join(m.fileGraphHead(), " ")); !strings.Contains(head, "second") {
		t.Errorf("the head should carry the PR's title:\n%s", head)
	}
	for _, want := range []string{"2 files changed · 1 impacted", "src/parse.go", "› parse.go", "load.go", "run.go", "Config"} {
		if !strings.Contains(out, want) {
			t.Errorf("parse.go hovered lacks %q:\n%s", want, out)
		}
	}

	press(m, "down")
	out = screen(m)
	if !strings.Contains(out, "› load.go") || !strings.Contains(out, "src/load.go") {
		t.Errorf("down should hover load.go and put its path in the head:\n%s", out)
	}
	if strings.Contains(out, "run.go") {
		t.Errorf("run.go is parse.go's neighbour, not load.go's, yet it is drawn:\n%s", out)
	}

	press(m, "enter")
	out = screen(m)
	if !strings.Contains(out, "LOADLINE") || strings.Contains(out, "PARSELINE") {
		t.Errorf("enter should show load.go's patch and only it:\n%s", out)
	}
	if m.pane.YOffset() != 0 {
		t.Fatalf("the diff should open at its top, at %d", m.pane.YOffset())
	}
	m.fileGraph.g.Diff = strings.Replace(m.fileGraph.g.Diff, strings.Repeat("+line\n", 8), strings.Repeat("+line\n", 200), 1)
	m.layout()
	press(m, "j")
	press(m, "j")
	press(m, "k")
	if m.fileGraph.line != 2 || m.pane.YOffset() != 0 {
		t.Errorf("j j k should put the cursor on the second patch line without scrolling; line %d, offset %d", m.fileGraph.line, m.pane.YOffset())
	}
	for range 205 {
		press(m, "j")
	}
	if out := screen(m); !strings.Contains(out, "›+ LOADLINE") {
		t.Errorf("moving the cursor to the last line should scroll it into view, marked:\n%s", out)
	}
	press(m, "esc")
	if m.fileGraph == nil || m.fileGraph.diff {
		t.Fatal("esc in a diff should go back to the files")
	}
	press(m, "esc")
	if m.fileGraph != nil {
		t.Fatal("esc on the files should close the pane")
	}
}

func TestAFindingIsDrawnAfterTheLastOfItsLinesThePatchShows(t *testing.T) {
	patch := "@@ -10,3 +10,4 @@\n keep10\n+add11\n keep12\n keep13"
	findings := []reviewstore.Finding{
		{Severity: "blocking", Class: "BUGA", StartLine: 11, EndLine: 40, Text: "spans past the hunk"},
		{Severity: "non-blocking", Class: "BUGB", StartLine: 90, EndLine: 95, Text: "off the diff"},
		{Severity: "non-blocking", Class: "BUGC", Text: "whole file"},
	}
	var rows []string
	for _, r := range reviewedPatch("a.go", patch, "", findings, 80) {
		rows = append(rows, ansi.Strip(r))
	}
	at := func(s string) int {
		for i, r := range rows {
			if strings.Contains(r, s) {
				return i
			}
		}
		t.Fatalf("%q not drawn:\n%s", s, strings.Join(rows, "\n"))
		return -1
	}
	if a, b := at("keep13"), at("BUGA"); b != a+2 {
		t.Errorf("BUGA should follow line 13, the last of 11-40 shown; keep13 at %d, BUGA at %d", a, b)
	}
	if b, loose := at("BUGB"), at("beside no line"); b < loose || at("BUGC") < loose {
		t.Errorf("off-diff and whole-file findings belong under the patch:\n%s", strings.Join(rows, "\n"))
	}
}

func TestTheCursorOnAFindingMarksEveryLineOfItsRangeAndNoOther(t *testing.T) {
	patch := "@@ -10,4 +10,4 @@\n keep10\n+add11\n+add12\n keep13"
	findings := []reviewstore.Finding{{Severity: "blocking", Class: "RANGE", StartLine: 11, EndLine: 12, Text: "two lines"}}
	items := reviewedItems(patch, "", findings, 80)
	cur := -1
	for i, it := range items {
		if it.finding == 0 {
			cur = i
		}
	}
	rows, from, to := renderItems("a.go", items, findings, findingDeco{}, cur, 80)
	marked := map[string]bool{}
	for _, r := range rows {
		s := ansi.Strip(r)
		for _, w := range []string{"keep10", "add11", "add12", "keep13"} {
			if strings.Contains(s, w) {
				marked[w] = strings.HasPrefix(s, "┃")
			}
		}
	}
	if !marked["add11"] || !marked["add12"] || marked["keep10"] || marked["keep13"] {
		t.Errorf("only lines 11-12 should be marked, got %v:\n%s", marked, ansi.Strip(strings.Join(rows, "\n")))
	}
	if box := ansi.Strip(strings.Join(rows[from:to+1], "\n")); !strings.Contains(box, "RANGE") || !strings.Contains(box, "two lines") {
		t.Errorf("the cursor's rows should be the finding's box:\n%s", box)
	}
}

func TestAFindingOutsideTheDiffIsDrawnUnderItsSourceLinesInLineOrder(t *testing.T) {
	var src []string
	for n := 1; n <= 30; n++ {
		src = append(src, fmt.Sprintf("src%02d", n))
	}
	source := strings.Join(src, "\n") + "\n"
	patch := "@@ -10,2 +10,2 @@\n keep10\n+add11"
	findings := []reviewstore.Finding{
		{Severity: "blocking", Class: "EARLY", StartLine: 3, EndLine: 3, Text: "before the hunk"},
		{Severity: "non-blocking", Class: "LATE", StartLine: 20, EndLine: 21, Text: "after the hunk"},
		{Severity: "non-blocking", Class: "WHOLE", Text: "whole file"},
	}
	var rows []string
	for _, r := range reviewedPatch("a.go", patch, source, findings, 80) {
		rows = append(rows, ansi.Strip(r))
	}
	joined := strings.Join(rows, "\n")
	at := func(s string) int {
		for i, r := range rows {
			if strings.Contains(r, s) {
				return i
			}
		}
		t.Fatalf("%q not drawn:\n%s", s, joined)
		return -1
	}
	if !(at("src01") < at("src03") && at("src03") < at("EARLY") && at("EARLY") < at("src05") && at("src05") < at("keep10") &&
		at("add11") < at("src18") && at("src21") < at("LATE") && at("LATE") < at("src23")) {
		t.Errorf("each off-diff finding should follow its own source lines, with two lines either side, in line order:\n%s", joined)
	}
	if strings.Contains(joined, "src06") || strings.Contains(joined, "src24") || at("WHOLE") < at("beside no line") {
		t.Errorf("only the findings' lines and their context should come from the source:\n%s", joined)
	}

	long := reviewstore.Finding{Severity: "blocking", Class: "LONG", StartLine: 12, EndLine: 25, Text: "long range"}
	list, _, _ := fileFindingRows("a.go", patch, source, []reviewstore.Finding{long}, findingDeco{}, -1, 80)
	got := ansi.Strip(strings.Join(list, "\n"))
	for n := 12; n <= 25; n++ {
		if !strings.Contains(got, fmt.Sprintf("src%02d", n)) {
			t.Errorf("the file's finding list should show every line of the range, missing %d:\n%s", n, got)
		}
	}
	if strings.Contains(got, "src11") || strings.Contains(got, "src26") || strings.Index(got, "src25") > strings.Index(got, "LONG") {
		t.Errorf("the range alone, no context, should sit above the finding:\n%s", got)
	}
}

func TestTheFileGraphDrawsAReviewMadeAtItsHeadAndIgnoresAStaleOne(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	stubFileGraph(t, g)
	review := reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "request changes", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 9, EndLine: 9, Text: "TEXTONLOAD"},
	}}
	stubReview(review)
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	out := screen(m)
	if !strings.Contains(out, "review · request changes · 1 blocking") || !strings.Contains(out, "1   load.go  +9 -0") {
		t.Errorf("head should show the review and the list load.go's count:\n%s", out)
	}
	press(m, "down")
	press(m, "enter")
	out = screen(m)
	if a, b := strings.Index(out, "LOADLINE"), strings.Index(out, "TEXTONLOAD"); a < 0 || b < a {
		t.Errorf("the finding on line 9 should follow LOADLINE:\n%s", out)
	}

	press(m, "esc")
	stale := review
	stale.Head = "older"
	runCmd(m, func() tea.Msg { return ReviewMsg{Review: stale} })
	press(m, "enter")
	out = screen(m)
	if head := ansi.Strip(strings.Join(m.fileGraphHead(), " ")); !strings.Contains(head, "the PR has moved") || strings.Contains(out, "TEXTONLOAD") {
		t.Errorf("a review of another head should say so and draw nothing:\n%s", out)
	}
}

func TestSelectingAFindingWhoseRangeOutgrowsThePaneStillShowsItsText(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	g.Diff = strings.Replace(g.Diff, strings.Repeat("+line\n", 8), strings.Repeat("+line\n", 80), 1)
	stubFileGraph(t, g)
	stubReview(reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "request changes", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 1, EndLine: 81, Text: "TALLFINDING"},
	}})
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	press(m, "down")
	press(m, "l")
	if out := screen(m); !strings.Contains(out, "TALLFINDING") || !strings.Contains(out, "LOADLINE") {
		t.Errorf("the selected finding's text should be on screen, under the end of its range:\n%s", out)
	}
}

func TestTheHoveredFileListsItsFindingsUnderTheirCodeAndXDismissesTheSelectedOne(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	stubFileGraph(t, g)
	stubReview(reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "request changes", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 9, EndLine: 9, Text: "FIRSTFINDING"},
		{Severity: "non-blocking", Class: "NAME", Path: "src/load.go", Text: "SECONDFINDING"},
	}})
	var saved []reviewstore.Review
	saveReview = func(r reviewstore.Review) error { saved = append(saved, r); return nil }
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	press(m, "down")
	out := screen(m)
	if a, b := strings.Index(out, "LOADLINE"), strings.Index(out, "FIRSTFINDING"); a < 0 || b < a {
		t.Fatalf("hovering load.go should list its findings, the first under line 9:\n%s", out)
	}

	press(m, "enter")
	at := func() int {
		items := m.diffItems()
		if l := m.fileGraph.line; l < len(items) {
			return items[l].finding
		}
		return -2
	}
	press(m, "]")
	first := at()
	press(m, "]")
	second := at()
	press(m, "]")
	third := at()
	press(m, "[")
	if first != 0 || second != 1 || third != 1 || at() != 0 {
		t.Errorf("] should step to each finding in turn and stop at the last, [ step back; got %d %d %d then %d", first, second, third, at())
	}
	press(m, "esc")

	press(m, "l")
	press(m, "h")
	if m.fileGraph.inFindings {
		t.Fatal("h should hand the keys back to the file list")
	}
	press(m, "l")
	press(m, "down")
	if out := screen(m); !strings.Contains(out, "SECONDFINDING") {
		t.Fatalf("selecting the second finding should scroll it into view:\n%s", out)
	}
	cr, cg, cb, _ := selectionColor.RGBA()
	selBg := fmt.Sprintf("48;2;%d;%d;%d", cr>>8, cg>>8, cb>>8)
	for _, l := range strings.Split(m.View().Content, "\n") {
		text := ansi.Strip(l)
		if hl := strings.Contains(l, selBg); strings.Contains(text, "SECONDFINDING") && !hl || strings.Contains(text, "FIRSTFINDING") && hl {
			t.Errorf("the selected finding's text, and only it, should carry the selection background: %q", text)
		}
	}
	press(m, "x")
	out = screen(m)
	if len(saved) != 1 || saved[0].Findings[0].Dismissed || !saved[0].Findings[1].Dismissed {
		t.Fatalf("x on the second finding should save it, and only it, dismissed; saved %+v", saved)
	}
	if strings.Contains(out, "SECONDFINDING") || !strings.Contains(out, "FIRSTFINDING") || !strings.Contains(out, "1 dismissed") {
		t.Errorf("the dismissed finding should leave the pane and be counted in the head:\n%s", out)
	}
	press(m, "x")
	if len(m.fileGraph.findingsOn("src/load.go")) != 0 || m.fileGraph.inFindings {
		t.Errorf("dismissing the last finding should hand the keys back to the file list")
	}
	press(m, "enter")
	if out := screen(m); strings.Contains(out, "FIRSTFINDING") {
		t.Errorf("a dismissed finding should not come back in the diff:\n%s", out)
	}
}

func TestXDismissesInTheDiffViewAndBracketsSelectInTheOverview(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	stubFileGraph(t, g)
	stubReview(reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "request changes", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 9, EndLine: 9, Text: "FIRSTFINDING"},
		{Severity: "non-blocking", Class: "NAME", Path: "src/load.go", Text: "SECONDFINDING"},
	}})
	var saved []reviewstore.Review
	saveReview = func(r reviewstore.Review) error { saved = append(saved, r); return nil }
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	press(m, "down")

	press(m, "l")
	press(m, "]")
	if m.fileGraph.sel != 1 {
		t.Fatalf("] in the overview should select the next finding, sel %d", m.fileGraph.sel)
	}
	press(m, "[")
	if m.fileGraph.sel != 0 {
		t.Fatalf("[ in the overview should select the previous finding, sel %d", m.fileGraph.sel)
	}
	press(m, "h")

	press(m, "enter")
	press(m, "x")
	if len(saved) != 0 {
		t.Fatal("x on a code line should dismiss nothing")
	}
	press(m, "]")
	press(m, "]")
	press(m, "x")
	if len(saved) != 1 || saved[0].Findings[0].Dismissed || !saved[0].Findings[1].Dismissed {
		t.Fatalf("x on the second finding in the diff view should save it, and only it, dismissed; saved %+v", saved)
	}
	if !m.fileGraph.diff || strings.Contains(screen(m), "SECONDFINDING") {
		t.Errorf("the diff view should stay open without the dismissed finding:\n%s", screen(m))
	}
}

func TestDAsksTheMainAgentToDraftTheUndismissedFindingsFromEveryView(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	for _, view := range []string{"files", "findings", "diff"} {
		t.Run(view, func(t *testing.T) {
			stubFileGraph(t, g)
			stubReview(reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "request changes", Findings: []reviewstore.Finding{
				{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 9, EndLine: 9, Text: "FIRSTFINDING",
					Thread: []reviewstore.Message{{Role: "user", Text: "ASKITASAQUESTION"}}},
				{Severity: "non-blocking", Class: "NAME", Path: "src/load.go", Text: "GONEFINDING", Dismissed: true},
			}})
			m, _ := pickerModel(t)
			runCmd(m, typeLine(m, "/prs"))
			press(m, "down")
			runCmd(m, press(m, "g"))
			press(m, "down")
			switch view {
			case "findings":
				press(m, "l")
			case "diff":
				press(m, "enter")
			}
			press(m, "d")
			brief := submitted(t, m, "pr_pending_review")
			for _, want := range []string{"acme/web#2", "`src/load.go` line 9 · blocking · RACE", "FIRSTFINDING", "ASKITASAQUESTION"} {
				if !strings.Contains(brief, want) {
					t.Errorf("brief lacks %q:\n%s", want, brief)
				}
			}
			if strings.Contains(brief, "GONEFINDING") {
				t.Errorf("a dismissed finding should stay out of the draft:\n%s", brief)
			}
		})
	}
}

func TestDWithAStaleReviewDraftsNothing(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	stubFileGraph(t, g)
	stubReview(reviewstore.Review{Repo: "acme/web", Number: 2, Head: "older", Findings: []reviewstore.Finding{
		{Severity: "blocking", Path: "src/load.go", Text: "STALE"},
	}})
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	press(m, "d")
	if q := m.session.Snapshot().Queued; len(q) != 0 || m.fileGraph == nil {
		t.Errorf("a review of an older head should draft nothing and keep the pane open; queued %v", q)
	}
}

func TestAFindingsChatIsBriefedWithItsStoredThreadAndStoresEachAnswerFromEitherView(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	stubFileGraph(t, g)
	current := reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "request changes", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 9, EndLine: 9, Text: "FIRSTFINDING",
			Thread: []reviewstore.Message{{Role: "user", Text: "OLDQ"}, {Role: "assistant", Text: "OLDA"}}},
		{Severity: "non-blocking", Class: "NAME", Path: "src/load.go", Text: "SECONDFINDING"},
	}}
	loadReview = func(string, int) (reviewstore.Review, bool, error) { return current, true, nil }
	saveReview = func(r reviewstore.Review) error { current = r; return nil }
	m, _ := pickerModel(t)
	chat := &fakeChat{reply: "NEWA"}
	var briefs []subagent.FindingBrief
	m.newFindingChat = func(_ string, b subagent.FindingBrief) (session.Chat, error) {
		briefs = append(briefs, b)
		return chat, nil
	}
	send(m, tea.WindowSizeMsg{Width: 130, Height: 70})
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	press(m, "down")
	if out := screen(m); !strings.Contains(out, "OLDQ") || !strings.Contains(out, "OLDA") {
		t.Fatalf("the stored thread should show under its finding:\n%s", out)
	}
	if out := screen(m); !strings.Contains(out, "l or tab selects one") || strings.Contains(out, "c asks about it") {
		t.Errorf("before a finding is selected the pane should say how to select one, not offer c:\n%s", out)
	}

	m.input.SetValue("MAINDRAFT")
	press(m, "l")
	if pane := ansi.Strip(m.pane.View()); strings.Count(pane, "c asks about it") != 1 {
		t.Errorf("the selected finding's box, and only it, should offer c:\n%s", pane)
	}
	press(m, "c")
	if !m.fileGraph.chatting || m.fileGraph.input.Value() != "" || m.input.Value() != "MAINDRAFT" {
		t.Fatalf("c should open an empty input in the finding and leave the main draft; chatting %v, main %q", m.fileGraph.chatting, m.input.Value())
	}
	for _, r := range "TYPEDQ" {
		press(m, string(r))
	}
	pane := ansi.Strip(m.pane.View())
	if box := pane[strings.Index(pane, "FIRSTFINDING"):]; !strings.Contains(box, "TYPEDQ") || strings.Index(box, "TYPEDQ") > strings.Index(box, "SECONDFINDING") {
		t.Errorf("typing should show inside the first finding's box:\n%s", pane)
	}
	if m.input.Value() != "MAINDRAFT" || m.View().Cursor != nil {
		t.Errorf("the main input should keep its draft and give up the terminal cursor; main %q", m.input.Value())
	}
	send(m, tea.PasteMsg{Content: "PASTED"})
	if m.fileGraph.input.Value() != "TYPEDQPASTED" || m.input.Value() != "MAINDRAFT" {
		t.Errorf("a paste should land in the finding's input; finding %q, main %q", m.fileGraph.input.Value(), m.input.Value())
	}
	ask := func(q string) tea.Cmd {
		m.fileGraph.input.SetValue(q)
		return press(m, "enter")
	}
	cmd := ask("NEWQ")
	if out := screen(m); !strings.Contains(out, "NEWQ") || !strings.Contains(out, "thinking…") {
		t.Errorf("the question should show as waiting while the chat answers:\n%s", out)
	}
	runCmd(m, cmd)
	th := current.Findings[0].Thread
	if len(th) != 4 || th[2].Text != "NEWQ" || th[3].Text != "NEWA" || len(current.Findings[1].Thread) != 0 {
		t.Fatalf("the question and answer should be stored on the first finding only; got %+v", current.Findings)
	}
	if len(briefs) != 1 || len(briefs[0].Thread) != 2 || briefs[0].Thread[0].Text != "OLDQ" || briefs[0].Text != "FIRSTFINDING" {
		t.Errorf("the chat should be briefed with the finding and its stored thread; got %+v", briefs)
	}
	if out := screen(m); !strings.Contains(out, "NEWA") || strings.Contains(out, "thinking…") {
		t.Errorf("the answer should replace the waiting line:\n%s", out)
	}
	press(m, "esc")
	if m.fileGraph.chatting || m.input.Value() != "MAINDRAFT" {
		t.Errorf("esc should hand the input back with the session's draft; input %q", m.input.Value())
	}

	press(m, "h")
	press(m, "enter")
	press(m, "]")
	if pane := ansi.Strip(m.pane.View()); strings.Count(pane, "c asks about it") != 1 {
		t.Errorf("the diff view's finding under the cursor should offer c too:\n%s", pane)
	}
	press(m, "c")
	runCmd(m, ask("DIFFQ"))
	if th := current.Findings[0].Thread; len(th) != 6 || th[4].Text != "DIFFQ" || len(briefs) != 1 {
		t.Errorf("c in the diff view should ask the same finding's chat, reusing it; thread %+v, chats opened %d", th, len(briefs))
	}
}

func TestTheFileListShowsLineCountsAndAChatsSpinnerThenUnreadDotUntilTheFindingIsSelected(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	stubFileGraph(t, g)
	current := reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "approve", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 9, EndLine: 9, Text: "FIRSTFINDING"},
	}}
	loadReview = func(string, int) (reviewstore.Review, bool, error) { return current, true, nil }
	saveReview = func(r reviewstore.Review) error { current = r; return nil }
	m, _ := pickerModel(t)
	m.newFindingChat = func(string, subagent.FindingBrief) (session.Chat, error) { return &fakeChat{reply: "A"}, nil }
	send(m, tea.WindowSizeMsg{Width: 120, Height: 50})
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	row := func(name, counts string) string {
		for _, line := range strings.Split(screen(m), "\n") {
			if strings.Contains(line, name) && strings.Contains(line, counts) {
				return line
			}
		}
		t.Fatalf("no file row for %s with %q:\n%s", name, counts, screen(m))
		return ""
	}
	row("parse.go", "+1 -1")
	spinning := func(line string) bool {
		for _, f := range spinner.MiniDot.Frames {
			if strings.Contains(line, f) {
				return true
			}
		}
		return false
	}

	press(m, "down")
	press(m, "l")
	press(m, "c")
	m.fileGraph.input.SetValue("Q")
	cmd := press(m, "enter")
	press(m, "esc")
	press(m, "h")
	press(m, "up")
	if r := row("load.go", "+9 -0"); !spinning(r) || strings.Contains(r, "●") {
		t.Errorf("a question in flight should spin beside its file: %q", r)
	}
	runCmd(m, cmd)
	if r := row("load.go", "+9 -0"); spinning(r) || !strings.Contains(r, "●") {
		t.Errorf("an answer not yet seen should leave a dot beside its file: %q", r)
	}
	press(m, "down")
	press(m, "l")
	if r := row("load.go", "+9 -0"); strings.Contains(r, "●") {
		t.Errorf("selecting the answered finding should clear the dot: %q", r)
	}
}

func TestThePickerMarksPRsWithAStoredReviewAndDimsOneMadeAtAnOlderHead(t *testing.T) {
	stubFileGraph(t, sampleFileGraph)
	loadReview = func(_ string, n int) (reviewstore.Review, bool, error) {
		heads := map[int]string{2: "h2", 9: "old"}
		if h, ok := heads[n]; ok {
			return reviewstore.Review{Head: h}, true, nil
		}
		return reviewstore.Review{}, false, nil
	}
	m, _ := pickerModel(t)
	withHead := func(it prs.Item, h string) prs.Item { it.HeadSHA = h; return it }
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: prs.Queue{
		Review: []prs.Item{withHead(pr(1, "first"), "h1"), withHead(pr(2, "second"), "h2")},
		Mine:   []prs.Item{withHead(pr(9, "mine"), "new")},
	}}}})
	runCmd(m, typeLine(m, "/prs"))
	row := func(n int) string {
		for _, l := range strings.Split(m.View().Content, "\n") {
			if strings.Contains(ansi.Strip(l), fmt.Sprintf("#%-7d", n)) {
				return l
			}
		}
		t.Fatalf("no row for #%d:\n%s", n, screen(m))
		return ""
	}
	marked := func(n int) bool { return strings.Contains(ansi.Strip(row(n)), fmt.Sprintf("R #%d", n)) }
	if marked(1) || !marked(2) || !marked(9) {
		t.Fatalf("#2 and #9 have reviews on file and #1 none:\n%s", screen(m))
	}
	if strings.Contains(row(2), dimStyle.Render("R")) || !strings.Contains(row(9), dimStyle.Render("R")) {
		t.Errorf("only #9's review, made at an older head, should be dim")
	}
	runCmd(m, func() tea.Msg { return ReviewMsg{Review: reviewstore.Review{Repo: "Acme/Web", Number: 1, Head: "h1"}} })
	if !marked(1) {
		t.Errorf("a review loaded while the picker is open should mark its PR:\n%s", screen(m))
	}
}
