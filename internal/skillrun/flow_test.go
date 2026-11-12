package skillrun

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/anthropics/anthropic-sdk-go"
)

type screenPass struct{}

func (screenPass) Screen(context.Context, string) guardian.Verdict {
	return guardian.Verdict{Agent: "test", Top: guardian.Read, Threshold: 0.5}
}
func (screenPass) Record(string, guardian.Verdict, string) {}
func (screenPass) Suggest(context.Context, string, []string) guardian.Suggestion {
	return guardian.Suggestion{}
}
func (screenPass) RecordSuggested(string, guardian.Verdict, guardian.Suggestion, string, string) {}

type flowRig struct {
	h       *Host
	a       *agent.Agent
	bash    []string
	passing func(command string) bool
	closed  string
	result  skills.Result
	n       int
}

// coderFlow is a host holding the built-in coder-task and coder-work, with a bash that records each command and passes per passing.
func coderFlow(t *testing.T, extra map[string]string) *flowRig {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"coder-task", "coder-work"} {
		b, err := skills.Builtin(name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(b)
	}
	for k, v := range extra {
		files[k] = v
	}
	h, a := host(t, files, "bash", "str_replace_based_edit_tool")
	rig := &flowRig{h: h, a: a, passing: func(string) bool { return true }}
	a.Screen = screenPass{}
	a.Handlers["bash"] = func(ctx context.Context, c agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(c.Input, &in)
		rig.bash = append(rig.bash, in.Command)
		if rig.passing(in.Command) {
			return ran("exit 0", "ok\n[exit 0]")(ctx, c, cb)
		}
		return ran("exit 1", "FAIL\n[exit 1]")(ctx, c, cb)
	}
	a.Handlers["str_replace_based_edit_tool"] = answer("edited")
	h.Closed = func(_ *skills.Skill, res skills.Result, how string) { rig.closed, rig.result = how, res }
	return rig
}

func (r *flowRig) call(t *testing.T, name, input string) (string, bool) {
	t.Helper()
	r.n++
	res := r.a.Run(t.Context(), agent.Call{ID: name + string(rune('a'+r.n)), Name: name, Input: []byte(input)}, agent.Callbacks{OnCall: r.h.Observe})
	return resultText(res), res.OfToolResult.IsError.Or(false)
}

func (r *flowRig) edit(t *testing.T) {
	r.call(t, "str_replace_based_edit_tool", `{"command":"str_replace","path":"a.go"}`)
}

func (r *flowRig) finish(t *testing.T, out string) (string, bool) {
	return r.call(t, FinishStageTool, `{"out":`+out+`}`)
}

const changed = `{"outcome":"changed","why":"made it"}`

