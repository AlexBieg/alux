package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/anthropics/anthropic-sdk-go"
)

var (
	passing = guardian.Verdict{Agent: "test", Top: guardian.Build, Danger: 0.02, Threshold: 0.3}
	stopped = guardian.Verdict{Agent: "test", Top: guardian.Network, Danger: 0.95, Threshold: 0.3, Flagged: true}
)

type fakeScreen struct {
	verdict    guardian.Verdict
	suggestion guardian.Suggestion
	mu         sync.Mutex
	asked      []string
	records    []string
	menus      [][]string
}

func (f *fakeScreen) Suggest(_ context.Context, _ string, tools []string) guardian.Suggestion {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.menus = append(f.menus, tools)
	return f.suggestion
}

func (f *fakeScreen) RecordSuggested(command string, _ guardian.Verdict, _ guardian.Suggestion, decision, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if outcome != "" {
		decision += "/" + outcome
	}
	f.records = append(f.records, decision+" "+command)
}

func (f *fakeScreen) Screen(_ context.Context, command string) guardian.Verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, command)
	return f.verdict
}

func (f *fakeScreen) Record(command string, _ guardian.Verdict, decision string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, decision+" "+command)
}

func shellTurn(name, command string) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: name, Input: map[string]any{"command": command}}}}
}

func lastResult(t *testing.T, a *Agent, r *recorder) string {
	t.Helper()
	return toolText(a.RunTurn(context.Background(), user("go"), r.callbacks()).History)
}

func toolText(history []anthropic.MessageParam) string {
	for _, m := range history {
		for _, b := range m.Content {
			if b.OfToolResult != nil {
				for _, c := range b.OfToolResult.Content {
					if c.OfText != nil {
						return c.OfText.Text
					}
				}
			}
		}
	}
	return ""
}

// No criteria, no shell: the one way to bypass the screen is to not have one, and that refuses.
func TestAnAgentWithoutCriteriaCannotRunAShellCommand(t *testing.T) {
	a, _ := testAgent(t, shellTurn("bash", "echo should-not-run"), mockapi.Turn{Text: "done"})
	a.Screen = nil
	out := lastResult(t, a, &recorder{})
	if !strings.Contains(out, "no command criteria") || strings.Contains(out, "should-not-run\n") {
		t.Errorf("a shell command ran without criteria: %q", out)
	}
}

// The fallback turns an unknown tool name into a shell command, so it must be screened like bash.
func TestEveryRouteToAShellIsScreened(t *testing.T) {
	for _, name := range []string{"bash", "made_up_tool", "run_background"} {
		a, _ := testAgent(t, shellTurn(name, "echo hi"), mockapi.Turn{Text: "done"})
		f := &fakeScreen{verdict: stopped}
		a.Screen = f
		lastResult(t, a, &recorder{})
		if len(f.asked) != 1 || f.asked[0] != "echo hi" {
			t.Errorf("%s: screen asked about %v, want [echo hi]", name, f.asked)
		}
	}
}

func TestAStoppedCommandIsPutToThePersonEvenWhenTheyApproveEverything(t *testing.T) {
	a, _ := testAgent(t, shellTurn("bash", "echo approved-ran"), mockapi.Turn{Text: "done"})
	f := &fakeScreen{verdict: stopped}
	a.Screen = f
	r := &recorder{}
	var asked []string
	cb := r.callbacks()
	cb.Require = func(_ context.Context, label, _, reason string) bool {
		asked = append(asked, label+" | "+reason)
		return true
	}
	res := a.RunTurn(context.Background(), user("go"), cb)
	if len(asked) != 1 || !strings.Contains(asked[0], "network") {
		t.Fatalf("not put to the person, or without the reason: %v", asked)
	}
	if !strings.Contains(strings.Join(r.results, "\n"), "approved-ran") {
		t.Errorf("an approved command did not run: %v (%s)", r.results, res.StopReason)
	}
	if len(f.records) != 1 || !strings.HasPrefix(f.records[0], "approved ") {
		t.Errorf("recorded %v, want one approved", f.records)
	}
}

// A subagent running unattended has no Require, so a stopped command is refused rather than run.
func TestAStoppedCommandWithNobodyToAskIsRefused(t *testing.T) {
	a, _ := testAgent(t, shellTurn("bash", "echo should-not-run"), mockapi.Turn{Text: "done"})
	f := &fakeScreen{verdict: stopped}
	a.Screen = f
	r := &recorder{}
	out := lastResult(t, a, r)
	if !strings.HasPrefix(out, "Refused") || strings.Contains(strings.Join(r.results, "\n"), "should-not-run") {
		t.Errorf("a stopped command was not refused: %q", out)
	}
	if len(f.records) != 1 || !strings.HasPrefix(f.records[0], "refused ") {
		t.Errorf("recorded %v, want one refused", f.records)
	}
}

