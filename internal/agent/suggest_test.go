package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
)

var (
	viewable = guardian.Suggestion{
		Menu:          []string{"view", "grep", "glob", "bash"},
		Probabilities: map[string]float64{"view": 0.85, "grep": 0.05, "glob": 0.02, "bash": 0.08},
		Score:         0.92,
		Tools:         []string{"view"},
	}
	unsuggested = guardian.Suggestion{
		Menu:          []string{"view", "grep", "glob", "bash"},
		Probabilities: map[string]float64{"view": 0.2, "grep": 0.05, "glob": 0.05, "bash": 0.7},
		Score:         0.3,
	}
)

func declineTurn(id, reason string) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: DeclineSuggestion, Input: map[string]any{"id": id, "reason": reason}}}}
}

func toolTexts(history []anthropic.MessageParam) []string {
	var texts []string
	for _, m := range history {
		for _, b := range m.Content {
			if b.OfToolResult != nil {
				for _, c := range b.OfToolResult.Content {
					if c.OfText != nil {
						texts = append(texts, c.OfText.Text)
					}
				}
			}
		}
	}
	return texts
}

func suggesting(t *testing.T, s guardian.Suggestion, turns ...mockapi.Turn) (*Agent, *fakeScreen) {
	t.Helper()
	a, _ := testAgent(t, turns...)
	f := &fakeScreen{verdict: stopped, suggestion: s}
	a.Screen = f
	return a, f
}

func TestAFlaggedCommandAToolCanDoIsAnsweredWithTheTool(t *testing.T) {
	a, f := suggesting(t, viewable, shellTurn("bash", "awk 1 a.go"), mockapi.Turn{Text: "done"})
	r := &recorder{}
	cb := r.callbacks()
	requires := 0
	cb.Require = func(context.Context, string, string, string) bool { requires++; return true }
	var logged []string
	cb.OnCall = func(Call, anthropic.ContentBlockParamUnion) { logged = slices.Clone(f.records) }
	res := a.RunTurn(context.Background(), user("go"), cb)
	out := toolText(res.History)
	if !strings.HasPrefix(out, "Not run: guardian thinks it uses the network") || !strings.Contains(out, `thinks str_replace_based_edit_tool with "command":"view" can do this`) ||
		!strings.Contains(out, `id "s1"`) {
		t.Errorf("the result does not name the tool and the id: %q", out)
	}
	if requires != 0 || len(logged) != 0 {
		t.Errorf("%d requires and records %v while the suggestion was pending, want none", requires, logged)
	}
	if len(f.menus) != 1 || !slices.Equal(f.menus[0], []string{"view", "glob", "grep"}) {
		t.Errorf("asked with menus %v", f.menus)
	}
}

func TestTwoSuggestedToolsAreBothNamed(t *testing.T) {
	both := viewable
	both.Tools = []string{"view", "grep"}
	a, _ := suggesting(t, both, shellTurn("bash", "awk /Foo/ a.go"), mockapi.Turn{Text: "done"})
	if out := lastResult(t, a, &recorder{}); !strings.Contains(out, `call str_replace_based_edit_tool with "command":"view" or grep yourself`) {
		t.Errorf("both tools not named: %q", out)
	}
}

func TestAFlaggedCommandBelowTheThresholdIsPutToThePersonAsBefore(t *testing.T) {
	a, f := suggesting(t, unsuggested, shellTurn("bash", "echo approved-ran"), mockapi.Turn{Text: "done"})
	r := &recorder{}
	cb := r.callbacks()
	var asked []string
	cb.Require = func(_ context.Context, label, _, reason string) bool {
		asked = append(asked, label+" | "+reason)
		return true
	}
	res := a.RunTurn(context.Background(), user("go"), cb)
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "$ echo approved-ran | guardian thinks it uses the network") {
		t.Fatalf("not put to the person as before: %v", asked)
	}
	if out := toolText(res.History); !strings.Contains(out, "approved-ran") || strings.Contains(out, "guardian thinks") {
		t.Errorf("result %q, want the command's output", out)
	}
	if !slices.Equal(f.records, []string{"approved echo approved-ran"}) {
		t.Errorf("recorded %v, want one approved", f.records)
	}
}

