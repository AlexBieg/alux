package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// A pair can be detached. A conversation nobody is watching bills for both halves of itself, but
// it reports like any other outcome — the pair's report reaches the agent — so what is left is a
// cost the roster shows rather than a reason the thing cannot work.
func TestAPairCanBeDetached(t *testing.T) {
	m, _ := manager(t, paired(),
		mockapi.Turn{Text: "We agreed on it."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Nothing outstanding."},
	)
	yes := true

	report, err := m.DispatchWith(context.Background(), "driver", "do it", "", &yes, agent.Callbacks{})
	if err != nil {
		t.Fatalf("a pair was refused the background: %v", err)
	}
	if !strings.Contains(report, "background") {
		t.Fatalf("the pair blocked anyway: %q", report)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the detached pair never finished")
	}
	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Report, "We agreed on it.") {
		t.Errorf("outcomes = %+v, want the pair to report like any other run", outcomes)
	}
}

const (
	drivingSystem    = "you are driving"
	navigatingSystem = "you are navigating"
	planningSystem   = "you are planning"
)

// paired is a driver with a navigator and no planner. The three system prompts are
// distinguishable on purpose: one mock server answers every agent, so telling their requests
// apart is how a test knows which of them was talking.
//
// The driver's edit and bash tools are stand-ins under the real names, so the checkpoint
// recorder treats them as it would the coder's.
func paired() *Definition {
	return &Definition{
		Name:        "driver",
		Description: "test driver",
		System:      func() string { return drivingSystem },
		Tools: func() []anthropic.ToolUnionParam {
			text := map[string]any{"type": "string"}
			return []anthropic.ToolUnionParam{
				strict("note", "record something", map[string]any{"text": text}, []string{"text"}),
				strict("grep", "search", map[string]any{"pattern": text}, []string{"pattern"}),
				strict("bash", "run", map[string]any{"command": text}, []string{"command"}),
				strict("str_replace_based_edit_tool", "edit", map[string]any{"command": text, "path": text}, []string{"command"}),
			}
		},
		Handlers: map[string]agent.Handler{
			"note": func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
				return anthropic.NewToolResultBlock(call.ID, "noted", false)
			},
			"grep": func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
				return anthropic.NewToolResultBlock(call.ID, "SEARCH-RESULT-BODY", false)
			},
			"bash": func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
				var lines []string
				for i := 1; i <= 40; i++ {
					lines = append(lines, fmt.Sprintf("L%02d", i))
				}
				return anthropic.NewToolResultBlock(call.ID, strings.Join(lines, "\n")+"\n[exit 1]", false)
			},
			"str_replace_based_edit_tool": func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
				var in struct{ Command, Path, File_text string }
				_ = json.Unmarshal(call.Input, &in)
				switch in.Command {
				case "view":
					return anthropic.NewToolResultBlock(call.ID, "READ-RESULT-BODY", false)
				case "create":
					if filepath.IsAbs(in.Path) {
						_ = os.WriteFile(in.Path, []byte(in.File_text), 0o644)
					}
				}
				return anthropic.NewToolResultBlock(call.ID, "edited", false)
			},
		},
		Model:    func() string { return "mock" },
		MaxSteps: 12,
		Trail:    func(_ agent.ToolKind, label, _ string) string { return label },
		Commands: &guardian.Criteria{Agent: "driver", Allowed: []guardian.Category{guardian.Read, guardian.Build, guardian.Edit}, Threshold: 0.32},
		Peer: &PeerSpec{
			Def: &Definition{
				Name:     "navigator",
				System:   func() string { return navigatingSystem },
				Tools:    func() []anthropic.ToolUnionParam { return nil },
				Model:    func() string { return "mock" },
				MaxSteps: 1,
				Commands: lookOnly("navigator"),
			},
			Tool:        askTool,
			Description: "ask the navigator",
		},
	}
}