func TestFinishStageCountsOnlyEvidenceFromSinceTheStageOpened(t *testing.T) {
	rig := coderFlow(t, nil)
	verifyPasses := false
	rig.passing = func(c string) bool { return !strings.Contains(c, "go test") || verifyPasses }
	if _, err := rig.h.Start("coder-task", map[string]any{"task": "fix it", "verify": "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	if text, refused := rig.finish(t, changed); !refused || !strings.Contains(text, "work") {
		t.Fatalf("finish_stage with no edit at all = %q, refused %v; want refused naming work", text, refused)
	}
	rig.edit(t)
	if text, refused := rig.finish(t, changed); refused || !strings.Contains(text, "reopened work") {
		t.Fatalf("a failing verify did not reopen work: %q", text)
	}
	if text, refused := rig.finish(t, changed); !refused {
		t.Fatalf("the edit made before work reopened counted in its new window: %q", text)
	}
	verifyPasses = true
	rig.edit(t)
	if text, refused := rig.finish(t, changed); refused || rig.closed != Finished || !rig.result.Complete {
		t.Fatalf("with a fresh edit and a passing verify the flow did not complete: %q, closed %q", text, rig.closed)
	}
}

func TestVerifyRunsFromTheShellRootWhereverTheAgentLeftIt(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh, err := shell.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	rig := coderFlow(t, nil)
	rig.a.Handlers["bash"] = func(ctx context.Context, c agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(c.Input, &in)
		cmd := exec.Command("bash", "-c", in.Command)
		cmd.Dir = filepath.Join(root, "web")
		if err := cmd.Run(); err != nil {
			return ran("exit 1", "FAIL\n[exit 1 · in web]")(ctx, c, cb)
		}
		return ran("exit 0", "ok\n[exit 0 · in web]")(ctx, c, cb)
	}
	if _, err := rig.h.Start("coder-task", map[string]any{"task": "fix it", "verify": "test -d web"}); err != nil {
		t.Fatal(err)
	}
	rig.edit(t)
	ctx := shell.With(t.Context(), sh)
	res := rig.a.Run(ctx, agent.Call{ID: "finish", Name: FinishStageTool, Input: []byte(`{"out":{"outcome":"nothing_to_do","why":"in place"}}`)}, agent.Callbacks{OnCall: rig.h.Observe})
	if text := resultText(res); rig.closed != Finished {
		t.Fatalf("a verify that passes from the root, started from web/, did not complete the flow: %q", text)
	}
}

func TestFinishStageIsRefusedWhenBatchedWithAnotherCall(t *testing.T) {
	rig := coderFlow(t, nil)
	if _, err := rig.h.Start("coder-task", map[string]any{"task": "fix it", "verify": "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	server, turn := talking(t, rig.h, rig.a,
		mockapi.Turn{Tools: []mockapi.ToolUse{
			{ID: "e1", Name: "str_replace_based_edit_tool", Input: map[string]any{"command": "view", "path": "a.go"}},
			{ID: "f1", Name: FinishStageTool, Input: map[string]any{"out": map[string]any{"outcome": "changed", "why": "x"}}},
		}},
		mockapi.Turn{Text: "stopped"},
	)
	turn(t.Context())
	sent, _ := json.Marshal(server.LastRequest()["messages"])
	if !strings.Contains(string(sent), "must be the only tool call in its message") || len(rig.bash) != 0 {
		t.Fatalf("a batched finish_stage was not refused, or advanced the flow (bash %v):\n%s", rig.bash, sent)
	}
}

const planner = `schema: 2
skill: planner
version: 1
when: testing a command out field
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - plan: { checklist: plan-list, out: { verify: { type: string, format: command } } }
  - check: { run: { tool: bash, input: { command: "echo ok" } } }
`

const planList = `schema: 2
skill: plan-list
version: 1
when: testing a command out field
requires:
  tools: [str_replace_based_edit_tool]
nodes:
  plan:
    say: Plan.
    evidence:
      - call: { tool: str_replace_based_edit_tool }
`

func TestFinishStageRefusesACommandFieldAluxWouldNotRun(t *testing.T) {
	rig := coderFlow(t, map[string]string{"planner": planner, "plan-list": planList})
	if _, err := rig.h.Start("planner", nil); err != nil {
		t.Fatal(err)
	}
	rig.edit(t)
	why := agent.CommandRefusal("$(mktemp) x")
	if why == "" {
		t.Fatal("alux's shell rules should refuse $(mktemp) x")
	}
	text, refused := rig.finish(t, `{"verify":"$(mktemp) x"}`)
	if !refused || !strings.Contains(text, why) || len(rig.bash) != 0 {
		t.Fatalf("finish_stage = %q (refused %v, bash %v), want refused quoting %q", text, refused, rig.bash, why)
	}
	if text, refused := rig.finish(t, `{"verify":"go test ./..."}`); refused || rig.closed != Finished {
		t.Fatalf("a command alux runs was refused: %q", text)
	}
}

func TestCoderTaskWithNothingToDoCompletesWithoutTheDescribedStage(t *testing.T) {
	rig := coderFlow(t, nil)
	if _, err := rig.h.Start("coder-task", map[string]any{"task": "fix it", "verify": "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	rig.edit(t)
	text, refused := rig.finish(t, `{"outcome":"nothing_to_do","why":"already there"}`)
	if refused || len(rig.bash) != 1 || rig.bash[0] != "go test ./..." || rig.closed != Finished || !rig.result.Complete {
		t.Fatalf("finish_stage = %q, bash %v, closed %q; want only verify run and the flow complete", text, rig.bash, rig.closed)
	}
	if got := rig.result.Nodes[2]; got.Name != "described" || got.State != skills.Skipped {
		t.Errorf("described = %+v, want skipped", got)
	}
}

func TestAFailingRunStageReopensItsOnFailStageUntilItsMaxThenStops(t *testing.T) {
	rig := coderFlow(t, nil)
	rig.passing = func(c string) bool { return !strings.Contains(c, "go test") }
	if _, err := rig.h.Start("coder-task", map[string]any{"task": "fix it", "verify": "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		rig.edit(t)
		text, _ := rig.finish(t, changed)
		reopened := strings.Contains(text, "reopened work")
		if i < 3 && (!reopened || rig.closed != "") {
			t.Fatalf("failure %d did not reopen work: %q", i, text)
		}
		if i == 3 && (reopened || rig.closed != "" || !strings.Contains(text, "The flow stopped at stage **verify**")) {
			t.Fatalf("the third failure, at max 3, = %q, closed %q; want the flow stopped at verify", text, rig.closed)
		}
	}
	if n := strings.Count(strings.Join(rig.bash, "\n"), "go test"); n != 3 {
		t.Errorf("verify ran %d times, want 3", n)
	}
}

func failingCheck(strict string) string {
	return `schema: 2
skill: failing
version: 1
when: testing a run stage that fails
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - check: { run: { tool: bash, input: { command: "make verify" } }` + strict + ` }
  - plan: { checklist: plan-list }
`
}

func TestAFailedRunStageWithoutOnFailStopsAndMovesOnOnceWaived(t *testing.T) {
	rig := coderFlow(t, map[string]string{"failing": failingCheck(""), "plan-list": planList})
	rig.passing = func(c string) bool { return c != "make verify" }
	text, _ := rig.call(t, StartTool, `{"skill":"failing"}`)
	if rig.closed != "" || !strings.Contains(text, "The flow stopped at stage **check**") {
		t.Fatalf("start_skill = %q, closed %q; want the flow stopped at check", text, rig.closed)
	}
	text, refused := rig.call(t, FinishStageTool, `{"waive":[{"step":"check","reason":"make verify is broken on main"}]}`)
	if refused || !strings.Contains(text, "Stage **plan**") {
		t.Fatalf("waiving check = %q (refused %v); want plan open", text, refused)
	}
	rig.edit(t)
	if text, refused := rig.finish(t, "null"); refused || rig.closed != Finished {
		t.Fatalf("finishing plan = %q, closed %q; want the flow finished", text, rig.closed)
	}
	if got := rig.result.Nodes[0]; got.Name != "check" || got.State != skills.Waived {
		t.Errorf("check = %+v, want waived", got)
	}
	if s := skills.Summary("failing", rig.closed, rig.result); !strings.Contains(s, "finished with 1 waiver") ||
		!strings.Contains(s, "check: make verify is broken on main") {
		t.Errorf("summary does not report the waiver:\n%s", s)
	}
}

func TestAStrictRunStageStillFailsTheFlow(t *testing.T) {
	rig := coderFlow(t, map[string]string{"failing": failingCheck(", strict: true"), "plan-list": planList})
	rig.passing = func(c string) bool { return c != "make verify" }
	text, _ := rig.call(t, StartTool, `{"skill":"failing"}`)
	if !strings.HasPrefix(rig.closed, "failed") || !strings.Contains(rig.closed, "check failed") {
		t.Fatalf("start_skill = %q, closed %q; want the flow failed at check", text, rig.closed)
	}
}

const capped = `schema: 2
skill: capped
version: 1
when: testing what the brief says of a max
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - plan: { checklist: plan-list }
  - check: { run: { tool: bash, input: { command: "make verify" } }, on_fail: plan, max: 2 }
  - gate: { run: { tool: bash, input: { command: "make gate" } }, on_fail: plan, max: 2, strict: true }
`

func TestTheBriefSaysOnlyAStrictStageFailsTheFlowAtItsMax(t *testing.T) {
	rig := coderFlow(t, map[string]string{"capped": capped, "plan-list": planList})
	brief, err := rig.h.Start("capped", nil)
	if err != nil {
		t.Fatal(err)
	}
	line := func(stage string) string {
		for l := range strings.Lines(brief) {
			if strings.Contains(l, "**"+stage+"**:") {
				return l
			}
		}
		t.Fatalf("no line for %s in the brief:\n%s", stage, brief)
		return ""
	}
	if l := line("check"); strings.Contains(l, "flow fails") || !strings.Contains(l, "after 2 failures the flow stops at it, to be waived with a reason or the run failed") {
		t.Errorf("a non-strict stage at its max stops the flow for a waiver, yet the brief says:\n%s", l)
	}
	if l := line("gate"); !strings.Contains(l, "the flow fails once it has failed 2 times") {
		t.Errorf("a strict stage at its max fails the flow, yet the brief says:\n%s", l)
	}
}

func TestAStoppedStageRunsAgainOnABareFinishStage(t *testing.T) {
	rig := coderFlow(t, map[string]string{"failing": failingCheck(""), "plan-list": planList})
	fixed := false
	rig.passing = func(c string) bool { return c != "make verify" || fixed }
	rig.call(t, StartTool, `{"skill":"failing"}`)
	fixed = true
	text, refused := rig.call(t, FinishStageTool, `{}`)
	if refused || strings.Count(strings.Join(rig.bash, "\n"), "make verify") != 2 || !strings.Contains(text, "Stage **plan**") {
		t.Fatalf("finish_stage at the stopped check = %q (refused %v, bash %v); want check run twice and plan open", text, refused, rig.bash)
	}
}

const drafting = `schema: 2
skill: drafting
version: 1
when: testing a waived step of a checklist stage
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - draft: { checklist: plan-list }
  - check: { run: { tool: bash, input: { command: "echo ok" } } }
`

func TestAWaivedStepOfAChecklistStageIsReportedUnderItsStage(t *testing.T) {
	rig := coderFlow(t, map[string]string{"drafting": drafting, "plan-list": planList})
	rig.call(t, StartTool, `{"skill":"drafting"}`)
	if text, refused := rig.call(t, FinishStageTool, `{"waive":[{"step":"plan","reason":"planned in the ticket"}]}`); refused || rig.closed != Finished {
		t.Fatalf("waiving plan = %q (refused %v, closed %q); want the flow finished", text, refused, rig.closed)
	}
	if want := []skills.Waiver{{Step: "draft.plan", Reason: "planned in the ticket"}}; !slices.Equal(rig.result.Waivers, want) {
		t.Fatalf("waivers = %+v, want %+v", rig.result.Waivers, want)
	}
}

func TestAWaiverOfAStepThatHasEvidenceIsNotReported(t *testing.T) {
	rig := coderFlow(t, map[string]string{"drafting": drafting, "plan-list": planList})
	rig.call(t, StartTool, `{"skill":"drafting"}`)
	rig.edit(t)
	if text, refused := rig.call(t, FinishStageTool, `{"waive":[{"step":"draft.plan","reason":"planned in the ticket"}]}`); refused || rig.closed != Finished {
		t.Fatalf("waiving an evidenced plan = %q (refused %v, closed %q); want the flow finished", text, refused, rig.closed)
	}
	if len(rig.result.Waivers) != 0 {
		t.Fatalf("waivers = %+v, want none for a step that had its evidence", rig.result.Waivers)
	}
}

const eachWaived = `schema: 2
skill: each-waived
version: 1
when: testing waivers across foreach items and an on_fail reopen
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - plan:
      checklist: plan-list
      out: { changes: { type: array, items: { type: object, properties: { id: string } } } }
  - each:
      foreach: { in: plan.changes, as: change }
      stages:
        - look: { checklist: plan-list }
        - check: { run: { tool: bash, input: { command: "echo ${change.id}" } }, on_fail: plan }
`

func TestAWaiverIsNamedByItsItemAndDroppedWhenOnFailRunsItsStageAgain(t *testing.T) {
	rig := coderFlow(t, map[string]string{"each-waived": eachWaived, "plan-list": planList})
	failedB := false
	rig.passing = func(c string) bool {
		if c == "echo 'b'" && !failedB {
			failedB = true
			return false
		}
		return true
	}
	rig.call(t, StartTool, `{"skill":"each-waived"}`)
	rig.edit(t)
	rig.finish(t, `{"changes":[{"id":"a"},{"id":"b"}]}`)
	waive := `{"waive":[{"step":"plan","reason":"looked already"}]}`
	rig.call(t, FinishStageTool, waive)
	if text, _ := rig.call(t, FinishStageTool, waive); !strings.Contains(text, "reopened plan") {
		t.Fatalf("b's failing check = %q, want plan reopened", text)
	}
	rig.edit(t)
	rig.finish(t, `{"changes":[{"id":"a"},{"id":"b"}]}`)
	rig.edit(t)
	if text, refused := rig.finish(t, "null"); refused || rig.closed != Finished {
		t.Fatalf("b's second look = %q (refused %v, closed %q); want the flow finished", text, refused, rig.closed)
	}
	if want := []skills.Waiver{{Step: "each[a].look.plan", Reason: "looked already"}}; !slices.Equal(rig.result.Waivers, want) {
		t.Fatalf("waivers = %+v, want only item a's, since b's look ran again", rig.result.Waivers)
	}
}

// waivedOn is each node holding a waiver, with how many.
func waivedOn(res skills.Result) map[string]int {
	out := map[string]int{}
	for _, n := range res.Nodes {
		if n.Waived > 0 {
			out[n.Name] = n.Waived
		}
	}
	return out
}

func TestAWaiverCountsOnTheBoxOfTheStageItWasMadeIn(t *testing.T) {
	spawn := spawnFlow(t)
	spawn.finish(t, `{"id":"abc"}`)
	spawn.h.Land(spawn.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: true,
		Waivers: []skills.Waiver{{Step: "verify", Reason: "no tests"}, {Step: "work.edit", Reason: "docs only"}}})
	if _, res, _ := spawn.h.check(); !maps.Equal(waivedOn(res), map[string]int{"code": 2}) {
		t.Errorf("a spawn's waivers counted on %v, want both on code", waivedOn(res))
	}

	each := coderFlow(t, map[string]string{"each-waived": eachWaived, "plan-list": planList})
	each.call(t, StartTool, `{"skill":"each-waived"}`)
	each.edit(t)
	each.finish(t, `{"changes":[{"id":"a"},{"id":"b"}]}`)
	each.call(t, FinishStageTool, `{"waive":[{"step":"plan","reason":"looked already"}]}`)
	_, res, _ := each.h.check()
	if !maps.Equal(waivedOn(res), map[string]int{"each.look": 1}) {
		t.Errorf("item a's waiver counted on %v, want on each.look", waivedOn(res))
	}
	if n := res.Nodes[1]; n.Name != "each" || n.Item != 2 || n.Items != 2 {
		t.Errorf("the foreach = %+v, want on item 2 of 2", n)
	}
}

const countedCheck = `schema: 2
skill: counted-check
version: 1
when: testing a waived stage whose int capture a later stage reads
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - check: { run: { tool: bash, input: { command: "make verify" }, capture: { n: { pattern: "exit (\\d+)", type: int } } } }
  - report: { run: { tool: bash, input: { command: "echo ${check.n}" } } }
  - plan: { checklist: plan-list }
`

func TestWaivingAStageWithAWronglyTypedOutFieldIsRefused(t *testing.T) {
	rig := coderFlow(t, map[string]string{"counted-check": countedCheck, "plan-list": planList})
	rig.passing = func(c string) bool { return c != "make verify" }
	rig.call(t, StartTool, `{"skill":"counted-check"}`)
	waive := `"waive":[{"step":"check","reason":"verified by hand"}]`
	if text, refused := rig.call(t, FinishStageTool, `{`+waive+`,"out":{"n":"three"}}`); !refused || !strings.Contains(text, "out.n") ||
		!strings.Contains(text, "integer") {
		t.Fatalf("waiving check with a string n = %q (refused %v); want refused naming out.n as an integer", text, refused)
	}
	if text, refused := rig.call(t, FinishStageTool, `{`+waive+`,"out":{"n":3}}`); refused || !strings.Contains(text, "Stage **plan**") {
		t.Fatalf("waiving check with an integer n = %q (refused %v); want plan open", text, refused)
	}
}

const whenCounted = `schema: 2
skill: when-counted
version: 1
when: testing a when on an int capture
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - count: { run: { tool: bash, input: { command: "echo count" }, capture: { n: { pattern: "exit (\\d+)", type: int } } } }
  - zero: { run: { tool: bash, input: { command: "echo zero" } }, when: { field: count.n, equals: 0 } }
  - plan: { checklist: plan-list }
`

func TestAWhenOnAnIntCaptureMatchesItsEquals(t *testing.T) {
	rig := coderFlow(t, map[string]string{"when-counted": whenCounted, "plan-list": planList})
	rig.call(t, StartTool, `{"skill":"when-counted"}`)
	if !slices.Contains(rig.bash, "echo zero") {
		t.Fatalf("bash ran %q; want zero run, since count.n captured 0", rig.bash)
	}
}

const readsCheck = `schema: 2
skill: reads-check
version: 1
when: testing a waived stage whose text a later stage reads
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - check: { run: { tool: bash, input: { command: "make verify" } } }
  - report: { run: { tool: bash, input: { command: "echo ${check.text}" } } }
  - plan: { checklist: plan-list }
`

func TestWaivingAStageWhoseFieldsLaterStagesReadNeedsThemInOut(t *testing.T) {
	rig := coderFlow(t, map[string]string{"reads-check": readsCheck, "plan-list": planList})
	rig.passing = func(c string) bool { return c != "make verify" }
	rig.call(t, StartTool, `{"skill":"reads-check"}`)
	waive := `"waive":[{"step":"check","reason":"verified by hand"}]`
	if text, refused := rig.call(t, FinishStageTool, `{`+waive+`}`); !refused || !strings.Contains(text, "out with text") {
		t.Fatalf("waiving check without out = %q (refused %v); want refused naming text", text, refused)
	}
	if text, refused := rig.call(t, FinishStageTool, `{`+waive+`,"out":{"text":"hand-checked"}}`); refused || rig.bash[len(rig.bash)-1] != "echo 'hand-checked'" {
		t.Fatalf("waiving check with out = %q (refused %v, bash %v); want report run with the given text", text, refused, rig.bash)
	}
}

const leading = `schema: 2
skill: leading
version: 1
when: testing a flow that opens with a run stage
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - check: { run: { tool: bash, input: { command: "echo ok" } } }
  - plan: { checklist: plan-list }
`

func TestALeadingRunStageRunsAtTheStartOfTheNextTurn(t *testing.T) {
	rig := coderFlow(t, map[string]string{"leading": leading, "plan-list": planList})
	if _, err := rig.h.StartText("leading", ""); err != nil {
		t.Fatal(err)
	}
	if len(rig.bash) != 0 {
		t.Fatalf("starting ran the leading stage outside a turn: bash %v", rig.bash)
	}
	server, turn := talking(t, rig.h, rig.a, mockapi.Turn{Text: "ok"})
	turn(t.Context())
	var sent []string
	for _, c := range lastMessage(t, server, 0) {
		sent = append(sent, c.Text)
	}
	if len(rig.bash) != 1 || !strings.Contains(strings.Join(sent, "\n"), "Stage **plan**") {
		t.Fatalf("the turn's first request = %q (bash %v); want the run stage made and plan opened before it", sent, rig.bash)
	}
}

func TestStartSkillRunsALeadingRunStageInItsOwnCall(t *testing.T) {
	rig := coderFlow(t, map[string]string{"leading": leading, "plan-list": planList})
	text, failed := rig.call(t, StartTool, `{"skill":"leading"}`)
	if failed || len(rig.bash) != 1 || !strings.Contains(text, "Stage **plan**") {
		t.Fatalf("start_skill = %q (failed %v, bash %v); want the run stage made and plan opened", text, failed, rig.bash)
	}
}

const twoRuns = `schema: 2
skill: two-runs
version: 1
when: testing a flow that opens with two run stages
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - first: { run: { tool: bash, input: { command: "echo first" } } }
  - second: { run: { tool: bash, input: { command: "echo second" } } }
  - plan: { checklist: plan-list }
`

func TestThePaneShowsARunStageDoneBeforeTheNextOneRuns(t *testing.T) {
	rig := coderFlow(t, map[string]string{"two-runs": twoRuns, "plan-list": planList})
	var latest *View
	rig.h.Changed = func(v *View) { latest = v }
	var firstWhileSecond skills.State
	rig.passing = func(c string) bool {
		if c == "echo second" && latest != nil {
			firstWhileSecond = latest.Nodes[0].State
		}
		return true
	}
	if _, failed := rig.call(t, StartTool, `{"skill":"two-runs"}`); failed {
		t.Fatal("start_skill failed")
	}
	if firstWhileSecond != skills.Done {
		t.Fatalf("while the second run stage ran, the pane showed the first as %v, want done", firstWhileSecond)
	}
}

const capturing = `schema: 2
skill: capturing
version: 1
when: testing a capture passed between run stages
requires:
  tools: [bash, lookup, str_replace_based_edit_tool]
stages:
  - target:
      run:
        tool: bash
        input: { command: "echo acme/web#7" }
        capture:
          repo: { pattern: "(\\S+/[^\\s#]+)#" }
          number: { pattern: "#([0-9]+)", type: int }
  - detail: { run: { tool: lookup, input: { repo: "${target.repo}", number: "${target.number}", seen: "${target.text}" } } }
  - plan: { checklist: plan-list }
`

func TestARunStagesCapturesFillALaterStagesInput(t *testing.T) {
	h, a := host(t, map[string]string{"capturing": capturing, "plan-list": planList}, "bash", "lookup", "str_replace_based_edit_tool")
	a.Screen = screenPass{}
	a.Handlers["bash"] = ran("exit 0", "acme/web#7\n[exit 0]")
	var got map[string]any
	a.Handlers["lookup"] = func(_ context.Context, c agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		_ = json.Unmarshal(c.Input, &got)
		return anthropic.NewToolResultBlock(c.ID, "ok", false)
	}
	res := a.Run(t.Context(), agent.Call{ID: "s1", Name: StartTool, Input: []byte(`{"skill":"capturing"}`)}, agent.Callbacks{OnCall: h.Observe})
	if res.OfToolResult.IsError.Or(false) {
		t.Fatalf("start_skill failed: %s", resultText(res))
	}
	if got["repo"] != "acme/web" || got["number"] != float64(7) || !strings.Contains(fmt.Sprint(got["seen"]), "acme/web#7") {
		t.Fatalf("lookup got %v; want repo acme/web, number 7 as a number, and the target stage's text", got)
	}
}

func TestPortedFlowsLoadEveryChecklistTheyName(t *testing.T) {
	spawns := map[string][]string{"dev-cycle": {"change-review", "coder-task"}, "spec": {"spec-critic"}}
	for flowName, input := range map[string]map[string]any{"pr-ship": {"change": "@-"}, "pr-review": {"pr": "acme/web#7"}, "e2e-verify": {"change": "x"},
		"dev-cycle": {"goal": "x"}, "spec": {"goal": "x"}} {
		t.Run(flowName, func(t *testing.T) {
			s, err := skills.Parse(flowName, mustBuiltin(t, flowName))
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{flowName: string(mustBuiltin(t, flowName))}
			s.Walk(func(_ string, st *skills.Stage) {
				if st.Checklist != "" {
					files[st.Checklist] = string(mustBuiltin(t, st.Checklist))
				}
			})
			tools := slices.Concat(s.Requires.Tools, s.Requires.OptionalTools)
			h, _ := host(t, files, tools...)
			checked := map[string]bool{}
			h.CheckDispatch = func(_, skill string) error { checked[skill] = true; return nil }
			h.Dispatch = func(context.Context, agent.Call, string, string, map[string]any, string, string, agent.Callbacks) (int, error) {
				return 1, nil
			}
			if _, err := h.StartInputs(flowName, input); err != nil {
				t.Fatalf("%s did not start: %v", flowName, err)
			}
			if got := slices.Sorted(maps.Keys(checked)); !slices.Equal(got, spawns[flowName]) {
				t.Errorf("%s checked spawns of %v, want %v", flowName, got, spawns[flowName])
			}
		})
	}
}

const dispatching = `schema: 2
skill: dispatching
version: 1
when: testing dispatches across stages
requires:
  tools: [bash, task]
stages:
  - plan: { checklist: dispatch-two }
  - self: { checklist: dispatch-one }
  - check: { run: { tool: bash, input: { command: "echo ok" } } }
`

func dispatchList(name, min string) string {
	return "schema: 2\nskill: " + name + "\nversion: 1\nwhen: testing dispatches\nrequires:\n  tools: [task]\n" +
		"nodes:\n  go:\n    say: Dispatch.\n    evidence:\n      - call: { tool: task, min: " + min + " }\n"
}

func TestAFlowCountsADispatchWhenMadeAndNotAgainWhenItLands(t *testing.T) {
	h, a := host(t, map[string]string{"dispatching": dispatching, "dispatch-two": dispatchList("dispatch-two", "2"),
		"dispatch-one": dispatchList("dispatch-one", "1")}, "bash", "task")
	h.Lands = func(tool string) bool { return tool == "task" }
	a.Screen = screenPass{}
	a.Handlers["bash"] = ran("exit 0", "ok\n[exit 0]")
	a.Handlers["task"] = answer("dispatched")
	run := func(id, name, input string) (string, bool) {
		res := a.Run(t.Context(), agent.Call{ID: id, Name: name, Input: []byte(input)}, agent.Callbacks{OnCall: h.Observe})
		return resultText(res), res.OfToolResult.IsError.Or(false)
	}
	if _, err := h.StartInputs("dispatching", nil); err != nil {
		t.Fatal(err)
	}
	first := agent.Call{ID: "r1", Name: "task", Input: []byte(`{"agent":"review"}`)}
	run("r1", "task", `{"agent":"review"}`)
	run("r2", "task", `{"agent":"review"}`)
	h.Land(first, false, nil)
	if text, refused := run("f1", FinishStageTool, `{}`); refused {
		t.Fatalf("plan with two dispatches made, one still running, was refused: %s", text)
	}
	h.Land(agent.Call{ID: "r2", Name: "task", Input: []byte(`{"agent":"review"}`)}, false, nil)
	if text, refused := run("f2", FinishStageTool, `{}`); !refused || !strings.Contains(text, "0 of 1") {
		t.Fatalf("a landing during self counted as self's dispatch: %q (refused %v)", text, refused)
	}
}

const spawning = `schema: 2
skill: spawning
version: 1
when: testing a spawn stage
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - plan: { checklist: plan-list, out: { id: string } }
  - code: { spawn: { agent: coder, run: coder-task, inputs: { task: "do ${plan.id}" }, change: "${plan.id}" }, on_fail: plan, max: 2 }
  - check: { run: { tool: bash, input: { command: "echo ok" } } }
`

type spawnRig struct {
	*flowRig
	calls   []agent.Call
	inputs  []map[string]any
	changes []string
	stopped []int
}

func spawnFlow(t *testing.T) *spawnRig {
	t.Helper()
	return spawnFlowOf(t, spawning)
}

// spawnFlowOf is a spawn rig started on body, a flow named spawning.
func spawnFlowOf(t *testing.T, body string) *spawnRig {
	t.Helper()
	rig := &spawnRig{flowRig: coderFlow(t, map[string]string{"spawning": body, "plan-list": planList})}
	home := t.TempDir()
	rig.h.Home = func() string { return home }
	rig.h.CheckDispatch = func(string, string) error { return nil }
	rig.h.Dispatch = func(_ context.Context, call agent.Call, _, _ string, inputs map[string]any, change, _ string, _ agent.Callbacks) (int, error) {
		rig.calls, rig.inputs, rig.changes = append(rig.calls, call), append(rig.inputs, inputs), append(rig.changes, change)
		return 40 + len(rig.calls), nil
	}
	rig.h.Stop = func(id int) error { rig.stopped = append(rig.stopped, id); return nil }
	if _, err := rig.h.StartInputs("spawning", nil); err != nil {
		t.Fatal(err)
	}
	rig.edit(t)
	return rig
}

func TestASpawnStageWaitsForItsSubagentAndMovesOnInTheNextTurn(t *testing.T) {
	rig := spawnFlow(t)
	text, refused := rig.finish(t, `{"id":"abc"}`)
	if refused || len(rig.calls) != 1 || rig.inputs[0]["task"] != "do abc" || rig.changes[0] != "abc" || !strings.Contains(text, "end your turn") {
		t.Fatalf("finish_stage = %q (refused %v), dispatches %v %v; want coder-task started with plan's id", text, refused, rig.inputs, rig.changes)
	}
	if text, refused := rig.finish(t, `{}`); !refused || !strings.Contains(text, "waits for coder") {
		t.Fatalf("finish_stage while the spawn ran = %q (refused %v), want refused", text, refused)
	}
	rig.h.Land(rig.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: true})
	if len(rig.bash) != 0 || !rig.h.Leading() {
		t.Fatalf("the landing ran the next stage outside a turn (bash %v) or left nothing to lead", rig.bash)
	}
	if text := rig.h.lead(t.Context(), agent.Callbacks{}); !strings.Contains(text, "landed complete") || len(rig.bash) != 1 || rig.closed != Finished {
		t.Fatalf("the next turn's lead = %q (bash %v, closed %q); want the landing reported and the flow complete", text, rig.bash, rig.closed)
	}
}

func TestASpawnThatLandsWithWaiversListsThemInTheLanding(t *testing.T) {
	rig := spawnFlow(t)
	rig.finish(t, `{"id":"abc"}`)
	rig.h.Land(rig.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: true,
		Waivers: []skills.Waiver{{Step: "verify", Reason: "no tests in this repo"}}})
	if text := rig.h.lead(t.Context(), agent.Callbacks{}); !strings.Contains(text, "landed complete with 1 waiver: verify: no tests in this repo") {
		t.Fatalf("lead after a landing with a waiver = %q, want the waiver listed", text)
	}
}

func TestASpawnsWaiversReachTheFlowsResultAndAreDroppedWhenOnFailRunsItAgain(t *testing.T) {
	rig := spawnFlowOf(t, strings.Replace(spawning, `command: "echo ok" } } }`, `command: "make verify" } }, on_fail: plan }`, 1))
	verified := false
	rig.passing = func(c string) bool { return c != "make verify" || verified }
	land := func(i int, w skills.Waiver) {
		t.Helper()
		rig.finish(t, `{"id":"abc"}`)
		rig.h.Land(rig.calls[i], false, &skills.SkillCheck{Name: "coder-task", Finished: true, Waivers: []skills.Waiver{w}})
		if _, res, _ := rig.h.check(); !slices.Contains(res.Waivers, skills.Waiver{Step: "code." + w.Step, Reason: w.Reason}) {
			t.Fatalf("after landing, the flow's waivers = %+v, want code.%s among them", res.Waivers, w.Step)
		}
	}
	land(0, skills.Waiver{Step: "verify", Reason: "no tests in this repo"})
	if text := rig.h.lead(t.Context(), agent.Callbacks{}); !strings.Contains(text, "reopened plan") {
		t.Fatalf("the failing check = %q, want plan reopened", text)
	}
	rig.edit(t)
	verified = true
	land(1, skills.Waiver{Step: "work.edit", Reason: "a docs-only change"})
	rig.h.lead(t.Context(), agent.Callbacks{})
	if want := []skills.Waiver{{Step: "code.work.edit", Reason: "a docs-only change"}}; rig.closed != Finished || !slices.Equal(rig.result.Waivers, want) {
		t.Fatalf("closed %q with waivers %+v, want finished with only the second spawn's", rig.closed, rig.result.Waivers)
	}
}

func TestASpawnWithAReportWritesTheLandedRunsReportThere(t *testing.T) {
	body := strings.Replace(spawning, "stages:\n", "vars:\n  RUN: ~/.local/state/alux/skills/spawning/${now_utc}\nstages:\n", 1)
	body = strings.Replace(body, `change: "${plan.id}" }`, `change: "${plan.id}", report: "${RUN}/r-${plan.id}.md" }`, 1)
	rig := spawnFlowOf(t, body)
	rig.finish(t, `{"id":"abc"}`)
	report := "[blocking] a.go:1: x -> y"
	rig.h.Land(rig.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: true, Report: report})
	paths, _ := filepath.Glob(filepath.Join(rig.h.home(), ".local/state/alux/skills/spawning/*/r-abc.md"))
	if len(paths) != 1 {
		t.Fatalf("report files %v, want one r-abc.md under the run dir", paths)
	}
	if got, err := os.ReadFile(paths[0]); err != nil || string(got) != report {
		t.Fatalf("report file = %q (%v), want %q", got, err, report)
	}
}

func TestASpawnReportOutsideTheSkillRunsDirDoesNotLoad(t *testing.T) {
	for _, report := range []string{"/tmp/r.md", "~/.local/state/alux/skills/../r.md"} {
		body := strings.Replace(spawning, `change: "${plan.id}" }`, `change: "${plan.id}", report: "`+report+`" }`, 1)
		if _, err := skills.Parse("spawning", []byte(body)); err == nil || !strings.Contains(err.Error(), "spawn.report") {
			t.Errorf("report %s loaded (%v), want refused at spawn.report", report, err)
		}
	}
}

const verdicting = `schema: 2
skill: verdicting
version: 1
when: testing the verdict stage
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - rounds:
      repeat: { max: 2, until: { field: verdict.kind, equals: clean } }
      stages:
        - verdict:
            checklist: dev-verdict
            inputs: { id: c1, bookmark: b1, task: do it, round: "${rounds.round}" }
            out: { kind: [clean, fix], task: string }
`

func TestFinishStageRefusesCleanWhileThisRoundHasABlockingFinding(t *testing.T) {
	builtin, err := skills.Builtin("dev-verdict.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rig := coderFlow(t, map[string]string{"verdicting": verdicting, "dev-verdict": string(builtin)})
	home := t.TempDir()
	rig.h.Home = func() string { return home }
	rig.h.Now = func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }
	dir := filepath.Join(home, ".local/state/alux/skills/dev-cycle/20261103T120000Z")
	line := "[blocking] a.go:1: nil map -> make it"
	for name, body := range map[string]string{"review-c1-1.md": line + "\n", "findings-c1.md": "## round 1\n" + line + "\n"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rig.h.StartInputs("verdicting", nil); err != nil {
		t.Fatal(err)
	}
	rig.call(t, "bash", `{"command":"jj diff -r b1"}`)
	rig.call(t, "str_replace_based_edit_tool", `{"command":"str_replace","path":"`+filepath.Join(dir, "findings-c1.md")+`"}`)
	if text, refused := rig.finish(t, `{"kind":"clean","task":""}`); !refused || !strings.Contains(text, line) || !strings.Contains(text, "overruled: <why>") || !strings.Contains(text, "kind fix") {
		t.Fatalf("finish_stage clean with a blocking finding = %q (refused %v); want refused naming the line and both ways out", text, refused)
	}
	if text, refused := rig.finish(t, `{"kind":"fix","task":"do it; `+line+`"}`); refused {
		t.Fatalf("finish_stage fix was refused: %q", text)
	}
}

func TestASpawnThatLandsIncompleteReopensItsOnFailStage(t *testing.T) {
	rig := spawnFlow(t)
	rig.finish(t, `{"id":"abc"}`)
	rig.h.Land(rig.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: false})
	if text := rig.h.lead(t.Context(), agent.Callbacks{}); !strings.Contains(text, "reopened plan") || !strings.Contains(text, "Stage **plan**") {
		t.Fatalf("lead after an incomplete landing = %q, want plan reopened", text)
	}
}

func TestFinishStageAfterASpawnLandsMovesTheFlowOnInsteadOfFinishingAnUnopenedStage(t *testing.T) {
	rig := spawnFlow(t)
	rig.finish(t, `{"id":"abc"}`)
	rig.h.Land(rig.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: false})
	rig.call(t, "str_replace_based_edit_tool", `{"command":"view","path":"a.go"}`)
	if rig.h.Leading() == false {
		t.Fatal("an unrelated call moved the flow on")
	}
	text, refused := rig.finish(t, `{"id":"abc"}`)
	if refused || !strings.Contains(text, "reopened plan") || !strings.Contains(text, "Stage **plan**") || len(rig.calls) != 1 {
		t.Fatalf("finish_stage after the landing = %q (refused %v, dispatches %d); want plan reopened, not finished", text, refused, len(rig.calls))
	}
}

