package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/charmbracelet/x/ansi"
)

type fakeSource struct {
	asked    []string
	synced   []bool
	files    []string
	filesFor []string
	app      prs.ReviewApp
	labelled []string
	merge    prs.MergeCheck
	mergeErr error
}

func (f *fakeSource) MergeReady(_ context.Context, repo string, number int) (prs.MergeCheck, error) {
	return f.merge, f.mergeErr
}

func (f *fakeSource) AddMergeLabel(ctx context.Context, repo string, number int) (string, error) {
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	return f.merge.Label, f.AddLabel(ctx, repo, number, f.merge.Label)
}

func (f *fakeSource) ReviewApp(_ context.Context, repo string, number int) (prs.ReviewApp, error) {
	return f.app, nil
}

func (f *fakeSource) AddLabel(_ context.Context, repo string, number int, label string) error {
	f.labelled = append(f.labelled, fmt.Sprintf("%s#%d %s", repo, number, label))
	return nil
}

func (f *fakeSource) Detail(_ context.Context, repo string, number int) (string, error) {
	f.asked = append(f.asked, repo)
	return "### detail for PR " + repo, nil
}

func (f *fakeSource) Files(_ context.Context, repo string, number int) ([]string, error) {
	f.filesFor = append(f.filesFor, fmt.Sprintf("%s#%d", repo, number))
	return f.files, nil
}

func (f *fakeSource) Sync(_ context.Context, force bool) { f.synced = append(f.synced, force) }

func pr(n int, title string) prs.Item {
	return prs.Item{PR: prs.PR{Repo: "acme/web", Number: n, Title: title, URL: "https://github.com/acme/web/pull/" + title}}
}

func pickerModel(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	return pickerModelOn(t, &agent.Agent{})
}

func pickerModelOn(t *testing.T, a *agent.Agent) (*Model, *fakeSource) {
	t.Helper()
	s := session.New(a, nil)
	s.WireSkills()
	m := New(s)
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	src := &fakeSource{}
	m.PRs = src
	mine := pr(9, "mine")
	mine.Status = prs.StatusFailing
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: prs.Queue{
		Review: []prs.Item{pr(1, "first"), pr(2, "second")},
		Mine:   []prs.Item{mine},
	}}}})
	return m, src
}

func press(m *Model, k string) tea.Cmd {
	var msg tea.KeyPressMsg
	switch k {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "shift+enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		msg = tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		msg = tea.KeyPressMsg{Code: tea.KeyUp}
	case "tab":
		msg = tea.KeyPressMsg{Code: tea.KeyTab}
	default:
		msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
	}
	_, cmd := m.Update(msg)
	return cmd
}

// runCmd executes a command and feeds its message back, as the program loop would.
func runCmd(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		m.Update(msg)
	}
}

func typeLine(m *Model, text string) tea.Cmd {
	m.input.SetValue(text)
	return press(m, "enter")
}

func TestTypedPRsOpensThePickerAndANumberStillPrints(t *testing.T) {
	m, src := pickerModel(t)
	cmd := typeLine(m, "/prs refresh")
	if m.picker == nil || len(m.session.Snapshot().Queued) != 0 {
		t.Fatal("/prs refresh should open the picker without queueing a command")
	}
	if !strings.Contains(m.renderPicker(100), "refreshing") {
		t.Error("the picker shows no sign of the refresh in flight")
	}
	runCmd(m, cmd)
	if strings.Contains(m.renderPicker(100), "refreshing") {
		t.Error("the refresh indicator stayed after the sync returned")
	}
	if len(src.synced) != 1 || !src.synced[0] {
		t.Errorf("/prs refresh synced %v, want one forced sync", src.synced)
	}
	press(m, "esc")
	if m.picker != nil {
		t.Fatal("esc left the picker open")
	}
	typeLine(m, "/prs 42")
	if m.picker != nil || len(m.session.Snapshot().Queued) != 1 {
		t.Fatal("/prs 42 should go to the session as a command, not open the picker")
	}
}

func TestLClosesThePickerAndPutsThePRInFrontOfTheDraft(t *testing.T) {
	m, _ := pickerModel(t)
	m.input.SetValue("why so big?")
	m.openPicker(false)
	press(m, "down")
	press(m, "l")
	if m.picker != nil {
		t.Fatal("l left the picker open")
	}
	want := `About acme/web#2 "second" (https://github.com/acme/web/pull/second): why so big?`
	if got := m.input.Value(); got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
	if len(m.session.Snapshot().Queued) != 0 {
		t.Error("l sent the prompt instead of leaving it to be typed")
	}
}

