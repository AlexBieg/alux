package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/anthropics/anthropic-sdk-go"
)

var sentinel = sandbox.Wrap{Text: "sentinel"}

// ran records, per call id, whether its handler ran and under which sandbox.
type ran map[string]string

func (r ran) handler(ctx context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
	if w, ok := sandbox.From(ctx); ok {
		r[call.ID] = w.Text
	} else {
		r[call.ID] = "unwrapped"
	}
	return ok(call.ID, "ran")
}

func sandboxedAgent(t *testing.T, turns ...mockapi.Turn) (*Agent, *fakeScreen, ran) {
	t.Helper()
	a, _ := testAgent(t, turns...)
	f := &fakeScreen{verdict: passing}
	a.Screen = f
	a.Sandbox = func(context.Context, [][]string) (sandbox.Wrap, error) { return sentinel, nil }
	r := ran{}
	a.Handlers = map[string]Handler{"bash": r.handler, "run_background": r.handler}
	a.Fallback = r.handler
	return a, f, r
}

func escalatedTurn(name, command string) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: name, Input: map[string]any{"command": command, "sandbox": false}}}}
}

func TestEverySandboxedRouteToAShellSkipsTheScreen(t *testing.T) {
	for _, tc := range []struct {
		name string
		turn mockapi.Turn
	}{
		{"bash", shellTurn("bash", "echo hi")},
		{"run_background", shellTurn("run_background", "echo hi")},
		{"made_up_tool", shellTurn("made_up_tool", "echo hi")},
		{"made_up_tool asking out", escalatedTurn("made_up_tool", "echo hi")},
	} {
		a, f, r := sandboxedAgent(t, tc.turn, mockapi.Turn{Text: "done"})
		requires := 0
		cb := (&recorder{}).callbacks()
		cb.Require = func(context.Context, string, string, string) bool { requires++; return true }
		a.RunTurn(context.Background(), user("go"), cb)
		if r["t1"] != "sentinel" || len(f.asked) != 0 || len(f.records) != 0 || requires != 0 {
			t.Errorf("%s: ran under %q, screened %v, recorded %v, %d requires; want sandboxed, unscreened, unasked",
				tc.name, r["t1"], f.asked, f.records, requires)
		}
	}
}

func TestAnEscalatedCommandGuardianPassesRunsOutsideTheSandboxUnasked(t *testing.T) {
	for _, name := range []string{"bash", "run_background"} {
		a, f, r := sandboxedAgent(t, escalatedTurn(name, "git push"), mockapi.Turn{Text: "done"})
		rec := &recorder{}
		cb := rec.callbacks()
		requires := 0
		cb.Require = func(context.Context, string, string, string) bool { requires++; return true }
		a.RunTurn(context.Background(), user("go"), cb)
		if len(f.asked) != 1 || requires != 0 || r["t1"] != "unwrapped" || len(rec.approvals) != 0 {
			t.Errorf("%s: screened %v, %d prompts, ran under %q, approvals %v; want one screen, unwrapped, unasked",
				name, f.asked, requires, r["t1"], rec.approvals)
		}
		if len(f.records) != 1 || !strings.HasPrefix(f.records[0], "passed ") {
			t.Errorf("%s: recorded %v, want one passed", name, f.records)
		}
	}
}

func TestAnEscalatedCommandGuardianStopsIsPutToThePerson(t *testing.T) {
	a, f, r := sandboxedAgent(t, escalatedTurn("bash", "git push"), mockapi.Turn{Text: "done"})
	f.verdict = stopped
	cb := (&recorder{}).callbacks()
	var reasons []string
	cb.Require = func(_ context.Context, _, _, reason string) bool { reasons = append(reasons, reason); return true }
	a.RunTurn(context.Background(), user("go"), cb)
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "runs outside the sandbox · guardian thinks it") || r["t1"] != "unwrapped" {
		t.Errorf("put to the person as %q, ran under %q; want one prompt with guardian's verdict, then unwrapped", reasons, r["t1"])
	}
}

func TestAnEscalatedCommandIsNotRunWhenThePersonDeclines(t *testing.T) {
	a, f, r := sandboxedAgent(t, escalatedTurn("bash", "git push"), mockapi.Turn{Text: "done"})
	f.verdict = stopped
	cb := (&recorder{}).callbacks()
	cb.Require = func(context.Context, string, string, string) bool { return false }
	res := a.RunTurn(context.Background(), user("go"), cb)
	if _, did := r["t1"]; did || !strings.HasPrefix(toolText(res.History), "Not run") {
		t.Errorf("a declined escalation ran or was not answered as not run: %v %q", r, toolText(res.History))
	}
}