// withPlanner gives the pair a planner whose one tool returns something a review must never see.
func withPlanner(def *Definition) *Definition {
	def.Peer.Planner = &Definition{
		Name:   "navigator",
		System: func() string { return planningSystem },
		Tools: func() []anthropic.ToolUnionParam {
			return []anthropic.ToolUnionParam{strict("grep", "search",
				map[string]any{"pattern": map[string]any{"type": "string"}}, []string{"pattern"})}
		},
		Handlers: map[string]agent.Handler{
			"grep": func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
				return anthropic.NewToolResultBlock(call.ID, "PLANNING-READ-RESULT", false)
			},
		},
		Model:    func() string { return "mock" },
		MaxSteps: 4,
		Commands: lookOnly("navigator"),
	}
	return def
}

func ask(id, question, work string) mockapi.ToolUse {
	return mockapi.ToolUse{ID: id, Name: askTool, Input: map[string]any{"question": question, "work": work}}
}

func editTurn(id string) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: "str_replace_based_edit_tool",
		Input: map[string]any{"command": "str_replace", "path": "a.go", "old_str": "OLD-TEXT", "new_str": "NEW-TEXT"}}}}
}

func noteTurn(id string) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: "note", Input: map[string]any{"text": "x"}}}}
}

func repeat(n int, turn func(id string) mockapi.Turn) []mockapi.Turn {
	var out []mockapi.Turn
	for i := range n {
		out = append(out, turn(fmt.Sprintf("t%d", i)))
	}
	return out
}

// requestsTo is every request made to one agent, identified by its system prompt.
func requestsTo(t *testing.T, server *mockapi.Server, system string) []string {
	t.Helper()
	var out []string
	for _, raw := range server.Requests {
		if systemOf(t, raw) == system {
			out = append(out, string(raw))
		}
	}
	return out
}

// systemOf is the first line of a request's system prompt, which is the test's agent marker.
func systemOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var req struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.System) == 0 {
		return ""
	}
	return strings.SplitN(req.System[0].Text, "\n", 2)[0]
}

// rowFor finds a run by the agent that produced it.
func rowFor(t *testing.T, m *Manager, name string) Run {
	t.Helper()
	for _, run := range m.Roster.Runs() {
		if run.Agent == name {
			return run
		}
	}
	t.Fatalf("no row for %s; rows are %v", name, m.Roster.Runs())
	return Run{}
}

func dispatch(t *testing.T, m *Manager) string {
	t.Helper()
	report, err := m.Dispatch(context.Background(), "driver", "fix the KERNELWIDGET cache", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// The plan reaches the driver, and the reviews carry on the planning conversation, reads and all.
func TestThePlanReachesTheDriverAndTheReviewsCarryOnThePlanningConversation(t *testing.T) {
	m, server := manager(t, withPlanner(paired()),
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "p1", Name: "grep", Input: map[string]any{"pattern": "cache"}}}},
		mockapi.Turn{Text: "PLAN: change cache.go"},
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Nothing outstanding."},
	)
	dispatch(t, m)

	navigator := requestsTo(t, server, planningSystem)
	if len(navigator) != 4 || !strings.Contains(navigator[0], "KERNELWIDGET") {
		t.Fatalf("the navigator made %d requests, want two to plan, the end-of-turn review and the closing view", len(navigator))
	}
	driven := requestsTo(t, server, drivingSystem)
	if !strings.Contains(driven[0], "PLAN: change cache.go") || !strings.Contains(driven[0], `plan from=\"navigator\"`) {
		t.Errorf("the plan did not reach the driver's first request:\n%s", driven[0])
	}
	if got := requestsTo(t, server, navigatingSystem); len(got) != 0 {
		t.Errorf("%d reviews ran on a fresh system prompt instead of the planner's", len(got))
	}
	for _, req := range navigator[2:] {
		if !strings.Contains(req, "PLANNING-READ-RESULT") || !strings.Contains(req, "PLAN: change cache.go") {
			t.Errorf("a review does not carry the planning conversation:\n%s", req)
		}
		if strings.Contains(req, "The task, as the driver was given it") {
			t.Error("a review restated the task the planning conversation already holds")
		}
	}
}

