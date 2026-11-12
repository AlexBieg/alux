package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/charmbracelet/x/ansi"
)

// flatScreen is the screen's words in order, with the frames' side bars taken out so text wrapped inside a box reads on.
func flatScreen(m *Model) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(screen(m), "│", " ")), " ")
}

func deepResearchView() *session.SkillView {
	v := &session.SkillView{Name: "deep-research"}
	for _, n := range []string{"scope", "vault", "search", "sweep", "read", "leads", "synthesis", "file"} {
		v.Nodes = append(v.Nodes, skills.NodeResult{Name: n, State: skills.Pending})
	}
	v.Nodes[0].State = skills.Done
	v.Edges = []skills.Edge{{From: "scope", To: "vault"}, {From: "vault", To: "sweep"}, {From: "sweep", To: "read"}, {From: "read", To: "leads"},
		{From: "leads", To: "synthesis"}, {From: "synthesis", To: "file"}, {From: "vault", To: "search"}, {From: "search", To: "sweep"}}
	return v
}

func TestTheSkillIsOneRowOfTheBoxWithItsCount(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.sidebarOpen = true
	m.snap.Skill = deepResearchView()
	m.layout()
	box := plainBox(m)
	_, after, _ := strings.Cut(box, skillGlyph+" deep-research")
	if row := strings.Fields(after); len(row) == 0 || row[0] != "1/8" {
		t.Errorf("the skill row is not its name and done count:\n%s", box)
	}
	if strings.Contains(box, "vault") {
		t.Errorf("the box still lists the skill's steps:\n%s", box)
	}
	if strings.Contains(screen(m), "╭─ skill") {
		t.Errorf("the skill still draws a panel of its own:\n%s", screen(m))
	}
}

func TestTheSkillDoesNotMoveTheTranscript(t *testing.T) {
	m, _ := harness(t, 130, 30)
	before := m.transcriptTop()
	m.snap.Skill = deepResearchView()
	if got := m.transcriptTop(); got != before {
		t.Errorf("a skill moved the transcript from row %d to %d", before, got)
	}
}

// drawSkill runs the open skill's diagram layout, which the pane does off the update goroutine.
func drawSkill(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("nothing was laid out")
	}
	m.Update(cmd())
}

func TestClickingTheSkillExpandsTheBoxToItsDiagramInStateColours(t *testing.T) {
	m, _ := harness(t, 140, 40)
	m.sidebarOpen = true
	m.snap.Skill = deepResearchView()
	m.layout()
	_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: m.mainWidth() + 4, Y: rowContaining(t, m, skillGlyph+" deep-research")})
	if m.skillPane == nil {
		t.Fatalf("a click on the skill did not expand the box:\n%s", screen(m))
	}
	drawSkill(t, m, cmd)
	lines := strings.Split(m.pane.GetContent(), "\n")
	for _, want := range []string{"│ scope", "│ synthesis", "│ file", "│ done", "│ pending"} {
		lineWith(t, lines, want)
	}
	if !sameInk(inkOf(t, lines[lineWith(t, lines, "scope")], "scope"), readyColor) {
		t.Error("a done step is not drawn in the done colour")
	}
	if sameInk(inkOf(t, lines[lineWith(t, lines, "vault")], "vault"), readyColor) {
		t.Error("a pending step is drawn as done")
	}
	if !strings.Contains(flatScreen(m), "skill deep-research · 1 of 8 done") || !strings.Contains(screen(m), skillHint) {
		t.Errorf("the expanded box does not name the skill or say how to leave:\n%s", screen(m))
	}

	m.snap.Skill.Nodes[1].State = skills.Done
	drawSkill(t, m, m.loadSkill())
	lines = strings.Split(m.pane.GetContent(), "\n")
	if !sameInk(inkOf(t, lines[lineWith(t, lines, "vault")], "vault"), readyColor) {
		t.Error("a step that finished while the box was open was not redrawn")
	}

	m.snap.Skill = nil
	m.layout()
	if !strings.Contains(flatScreen(m), "2 of 8 done · ended") || !strings.Contains(m.pane.GetContent(), "synthesis") {
		t.Errorf("a skill that ended should keep its last diagram up:\n%s", screen(m))
	}
	send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.panelOpen() {
		t.Error("esc did not collapse the box")
	}
}

func TestADoneStageHoldingWaiversIsDrawnWaivedWithItsCount(t *testing.T) {
	m, _ := harness(t, 140, 40)
	m.snap.Skill = deepResearchView()
	m.snap.Skill.Nodes[0].Waived = 2
	drawSkill(t, m, m.openSkill())
	lines := strings.Split(m.pane.GetContent(), "\n")
	lineWith(t, lines, "done · 2 waived")
	if !sameInk(inkOf(t, lines[lineWith(t, lines, "scope")], "scope"), pendingColor) {
		t.Error("a done stage holding waivers is not drawn in the waived colour")
	}
}

func TestTheSkillPaneListsEachWaiverWithItsReason(t *testing.T) {
	m, _ := harness(t, 140, 40)
	m.snap.Skill = deepResearchView()
	m.snap.Skill.Waivers = []skills.Waiver{{Step: "each[ch-2].code.verify", Reason: "no tests in this repo"}}
	drawSkill(t, m, m.openSkill())
	lines := strings.Split(ansi.Strip(m.pane.GetContent()), "\n")
	at := lineWith(t, lines, "Waived")
	if at+2 >= len(lines) || lines[at+1] != "each[ch-2].code.verify" || lines[at+2] != "  no tests in this repo" {
		t.Errorf("the waiver is not listed as its path over its reason:\n%s", strings.Join(lines, "\n"))
	}
}

func TestTheRailCountsTheSkillsDoneNodes(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.setSidebar(false)
	m.snap.Skill = deepResearchView()
	if box := plainBox(m); !strings.Contains(box, skillGlyph+"1") {
		t.Errorf("the rail lacks the skill's count:\n%s", box)
	}
}