func TestPickerShowsSectionsAndMovesTheCursor(t *testing.T) {
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	out := screen(m)
	for _, want := range []string{"review", "#1", "first", "mine", "●  #9", "⇧⏎"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker is missing %q:\n%s", want, out)
		}
	}
	for _, width := range []int{60, 100} {
		send(m, tea.WindowSizeMsg{Width: width, Height: 30})
		lines := strings.Split(screen(m), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "│ ✓ ready") && !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "│ ↑↓ move") {
				t.Fatalf("legend wrapped at %d columns:\n%s\n%s", width, line, lines[i+1])
			}
		}
	}
	for range 5 {
		press(m, "down")
	}
	if m.picker.cursor != 2 {
		t.Errorf("cursor = %d, want it held on the last row", m.picker.cursor)
	}
	press(m, "k")
	if m.picker.cursor != 1 {
		t.Errorf("k moved the cursor to %d, want 1", m.picker.cursor)
	}
}

func TestPickerDrawsAStackWithUnlistedMembersAsRowsTheCursorSkips(t *testing.T) {
	m, _ := pickerModel(t)
	stacked := func(n int, title, head, base string) prs.Item {
		it := pr(n, title)
		it.Head, it.Base = head, base
		return it
	}
	ui := stacked(12, "intake ui", "ui", "api")
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web",
		Queue: prs.Queue{Review: []prs.Item{pr(1, "first"), ui}},
		Stack: []prs.PR{stacked(11, "intake api", "api", "main").PR, stacked(13, "intake copy", "copy", "ui").PR},
	}}})
	runCmd(m, typeLine(m, "/prs"))

	out := screen(m)
	at := func(s string) int {
		i := strings.Index(out, s)
		if i < 0 {
			t.Fatalf("picker is missing %q:\n%s", s, out)
		}
		return i
	}
	if !(at("first") < at("┬ #13") && at("┬ #13") < at("├ #12") && at("├ #12") < at("└ #11")) {
		t.Errorf("the stack is not drawn top first beneath #1:\n%s", out)
	}
	press(m, "down")
	if got := m.picker.rows[m.picker.cursor].item.Number; got != 12 {
		t.Fatalf("down from #1 landed on #%d, want it to skip unlisted #13 for #12", got)
	}
	press(m, "down")
	if got := m.picker.rows[m.picker.cursor].item.Number; got != 12 {
		t.Errorf("down from #12 landed on #%d, want it held: only unlisted #11 is below", got)
	}
}

func TestEnterPrintsTheSelectedPRsDetail(t *testing.T) {
	m, src := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "enter"))
	if len(src.asked) != 1 || src.asked[0] != "acme/web" {
		t.Fatalf("detail asked for %v", src.asked)
	}
	items := m.session.Snapshot().Items
	if last := items[len(items)-1]; !strings.Contains(last.Text, "detail for PR acme/web") {
		t.Fatalf("the detail did not reach the transcript: %+v", last)
	}
	if m.picker == nil || m.picker.loading != 0 {
		t.Error("the picker should stay open and stop loading once the detail lands")
	}
}

const reviewSkill = `schema: 2
skill: pr-review
version: 1
when: testing the picker
requires:
  tools: [glob]
inputs:
  pr: { type: string }
  reviewers: { type: string, default: "test-skeptic" }
  reviewer_count: { type: int, default: 1 }
  open_slots: { type: int, default: 4 }
  rereview: { type: bool, default: false }
  prior_head: { type: string, default: "" }
  prior: { type: string, default: "" }
nodes:
  read:
    say: Review ${pr} with ${reviewers}, adding up to ${open_slots}.
    evidence:
      - call: { tool: glob }
  write:
    say: Write it.
    evidence:
      - call: { tool: glob, min: "${reviewer_count}" }
order:
  - [read, write]
`

const reviewPersonas = `cap: 2
personas:
  - { id: test-skeptic, name: Test skeptic, always: true, lens: tests }
  - { id: database-engineer, name: Database engineer, paths: ["db/migrations/**"], lens: locks }
  - { id: frontend-engineer, name: Frontend engineer, paths: ["web/**"], lens: UX }
`