// A navigator that reads gets the edited paths, not the edits, and the whole diff at the end.
func TestANavigatorThatReadsIsGivenFilePathsAndTheDiffAtTheEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	def := withViewingPlanner(paired())
	def.Peer.Root = root
	turns := append([]mockapi.Turn{{Text: "PLAN: edit a.go"}}, repeat(8, editTurn)...)
	turns = append(turns, mockapi.Turn{Text: none}, mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: none}, mockapi.Turn{Text: "Fine."})
	m, server := manager(t, def, turns...)
	dispatch(t, m)

	reviews := requestsTo(t, server, planningSystem)[1:3]
	checkpoint, end := reviews[0], reviews[1]
	if !strings.Contains(checkpoint, `Files edited (read them to see the change):\n- a.go\n`) {
		t.Errorf("the checkpoint does not list the edited file once:\n%s", checkpoint)
	}
	if strings.Contains(checkpoint, "NEW-TEXT") {
		t.Error("the checkpoint carried the edit itself")
	}
	if !strings.Contains(end, "The working copy is unchanged since the run began.") {
		t.Errorf("the end-of-turn review was not shown the diff:\n%s", end)
	}
}

// A planner out of turns is still asked for its plan, and a failure there is explained, not blank.
func TestAPlanForcedAtTheStepLimitReportsWhyItFailed(t *testing.T) {
	grep := mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "g", Name: "grep", Input: map[string]any{"pattern": "x"}}}}
	for _, tc := range []struct {
		name   string
		forced mockapi.Turn
		want   string
	}{
		{"recovered", mockapi.Turn{Text: "FORCED-PLAN"}, VerdictSaid},
		{"failed", mockapi.Turn{Status: 400}, VerdictFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := append(repeat(4, func(string) mockapi.Turn { return grep }), tc.forced,
				mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: none}, mockapi.Turn{Text: "Fine."})
			m, server := manager(t, withPlanner(paired()), turns...)
			var logged []PeerExchange
			m.PeerLog = func(e PeerExchange) { logged = append(logged, e) }
			dispatch(t, m)

			plan := logged[0]
			if plan.Kind != ExchangePlan || plan.Verdict != tc.want || plan.Steps != 4 || plan.Stop != agent.StepLimit {
				t.Errorf("plan record = %+v", plan)
			}
			trail := rowFor(t, m, "navigator").TrailText()
			driven := requestsTo(t, server, drivingSystem)[0]
			if tc.want == VerdictSaid && !strings.Contains(driven, "FORCED-PLAN") {
				t.Error("the forced plan did not reach the driver")
			}
			if tc.want == VerdictFailed && (plan.Err == "" || !strings.Contains(trail, "the plan it was then asked for failed")) {
				t.Errorf("the failure is not explained: err %q, trail:\n%s", plan.Err, trail)
			}
		})
	}
}

// Planning is help, not a gate: a planner that fails leaves a driver without a plan, not a
// failed run.
func TestAFailedPlanLeavesTheDriverWorkingWithoutOne(t *testing.T) {
	m, server := manager(t, withPlanner(paired()),
		mockapi.Turn{Status: 400},
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Nothing outstanding."},
	)
	if report := dispatch(t, m); !strings.HasPrefix(report, "Done.") {
		t.Errorf("report = %q, want the driver's", report)
	}
	if driven := requestsTo(t, server, drivingSystem); strings.Contains(driven[0], "<plan") {
		t.Errorf("a failed plan still put a plan in the driver's prompt:\n%s", driven[0])
	}
}

// alux reviews after eight steps that edited, and does not count steps that only looked.
func TestACheckpointFiresAfterEightEditingStepsAndNotAfterEightOthers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps func(string) mockapi.Turn
		want  int
	}{
		{"editing", editTurn, 3},
		{"non-editing", noteTurn, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := repeat(8, tc.steps)
			if tc.want == 3 {
				turns = append(turns, mockapi.Turn{Text: none})
			}
			turns = append(turns, mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: none}, mockapi.Turn{Text: "Fine."})
			m, server := manager(t, paired(), turns...)
			dispatch(t, m)

			if got := len(requestsTo(t, server, navigatingSystem)); got != tc.want {
				t.Fatalf("the navigator was asked %d times, want %d", got, tc.want)
			}
			if tc.want == 3 && systemOf(t, server.Requests[8]) != navigatingSystem {
				t.Error("the checkpoint did not come straight after the eighth editing step")
			}
		})
	}
}

