package skillrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/anthropics/anthropic-sdk-go"
)

const probe = `schema: 2
skill: probe
version: 1
when: testing the run loop
requires:
  tools: [glob, grep]
inputs:
  topic: { type: string }
nodes:
  look:
    say: Look for ${topic} files.
    evidence:
      - call: { tool: glob }
  tidy:
    say: Nothing to check here.
    optional: true
    evidence:
      - call: { tool: grep }
order:
  - [look, tidy]
`

func host(t *testing.T, files map[string]string, offered ...string) (*Host, *agent.Agent) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := &agent.Agent{Tools: func() []anthropic.ToolUnionParam {
		var tools []anthropic.ToolUnionParam
		for _, name := range offered {
			tools = append(tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: name}})
		}
		return tools
	}}
	h := &Host{Dir: func() (string, error) { return dir, nil }, Home: t.TempDir}
	h.Wire(a)
	return h, a
}

func TestOnlyAUserStartedRunPreapprovesItsDeclaredTools(t *testing.T) {
	h, a := host(t, map[string]string{"probe": probe}, "glob", "grep", "web_fetch")
	if _, err := h.StartText("probe", "yaml"); err != nil {
		t.Fatal(err)
	}
	if !a.Preapproved("glob") || !a.Preapproved("grep") || a.Preapproved("web_fetch") {
		t.Error("a user-started run must preapprove exactly its declared tools")
	}
	if !a.PreapprovedStep(stepPrefix+"1") || a.PreapprovedStep("toolu_01") {
		t.Error("a user-started run must preapprove its own step calls and nothing the model issued")
	}
	h.End("ended")
	if a.Preapproved("glob") || a.PreapprovedStep(stepPrefix+"1") {
		t.Error("closing the run must withdraw its preapproval")
	}

	res := a.Handlers[StartTool](t.Context(), agent.Call{ID: "c1", Name: StartTool, Input: []byte(`{"skill":"probe","inputs":{"topic":"yaml"}}`)}, agent.Callbacks{})
	if res.OfToolResult.IsError.Or(false) {
		t.Fatalf("start_skill refused: %+v", res.OfToolResult.Content)
	}
	if a.Preapproved("glob") || a.Preapproved("grep") || a.PreapprovedStep(stepPrefix+"1") {
		t.Error("a model-started run must preapprove nothing")
	}
}

func TestStartSkillListsOnlySkillsThatCanStartNow(t *testing.T) {
	fetcher := strings.NewReplacer("skill: probe", "skill: fetcher", "tools: [glob, grep]", "tools: [glob, grep, web_fetch]").Replace(probe)
	_, a := host(t, map[string]string{"probe": probe, "fetcher": fetcher}, "glob", "grep", "web_fetch")
	if got := startDescription(a); !strings.Contains(got, "- fetcher:") || !strings.Contains(got, "- probe:") {
		t.Errorf("both skills can start, yet start_skill reads:\n%s", got)
	}
	taint.Set()
	t.Cleanup(taint.ResetForTest)
	_, tainted := host(t, map[string]string{"probe": probe, "fetcher": fetcher}, "glob", "grep", "web_fetch")
	if got := startDescription(tainted); strings.Contains(got, "- fetcher:") || !strings.Contains(got, "- probe:") {
		t.Errorf("a skill needing a tool the PHI latch closed must not be listed:\n%s", got)
	}

	_, bare := host(t, nil, "glob")
	if names := toolNames(bare); slices.Contains(names, StartTool) {
		t.Errorf("with no skill to start, start_skill must not be offered: %v", names)
	}
}