func withReviewSkill(t *testing.T, personas string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, paths.Config, "skills")
	if err := os.MkdirAll(filepath.Join(dir, "pr-review"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr-review.yaml"), []byte(reviewSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	if personas != "" {
		if err := os.WriteFile(filepath.Join(dir, "pr-review", "personas.yaml"), []byte(personas), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRStartsTheReviewSkillWithReviewersChosenFromTheChangedFiles(t *testing.T) {
	withReviewSkill(t, reviewPersonas)
	m, src := pickerModelOn(t, &agent.Agent{Tools: agent.BuiltinTools})
	src.files = []string{"db/migrations/20261015-add.py", "api/modules/orders/models/order.py"}
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "r"))
	if m.picker != nil {
		t.Fatal("r left the picker open")
	}
	if len(src.filesFor) != 1 || src.filesFor[0] != "acme/web#2" {
		t.Fatalf("asked for files of %v, want acme/web#2", src.filesFor)
	}
	if v := m.session.Snapshot().Skill; v == nil || v.Name != "pr-review" {
		t.Fatalf("open skill run = %+v, want pr-review", v)
	}
	brief := submitted(t, m, "**pr-review**")
	for _, want := range []string{"pr = acme/web#2", "reviewers = test-skeptic, database-engineer", "reviewer_count = 2", "open_slots = 1"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
}

func TestChooseReviewersFlagsARereviewOnlyWhenTheStoredHeadDiffers(t *testing.T) {
	withReviewSkill(t, reviewPersonas)
	home, _ := os.UserHomeDir()
	file, _ := reviewstore.Path(home, "acme/web", 2)
	old := loadReview
	t.Cleanup(func() { loadReview = old })
	for _, c := range []struct {
		name, stored, head string
		want               bool
	}{
		{"no stored review", "", "new", false},
		{"stored head is current", "same", "same", false},
		{"stored head is older", "old", "new", true},
		{"row has no head", "old", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			loadReview = func(string, int) (reviewstore.Review, bool, error) {
				return reviewstore.Review{Head: c.stored}, c.name != "no stored review", nil
			}
			in := chooseReviewers(&fakeSource{}, "acme/web", 2, c.head).inputs
			wantHead, wantPrior := "", ""
			if c.want {
				wantHead, wantPrior = c.stored, file
			}
			if in["rereview"] != c.want || in["prior_head"] != wantHead || in["prior"] != wantPrior {
				t.Errorf("rereview=%v prior_head=%q prior=%q, want %v %q %q", in["rereview"], in["prior_head"], in["prior"], c.want, wantHead, wantPrior)
			}
		})
	}
}

func TestRPassesTheRowsHeadSoACurrentReviewIsNotARereview(t *testing.T) {
	withReviewSkill(t, reviewPersonas)
	old := loadReview
	t.Cleanup(func() { loadReview = old })
	loadReview = func(string, int) (reviewstore.Review, bool, error) { return reviewstore.Review{Head: "abc"}, true, nil }
	m, _ := pickerModelOn(t, &agent.Agent{Tools: agent.BuiltinTools})
	it := pr(1, "first")
	it.HeadSHA = "abc"
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: prs.Queue{Review: []prs.Item{it}}}}})
	runCmd(m, typeLine(m, "/prs"))
	msg, ok := press(m, "r")().(reviewMsg)
	if !ok || msg.err != nil || msg.inputs["rereview"] != false {
		t.Fatalf("got %+v", msg)
	}
}

// submitted finds a prompt holding marker, queued or already taken into the transcript by the drain.
func submitted(t *testing.T, m *Model, marker string) string {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		snap := m.session.Snapshot()
		for _, q := range snap.Queued {
			if strings.Contains(q.Text, marker) {
				return q.Text
			}
		}
		for _, it := range snap.Items {
			if it.Kind == session.KindUser && strings.Contains(it.Text, marker) {
				return it.Text
			}
		}
	}
	t.Fatalf("no prompt containing %q was submitted", marker)
	return ""
}

const e2eTestSkill = `schema: 2
skill: e2e-verify
version: 1
when: testing the picker
requires:
  tools: [glob]
inputs:
  change: { type: string }
  app_url: { type: string, default: "http://localhost:3000" }
  api_url: { type: string, default: "http://localhost:8080" }
  diff: { type: string, default: "jj diff" }
nodes:
  open:
    say: Open ${app_url}, log in through ${api_url}, read ${diff} for ${change}.
    evidence:
      - call: { tool: glob }
  report:
    say: Report.
    evidence:
      - call: { tool: glob }
order:
  - [open, report]
`