func TestEndingAFlowStopsTheSubagentsItSpawned(t *testing.T) {
	rig := spawnFlow(t)
	rig.finish(t, `{"id":"abc"}`)
	rig.h.End("ended with /skills end")
	if !slices.Equal(rig.stopped, []int{41}) {
		t.Fatalf("stopped %v, want the spawned run 41", rig.stopped)
	}
}

const spawningEach = `schema: 2
skill: spawning
version: 1
when: testing a parallel foreach
requires:
  tools: [bash, str_replace_based_edit_tool]
vars:
  RUN: ~/.local/state/alux/skills/spawning/${now_utc}
stages:
  - plan:
      checklist: plan-list
      out: { changes: { type: array, items: { type: object, properties: { id: string } } } }
  - each:
      foreach: { in: plan.changes, as: change, parallel: true }
      stages:
        - code: { spawn: { agent: coder, run: coder-task, inputs: { task: "do ${change.id}" }, report: "${RUN}/r-${change.id}.md" } }
  - check: { run: { tool: bash, input: { command: "echo ok" } } }
`

func TestAParallelForeachStartsEverySpawnAtOnceAndMovesOnAfterTheLastLands(t *testing.T) {
	rig := spawnFlowOf(t, spawningEach)
	rig.finish(t, `{"changes":[{"id":"a"},{"id":"b"}]}`)
	if len(rig.calls) != 2 || rig.inputs[0]["task"] != "do a" || rig.inputs[1]["task"] != "do b" {
		t.Fatalf("dispatched %v before any landed, want both items started", rig.inputs)
	}
	rig.h.Land(rig.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: true, Report: "ra"})
	if rig.h.Leading() {
		t.Fatal("the flow moved on with b still running")
	}
	rig.h.Land(rig.calls[1], false, &skills.SkillCheck{Name: "coder-task", Finished: true, Report: "rb"})
	if text := rig.h.lead(t.Context(), agent.Callbacks{}); rig.closed != Finished || len(rig.bash) != 1 {
		t.Fatalf("lead after both landed = %q (closed %q, bash %v), want the flow complete", text, rig.closed, rig.bash)
	}
	for id, want := range map[string]string{"a": "ra", "b": "rb"} {
		paths, _ := filepath.Glob(filepath.Join(rig.h.home(), ".local/state/alux/skills/spawning/*/r-"+id+".md"))
		if got, err := os.ReadFile(strings.Join(paths, "")); err != nil || string(got) != want {
			t.Errorf("report for %s = %q (%v), want %q", id, got, err, want)
		}
	}
}