// What the navigator is shown is what a reviewer needs and not what is cheap to collect: edits in
// full and commands with their tails, but none of the reading.
func TestTheDeltaCarriesEditsAndCommandTailsButNotReads(t *testing.T) {
	m, server := manager(t, paired(),
		mockapi.Turn{Text: "I am changing the cache key.", Tools: []mockapi.ToolUse{
			{ID: "v1", Name: "str_replace_based_edit_tool", Input: map[string]any{"command": "view", "path": "a.go"}},
			{ID: "g1", Name: "grep", Input: map[string]any{"pattern": "cache"}},
		}},
		editTurn("e1"),
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "b1", Name: "bash", Input: map[string]any{"command": "go test ./..."}}}},
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Fine."},
	)
	dispatch(t, m)

	review := requestsTo(t, server, navigatingSystem)[0]
	for _, want := range []string{"I am changing the cache key.", "OLD-TEXT", "NEW-TEXT", "$ go test ./...", "L40", "[exit 1]"} {
		if !strings.Contains(review, want) {
			t.Errorf("the review is missing %q", want)
		}
	}
	for _, leaked := range []string{"READ-RESULT-BODY", "SEARCH-RESULT-BODY", "L05", drivingSystem} {
		if strings.Contains(review, leaked) {
			t.Errorf("%q reached the review", leaked)
		}
	}
}

// Silence is the default, and costs the driver nothing: no text in its next request, and the
// row says the navigator never intervened.
func TestASilentCheckpointInjectsNothing(t *testing.T) {
	turns := append(repeat(8, editTurn), mockapi.Turn{Text: none}, mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: none}, mockapi.Turn{Text: "Fine."})
	m, server := manager(t, paired(), turns...)
	report := dispatch(t, m)

	for _, req := range requestsTo(t, server, drivingSystem) {
		if strings.Contains(req, "navigator-steer") {
			t.Fatal("a silent checkpoint put a steer in the driver's conversation")
		}
	}
	if !strings.HasPrefix(report, "Done.") {
		t.Errorf("report = %q", report)
	}
	if nav := rowFor(t, m, "navigator"); !strings.Contains(nav.Result, "Stayed silent at every checkpoint") || nav.Live() {
		t.Errorf("the navigator's row = %s %q, want it closed and saying it stayed silent", nav.State, nav.Result)
	}
}

// A pass and a reply with no text are different findings, in the trail and in the log.
func TestAnEmptyReviewIsLoggedApartFromAPass(t *testing.T) {
	turns := append(repeat(8, editTurn), mockapi.Turn{Thinking: "WHY-I-PASSED", Text: none},
		mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: ""}, mockapi.Turn{Text: "Fine."})
	m, _ := manager(t, paired(), turns...)
	var logged []PeerExchange
	m.PeerLog = func(e PeerExchange) { logged = append(logged, e) }
	dispatch(t, m)

	trail := rowFor(t, m, "navigator").TrailText()
	if !strings.Contains(trail, "navigator: "+none) || !strings.Contains(trail, "navigator: (empty reply") {
		t.Errorf("the trail does not tell the pass from the empty reply:\n%s", trail)
	}

	var kinds, verdicts []string
	for _, e := range logged {
		kinds, verdicts = append(kinds, e.Kind), append(verdicts, e.Verdict)
	}
	if want := []string{ExchangeCheckpoint, ExchangeEnd, ExchangeClosing}; !slices.Equal(kinds, want) {
		t.Fatalf("logged kinds = %v, want %v", kinds, want)
	}
	if want := []string{VerdictNone, VerdictEmpty, VerdictSaid}; !slices.Equal(verdicts, want) {
		t.Errorf("logged verdicts = %v, want %v", verdicts, want)
	}
	if cp := logged[0]; cp.Thinking != "WHY-I-PASSED" || !strings.Contains(cp.Sent, "NEW-TEXT") || !strings.Contains(cp.Sent, "<task>") {
		t.Errorf("the checkpoint record lacks the thinking, the edits or the preface: %+v", cp)
	}
}