func withE2ESkill(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, paths.Config, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "e2e-verify.yaml"), []byte(e2eTestSkill), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEStartsE2EAgainstTheAppTheChangedFilesTouch(t *testing.T) {
	withE2ESkill(t)
	m, src := pickerModelOn(t, &agent.Agent{Tools: agent.BuiltinTools})
	src.app = prs.ReviewApp{State: prs.EnvReady, Slug: "2-calm-cat", Apps: map[string]string{
		"site": "https://site-2-calm-cat.test.example", "dashboard": "https://dashboard-2-calm-cat.test.example"},
		Config: prs.ReviewAppConfig{AppsDir: "apps/", Skip: []string{"dashboard"}, Rank: []string{"dashboard", "site"}, LoginApp: "dashboard"}}
	src.files = []string{"apps/site/routes/home.tsx", "server/x.py"}
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "e"))
	if m.picker != nil {
		t.Fatal("e left the picker open once the skill started")
	}
	brief := submitted(t, m, "**e2e-verify**")
	for _, want := range []string{"app_url = https://site-2-calm-cat.test.example", "api_url = https://dashboard-2-calm-cat.test.example",
		"diff = pr_diff on acme/web#2", "acme/web#2, \"second\", tested in its review app's site"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
}

func TestEOnAPRWithoutAReviewAppOffersTheDeployLabel(t *testing.T) {
	m, src := pickerModel(t)
	src.app = prs.ReviewApp{State: prs.EnvNone, Label: "deploy-preview"}
	runCmd(m, typeLine(m, "/prs"))
	runCmd(m, press(m, "e"))
	if !strings.Contains(screen(m), "#1 has no review app") {
		t.Fatalf("no offer on screen:\n%s", screen(m))
	}
	press(m, "down")
	if m.picker.cursor != 0 || len(src.labelled) != 0 || m.picker.offer != nil {
		t.Fatalf("a key other than y must only cancel: cursor %d, labelled %v", m.picker.cursor, src.labelled)
	}
	runCmd(m, press(m, "e"))
	runCmd(m, press(m, "y"))
	if len(src.labelled) != 1 || src.labelled[0] != "acme/web#1 deploy-preview" {
		t.Fatalf("labelled %v", src.labelled)
	}
	if !strings.Contains(screen(m), "added deploy-preview to #1") {
		t.Errorf("no notice after labelling:\n%s", screen(m))
	}
	if len(m.session.Snapshot().Queued) != 0 {
		t.Error("a skill started with no review app")
	}
}

func TestEOnADeployingReviewAppRefuses(t *testing.T) {
	for _, state := range []prs.EnvState{prs.EnvDeploying, prs.EnvFailed} {
		m, src := pickerModel(t)
		src.app = prs.ReviewApp{State: state}
		runCmd(m, typeLine(m, "/prs"))
		runCmd(m, press(m, "e"))
		if m.picker.offer != nil || len(m.session.Snapshot().Queued) != 0 {
			t.Errorf("%s: offered a label or started a run", state)
		}
		if !strings.Contains(screen(m), "e2e #1: its review app") {
			t.Errorf("%s: no notice:\n%s", state, screen(m))
		}
	}
}

func TestMAsksBeforeAddingTheMergeLabel(t *testing.T) {
	m, src := pickerModel(t)
	src.merge = prs.MergeCheck{Label: "merge-queue", Pending: []string{"e2e"}}
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	press(m, "down")
	runCmd(m, press(m, "m"))
	if !strings.Contains(screen(m), "#9 is yours, approved and merges cleanly, with 1 check still running") {
		t.Fatalf("no offer on screen:\n%s", screen(m))
	}
	press(m, "n")
	if len(src.labelled) != 0 || m.picker.offer != nil {
		t.Fatalf("a key other than y must only cancel: labelled %v", src.labelled)
	}
	runCmd(m, press(m, "m"))
	runCmd(m, press(m, "y"))
	if len(src.labelled) != 1 || src.labelled[0] != "acme/web#9 merge-queue" {
		t.Fatalf("labelled %v", src.labelled)
	}
	if !strings.Contains(screen(m), "added merge-queue to #9") {
		t.Errorf("no notice after labelling:\n%s", screen(m))
	}
	if len(src.synced) != 1 || !src.synced[0] {
		t.Errorf("synced %v, want one forced sync so the row shows the label", src.synced)
	}
}

