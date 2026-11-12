package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/skillrun"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/taint"
)

const probeSkill = `schema: 2
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

// withSkills writes each skill file into a fresh HOME's skill directory.
func withSkills(t *testing.T, files map[string]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, paths.Config, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func offeredTools(t *testing.T, request []byte) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestASkillRunIsFedEveryAnsweredCall(t *testing.T) {
	withSkills(t, map[string]string{"probe": probeSkill})
	s, server := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: skillrun.FinishTool, Input: map[string]any{}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: "glob", Input: map[string]any{"pattern": "*.nothing-here"}}}},
		mockapi.Turn{Text: "pausing to ask you something"},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t3", Name: skillrun.FinishTool, Input: map[string]any{}}}},
		mockapi.Turn{Text: "all done"},
	)
	s.WireSkills()

	s.runCommand("skills", "run probe yaml")
	if snap := s.Snapshot(); snap.Skill == nil || len(snap.Skill.Edges) != 1 {
		t.Fatalf("the strip must open with the run: %+v", snap.Skill)
	}
	snap := settled(t, s, "pausing to ask you something")
	text := transcript(snap)
	if !strings.Contains(text, "Look for yaml files.") {
		t.Errorf("the brief did not reach the agent:\n%s", text)
	}
	if got := string(server.Requests[1]); !strings.Contains(got, `Refused: these required steps have no evidence yet:\n- look: glob: 0 of 1 calls`) {
		t.Errorf("an early finish_skill must be refused with what is missing; the model was sent:\n%s", got)
	}
	if !strings.Contains(text, "tool: finish_skill\n  | Refused: these required steps have no evidence yet:") {
		t.Errorf("the refusal must show in a box of its own:\n%s", text)
	}
	if snap.Skill == nil || snap.Skill.Nodes[0].State != skills.Done {
		t.Fatalf("the run must stay open across the turn's end: %+v", snap.Skill)
	}

	s.Submit("go on")
	snap = settled(t, s, "all done")
	text = transcript(snap)
	if snap.Skill != nil {
		t.Error("finish_skill with every required step evidenced must close the run")
	}
	for _, want := range []string{"probe** finished, complete", "```mermaid", `n0["look<br/>done"]`, `n0 --> n1`} {
		if !strings.Contains(text, want) {
			t.Errorf("the closing report lacks %q:\n%s", want, text)
		}
	}
	if box, report := strings.LastIndex(text, "The run is closed; every required step"), strings.Index(text, "probe** finished"); box < 0 || box > report {
		t.Errorf("the finish_skill box must come before the report it caused:\n%s", text)
	}
}

func TestTheModelStartsASkillAndGetsItsStepsBack(t *testing.T) {
	userOnly := strings.Replace(strings.Replace(probeSkill, "skill: probe", "skill: manual\nstarts: [user]", 1), "when: testing the run loop", "when: only the user says so", 1)
	withSkills(t, map[string]string{"probe": probeSkill, "manual": userOnly})
	s, server := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: skillrun.StartTool, Input: map[string]any{"skill": "manual"}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: skillrun.StartTool, Input: map[string]any{"skill": "probe", "inputs": map[string]any{"topic": "yaml"}}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t3", Name: "glob", Input: map[string]any{"pattern": "*.nothing-here"}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t4", Name: skillrun.FinishTool, Input: map[string]any{}}}},
		mockapi.Turn{Text: "all done"},
	)
	s.WireSkills()
	s.Submit("look into yaml files")
	snap := settled(t, s, "all done")
	text := transcript(snap)

	first := string(server.Requests[0])
	if !strings.Contains(first, "- probe: use when testing the run loop. Inputs: topic (string, required).") {
		t.Errorf("start_skill must list the skill with its inputs; the model was sent:\n%s", first)
	}
	if !strings.Contains(first, "- manual: use when only the user says so. Starts only when the user names it in their latest message.") {
		t.Errorf("a starts: [user] skill must be listed with how it starts; the model was sent:\n%s", first)
	}
	if got := string(server.Requests[1]); !strings.Contains(got, "Not started: skill manual starts only when the user names it") {
		t.Errorf("starting a user-only skill the user did not name must be refused; the model was sent:\n%s", got)
	}
	second := string(server.Requests[2])
	if !strings.Contains(second, "Look for yaml files.") {
		t.Errorf("the brief must come back as start_skill's result; the model was sent:\n%s", second)
	}
	before, open := offeredTools(t, server.Requests[0]), offeredTools(t, server.Requests[2])
	if !slices.Equal(before, open) || !slices.Contains(open, skillrun.StartTool) || !slices.Contains(open, skillrun.FinishTool) {
		t.Errorf("opening a run must not change the offered tools; before %v, while open %v", before, open)
	}
	for _, want := range []string{"tool: start_skill probe", "probe** finished, complete"} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript lacks %q:\n%s", want, text)
		}
	}
	if snap.Skill != nil {
		t.Error("finish_skill must close a model-started run")
	}
}