// A steer reaches the driver in its very next request, framed as a hypothesis, and the review
// that produced it is not one of the driver's steps.
func TestASteerReachesTheNextRequestWithThePreambleAndCostsNoStep(t *testing.T) {
	def := paired()
	// Eight edits and a report: a review charged to the driver would leave no step to report in.
	def.MaxSteps = 9
	turns := append(repeat(8, editTurn), mockapi.Turn{Text: "STEER: the key is the raw string."},
		mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: "Closing."})
	m, server := manager(t, def, turns...)
	report := dispatch(t, m)

	if strings.Contains(report, "Stopped after") {
		t.Errorf("the driver ran out of steps: %q", report)
	}
	driven := requestsTo(t, server, drivingSystem)
	if len(driven) != 9 {
		t.Fatalf("the driver made %d requests, want 9", len(driven))
	}
	if !strings.Contains(driven[8], "STEER: the key is the raw string.") || !strings.Contains(driven[8], "cannot build or run anything") {
		t.Errorf("the steer did not reach the next request with its preamble:\n%s", driven[8])
	}
	if strings.Contains(driven[7], "STEER") {
		t.Error("the steer arrived before the checkpoint that produced it")
	}
	if !slices.ContainsFunc(rowFor(t, m, "driver").Trail, func(e Entry) bool {
		return e.Kind == EntrySteer && e.From == "navigator" && strings.HasPrefix(e.Text, "STEER")
	}) {
		t.Errorf("the driver's row has no marker for the steer:\n%s", rowFor(t, m, "driver").TrailText())
	}
	if trail := rowFor(t, m, "navigator").TrailText(); !strings.Contains(trail, "the key is the raw string.") {
		t.Errorf("the steer is not on the navigator's row:\n%s", trail)
	}
}

// The end of the turn is reviewed against the working copy as alux sees it, and a steer
// there reopens the turn — once. Without the bound a navigator that always objects would hold the
// driver open until the steer budget ran out.
func TestTheEndOfTurnReviewSeesTheRealDiffAndSteersOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	def := paired()
	def.Peer.Root = root
	path := filepath.Join(root, "made.go")

	m, server := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "c1", Name: "str_replace_based_edit_tool",
			Input: map[string]any{"command": "create", "path": path, "file_text": "WRITTEN-TO-DISK"}}}},
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: "STEER: handle nil too."},
		mockapi.Turn{Text: "Handled nil."},
		mockapi.Turn{Text: "STEER: and again."},
	)
	report := dispatch(t, m)

	reviews := requestsTo(t, server, navigatingSystem)
	if len(reviews) != 2 {
		t.Fatalf("the navigator was asked %d times, want one end-of-turn review and the closing view", len(reviews))
	}
	if !strings.Contains(reviews[0], "diff --git") || !strings.Contains(reviews[0], "+WRITTEN-TO-DISK") {
		t.Errorf("the end-of-turn review was not shown the working copy's diff:\n%s", reviews[0])
	}
	driven := requestsTo(t, server, drivingSystem)
	if len(driven) != 3 || !strings.Contains(driven[2], "handle nil too") {
		t.Fatalf("the steer did not reopen the turn (%d driver requests)", len(driven))
	}
	if !strings.HasPrefix(report, "Handled nil.") {
		t.Errorf("report = %q, want the driver's answer to the steer", report)
	}
}

// Asking continues the review conversation, so the navigator answers with its own steers and
// earlier answers in mind.
func TestAskingTheNavigatorContinuesTheSameConversation(t *testing.T) {
	m, server := manager(t, paired(),
		mockapi.Turn{Tools: []mockapi.ToolUse{ask("c1", "should I use a mutex here?", "var n int")}},
		mockapi.Turn{Text: "No. Make it a channel."},
		mockapi.Turn{Tools: []mockapi.ToolUse{ask("c2", "like this?", "ch := make(chan int)")}},
		mockapi.Turn{Text: "Yes, that is what I meant."},
		mockapi.Turn{Text: "Done — switched to a channel."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "It took the advice."},
	)
	report := dispatch(t, m)
	if !strings.HasPrefix(report, "Done — switched to a channel.") {
		t.Errorf("report = %q, want the driver's own words first", report)
	}

	asked := requestsTo(t, server, navigatingSystem)
	if len(asked) != 4 {
		t.Fatalf("the navigator was asked %d times, want two questions, the end review and the closing view", len(asked))
	}
	for _, want := range []string{"KERNELWIDGET", "should I use a mutex here?", "No. Make it a channel.", "like this?"} {
		if !strings.Contains(asked[1], want) {
			t.Errorf("the second question does not carry %q, so the navigator is answering without its history", want)
		}
	}
}

