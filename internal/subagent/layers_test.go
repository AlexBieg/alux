package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
)

type fakeLayers struct {
	dir, origin string
	unseen      []string
	openErr     error
	landErr     error
	onClose     func()

	mu      sync.Mutex
	base    string
	landed  int
	dropped int
}

func (f *fakeLayers) Open(_ context.Context, _, _, base string) (Layer, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.mu.Lock()
	f.base = base
	f.mu.Unlock()
	return fakeLayer{f}, nil
}

type fakeLayer struct{ f *fakeLayers }

func (l fakeLayer) Dir() string      { return l.f.dir }
func (l fakeLayer) Origin() string   { return l.f.origin }
func (l fakeLayer) Unseen() []string { return l.f.unseen }
func (l fakeLayer) Base() string     { return fakeWork(l).Base() }
func (l fakeLayer) Close(context.Context) (Work, error) {
	if l.f.onClose != nil {
		l.f.onClose()
	}
	if _, err := os.Stat(filepath.Join(l.f.dir, "made.txt")); err != nil {
		return nil, nil
	}
	return fakeWork{l.f}, nil
}

type fakeWork struct{ f *fakeLayers }

func (w fakeWork) Change() string  { return "kqxzchange" }
func (w fakeWork) Files() []string { return []string{"made.txt"} }
func (w fakeWork) Base() string {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	return w.f.base
}
func (w fakeWork) Land(context.Context) (string, error) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	if w.f.landErr != nil {
		return "", w.f.landErr
	}
	w.f.landed++
	return "Landed in your working copy: made.txt.", nil
}
func (w fakeWork) Drop(context.Context) error {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	w.f.dropped++
	return nil
}

func (f *fakeLayers) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.landed, f.dropped
}

func layeredCoder(t *testing.T, turns ...mockapi.Turn) (*Manager, *mockapi.Server, *fakeLayers) {
	t.Helper()
	root := repoWith(t, "go.mod")
	ws, err := filepath.EvalSymlinks(repoWith(t, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	def := coderDef(root, nil)
	def.Model = func() string { return "mock" }
	m, server := manager(t, def, turns...)
	layers := &fakeLayers{dir: ws, origin: root}
	m.Layers = layers
	return m, server, layers
}

func createTurn() mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{
		{ID: "e1", Name: "str_replace_based_edit_tool", Input: map[string]any{"command": "create", "path": "made.txt", "file_text": "hi\n"}},
	}}
}

// foreground dispatches task and waits, approving the run's edits as the user would.
func foreground(m *Manager, task string) (string, error) {
	detach := false
	yes := agent.Callbacks{Approve: func(context.Context, string, string) bool { return true }}
	return m.DispatchWith(context.Background(), "coder", task, "", &detach, yes)
}

func land(t *testing.T, m *Manager, input map[string]any) (string, bool) {
	t.Helper()
	return toolText(t, m.runLand(context.Background(), call(t, LandTool, input), agent.Callbacks{}))
}

func TestABackgroundLayeredRunKeepsItsWorkUntilTheMainAgentLandsIt(t *testing.T) {
	m, server, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
	layers.unseen = []string{"wip.go"}
	detach := true
	report, err := m.DispatchWith(context.Background(), "coder", "make a file", "", &detach, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, LandTool) || !strings.Contains(report, "wip.go") {
		t.Errorf("the dispatch result %q does not name %s and the edits the run cannot see", report, LandTool)
	}
	if !m.Roster.Drain(10e9) {
		t.Fatal("the run never finished")
	}
	if _, err := os.Stat(filepath.Join(layers.origin, "made.txt")); !os.IsNotExist(err) {
		t.Error("the run wrote into the user's working copy")
	}
	sent, _ := json.Marshal(server.LastRequest()["messages"])
	if !strings.Contains(string(sent), "separate workspace at "+layers.dir) || !strings.Contains(string(sent), "wip.go") {
		t.Error("the run was not told where its workspace is and what it cannot see")
	}

	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Report, "Made it.") || !strings.Contains(outcomes[0].Report, "kqxzchange") {
		t.Fatalf("outcomes %+v, want the run's report naming its kept change", outcomes)
	}
	if landed, _ := layers.counts(); landed != 0 {
		t.Fatal("the run's work landed before the main agent asked")
	}

	run := m.Roster.Runs()[0].ID
	if text, failed := land(t, m, map[string]any{"run": run}); failed || !strings.Contains(text, "Landed") {
		t.Fatalf("%s gave %q (failed %v)", LandTool, text, failed)
	}
	if text, failed := land(t, m, map[string]any{"run": run}); !failed {
		t.Errorf("landing the same run again gave %q, want a refusal", text)
	}
	if landed, _ := layers.counts(); landed != 1 {
		t.Errorf("landed %d times, want once", landed)
	}
}