func TestAParallelItemThatLandsIncompleteStopsTheFlowAndARetryRerunsOnlyIt(t *testing.T) {
	rig := spawnFlowOf(t, spawningEach)
	rig.finish(t, `{"changes":[{"id":"a"},{"id":"b"}]}`)
	rig.h.Land(rig.calls[0], false, &skills.SkillCheck{Name: "coder-task", Finished: true})
	rig.h.Land(rig.calls[1], false, &skills.SkillCheck{Name: "coder-task", Finished: false})
	if text := rig.h.lead(t.Context(), agent.Callbacks{}); !strings.Contains(text, "did not land complete for b") {
		t.Fatalf("lead after b landed incomplete = %q, want the flow stopped naming b", text)
	}
	if text, refused := rig.finish(t, `{}`); refused || len(rig.calls) != 3 || rig.inputs[2]["task"] != "do b" {
		t.Fatalf("retry = %q (refused %v), dispatched %v; want only b started again", text, refused, rig.inputs)
	}
	rig.h.Land(rig.calls[2], false, &skills.SkillCheck{Name: "coder-task", Finished: true})
	if rig.h.lead(t.Context(), agent.Callbacks{}); rig.closed != Finished {
		t.Fatalf("closed %q after b's rerun landed, want finished", rig.closed)
	}
}