// guardian down must not mean unchecked: a command it could not judge stops like a flagged one.
func TestACommandGuardianCouldNotJudgeIsStopped(t *testing.T) {
	a, _ := testAgent(t, shellTurn("bash", "echo should-not-run"), mockapi.Turn{Text: "done"})
	a.Screen = &fakeScreen{}
	if out := lastResult(t, a, &recorder{}); !strings.Contains(out, "could not judge") {
		t.Errorf("an unjudged command was not stopped: %q", out)
	}
}

func allows(cats ...guardian.Category) func(guardian.Category) bool {
	return func(c guardian.Category) bool { return slices.Contains(cats, c) }
}

// With a mode, the screen is the whole decision for a shell command: no second prompt either way.
func TestAModedCommandIsAskedAboutAtMostOnce(t *testing.T) {
	for _, tc := range []struct {
		verdict  guardian.Verdict
		requires int
	}{{passing, 0}, {stopped, 1}} {
		a, _ := testAgent(t, shellTurn("bash", "echo ran"), mockapi.Turn{Text: "done"})
		a.Screen = &fakeScreen{verdict: tc.verdict}
		a.Allows = allows(guardian.Read)
		r := &recorder{}
		cb := r.callbacks()
		requires := 0
		cb.Require = func(context.Context, string, string, string) bool { requires++; return true }
		a.RunTurn(context.Background(), user("go"), cb)
		if requires != tc.requires || len(r.approvals) != 0 {
			t.Errorf("%s: %d requires, approvals %v; want %d and none", tc.verdict.Top, requires, r.approvals, tc.requires)
		}
		if !strings.Contains(strings.Join(r.results, "\n"), "ran") {
			t.Errorf("%s: the command did not run: %v", tc.verdict.Top, r.results)
		}
	}
}

// Without a mode the handler still asks, which is how read-only mode reaches a subagent's shell.
func TestAnUnmodedCommandStillGoesThroughTheHandlersApproval(t *testing.T) {
	a, _ := testAgent(t, shellTurn("bash", "echo ran"), mockapi.Turn{Text: "done"})
	r := &recorder{}
	lastResult(t, a, r)
	if len(r.approvals) != 1 {
		t.Errorf("approvals %v, want the handler's one", r.approvals)
	}
}

func TestANetworkToolAsksOutsideAutoAndRunsInIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		allowed  []guardian.Category
		requires int
	}{{"local", []guardian.Category{guardian.Read, guardian.Edit}, 1}, {"auto", []guardian.Category{guardian.Read, guardian.Network}, 0}} {
		a, _ := testAgent(t, mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "mcp__srv__send", Input: map[string]any{"x": 1}}}},
			mockapi.Turn{Text: "done"})
		a.Handlers = map[string]Handler{"mcp__srv__send": func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
			if !cb.Approve(ctx, "mcp: send", "") {
				return declined(call.ID, "tool call")
			}
			return ok(call.ID, "sent")
		}}
		a.Allows = allows(tc.allowed...)
		r := &recorder{}
		cb := r.callbacks()
		var reasons []string
		cb.Require = func(_ context.Context, _, _, reason string) bool { reasons = append(reasons, reason); return true }
		res := a.RunTurn(context.Background(), user("go"), cb)
		if len(reasons) != tc.requires || len(r.approvals) != 0 {
			t.Errorf("%s: requires %v, approvals %v; want %d and none", tc.name, reasons, r.approvals, tc.requires)
		}
		if got := toolText(res.History); got != "sent" {
			t.Errorf("%s: tool answered %q, want sent", tc.name, got)
		}
	}
}