// A pair is two rows, and the second says whose it is.
func TestAPairIsTwoRowsLinkedToTheDriver(t *testing.T) {
	m, _ := manager(t, paired(),
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Nothing outstanding."},
	)

	var boxes []int
	cb := agent.Callbacks{OnRun: func(id int) { boxes = append(boxes, id) }}
	if _, err := m.Dispatch(context.Background(), "driver", "do it", "", cb); err != nil {
		t.Fatal(err)
	}

	runs := m.Roster.Runs()
	if len(runs) != 2 {
		t.Fatalf("a pair made %d rows, want 2", len(runs))
	}
	driver, nav := rowFor(t, m, "driver"), rowFor(t, m, "navigator")
	if nav.Parent != driver.ID {
		t.Errorf("the navigator's parent is %d, want the driver's id %d", nav.Parent, driver.ID)
	}
	if driver.Parent != 0 {
		t.Errorf("the driver has a parent (%d); it is the top of the pair", driver.Parent)
	}
	if len(boxes) != 2 {
		t.Errorf("a pair announced %d boxes, want 2", len(boxes))
	}
	if nav.Live() {
		t.Errorf("the navigator is still %s after its driver finished", nav.State)
	}
}

// The navigator's advice lives in its answers, and asking again gets a different conversation.
func TestTheNavigatorsAnswersAreNeverCleared(t *testing.T) {
	m, server := manager(t, paired(),
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Nothing outstanding."},
	)
	dispatch(t, m)

	driving := requestsTo(t, server, drivingSystem)
	if len(driving) == 0 {
		t.Fatal("the driver made no request")
	}
	if got := clearedSpares(t, []byte(driving[0])); got != "["+askTool+"]" {
		t.Errorf("exclude_tools = %s, want [%s]", got, askTool)
	}
}

// The navigator's own closing view reaches the dispatching agent, attributed and after the
// driver's report: two agents who disagreed must not arrive as one consensus.
func TestTheNavigatorsClosingViewReachesTheReport(t *testing.T) {
	m, _ := manager(t, paired(),
		mockapi.Turn{Text: "Did it anyway."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "I objected to the shared cache key and it went ahead regardless."},
	)
	report := dispatch(t, m)

	if !strings.Contains(report, "I objected to the shared cache key and it went ahead regardless.") {
		t.Errorf("the navigator's closing view did not reach the report: %q", report)
	}
	if !strings.Contains(report, "<navigator>") {
		t.Errorf("the navigator's view is unattributed, so it reads as the driver's: %q", report)
	}
	if strings.Index(report, "Did it anyway.") > strings.Index(report, "<navigator>") {
		t.Error("the navigator's view comes before the driver's report")
	}
}

// A run that stops on its step limit never had its end reviewed, and the closing view is told
// so and shown the state it was left in.
func TestAnUnfinishedRunsClosingViewSaysSoAndSeesTheEdits(t *testing.T) {
	def := paired()
	def.MaxSteps = 1
	m, server := manager(t, def,
		editTurn("e1"),
		mockapi.Turn{Text: "Ran out."},
		mockapi.Turn{Text: "It did not finish."},
	)
	dispatch(t, m)

	reviews := requestsTo(t, server, navigatingSystem)
	if len(reviews) != 1 {
		t.Fatalf("the navigator was asked %d times, want only the closing view", len(reviews))
	}
	if !strings.Contains(reviews[0], "ran out of turns") || !strings.Contains(reviews[0], "NEW-TEXT") {
		t.Errorf("the closing view was not told the run is unfinished or not shown its edits:\n%s", reviews[0])
	}
}