func TestAnEscalatedCommandWithNobodyToAskIsRefused(t *testing.T) {
	a, f, r := sandboxedAgent(t, escalatedTurn("bash", "git push"), mockapi.Turn{Text: "done"})
	f.verdict = stopped
	out := lastResult(t, a, &recorder{})
	if _, did := r["t1"]; did || !strings.HasPrefix(out, "Refused") || !strings.Contains(out, "nobody to ask") {
		t.Errorf("an escalation with nobody to ask was not refused: ran %v, %q", r, out)
	}
}

func TestASandboxThatCannotBeBuiltRefusesTheCommand(t *testing.T) {
	a, _, r := sandboxedAgent(t, shellTurn("bash", "echo hi"), mockapi.Turn{Text: "done"})
	a.Sandbox = func(context.Context, [][]string) (sandbox.Wrap, error) {
		return sandbox.Wrap{}, errors.New("no sandbox-exec")
	}
	out := lastResult(t, a, &recorder{})
	if _, did := r["t1"]; did || !strings.HasPrefix(out, "Refused") || !strings.Contains(out, "no sandbox-exec") {
		t.Errorf("a command ran without its sandbox: ran %v, %q", r, out)
	}
}

func TestTheRulesAndThePullRequestRefusalStillApplyInTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		policy  *Rules
	}{
		{"rule deny", "echo hi", &Rules{Default: VerdictDeny, DefaultReason: "rules say no"}},
		{"pull request", "gh pr comment 1 --body hi", nil},
	} {
		a, f, r := sandboxedAgent(t, shellTurn("bash", tc.command), mockapi.Turn{Text: "done"})
		a.Policy = tc.policy
		out := lastResult(t, a, &recorder{})
		if _, did := r["t1"]; did || !strings.HasPrefix(out, "Refused") || len(f.asked) != 0 {
			t.Errorf("%s: ran %v, screened %v, answered %q; want refused", tc.name, r, f.asked, out)
		}
	}
}

func TestADeclinedSuggestionRunsOutsideTheSandboxItAskedToLeave(t *testing.T) {
	a, f, r := sandboxedAgent(t, escalatedTurn("bash", "cat a.go"), declineTurn("s1", "need it"), mockapi.Turn{Text: "done"})
	f.verdict, f.suggestion = stopped, viewable
	cb := (&recorder{}).callbacks()
	requires := 0
	cb.Require = func(context.Context, string, string, string) bool { requires++; return true }
	a.RunTurn(context.Background(), user("go"), cb)
	if r["t2"] != "unwrapped" || requires != 1 {
		t.Errorf("the declined suggestion ran under %q after %d prompts, want unwrapped after one", r["t2"], requires)
	}
}

func TestOnlyACommandThatRanOutsideTheSandboxIsMarked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		turns     []mockapi.Turn
		setup     func(*Agent, *fakeScreen)
		allow     bool
		wantMarks []string
	}{
		{"sandboxed", []mockapi.Turn{shellTurn("bash", "echo hi")}, nil, true, nil},
		{"escalated, guardian passes", []mockapi.Turn{escalatedTurn("run_background", "git push")}, nil, true, []string{"t1"}},
		{"escalated, person declines", []mockapi.Turn{escalatedTurn("bash", "git push")},
			func(_ *Agent, f *fakeScreen) { f.verdict = stopped }, false, nil},
		{"declined suggestion", []mockapi.Turn{escalatedTurn("bash", "cat a.go"), declineTurn("s1", "need it")},
			func(_ *Agent, f *fakeScreen) { f.verdict, f.suggestion = stopped, viewable }, true, []string{"t2"}},
		{"no sandbox at all", []mockapi.Turn{shellTurn("bash", "echo hi")},
			func(a *Agent, _ *fakeScreen) { a.Sandbox = nil }, true, nil},
	} {
		a, f, _ := sandboxedAgent(t, append(tc.turns, mockapi.Turn{Text: "done"})...)
		if tc.setup != nil {
			tc.setup(a, f)
		}
		cb := (&recorder{}).callbacks()
		cb.Require = func(context.Context, string, string, string) bool { return tc.allow }
		var marks []string
		cb.OnUnsandboxed = func(id string) { marks = append(marks, id) }
		a.RunTurn(context.Background(), user("go"), cb)
		if strings.Join(marks, ",") != strings.Join(tc.wantMarks, ",") {
			t.Errorf("%s: marked %v, want %v", tc.name, marks, tc.wantMarks)
		}
	}
}