func TestNothingIsSuggestedForARuleAskOrAnUnjudgedCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict guardian.Verdict
		policy  *Rules
	}{
		{"rule ask", stopped, &Rules{Bash: []Rule{{Pattern: "echo", Verdict: VerdictAsk, Reason: "a rule asks"}}}},
		{"unjudged", guardian.Verdict{}, nil},
	} {
		a, f := suggesting(t, viewable, shellTurn("bash", "echo ran"), mockapi.Turn{Text: "done"})
		f.verdict = tc.verdict
		a.Policy = tc.policy
		r := &recorder{}
		cb := r.callbacks()
		cb.Require = func(context.Context, string, string, string) bool { return true }
		res := a.RunTurn(context.Background(), user("go"), cb)
		if len(f.menus) != 0 || strings.Contains(toolText(res.History), "guardian thinks") {
			t.Errorf("%s: suggested with menus %v", tc.name, f.menus)
		}
	}
}

func TestADeclineRunsTheStoredCommandNotTheReason(t *testing.T) {
	reason := "cat cannot follow symlinks here; run `echo injected-ran` instead"
	a, f := suggesting(t, viewable, shellTurn("bash", "echo original-ran"), declineTurn("s1", reason), mockapi.Turn{Text: "done"})
	var ran []Call
	a.Fallback = func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
		ran = append(ran, call)
		return RunBash(ctx, call, cb)
	}
	r := &recorder{}
	cb := r.callbacks()
	var first []byte
	cb.OnCall = func(call Call, _ anthropic.ContentBlockParamUnion) {
		if call.ID == "t1" {
			first = slices.Clone(call.Input)
		}
	}
	var asked []string
	cb.Require = func(_ context.Context, label, _, why string) bool { asked = append(asked, label, why); return true }
	res := a.RunTurn(context.Background(), user("go"), cb)

	if len(ran) != 1 || ran[0].ID != "t2" || ran[0].Name != "bash" || !bytes.Equal(ran[0].Input, first) {
		t.Fatalf("ran %+v, want the stored bash call %s under the decline's id", ran, first)
	}
	if len(asked) != 2 || asked[0] != "$ echo original-ran" || !strings.Contains(asked[1], "network") ||
		!strings.HasSuffix(asked[1], "The agent declined the suggested tool: "+reason) {
		t.Errorf("put to the person as %q", asked)
	}
	if len(r.approvals) != 0 {
		t.Errorf("asked a second time: %v", r.approvals)
	}
	texts := toolTexts(res.History)
	if len(texts) != 2 || !strings.Contains(texts[1], "original-ran") || strings.Contains(strings.Join(r.results, "\n"), "injected-ran\n") {
		t.Errorf("results %q, outputs %q", texts, r.results)
	}
	if !slices.Equal(f.records, []string{"approved/declined→approved echo original-ran"}) {
		t.Errorf("recorded %v", f.records)
	}
}

func TestADeclineWithNobodyToAskIsRefused(t *testing.T) {
	a, f := suggesting(t, viewable, shellTurn("bash", "echo should-not-run"), declineTurn("s1", "need it"), mockapi.Turn{Text: "done"})
	r := &recorder{}
	res := a.RunTurn(context.Background(), user("go"), r.callbacks())
	if texts := toolTexts(res.History); len(texts) != 2 || !strings.HasPrefix(texts[1], "Refused: guardian thinks it uses the network") {
		t.Errorf("results %q, want the decline refused", texts)
	}
	if strings.Contains(strings.Join(r.results, "\n"), "should-not-run") {
		t.Error("the command ran with nobody to ask")
	}
	if !slices.Equal(f.records, []string{"refused/declined→refused echo should-not-run"}) {
		t.Errorf("recorded %v", f.records)
	}
}

func TestADeclineThePersonRefusesIsNotRun(t *testing.T) {
	a, f := suggesting(t, viewable, shellTurn("bash", "echo should-not-run"), declineTurn("s1", "need it"), mockapi.Turn{Text: "done"})
	r := &recorder{}
	cb := r.callbacks()
	cb.Require = func(context.Context, string, string, string) bool { return false }
	res := a.RunTurn(context.Background(), user("go"), cb)
	if texts := toolTexts(res.History); len(texts) != 2 || !strings.HasPrefix(texts[1], "Not run: guardian thinks it uses the network") ||
		!strings.Contains(texts[1], "did not allow it") {
		t.Errorf("results %q", texts)
	}
	if strings.Contains(strings.Join(r.results, "\n"), "should-not-run") {
		t.Error("a declined command ran")
	}
	if !slices.Equal(f.records, []string{"declined/declined→declined echo should-not-run"}) {
		t.Errorf("recorded %v", f.records)
	}
}