func TestABackgroundLayeredRunStillShowsRunningWhileItsWorkspaceCloses(t *testing.T) {
	m, _, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
	var during State
	layers.onClose = func() { during = m.Roster.Runs()[0].State }
	detach := true
	if _, err := m.DispatchWith(context.Background(), "coder", "make a file", "", &detach, agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(10e9) {
		t.Fatal("the run never finished")
	}
	if during != Running {
		t.Errorf("the run was %v while its workspace closed, want %v", during, Running)
	}
	if after := m.Roster.Runs()[0].State; after != Done {
		t.Errorf("the run ended %v, want %v", after, Done)
	}
}

func TestAForegroundLayeredRunReturnsWithItsWorkKeptNotLanded(t *testing.T) {
	m, _, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
	report, err := foreground(m, "make a file")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "Made it.") || !strings.Contains(report, "kqxzchange") || !strings.Contains(report, LandTool) {
		t.Errorf("report %q, want the run's report, its kept change and how to land it", report)
	}
	if landed, _ := layers.counts(); landed != 0 {
		t.Error("a foreground run landed on its own")
	}
}

func TestDroppingARunsWorkAbandonsItWithoutLanding(t *testing.T) {
	m, _, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
	if _, err := foreground(m, "make a file"); err != nil {
		t.Fatal(err)
	}
	if text, failed := land(t, m, map[string]any{"run": m.Roster.Runs()[0].ID, "drop": true}); failed {
		t.Fatalf("drop gave %q", text)
	}
	if landed, dropped := layers.counts(); landed != 0 || dropped != 1 {
		t.Errorf("landed %d, dropped %d; want only a drop", landed, dropped)
	}
}

func TestAFailedLandingKeepsTheWorkForAnotherTry(t *testing.T) {
	m, _, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
	if _, err := foreground(m, "make a file"); err != nil {
		t.Fatal(err)
	}
	run := m.Roster.Runs()[0].ID
	layers.landErr = errors.New("jj squash failed")
	if text, failed := land(t, m, map[string]any{"run": run}); !failed || !strings.Contains(text, "still kept") {
		t.Fatalf("a failed landing gave %q (failed %v), want it kept", text, failed)
	}
	layers.landErr = nil
	if text, failed := land(t, m, map[string]any{"run": run}); failed {
		t.Errorf("the retry gave %q", text)
	}
}

func TestARunShowsItsKeptFilesUntilTheyAreLanded(t *testing.T) {
	m, _, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
	if _, err := foreground(m, "make a file"); err != nil {
		t.Fatal(err)
	}
	kept := func() []string { return m.Roster.Runs()[0].Kept }
	if got := kept(); len(got) != 1 || got[0] != "made.txt" {
		t.Fatalf("kept %v after the run, want made.txt", got)
	}
	run := m.Roster.Runs()[0].ID
	layers.landErr = errors.New("jj squash failed")
	land(t, m, map[string]any{"run": run})
	if got := kept(); len(got) != 1 {
		t.Errorf("kept %v after a failed landing, want the change still listed", got)
	}
	layers.landErr = nil
	land(t, m, map[string]any{"run": run})
	if got := kept(); len(got) != 0 {
		t.Errorf("kept %v after landing, want nothing", got)
	}
}

func TestARunThatChangedNothingHasNothingToLand(t *testing.T) {
	m, _, _ := layeredCoder(t, mockapi.Turn{Text: "Nothing to do."})
	report, err := foreground(m, "look")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "changed no files") {
		t.Errorf("report %q, want it to say nothing changed", report)
	}
	if text, failed := land(t, m, map[string]any{"run": m.Roster.Runs()[0].ID}); !failed {
		t.Errorf("landing a run with no change gave %q, want a refusal", text)
	}
}

func TestACoderWithoutWorkspacesSaysItEditsTheWorkingCopy(t *testing.T) {
	m, _, _ := layeredCoder(t, mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: "Done."})
	m.Layers = nil
	detach := true
	started, err := m.DispatchWith(context.Background(), "coder", "make a file", "", &detach, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(started, "works in your working copy") {
		t.Errorf("the background start %q does not say the run edits the working copy", started)
	}
	if !m.Roster.Drain(10e9) {
		t.Fatal("the run never finished")
	}
	report, err := foreground(m, "make a file")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "works in your working copy") {
		t.Errorf("the foreground report %q does not say the run edited the working copy", report)
	}
}

func TestTheLandToolIsOfferedOnlyWhenCodersHaveWorkspaces(t *testing.T) {
	m, _, _ := layeredCoder(t)
	offered := func() bool {
		for _, tool := range m.Tools() {
			if tool.OfTool != nil && tool.OfTool.Name == LandTool {
				return true
			}
		}
		return false
	}
	if !offered() || m.Handlers()[LandTool] == nil {
		t.Error("the land tool is missing with layers on")
	}
	m.Layers = nil
	if offered() || m.Handlers()[LandTool] != nil {
		t.Error("the land tool is offered with layers off")
	}
}