// The exchange is recorded on the navigator's row, in full and in both directions, and the
// driver's row keeps a marker rather than the text.
func TestTheExchangeIsRecordedOnTheNavigatorsRow(t *testing.T) {
	objection := "This caches the parsed value but the key is the raw string, so two spellings of the " +
		"same path get two entries and the second one wins at random.\n\nKey it on the cleaned path instead."

	m, _ := manager(t, paired(),
		mockapi.Turn{Tools: []mockapi.ToolUse{ask("c1", "is the cache key sound?", "cache[s] = v")}},
		mockapi.Turn{Text: objection},
		mockapi.Turn{Text: "Fixed."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Nothing outstanding."},
	)
	dispatch(t, m)

	nav := rowFor(t, m, "navigator")
	if !slices.ContainsFunc(nav.Trail, func(e Entry) bool {
		return e.Kind == EntryExchange && e.From == "driver" && e.To == "navigator" &&
			e.Prompt == "is the cache key sound?" && e.Text == objection
	}) {
		t.Errorf("the exchange is not on the navigator's row, question and whole reply:\n%s", nav.TrailText())
	}
	driver := rowFor(t, m, "driver").TrailText()
	if !strings.Contains(driver, "navigator · ") {
		t.Errorf("the driver's row does not show that it asked:\n%s", driver)
	}
	if strings.Contains(driver, "Key it on the cleaned path instead.") {
		t.Errorf("the whole objection is duplicated onto the driver's row:\n%s", driver)
	}
}

// Neither the planner's turns nor the navigator's are the driver's. A peer that reported through
// its driver's callbacks would rewrite the driver's step count, so the row is watched as it
// changes.
func TestThePeerDoesNotRewindTheDriversProgress(t *testing.T) {
	server := mockapi.New(
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "p1", Name: "grep", Input: map[string]any{"pattern": "a"}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "p2", Name: "grep", Input: map[string]any{"pattern": "b"}}}},
		mockapi.Turn{Text: "PLAN"},
		mockapi.Turn{Tools: []mockapi.ToolUse{ask("c1", "well?", "x := 1")}},
		mockapi.Turn{Text: "Looks fine."},
		mockapi.Turn{Tools: []mockapi.ToolUse{ask("c2", "and now?", "x := 2")}},
		mockapi.Turn{Text: "Still fine."},
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: none},
		mockapi.Turn{Text: "Nothing outstanding."},
	)
	t.Cleanup(server.Close)
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))

	var mu sync.Mutex
	var seen []int
	roster := NewRoster(nil)
	roster.notify = func() {
		mu.Lock()
		defer mu.Unlock()
		for _, run := range roster.Runs() {
			if run.Parent == 0 {
				seen = append(seen, run.Step)
			}
		}
	}

	m, err := NewManager(roster, withPlanner(paired()))
	if err != nil {
		t.Fatal(err)
	}
	m.Client = &client
	dispatch(t, m)

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("the driver's step went %d → %d: a peer's turn is driving the driver's row (saw %v)",
				seen[i-1], seen[i], seen)
		}
	}
}

// Asking does not come out of the driver's turns: a driver short on turns would otherwise stop
// asking exactly when a second opinion is worth most.
func TestAskingDoesNotSpendTheDriversTurns(t *testing.T) {
	def := paired()
	def.MaxSteps = 2
	m, server := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{ask("c1", "shall I?", "x := 1")}},
		mockapi.Turn{Text: "Go ahead."},
		noteTurn("t1"),
		mockapi.Turn{Text: "Done."},
		mockapi.Turn{Text: "Nothing outstanding."},
	)
	if report := dispatch(t, m); strings.Contains(report, "Stopped after") {
		t.Errorf("the driver ran out of turns paying for its question: %q", report)
	}
	if got := len(requestsTo(t, server, drivingSystem)); got != 3 {
		t.Errorf("the driver made %d requests on a budget of 2 plus one free question, want 3", got)
	}
}