func TestAParallelForeachOfAnythingButOneSpawnDoesNotLoad(t *testing.T) {
	body := strings.Replace(spawningEach, `- code: { spawn:`, `- code: { when: { input: x, equals: 1 }, spawn:`, 1)
	if _, err := skills.Parse("spawning", []byte(body)); err == nil || !strings.Contains(err.Error(), "foreach.parallel") {
		t.Fatalf("a parallel foreach whose spawn has a when loaded (%v), want refused at foreach.parallel", err)
	}
}

const eachChange = `schema: 2
skill: each-change
version: 1
when: testing a foreach
requires:
  tools: [bash, str_replace_based_edit_tool]
stages:
  - plan:
      checklist: plan-list
      out: { changes: { type: array, items: { type: object, properties: { id: string } } } }
  - each:
      foreach: { in: plan.changes, as: change }
      stages:
        - check: { run: { tool: bash, input: { command: "echo ${change.id}" } }, on_fail: plan }
`

func TestAForeachRunsEachItemOnceAndAReopenedPlanSkipsTheFinishedOnes(t *testing.T) {
	rig := coderFlow(t, map[string]string{"each-change": eachChange, "plan-list": planList})
	failedB := false
	rig.passing = func(c string) bool {
		if c == "echo 'b'" && !failedB {
			failedB = true
			return false
		}
		return true
	}
	if _, err := rig.h.StartInputs("each-change", nil); err != nil {
		t.Fatal(err)
	}
	rig.edit(t)
	if text, _ := rig.finish(t, `{"changes":[{"id":"a"},{"id":"b"}]}`); !strings.Contains(text, "reopened plan") {
		t.Fatalf("b's failure did not reopen plan, in the list around the foreach: %q", text)
	}
	rig.edit(t)
	if text, refused := rig.finish(t, `{"changes":[{"id":"a"},{"id":"b"},{"id":"c"}]}`); refused || rig.closed != Finished {
		t.Fatalf("the replanned flow did not complete: %q (closed %q)", text, rig.closed)
	}
	if want := []string{"echo 'a'", "echo 'b'", "echo 'b'", "echo 'c'"}; !slices.Equal(rig.bash, want) {
		t.Fatalf("bash ran %q, want %q: a once, b again after the reopen, then c", rig.bash, want)
	}
}

