package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/skillrun"
)

const probeSkill = `schema: 2
skill: probe
version: 1
when: testing a dispatched skill
requires: { tools: [note] }
inputs:
  target: { type: string }
nodes:
  first:
    say: Note the target.
    evidence:
      - call: { tool: note }
  second:
    say: Note it again.
    evidence:
      - call: { tool: note, min: 2 }
order: [[first, second]]
`

const browseSkill = `schema: 2
skill: browse
version: 1
when: testing a skill an agent cannot run
requires: { tools: [note, browser] }
nodes:
  first:
    say: Look.
    evidence:
      - call: { tool: note }
  second:
    say: Look again.
    evidence:
      - call: { tool: note, min: 2 }
order: [[first, second]]
`

func probing(t *testing.T) *Definition {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"probe": probeSkill, "browse": browseSkill} {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	def := echo(false)
	def.SkillDir = func() (string, error) { return dir, nil }
	return def
}

func dispatchProbe(t *testing.T, m *Manager) agent.Call {
	t.Helper()
	call := agent.Call{ID: "s1", Name: "dispatch_skill", Input: []byte(`{"skill":"probe"}`)}
	if _, err := m.DispatchSkill(call, "echo", "probe", map[string]any{"target": "checkout flow"}, "", "verify the change", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the skill run never finished")
	}
	return call
}

func TestDispatchSkillBindsItsInputsIntoTheTaskTheModelSees(t *testing.T) {
	m, server := manager(t, probing(t), note("n1"), note("n2"), finish("f1"), mockapi.Turn{Text: "Checked."})
	dispatchProbe(t, m)
	if server.RequestCount() == 0 {
		t.Fatal("the run never reached the model")
	}
	first := string(server.Requests[0])
	if !strings.Contains(first, "verify the change") || !strings.Contains(first, "target = checkout flow") {
		t.Fatalf("the first request should carry the task and the bound input:\n%s", first)
	}
}

func TestDispatchSkillLandsWithTheCallAndWhetherTheSkillFinished(t *testing.T) {
	for _, tc := range []struct {
		name     string
		turns    []mockapi.Turn
		finished bool
	}{
		{"finished", []mockapi.Turn{note("n1"), note("n2"), finish("f1"), {Text: "Checked."}}, true},
		{"stopped early", []mockapi.Turn{{Text: "Gave up."}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := manager(t, probing(t), tc.turns...)
			got := listen(m)
			call := dispatchProbe(t, m)
			if len(*got) != 1 {
				t.Fatalf("landings = %+v, want exactly one", *got)
			}
			l := (*got)[0]
			if l.call.ID != call.ID || l.call.Name != call.Name || string(l.call.Input) != string(call.Input) {
				t.Errorf("landed call = %+v, want %+v", l.call, call)
			}
			if l.failed || l.check == nil || l.check.Name != "probe" || l.check.Finished != tc.finished {
				t.Errorf("landing = %+v, want not failed, probe finished=%v", l, tc.finished)
			}
		})
	}
}

func TestCheckSkillAgentNamesTheToolTheAgentLacks(t *testing.T) {
	m, _ := manager(t, probing(t))
	if err := m.CheckSkillAgent("echo", "probe"); err != nil {
		t.Fatalf("echo has every tool probe needs: %v", err)
	}
	err := m.CheckSkillAgent("echo", "browse")
	if err == nil || !strings.Contains(err.Error(), "echo cannot run browse: it has no browser") {
		t.Fatalf("err = %v, want it to name the missing browser tool", err)
	}
}

func TestTheBuiltInDevCycleChildSkillsLoadAndTheirAgentsCanRunThem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, _ := managerWith(t, []*Definition{coderDef(repoWith(t, "go.mod"), nil), Research()})
	for agentName, skill := range map[string]string{"coder": "coder-task", "research": "change-review"} {
		if err := m.CheckSkillAgent(agentName, skill); err != nil {
			t.Errorf("%s cannot run %s: %v", agentName, skill, err)
		}
	}
}

const verifyFlow = `schema: 2
skill: verify-flow
version: 1
when: testing a flow in a dispatched coder
starts: [model]
requires: { tools: [str_replace_based_edit_tool, bash] }
inputs:
  verify: { type: string }
stages:
  - make: { checklist: make-file, out: { outcome: [changed, nothing_to_do] } }
  - check: { run: { tool: bash, input: { command: "${verify}" } }, on_fail: make, max: 2 }
`

const makeFile = `schema: 2
skill: make-file
version: 1
when: testing a flow in a dispatched coder
requires: { tools: [str_replace_based_edit_tool] }
nodes:
  make:
    say: Make the file.
    evidence:
      - call: { tool: str_replace_based_edit_tool, when: 'call.args.command == "create"' }
`

func TestAPinnedCoderRunningAFlowLandsCompleteOnlyWhenTheFlowCompletes(t *testing.T) {
	for verify, want := range map[string]bool{"grep -q hi made.txt": true, "grep -q bye made.txt": false} {
		finishStage := mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "f1", Name: skillrun.FinishStageTool, Input: map[string]any{"out": map[string]any{"outcome": "changed"}}}}}
		m, _, _ := layeredCoder(t, createTurn(), finishStage, mockapi.Turn{Text: "Made it."})
		dir := t.TempDir()
		for name, body := range map[string]string{"verify-flow": verifyFlow, "make-file": makeFile} {
			if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		def := m.defs["coder"]
		at := def.At
		def.At = func(ws string) *Definition {
			d := at(ws)
			d.SkillDir = func() (string, error) { return dir, nil }
			return d
		}
		def.SkillDir = func() (string, error) { return dir, nil }
		got := listen(m)
		if _, err := m.DispatchSkill(agent.Call{ID: "s1", Name: "dispatch_skill"}, "coder", "verify-flow", map[string]any{"verify": verify}, "", "make a file", agent.Callbacks{}); err != nil {
			t.Fatal(err)
		}
		if !m.Roster.Drain(10 * time.Second) {
			t.Fatal("the flow run never finished")
		}
		if len(*got) != 1 || (*got)[0].check == nil || (*got)[0].check.Name != "verify-flow" || (*got)[0].check.Finished != want {
			t.Errorf("verify %q: landings = %+v, want verify-flow finished=%v", verify, *got, want)
		}
	}
}