func TestTheModelStartsAUserOnlySkillOnlyWhileTheLatestMessageNamesIt(t *testing.T) {
	userOnly := strings.Replace(probeSkill, "skill: probe", "skill: slow-cycle\nstarts: [user]", 1)
	withSkills(t, map[string]string{"slow-cycle": userOnly})
	start := mockapi.ToolUse{Name: skillrun.StartTool, Input: map[string]any{"skill": "slow-cycle", "inputs": map[string]any{"topic": "yaml"}}}
	first, again := start, start
	first.ID, again.ID = "t1", "t4"
	s, server := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{first}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: "glob", Input: map[string]any{"pattern": "*.nothing-here"}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t3", Name: skillrun.FinishTool, Input: map[string]any{}}}},
		mockapi.Turn{Text: "first done"},
		mockapi.Turn{Tools: []mockapi.ToolUse{again}},
		mockapi.Turn{Text: "second done"},
	)
	s.WireSkills()

	s.Submit("Run slow cycle on the yaml files")
	settled(t, s, "first done")
	if got := string(server.Requests[1]); !strings.Contains(got, "Look for yaml files.") {
		t.Errorf("a user-only skill the message names must start; the model was sent:\n%s", got)
	}

	s.Submit("thanks, now tidy up")
	settled(t, s, "second done")
	if got := string(server.Requests[5]); !strings.Contains(got, "Not started: skill slow-cycle starts only when the user names it") {
		t.Errorf("a later message that does not name the skill must withdraw the permission; the model was sent:\n%s", got)
	}
}

func TestFailRunClosesTheRunWithItsReason(t *testing.T) {
	withSkills(t, map[string]string{"probe": probeSkill})
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: skillrun.FailTool, Input: map[string]any{"reason": "no files to look at"}}}},
		mockapi.Turn{Text: "gave up"},
	)
	s.WireSkills()
	s.runCommand("skills", "run probe yaml")
	snap := settled(t, s, "gave up")
	if snap.Skill != nil || !strings.Contains(transcript(snap), "probe** failed: no files to look at, not complete") ||
		!strings.Contains(transcript(snap), "tool: fail_run no files to look at") {
		t.Errorf("fail_run must close the run and say why:\n%s", transcript(snap))
	}
}

func TestASkillIsRefusedUpFrontWhenARequiredToolIsUnusable(t *testing.T) {
	fetching := strings.Replace(probeSkill, "tools: [glob, grep]", "tools: [glob, grep, web_fetch]", 1)
	missing := strings.Replace(strings.Replace(probeSkill, "skill: probe", "skill: needy", 1), "tools: [glob, grep]", "tools: [glob, grep, linear_search]", 1)
	withSkills(t, map[string]string{"fetching": strings.Replace(fetching, "skill: probe", "skill: fetching", 1), "needy": missing})
	s, server := newTestSession(t)
	s.WireSkills()

	s.runCommand("skills", "run needy yaml")
	if snap := s.Snapshot(); snap.Skill != nil || !strings.Contains(transcript(snap), "needy cannot start: it needs linear_search, which this session does not offer") {
		t.Errorf("an unoffered tool must refuse the run:\n%s", transcript(snap))
	}

	taint.Set()
	t.Cleanup(taint.ResetForTest)
	s.runCommand("skills", "run fetching yaml")
	if snap := s.Snapshot(); snap.Skill != nil || !strings.Contains(transcript(snap), "it needs web_fetch, which is closed for the rest of this session because it read a sensitive note") {
		t.Errorf("a tool the sensitivity latch closed must refuse the run:\n%s", transcript(snap))
	}
	if server.RequestCount() != 0 {
		t.Error("a refused run must send nothing to the model")
	}
}

func TestAClosedToolIsLeftOutOfTheRun(t *testing.T) {
	gated := strings.Replace(probeSkill, "tools: [glob, grep]", "tools: [glob, grep]\n  optional_tools: [web_fetch]", 1)
	gated = strings.Replace(gated, "  tidy:\n    say: Nothing to check here.\n    optional: true\n    evidence:\n      - call: { tool: grep }",
		"  tidy:\n    say: Fetch something.\n    optional: true\n    when: { available: web_fetch }\n    evidence:\n      - call: { tool: web_fetch }", 1)
	withSkills(t, map[string]string{"probe": gated})
	s, _ := newTestSession(t, mockapi.Turn{Text: "ok"})
	s.WireSkills()
	taint.Set()
	t.Cleanup(taint.ResetForTest)
	s.runCommand("skills", "run probe yaml")
	snap := s.Snapshot()
	if snap.Skill == nil {
		t.Fatalf("an optional closed tool must not refuse the run:\n%s", transcript(snap))
	}
	if tidy := snap.Skill.Nodes[1]; tidy.Name != "tidy" || tidy.State != skills.Skipped {
		t.Errorf("a node gated on a closed tool must be skipped, got %+v", tidy)
	}
}

func TestOnlyOneSkillRunsAtATime(t *testing.T) {
	withSkills(t, map[string]string{"probe": probeSkill})
	s, _ := newTestSession(t, mockapi.Turn{Text: "on it"})
	s.WireSkills()
	s.runCommand("skills", "run probe yaml")
	settled(t, s, "on it")
	s.runCommand("skills", "run probe again")
	if !strings.Contains(transcript(s.Snapshot()), "skill probe is still running; /skills end closes it") {
		t.Error("a second run must be refused while one is open")
	}
	s.runCommand("skills", "end")
	if _, open := s.skill.Open(); open {
		t.Error("end must close the run")
	}
	if snap := s.Snapshot(); snap.Skill != nil || !strings.Contains(transcript(snap), "ended with /skills end, not complete") {
		t.Errorf("end must close the run early and report it:\n%s", transcript(snap))
	}
}

func TestASkillRunRefusesMissingText(t *testing.T) {
	withSkills(t, map[string]string{"probe": probeSkill})
	s, _ := newTestSession(t)
	s.runCommand("skills", "run probe")
	snap := s.Snapshot()
	if snap.Skill != nil || !strings.Contains(transcript(snap), "input topic is required") {
		t.Errorf("snapshot = %+v\n%s", snap.Skill, transcript(snap))
	}
}