const rounds = `schema: 2
skill: rounds
version: 1
when: testing a repeat
requires:
  tools: [bash, str_replace_based_edit_tool, ask_user]
inputs:
  most: { type: int, default: 2 }
stages:
  - rounds:
      repeat: { max: "${most}", until: { field: verdict.kind, equals: clean } }
      stages:
        - verdict: { checklist: plan-list, out: { kind: [clean, fix] } }
        - fix: { run: { tool: bash, input: { command: "echo fix" } }, when: { field: verdict.kind, equals: fix } }
`

func roundsFlow(t *testing.T, reply string) (*flowRig, *[]string) {
	t.Helper()
	h, a := host(t, map[string]string{"rounds": rounds, "plan-list": planList}, "bash", "str_replace_based_edit_tool", "ask_user")
	rig := &flowRig{h: h, a: a}
	a.Screen = screenPass{}
	a.Handlers["bash"] = func(ctx context.Context, c agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		rig.bash = append(rig.bash, "fix")
		return ran("exit 0", "ok\n[exit 0]")(ctx, c, cb)
	}
	a.Handlers["str_replace_based_edit_tool"] = answer("edited")
	var asked []string
	a.Handlers["ask_user"] = func(_ context.Context, c agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		asked = append(asked, string(c.Input))
		return anthropic.NewToolResultBlock(c.ID, "The user answered: "+reply, false)
	}
	h.Closed = func(_ *skills.Skill, res skills.Result, how string) { rig.closed, rig.result = how, res }
	if _, err := h.StartInputs("rounds", nil); err != nil {
		t.Fatal(err)
	}
	return rig, &asked
}