func TestMOnAPRThatFailsTheCheckSaysWhyAndOffersNothing(t *testing.T) {
	m, src := pickerModel(t)
	src.mergeErr = errors.New("not adding merge-queue: it is kim's, not yours")
	runCmd(m, typeLine(m, "/prs"))
	runCmd(m, press(m, "m"))
	if m.picker.offer != nil || len(src.labelled) != 0 {
		t.Fatalf("offered or labelled %v after a failed check", src.labelled)
	}
	if !strings.Contains(screen(m), "merge #1: not adding merge-queue") {
		t.Errorf("no notice:\n%s", screen(m))
	}
}

func TestShiftEnterOpensTheSelectedPR(t *testing.T) {
	m, _ := pickerModel(t)
	var opened []string
	was := openURL
	openURL = func(url string) error { opened = append(opened, url); return nil }
	t.Cleanup(func() { openURL = was })

	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "shift+enter"))
	if len(opened) != 1 || opened[0] != "https://github.com/acme/web/pull/second" {
		t.Fatalf("opened %v, want the second PR", opened)
	}
}

const longTitle = "[ABC-630] Give the support agent call context, knowledge and the team's rules (P2.1)"

func longModel(t *testing.T, width int) *Model {
	t.Helper()
	m, _ := harness(t, width, 40)
	m.PRs = &fakeSource{}
	item := func(n int) prs.Item {
		it := pr(n, longTitle)
		it.Author = "octocat-long-login"
		return it
	}
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: prs.Queue{
		Review: []prs.Item{item(101), item(102)},
	}}}})
	runCmd(m, typeLine(m, "/prs"))
	return m
}

// pickerLines is the picker's frame contents, one entry per screen row.
func pickerLines(m *Model) []string {
	var out []string
	in := false
	for _, line := range strings.Split(ansi.Strip(m.renderBottom()), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "╭"):
			in = true
		case strings.HasPrefix(trimmed, "╰"):
			in = false
		case in:
			out = append(out, trimmed)
		}
	}
	return out
}

func TestPickerRowsNeverWrapAndCutAtAWord(t *testing.T) {
	for _, width := range []int{60, 80, 120} {
		m := longModel(t, width)
		lines := pickerLines(m)
		var unselected string
		for i, l := range lines {
			if !strings.HasSuffix(l, "│") {
				t.Fatalf("width %d: row %d overflowed the frame: %q", width, i, l)
			}
			body := strings.TrimSpace(strings.Trim(l, "│"))
			if strings.Contains(body, "#102") {
				unselected = body
			}
			inner := strings.TrimPrefix(l, "│ ")
			known := false
			for _, start := range []string{"Pull requests", "review", "mine", "❯", "  ", "✓ ready", "↑↓"} {
				known = known || strings.HasPrefix(inner, start)
			}
			if !known {
				t.Fatalf("width %d: a stray fragment wrapped onto its own row: %q", width, l)
			}
		}
		if width < 120 && (!strings.HasSuffix(unselected, "…") || strings.Contains(unselected, " …")) {
			t.Errorf("width %d: unselected title not cut at a word with an ellipsis: %q", width, unselected)
		}
		if !strings.Contains(strings.Join(lines, " "), "(P2.1)") {
			t.Errorf("width %d: the selected row's title is not shown in full:\n%s", width, strings.Join(lines, "\n"))
		}
	}
}

func TestEllipsizePrefersAWordBoundary(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"Create the Front Desk phone tables", 20, "Create the Front…"},
		{"supercalifragilistic", 8, "superca…"},
		{"a — b", 4, "a…"},
	} {
		if got := ellipsize(tc.in, tc.width); got != tc.want {
			t.Errorf("ellipsize(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

func TestAPollKeepsTheCursorOnTheSamePR(t *testing.T) {
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: prs.Queue{
		Review: []prs.Item{pr(5, "new"), pr(1, "first"), pr(2, "second")},
	}}}})
	if got := m.picker.rows[m.picker.cursor].item.Number; got != 2 {
		t.Fatalf("cursor on #%d after the poll, want #2", got)
	}
}