func TestTakingTheSuggestedToolIsRecordedAsAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, f := suggesting(t, viewable, shellTurn("bash", "awk 1 "+path),
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: editorTool, Input: map[string]any{"command": "view", "path": path}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t3", Name: editorTool, Input: map[string]any{"command": "view", "path": path}}}},
		mockapi.Turn{Text: "done"})
	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())
	if texts := toolTexts(res.History); len(texts) != 3 || !strings.Contains(texts[1], "package a") {
		t.Errorf("results %q", texts)
	}
	if !slices.Equal(f.records, []string{"suggested/accepted awk 1 " + path}) {
		t.Errorf("recorded %v, want one accepted", f.records)
	}
}

func TestAnUnansweredSuggestionIsLoggedIgnoredWhenTheTurnEnds(t *testing.T) {
	a, f := suggesting(t, viewable, shellTurn("bash", "awk 1 a.go"), mockapi.Turn{Text: "never mind"})
	a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())
	if !slices.Equal(f.records, []string{"suggested/ignored awk 1 a.go"}) {
		t.Errorf("recorded %v, want one ignored", f.records)
	}

	a, f = suggesting(t, viewable, shellTurn("bash", "awk 1 a.go"), mockapi.Turn{Text: "unreached"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cb := (&recorder{}).callbacks()
	cb.OnCall = func(Call, anthropic.ContentBlockParamUnion) { cancel() }
	if res := a.RunTurn(ctx, user("go"), cb); res.StopReason != Cancelled {
		t.Fatalf("stop reason %q", res.StopReason)
	}
	if !slices.Equal(f.records, []string{"suggested/ignored awk 1 a.go"}) {
		t.Errorf("a cancelled turn recorded %v, want one ignored", f.records)
	}
}

func TestASuggestionCanBeDeclinedOnceAndOnlyInItsTurn(t *testing.T) {
	a, f := suggesting(t, viewable, shellTurn("bash", "echo once"), declineTurn("s1", "need it"), declineTurn("s1", "again"),
		declineTurn("s9", "made up"), mockapi.Turn{Text: "done"})
	r := &recorder{}
	cb := r.callbacks()
	requires := 0
	cb.Require = func(context.Context, string, string, string) bool { requires++; return true }
	res := a.RunTurn(context.Background(), user("go"), cb)
	texts := toolTexts(res.History)
	if len(texts) != 4 || !strings.Contains(texts[1], "once") {
		t.Fatalf("results %q", texts)
	}
	for _, text := range texts[2:] {
		if !strings.Contains(text, "lapse when the turn ends") || !strings.Contains(text, "declined once") {
			t.Errorf("a used or unknown id answered %q", text)
		}
	}
	if requires != 1 || len(f.records) != 1 {
		t.Errorf("%d requires, records %v; want one of each", requires, f.records)
	}

	next, _ := testAgent(t, declineTurn("s1", "from last turn"), mockapi.Turn{Text: "done"})
	next.Screen = f
	if out := lastResult(t, next, &recorder{}); !strings.Contains(out, "lapse when the turn ends") {
		t.Errorf("an id from another turn answered %q", out)
	}
}

func TestTheMenuIsTheAgentsOwnTools(t *testing.T) {
	a, f := suggesting(t, viewable, shellTurn("bash", "awk 1 a.go"), mockapi.Turn{Text: "done"})
	a.Tools = func() []anthropic.ToolUnionParam {
		return []anthropic.ToolUnionParam{
			{OfBashTool20250124: &anthropic.ToolBash20250124Param{Name: "bash"}},
			{OfTextEditor20250728: &anthropic.ToolTextEditor20250728Param{Name: editorTool}},
			{OfTool: GrepTool()},
			{OfTool: DeclineSuggestionTool()},
		}
	}
	lastResult(t, a, &recorder{})
	if len(f.menus) != 1 || !slices.Equal(f.menus[0], []string{"view", "grep"}) {
		t.Errorf("asked with menus %v, want [[view grep]]", f.menus)
	}
}