func TestARepeatEndsWhenItsUntilHoldsAndAsksTheUserAtItsMax(t *testing.T) {
	rig, asked := roundsFlow(t, moreRound)
	for round := 1; round <= 2; round++ {
		rig.edit(t)
		if text, refused := rig.finish(t, `{"kind":"fix"}`); refused || !strings.Contains(text, "Stage **verdict**") {
			t.Fatalf("round %d's fix did not open the next verdict: %q", round, text)
		}
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "2 of at most 2 rounds") {
		t.Fatalf("asked %q, want the user asked once, at the second round", *asked)
	}
	rig.edit(t)
	if text, refused := rig.finish(t, `{"kind":"clean"}`); refused || rig.closed != Finished || len(rig.bash) != 2 {
		t.Fatalf("a clean verdict in the granted round = %q (closed %q, fixes %d); want the flow complete after two fixes", text, rig.closed, len(rig.bash))
	}
}

func TestARepeatAtItsMaxFailsTheRunWhenTheUserSaysSo(t *testing.T) {
	rig, _ := roundsFlow(t, "Fail the run")
	for range 2 {
		rig.edit(t)
		rig.finish(t, `{"kind":"fix"}`)
	}
	if !strings.Contains(rig.closed, "the user chose to fail the run") {
		t.Fatalf("closed %q, want failed by the user's answer", rig.closed)
	}
}