func TestAPreapprovedToolSkipsItsPromptUntilPHIIsRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preapproved bool
		tainted     bool
		asks        int
	}{{"not preapproved", false, false, 1}, {"preapproved", true, false, 0}, {"preapproved after PHI", true, true, 1}} {
		if tc.tainted {
			taint.Set()
		}
		a, _ := testAgent(t, mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "mcp__glean__search", Input: map[string]any{"q": 1}}}},
			mockapi.Turn{Text: "done"})
		a.Handlers = map[string]Handler{"mcp__glean__search": func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
			if !cb.Approve(ctx, "mcp: search", "") {
				return declined(call.ID, "tool call")
			}
			return ok(call.ID, "found")
		}}
		a.Allows = allows(guardian.Read, guardian.Edit)
		a.Preapproved = func(name string) bool { return tc.preapproved && name == "mcp__glean__search" }
		r := &recorder{}
		cb := r.callbacks()
		asks := 0
		cb.Require = func(context.Context, string, string, string) bool { asks++; return true }
		res := a.RunTurn(context.Background(), user("go"), cb)
		if asks != tc.asks || len(r.approvals) != 0 || toolText(res.History) != "found" {
			t.Errorf("%s: asked %d times, approvals %v, answer %q; want %d asks", tc.name, asks, r.approvals, toolText(res.History), tc.asks)
		}
		taint.ResetForTest()
	}
}

func TestOnlyAPreapprovedCallRunsWaived(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preapproved bool
		tainted     bool
		inherited   bool
		waived      bool
	}{
		{"not preapproved", false, false, false, false},
		{"preapproved", true, false, false, true},
		{"preapproved after PHI", true, true, false, false},
		{"marker inherited from the caller", false, false, true, false},
	} {
		if tc.tainted {
			taint.Set()
		}
		a, _ := testAgent(t, mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "mcp__glean__search", Input: map[string]any{"q": 1}}}},
			mockapi.Turn{Text: "done"})
		var waived bool
		a.Handlers = map[string]Handler{"mcp__glean__search": func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
			waived = SkillWaived(ctx)
			return ok(call.ID, "found")
		}}
		a.Allows = allows(guardian.Read, guardian.Edit)
		a.Preapproved = func(name string) bool { return tc.preapproved }
		cb := (&recorder{}).callbacks()
		cb.Require = func(context.Context, string, string, string) bool { return true }
		ctx := context.Background()
		if tc.inherited {
			ctx = WithSkillWaiver(ctx)
		}
		a.RunTurn(ctx, user("go"), cb)
		if waived != tc.waived {
			t.Errorf("%s: SkillWaived = %v, want %v", tc.name, waived, tc.waived)
		}
		taint.ResetForTest()
	}
}

func TestAPreapprovedShellToolIsStillScreened(t *testing.T) {
	a, _ := testAgent(t, shellTurn("bash", "echo ran"), mockapi.Turn{Text: "done"})
	a.Preapproved = func(string) bool { return true }
	r := &recorder{}
	lastResult(t, a, r)
	if len(r.approvals) != 1 {
		t.Errorf("approvals %v: a preapproved bash must still go through its handler's approval", r.approvals)
	}
}

func TestAPreapprovedSkillStepSkipsTheScreenUntilPHIIsRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		step    bool
		tainted bool
		asks    int
	}{{"model's own command", false, false, 1}, {"skill step", true, false, 0}, {"skill step after PHI", true, true, 1}} {
		if tc.tainted {
			taint.Set()
		}
		a, _ := testAgent(t, shellTurn("bash", "echo step-ran"), mockapi.Turn{Text: "done"})
		f := &fakeScreen{verdict: stopped}
		a.Screen = f
		a.Allows = allows(guardian.Read)
		a.PreapprovedStep = func(id string) bool { return tc.step && id == "t1" }
		r := &recorder{}
		cb := r.callbacks()
		asks := 0
		cb.Require = func(context.Context, string, string, string) bool { asks++; return true }
		a.RunTurn(context.Background(), user("go"), cb)
		if asks != tc.asks || len(r.approvals) != 0 || !strings.Contains(strings.Join(r.results, "\n"), "step-ran") {
			t.Errorf("%s: asked %d times, approvals %v, results %v; want %d asks and the command run", tc.name, asks, r.approvals, r.results, tc.asks)
		}
		taint.ResetForTest()
	}
}

func TestAPassingCommandRunsAndIsRecorded(t *testing.T) {
	a, _ := testAgent(t, shellTurn("bash", "echo passed-ran"), mockapi.Turn{Text: "done"})
	f := &fakeScreen{verdict: passing}
	a.Screen = f
	r := &recorder{}
	lastResult(t, a, r)
	if !strings.Contains(strings.Join(r.results, "\n"), "passed-ran") {
		t.Errorf("a passing command did not run: %v", r.results)
	}
	if len(f.records) != 1 || f.records[0] != "passed echo passed-ran" {
		t.Errorf("recorded %v", f.records)
	}
}