func TestALayeredAgentIsNotStartedWhenItsWorkspaceCannotBeMade(t *testing.T) {
	m, server, layers := layeredCoder(t, mockapi.Turn{Text: "should not run"})
	layers.openErr = errors.New("prepare failed")
	detach := true
	if _, err := m.DispatchWith(context.Background(), "coder", "make a file", "", &detach, agent.Callbacks{}); err == nil || !strings.Contains(err.Error(), "prepare failed") {
		t.Fatalf("dispatch gave %v, want the workspace failure", err)
	}
	if len(m.Roster.Runs()) != 0 || server.LastRequest() != nil {
		t.Error("the run started anyway, in the user's working copy")
	}
}

func TestLandRunRefusesATrunkBasedChangeAndKeepsIt(t *testing.T) {
	m, _, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
	yes := agent.Callbacks{Approve: func(context.Context, string, string) bool { return true }}
	report, failed := toolText(t, m.runDispatch(context.Background(), call(t, DispatchTool, map[string]any{
		"agent": "coder", "name": "make a file", "objective": "make a file", "done": "it exists", "report": "say so",
		"base": BaseTrunk, "background": false,
	}), yes))
	if failed {
		t.Fatalf("the dispatch failed: %s", report)
	}
	run := m.Roster.Runs()[0].ID
	want := fmt.Sprintf("Run %d's change kqxzchange was cut from trunk, not from your working copy, so it cannot be landed into @. "+
		"It stays as its own change on trunk: run `jj bookmark create <name> -r kqxzchange` and pr_push it, or land_run drop: true to abandon it.", run)
	if !strings.Contains(report, want) {
		t.Errorf("the run's report %q does not say how to ship its trunk change", report)
	}
	if text, failed := land(t, m, map[string]any{"run": run}); !failed || text != want {
		t.Errorf("%s gave %q (failed %v), want the trunk refusal", LandTool, text, failed)
	}
	if landed, _ := layers.counts(); landed != 0 || len(m.Roster.Runs()[0].Kept) != 1 {
		t.Fatalf("landed %d, kept %v; want nothing landed and the change still kept", landed, m.Roster.Runs()[0].Kept)
	}
	if text, failed := land(t, m, map[string]any{"run": run, "drop": true}); failed {
		t.Errorf("drop gave %q", text)
	}
	if _, dropped := layers.counts(); dropped != 1 {
		t.Errorf("dropped %d, want the trunk change dropped", dropped)
	}
}

func TestPushingATrunkRunsChangeStopsItWaitingToLandWithoutDroppingIt(t *testing.T) {
	for _, base := range []string{BaseTrunk, BaseParent} {
		m, _, layers := layeredCoder(t, createTurn(), mockapi.Turn{Text: "Made it."})
		yes := agent.Callbacks{Approve: func(context.Context, string, string) bool { return true }}
		if report, failed := toolText(t, m.runDispatch(context.Background(), call(t, DispatchTool, map[string]any{
			"agent": "coder", "name": "make a file", "objective": "make a file", "done": "it exists", "report": "say so",
			"base": base, "background": false,
		}), yes)); failed {
			t.Fatalf("%s: the dispatch failed: %s", base, report)
		}
		run, shipped := m.Roster.Shipped("kqxzchangefullid")
		if shipped != (base == BaseTrunk) || (shipped && run != m.Roster.Runs()[0].ID) {
			t.Errorf("%s: Shipped gave run %d, %v", base, run, shipped)
		}
		if kept := len(m.Roster.Runs()[0].Kept) > 0; kept == shipped {
			t.Errorf("%s: shipped %v but kept %v", base, shipped, m.Roster.Runs()[0].Kept)
		}
		if _, dropped := layers.counts(); dropped != 0 {
			t.Errorf("%s: dropped %d, want the pushed change left alone", base, dropped)
		}
	}
}

func TestABaseIsRefusedWithoutAWorkspaceOrWithAChangeToEdit(t *testing.T) {
	m, server, layers := layeredCoder(t, mockapi.Turn{Text: "should not run"})
	detach := false
	_, _, err := m.dispatchDef(context.Background(), agent.Call{}, m.defs["coder"], "", nil, "kqxzchange", BaseTrunk, nil, "", "edit it", "", &detach, agent.Callbacks{})
	if err == nil || !strings.Contains(err.Error(), "agent coder") {
		t.Errorf("base with a change gave %v, want it refused naming the agent", err)
	}
	m.Layers = nil
	if _, err := m.dispatchWith(context.Background(), agent.Call{}, "coder", "", "make a file", "", BaseTrunk, &detach, nil, agent.Callbacks{}); err == nil ||
		!strings.Contains(err.Error(), "agent coder") {
		t.Errorf("base without a workspace gave %v, want it refused naming the agent", err)
	}
	if (fakeWork{layers}).Base() != "" || server.LastRequest() != nil {
		t.Error("a refused dispatch opened a workspace or started the run")
	}
}