const eachLook = `schema: 2
skill: each-look
version: 1
when: testing a checklist stage's inputs inside a foreach
requires:
  tools: [str_replace_based_edit_tool]
stages:
  - plan:
      checklist: plan-list
      out: { changes: { type: array, items: { type: object, properties: { id: string } } } }
  - each:
      foreach: { in: plan.changes, as: scenario }
      stages:
        - look: { checklist: look-at, inputs: { id: "${scenario.id}" } }
`

const lookAt = `schema: 2
skill: look-at
version: 1
when: testing a checklist stage's inputs
requires:
  tools: [str_replace_based_edit_tool]
inputs:
  id: { type: string }
nodes:
  look:
    say: View ${id}.
    evidence:
      - call: { tool: str_replace_based_edit_tool, when: 'call.args.path == inputs.id' }
`

func TestAChecklistInsideAForeachSeesItsItemThroughItsInputs(t *testing.T) {
	rig := coderFlow(t, map[string]string{"each-look": eachLook, "plan-list": planList, "look-at": lookAt})
	if _, err := rig.h.StartInputs("each-look", nil); err != nil {
		t.Fatal(err)
	}
	rig.edit(t)
	if text, _ := rig.finish(t, `{"changes":[{"id":"a"},{"id":"b"}]}`); !strings.Contains(text, "View a.") {
		t.Fatalf("item a's stage brief = %q, want it to name a", text)
	}
	view := func(path string) { rig.call(t, "str_replace_based_edit_tool", `{"command":"view","path":"`+path+`"}`) }
	view("b")
	if _, refused := rig.finish(t, `{}`); !refused {
		t.Fatal("viewing b counted as evidence for item a")
	}
	view("a")
	if text, refused := rig.finish(t, `{}`); refused || !strings.Contains(text, "View b.") {
		t.Fatalf("finishing item a = %q (refused %v), want item b's stage open", text, refused)
	}
}

func mustBuiltin(t *testing.T, name string) []byte {
	t.Helper()
	b, err := skills.Builtin(name + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStartSkillLeavesOutChecklistsAFlowNamesAsStages(t *testing.T) {
	rig := coderFlow(t, map[string]string{"plan-list": planList})
	param, ok := rig.h.startParam([]string{"bash", "str_replace_based_edit_tool"})
	if !ok {
		t.Fatal("start_skill was not offered")
	}
	listed, _ := param.OfTool.InputSchema.Properties.(map[string]any)["skill"].(map[string]any)["enum"].([]string)
	if slices.Contains(listed, "coder-work") || !slices.Contains(listed, "coder-task") || !slices.Contains(listed, "plan-list") {
		t.Fatalf("start_skill lists %v; want coder-task and plan-list but not coder-work", listed)
	}
}