func TestASkillIsNamedOnlyAsWholeWords(t *testing.T) {
	for text, want := range map[string]bool{
		"run dev-cycle on this":         true,
		"Run Dev Cycle please":          true,
		"try devcycle":                  true,
		"use dev_cycle.":                true,
		"the dev cycles are long":       false,
		"my devcycles":                  false,
		"a dev, then a cycle":           false,
		"nothing about it":              false,
		"a predev-cycle-like workflow!": false,
	} {
		if got := named(text, "dev-cycle"); got != want {
			t.Errorf("named(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestStartSkillRefusesANameOutsideTheSkillDirectory(t *testing.T) {
	_, a := host(t, nil, "glob", "grep")
	outside := filepath.Join(t.TempDir(), "probe.yaml")
	if err := os.WriteFile(outside, []byte(probe), 0o644); err != nil {
		t.Fatal(err)
	}
	res := a.Handlers[StartTool](t.Context(), agent.Call{ID: "c1", Name: StartTool, Input: []byte(`{"skill":"../` + filepath.Base(filepath.Dir(outside)) + `/probe","inputs":{"topic":"x"}}`)}, agent.Callbacks{})
	if !res.OfToolResult.IsError.Or(false) || !strings.Contains(res.OfToolResult.Content[0].OfText.Text, "not a skill name") {
		t.Errorf("a path in the skill name must be refused, got %+v", res.OfToolResult.Content)
	}
}

const fanout = `schema: 2
skill: fanout
version: 1
when: testing dispatch evidence
requires:
  tools: [task]
nodes:
  dispatch:
    say: Dispatch two subagents.
    evidence:
      - call: { tool: task, min: 2 }
  gather:
    say: Have one of them tidy.
    evidence:
      - subagent_skill: { skill: tidy }
order:
  - [dispatch, gather]
`

func TestADispatchCountsWhenItsRunLandsNotWhenTheCallReturns(t *testing.T) {
	h, _ := host(t, map[string]string{"fanout": fanout}, "task")
	h.Lands = func(tool string) bool { return tool == "task" }
	if _, err := h.StartText("fanout", ""); err != nil {
		t.Fatal(err)
	}
	dispatch := agent.Call{ID: "d1", Name: "task", Input: []byte(`{"agent":"research"}`)}
	state := func(node string) string {
		_, res, _ := h.check()
		for _, n := range res.Nodes {
			if n.Name == node {
				return string(n.State)
			}
		}
		return "absent"
	}

	h.Observe(dispatch, anthropic.NewToolResultBlock("d1", "Started in the background as run 1", false))
	h.Observe(dispatch, anthropic.NewToolResultBlock("d1", "Started in the background as run 2", false))
	if got := state("dispatch"); got != "pending" {
		t.Fatalf("two dispatches that only started already count: dispatch is %s", got)
	}

	h.Land(dispatch, true, nil)
	h.Land(dispatch, false, &skills.SkillCheck{Name: "tidy", Finished: false})
	if got := state("dispatch"); got != "pending" {
		t.Errorf("a failed run must not count as a dispatch: dispatch is %s", got)
	}
	if got := state("gather"); got != "pending" {
		t.Errorf("a pinned skill that never reached finish_skill must not count: gather is %s", got)
	}

	h.Land(dispatch, false, &skills.SkillCheck{Name: "tidy", Finished: true})
	if got := state("dispatch"); got != "done" {
		t.Errorf("two runs landed, yet dispatch is %s", got)
	}
	if got := state("gather"); got != "done" {
		t.Errorf("a finished tidy run landed, yet gather is %s", got)
	}
}

const differ = `schema: 2
skill: differ
version: 1
when: testing call conditions over bash commands
requires:
  tools: [bash]
nodes:
  diff:
    say: Read the diff.
    evidence:
      - call: { tool: bash, when: 'call.commands.exists(c, cmd(c, "jj diff"))' }
  report:
    say: Report.
    optional: true
    evidence:
      - call: { tool: bash }
order:
  - [diff, report]
`

func TestObserveGivesABashCallTheCommandsHoldRulesSee(t *testing.T) {
	h, _ := host(t, map[string]string{"differ": differ}, "bash")
	if _, err := h.StartText("differ", ""); err != nil {
		t.Fatal(err)
	}
	h.Observe(agent.Call{ID: "b1", Name: "bash", Input: []byte(`{"command":"cd d && jj diff -r y"}`)},
		anthropic.NewToolResultBlock("b1", "", false))
	_, res, _ := h.check()
	for _, n := range res.Nodes {
		if n.Name == "diff" && n.State != skills.Done {
			t.Errorf("a jj diff after cd must satisfy cmd(c, \"jj diff\"): %+v", n)
		}
	}
}

func TestABashCallIsEvidenceByItsExitWhateverItsOutputSays(t *testing.T) {
	h, a := host(t, map[string]string{"differ": differ}, "bash")
	a.Screen = screenPass{}
	if _, err := h.StartText("differ", ""); err != nil {
		t.Fatal(err)
	}
	diff := func() skills.NodeResult {
		_, res, _ := h.check()
		for _, n := range res.Nodes {
			if n.Name == "diff" {
				return n
			}
		}
		t.Fatalf("no diff node in %+v", res.Nodes)
		return skills.NodeResult{}
	}
	run := func(exit, text string) {
		a.Handlers["bash"] = ran(exit, text)
		a.Run(t.Context(), agent.Call{ID: "b1", Name: "bash", Input: []byte(`{"command":"jj diff -r y"}`)}, agent.Callbacks{OnCall: h.Observe})
	}
	run("exit 1", "Error: Revision `y` doesn't exist")
	if got := diff(); got.State != skills.Pending {
		t.Fatalf("a jj diff that exited 1 with no trailer counted: %+v", got)
	}
	run("exit 0", "diff --git a/x b/x\n[exit 1]")
	if got := diff(); got.State != skills.Done {
		t.Errorf("a jj diff that exited 0 did not count: %+v", got)
	}
}

const watch = `schema: 2
skill: watch
version: 1
when: testing poll evidence
requires:
  tools: [poll]
nodes:
  start:
    say: Start a poll on CI.
    evidence:
      - call: { tool: poll }
  wait:
    say: Wait for it to stop.
    evidence:
      - poll_stopped: {}
order:
  - [start, wait]
`

func TestAPollCountsOnceItsEndIsLoggedUnderTheIdItsCallStarted(t *testing.T) {
	h, _ := host(t, map[string]string{"watch": watch}, "poll")
	h.PollOf = func(call string) int {
		if call == "p1" {
			return 7
		}
		return 0
	}
	if _, err := h.StartText("watch", ""); err != nil {
		t.Fatal(err)
	}
	wait := func() skills.NodeResult {
		_, res, _ := h.check()
		for _, n := range res.Nodes {
			if n.Name == "wait" {
				return n
			}
		}
		t.Fatalf("no wait node in %+v", res.Nodes)
		return skills.NodeResult{}
	}

	h.Observe(agent.Call{ID: "p1", Name: "poll", Input: []byte(`{"command":"gh pr checks"}`)}, anthropic.NewToolResultBlock("p1", "Polling as proc 7", false))
	if got := wait(); got.State != skills.Pending || !slices.Contains(got.Missing, "poll 7 still running") {
		t.Fatalf("a poll only started counts: %+v", got)
	}
	h.PollEnded(8, skills.PollMet)
	if got := wait(); got.State != skills.Pending {
		t.Fatalf("another process's end counted for poll 7: %+v", got)
	}
	h.PollEnded(7, skills.PollMet)
	if got := wait(); got.State != skills.Done {
		t.Errorf("poll 7 met its condition, yet wait is %+v", got)
	}
}

func TestTheToolsStayTheSameAsARunOpensAndCloses(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		h, a := host(t, map[string]string{"probe": probe}, "glob", "grep")
		h.Pinned = pinned
		type offer struct {
			names []string
			raw   string
		}
		snapshot := func() offer {
			tools := a.Tools()
			raw, err := json.Marshal(tools)
			if err != nil {
				t.Fatal(err)
			}
			return offer{names(tools), string(raw)}
		}
		call := func(name, input string) {
			c := agent.Call{ID: name, Name: name, Input: []byte(input)}
			res := a.Handlers[name](t.Context(), c, agent.Callbacks{})
			if res.OfToolResult.IsError.Or(false) {
				t.Fatalf("%s refused: %+v", name, res.OfToolResult.Content)
			}
			h.Observe(c, res)
		}

		before := snapshot()
		if pinned {
			if _, err := h.Start("probe", map[string]any{"topic": "yaml"}); err != nil {
				t.Fatal(err)
			}
		} else {
			call(StartTool, `{"skill":"probe","inputs":{"topic":"yaml"}}`)
		}
		open := snapshot()
		h.Observe(agent.Call{ID: "g", Name: "glob", Input: []byte(`{}`)}, anthropic.NewToolResultBlock("g", "", false))
		call(FinishTool, `{}`)
		if _, ok := h.Open(); ok {
			t.Fatal("finish_skill did not close the run")
		}
		after := snapshot()

		want := []string{"glob", "grep", StartTool, FinishTool, FinishStageTool, FailTool}
		if pinned {
			want = []string{"glob", "grep", FinishTool, FinishStageTool, FailTool}
		}
		if !slices.Equal(before.names, want) {
			t.Errorf("pinned=%v: offered %v, want %v", pinned, before.names, want)
		}
		if !reflect.DeepEqual(before, open) || !reflect.DeepEqual(before, after) {
			t.Errorf("pinned=%v: the tools changed across the run:\nbefore %s\nopen   %s\nafter  %s", pinned, before.raw, open.raw, after.raw)
		}
	}
}

func TestSkillToolsCalledInTheWrongStateAreRefused(t *testing.T) {
	h, a := host(t, map[string]string{"probe": probe}, "glob", "grep")
	refused := func(name, input, want string) {
		t.Helper()
		res := a.Handlers[name](t.Context(), agent.Call{ID: "c", Name: name, Input: []byte(input)}, agent.Callbacks{})
		if !res.OfToolResult.IsError.Or(false) || !strings.Contains(res.OfToolResult.Content[0].OfText.Text, want) {
			t.Errorf("%s %s: want an error saying %q, got %+v", name, input, want, res.OfToolResult.Content)
		}
	}
	refused(FinishTool, `{}`, "No skill run is open.")
	refused(FailTool, `{"reason":"stuck"}`, "No skill run is open.")

	if _, err := h.StartText("probe", "yaml"); err != nil {
		t.Fatal(err)
	}
	refused(StartTool, `{"skill":"probe","inputs":{"topic":"json"}}`, "skill probe is still running; finish it or fail it first")

	h.End("ended")
	h.Pinned = true
	refused(StartTool, `{"skill":"probe","inputs":{"topic":"json"}}`, "this agent runs only the skill it was given")
}

func finishSkill(t *testing.T, a *agent.Agent, input string) (string, bool) {
	t.Helper()
	res := a.Handlers[FinishTool](t.Context(), agent.Call{ID: "f", Name: FinishTool, Input: []byte(input)}, agent.Callbacks{})
	return resultText(res), res.OfToolResult.IsError.Or(false)
}

func TestAWaivedRequiredStepLetsFinishSkillCloseAndIsReported(t *testing.T) {
	h, a := host(t, map[string]string{"probe": probe}, "glob", "grep")
	var how string
	var res skills.Result
	h.Closed = func(_ *skills.Skill, r skills.Result, closedHow string) { how, res = closedHow, r }
	if _, err := h.StartText("probe", "yaml"); err != nil {
		t.Fatal(err)
	}
	if text, refused := finishSkill(t, a, `{}`); !refused || !strings.Contains(text, "waive the step in waive with a reason") {
		t.Fatalf("finish_skill with look unevidenced = %q (refused %v); want refused offering a waiver", text, refused)
	}
	text, refused := finishSkill(t, a, `{"waive":[{"step":"look","reason":"glob is broken here"}]}`)
	if refused || !strings.Contains(text, "look: glob is broken here") {
		t.Fatalf("finish_skill with look waived = %q (refused %v)", text, refused)
	}
	h.End(Finished)
	if how != Finished || !res.Complete || res.Nodes[0].Name != "look" || res.Nodes[0].State != skills.Waived {
		t.Fatalf("closed %q with %+v; want finished, complete, look waived", how, res)
	}
	if s := skills.Summary("probe", how, res); !strings.Contains(s, "finished with 1 waiver") || !strings.Contains(s, "look: glob is broken here") {
		t.Errorf("summary does not report the waiver:\n%s", s)
	}
}

func TestAWaiverOnAStrictStepIsRefused(t *testing.T) {
	strict := strings.Replace(probe, "    say: Look for", "    strict: true\n    say: Look for", 1)
	h, a := host(t, map[string]string{"probe": strict}, "glob", "grep")
	if _, err := h.StartText("probe", "yaml"); err != nil {
		t.Fatal(err)
	}
	text, refused := finishSkill(t, a, `{"waive":[{"step":"look","reason":"glob is broken here"}]}`)
	if !refused || !strings.Contains(text, "step look is strict") {
		t.Fatalf("waiving strict look = %q (refused %v); want refused naming look", text, refused)
	}
}

func toolNames(a *agent.Agent) []string { return names(a.Tools()) }

func startDescription(a *agent.Agent) string {
	for _, tool := range a.Tools() {
		if tool.OfTool != nil && tool.OfTool.Name == StartTool {
			return tool.OfTool.Description.Value
		}
	}
	return ""
}