// A peer that can stop to ask the user deadlocks the pair, and has to fail at boot because a
// hang gives no clue where it came from. The planner runs where nothing can answer either.
func TestAPeerOrPlannerThatAsksIsRefusedAtBoot(t *testing.T) {
	asks := &agent.Rules{
		Tools:         []agent.Rule{{Pattern: "anything", Verdict: agent.VerdictAsk}},
		DefaultReason: "test",
	}
	reviewer := paired()
	reviewer.Peer.Def.Policy = asks
	planner := withPlanner(paired())
	planner.Peer.Planner.Policy = asks

	for name, def := range map[string]*Definition{"reviewer": reviewer, "planner": planner} {
		_, err := NewManager(NewRoster(nil), def)
		if err == nil {
			t.Fatalf("a %s that can ask was accepted", name)
		}
		if !strings.Contains(err.Error(), "approval") {
			t.Errorf("the error does not say what the problem is: %v", err)
		}
	}
}

// A planner with a fallback reaches whatever name its handlers do not match, so it is not
// read-only whatever its handlers are.
func TestAPlannerWithAFallbackIsRefusedAtBoot(t *testing.T) {
	def := withPlanner(paired())
	def.Peer.Planner.Fallback = agent.RunBash
	if _, err := NewManager(NewRoster(nil), def); err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Errorf("a planner with a fallback was not refused: %v", err)
	}
}

// Offering a tool the driver's own policy will refuse costs it a turn to discover, in the middle
// of a run, as a refusal that reads like a bug. It is decidable at boot, so it is decided there.
func TestADriverWhosePolicyDeniesTheAskToolIsRefusedAtBoot(t *testing.T) {
	def := paired()
	def.Policy = &agent.Rules{DefaultReason: "test"}

	_, err := NewManager(NewRoster(nil), def)
	if err == nil {
		t.Fatal("a driver that may not call its own ask tool was accepted")
	}
	if !strings.Contains(err.Error(), askTool) {
		t.Errorf("the error does not name the tool: %v", err)
	}
}

// The pair's extra permission does not reach the plain coder. Both are built from CoderRules, and
// the whole defence is that it returns a fresh value per call.
func TestPairingDoesNotWidenThePlainCoder(t *testing.T) {
	root := t.TempDir()
	pair := pairedCoderDef(root, nil)
	if err := pair.validate(); err != nil {
		t.Fatalf("the paired coder does not validate: %v", err)
	}
	if !pair.Policy.Permits(askTool) {
		t.Fatal("the paired coder may not call its own ask tool")
	}
	if solo := coderDef(root, nil); solo.Policy.Permits(askTool) {
		t.Error("building a pair widened the plain coder's policy")
	}
}

// The reviewer ships with nothing to call: it is what makes a review unable to raise an
// approval. A nil policy is unbounded, not empty, so the tool list is the guarantee.
func TestTheNavigatorHasNoTools(t *testing.T) {
	nav := Navigator()
	if tools := nav.Tools(); len(tools) != 0 {
		t.Errorf("the navigator offers %d tools", len(tools))
	}
	if len(nav.Handlers) != 0 {
		t.Errorf("the navigator has %d handlers", len(nav.Handlers))
	}
	if nav.Fallback != nil {
		t.Error("the navigator has a fallback, so an unmatched tool name reaches something")
	}
}

// The planner can look and nothing else: its editor refuses a write, and its shell is the read-only one.
func TestThePlannerOnlyReads(t *testing.T) {
	root := t.TempDir()
	planner := Planner(root)
	var names []string
	for _, tool := range planner.Tools() {
		names = append(names, *tool.GetName())
	}
	if strings.Join(names, ",") != "str_replace_based_edit_tool,glob,grep" {
		t.Errorf("the planner offers %v", names)
	}
	if _, ok := planner.Handlers["bash"]; ok || planner.Fallback != nil {
		t.Error("the planner can still reach a shell")
	}
	path := filepath.Join(root, "written.txt")
	res := planner.Handlers["str_replace_based_edit_tool"](context.Background(), agent.Call{
		ID: "c1", Name: "str_replace_based_edit_tool",
		Input: json.RawMessage(`{"command":"create","path":"` + path + `","file_text":"x"}`),
	}, agent.Callbacks{})
	if !res.OfToolResult.IsError.Or(false) {
		t.Error("the planner's editor accepted a write")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the planner wrote a file")
	}
}
