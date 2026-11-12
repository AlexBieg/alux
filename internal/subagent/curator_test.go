package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/skills"
)

const tidySkill = `schema: 2
skill: tidy
version: 1
when: testing a subagent's skill run
requires: { tools: [note] }
nodes:
  first:
    say: Note the first thing.
    evidence:
      - call: { tool: note }
  second:
    say: Note it again.
    evidence:
      - call: { tool: note, min: 2 }
order: [[first, second]]
`

func skilled(t *testing.T, background bool) *Definition {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tidy.yaml"), []byte(tidySkill), 0o644); err != nil {
		t.Fatal(err)
	}
	def := echo(background)
	def.Name = curatorName
	def.Commands = lookOnly(curatorName)
	def.Skill = "tidy"
	def.SkillDir = func() (string, error) { return dir, nil }
	return def
}

func finish(id string) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: "finish_skill", Input: map[string]any{}}}}
}

func TestASkilledAgentIsRefusedFinishUntilTheEvidenceIsThere(t *testing.T) {
	m, server := manager(t, skilled(t, false),
		note("n1"),
		finish("f1"),
		note("n2"),
		finish("f2"),
		mockapi.Turn{Text: "Tidied two things."},
	)
	report, err := m.Dispatch(t.Context(), curatorName, "tidy up", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(report, "Tidied two things.") || !strings.Contains(report, "Skill **tidy** finished, complete.") {
		t.Fatalf("the report should carry the agent's words then the skill's final check:\n%s", report)
	}
	sent, _ := json.Marshal(server.LastRequest())
	if !strings.Contains(string(sent), "Refused: these required steps have no evidence yet") {
		t.Fatal("the first finish_skill, with one note of two, should have been refused")
	}
}

func TestASkilledAgentThatStopsEarlyReportsTheRunIncomplete(t *testing.T) {
	m, _ := manager(t, skilled(t, false),
		note("n1"),
		mockapi.Turn{Text: "Done."},
	)
	report, err := m.Dispatch(t.Context(), curatorName, "tidy up", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "not complete") || !strings.Contains(report, "**second**: pending") {
		t.Fatalf("an unfinished run must say so in the report:\n%s", report)
	}
}

func TestASkilledAgentWithAMissingSkillFailsBeforeAnyTurn(t *testing.T) {
	def := skilled(t, false)
	def.Skill = "absent"
	m, server := manager(t, def)
	if _, err := m.Dispatch(t.Context(), curatorName, "tidy up", "", agent.Callbacks{}); err == nil {
		t.Fatal("want an error for a skill file that does not exist")
	}
	if server.RequestCount() != 0 {
		t.Fatal("a run whose skill cannot open must not reach the model")
	}
}

type landing struct {
	call   agent.Call
	failed bool
	check  *skills.SkillCheck
}

func listen(m *Manager) *[]landing {
	var mu sync.Mutex
	var got []landing
	m.Landed = func(call agent.Call, failed bool, check *skills.SkillCheck) {
		mu.Lock()
		got = append(got, landing{call, failed, check})
		mu.Unlock()
	}
	return &got
}

func TestADetachedDispatchLandsOnceWithItsSkillResult(t *testing.T) {
	m, _ := manager(t, skilled(t, true), note("n1"), note("n2"), finish("f1"), mockapi.Turn{Text: "Tidied."})
	got := listen(m)
	call := agent.Call{ID: "d1", Name: DispatchTool, Input: []byte(`{"agent":"` + curatorName + `","name":"tidy","objective":"tidy up","done":"d","report":"r"}`)}
	res := m.Handlers()[DispatchTool](t.Context(), call, agent.Callbacks{})
	if res.OfToolResult.IsError.Or(false) {
		t.Fatalf("dispatch refused: %+v", res.OfToolResult.Content)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the background run never finished")
	}
	if len(*got) != 1 {
		t.Fatalf("landings = %+v, want exactly one", *got)
	}
	l := (*got)[0]
	if l.call.ID != "d1" || l.failed || l.check == nil || l.check.Name != "tidy" || !l.check.Finished {
		t.Errorf("landing = %+v, want call d1, not failed, tidy finished", l)
	}
}

func TestADispatchLandsWithTheWaiversItsSkillRunMade(t *testing.T) {
	waive := mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "f1", Name: "finish_skill", Input: map[string]any{"waive": []map[string]any{
		{"step": "first", "reason": "noted elsewhere"}, {"step": "second", "reason": "noted elsewhere"}}}}}}
	m, _ := manager(t, skilled(t, true), waive, mockapi.Turn{Text: "Tidied."})
	got := listen(m)
	call := agent.Call{ID: "d1", Name: DispatchTool, Input: []byte(`{"agent":"` + curatorName + `","name":"tidy","objective":"tidy up","done":"d","report":"r"}`)}
	m.Handlers()[DispatchTool](t.Context(), call, agent.Callbacks{})
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the background run never finished")
	}
	if len(*got) != 1 || (*got)[0].check == nil || !(*got)[0].check.Finished || len((*got)[0].check.Waivers) != 2 {
		t.Fatalf("landings = %+v, want tidy finished with its two waivers", *got)
	}
	if report := (*got)[0].check.Report; !strings.HasPrefix(report, "Tidied.") {
		t.Errorf("landed report = %q, want the run's final report", report)
	}
}

func TestAFailedForegroundDispatchLandsAsFailed(t *testing.T) {
	def := skilled(t, false)
	def.Skill = "absent"
	m, _ := manager(t, def)
	got := listen(m)
	call := agent.Call{ID: "d1", Name: DispatchTool, Input: []byte(`{"agent":"` + curatorName + `","name":"tidy","objective":"tidy up","done":"d","report":"r"}`)}
	m.Handlers()[DispatchTool](t.Context(), call, agent.Callbacks{})
	if len(*got) != 1 || !(*got)[0].failed || (*got)[0].check != nil {
		t.Errorf("landings = %+v, want one failed landing with no skill result", *got)
	}
}

const every = 3 * time.Hour

func upkeep(t *testing.T) *Definition {
	t.Helper()
	def := skilled(t, true)
	def.Upkeep = true
	return def
}

func runIfDue(m *Manager, cb agent.Callbacks, stamp string, now time.Time) (int, bool, error) {
	return m.RunIfDue("tidy-job", curatorName, cb, stamp, every, now, nil)
}

func TestRunIfDueStartsOncePerWindow(t *testing.T) {
	m, _ := manager(t, upkeep(t),
		mockapi.Turn{Text: "Nothing to fix."},
		mockapi.Turn{Text: "Nothing to fix."},
	)
	stamp := filepath.Join(t.TempDir(), "state", "jobs", "tidy-job.last")
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	announced := 0
	cb := agent.Callbacks{OnRun: func(int) { announced++ }}

	if _, started, err := runIfDue(m, cb, stamp, at); err != nil || !started {
		t.Fatalf("first session should start a run: started=%v err=%v", started, err)
	}
	if _, started, _ := runIfDue(m, cb, stamp, at.Add(every-time.Minute)); started {
		t.Fatal("a session inside the window started a second run")
	}
	if _, started, err := runIfDue(m, cb, stamp, at.Add(every)); err != nil || !started {
		t.Fatalf("a session after the window should start a run: started=%v err=%v", started, err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("runs never finished")
	}
	if announced != 2 {
		t.Fatalf("want two boxes, got %d", announced)
	}
	if out := m.Roster.TakeOutcomes(); len(out) != 0 {
		t.Fatalf("a scheduled run was queued for the main agent: %+v", out)
	}
}

func TestRunIfDueRefusesAnAgentThatCanChangeFiles(t *testing.T) {
	def := upkeep(t)
	def.Commands.Allowed = append(def.Commands.Allowed, guardian.Edit)
	m, server := manager(t, def, mockapi.Turn{Text: "Edited."})
	stamp := filepath.Join(t.TempDir(), "tidy-job.last")
	if _, started, err := runIfDue(m, agent.Callbacks{}, stamp, time.Now()); err == nil || started {
		t.Fatalf("want a refusal and no run: started=%v err=%v", started, err)
	}
	m.Roster.Drain(5 * time.Second)
	if server.RequestCount() != 0 {
		t.Fatal("a refused job reached the model")
	}
}

func editingUpkeep(t *testing.T, writable string) *Definition {
	t.Helper()
	def := upkeep(t)
	def.Handlers["str_replace_based_edit_tool"] = agent.RunEdit
	def.Policy = readOnlyShell(curatorName)
	def.Policy.Edit = append(def.Policy.Edit, agent.Rule{Pattern: writable, Verdict: agent.VerdictAllow})
	return def
}

func TestRunIfDueStartsAnAgentWhoseEditorWritesOnlyUnderJobsOut(t *testing.T) {
	home, _ := os.UserHomeDir()
	m, _ := manager(t, editingUpkeep(t, filepath.Join(home, paths.JobsOut, "tidy-job")), mockapi.Turn{Text: "Wrote it."})
	if _, started, err := runIfDue(m, agent.Callbacks{}, filepath.Join(t.TempDir(), "tidy-job.last"), time.Now()); err != nil || !started {
		t.Fatalf("want a run: started=%v err=%v", started, err)
	}
	m.Roster.Drain(5 * time.Second)
}

func TestRunIfDueRefusesAnAgentWhoseEditorWritesElsewhere(t *testing.T) {
	home, _ := os.UserHomeDir()
	m, server := manager(t, editingUpkeep(t, filepath.Join(home, paths.Config)), mockapi.Turn{Text: "Edited."})
	if _, started, err := runIfDue(m, agent.Callbacks{}, filepath.Join(t.TempDir(), "tidy-job.last"), time.Now()); err == nil || started {
		t.Fatalf("want a refusal and no run: started=%v err=%v", started, err)
	}
	m.Roster.Drain(5 * time.Second)
	if server.RequestCount() != 0 {
		t.Fatal("a refused job reached the model")
	}
}

func TestRunIfDueHandsTheReportToOnEnd(t *testing.T) {
	m, _ := manager(t, upkeep(t), mockapi.Turn{Text: "Fixed one."})
	got := make(chan string, 1)
	onEnd := func(report string, err error) {
		if err != nil {
			t.Error(err)
		}
		got <- report
	}
	if _, started, err := m.RunIfDue("tidy-job", curatorName, agent.Callbacks{}, filepath.Join(t.TempDir(), "tidy-job.last"), every, time.Now(), onEnd); err != nil || !started {
		t.Fatalf("want a run: started=%v err=%v", started, err)
	}
	select {
	case report := <-got:
		if !strings.HasPrefix(report, "Fixed one.") {
			t.Fatalf("onEnd got %q, want the run's report", report)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onEnd was never called")
	}
}

func TestRunIfDueLeavesTheStampAloneWhenTheSkillWillNotLoad(t *testing.T) {
	def := upkeep(t)
	def.Skill = "absent"
	m, _ := manager(t, def)
	stamp := filepath.Join(t.TempDir(), "tidy-job.last")
	if _, started, err := runIfDue(m, agent.Callbacks{}, stamp, time.Now()); err == nil || started {
		t.Fatalf("want an error and no run: started=%v err=%v", started, err)
	}
	if data, _ := os.ReadFile(stamp); len(data) != 0 {
		t.Fatalf("a run that never started was stamped: %q", data)
	}
}

func TestRunIfDueWaitsForABacklog(t *testing.T) {
	def := upkeep(t)
	backlog := 0
	def.Backlog = func(string) (int, error) { return backlog, nil }
	m, server := manager(t, def, mockapi.Turn{Text: "Fixed one."})
	stamp := filepath.Join(t.TempDir(), "tidy-job.last")
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

	if _, started, err := runIfDue(m, agent.Callbacks{}, stamp, at); err != nil || started {
		t.Fatalf("an empty store should start nothing: started=%v err=%v", started, err)
	}
	if data, _ := os.ReadFile(stamp); len(data) != 0 {
		t.Fatalf("a skipped run was stamped, so a backlog arriving now would wait out the window: %q", data)
	}
	backlog = 1
	if _, started, err := runIfDue(m, agent.Callbacks{}, stamp, at.Add(time.Minute)); err != nil || !started {
		t.Fatalf("a backlog should start a run: started=%v err=%v", started, err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the run never finished")
	}
	if server.RequestCount() != 1 {
		t.Fatalf("want one model request, from the run with a backlog; got %d", server.RequestCount())
	}
}

func TestCuratorBacklogCountsAnEndedSessionWithNoGist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	write := agent.RecordHandlers()["vault_record"]
	for _, input := range []string{
		`{"op":"entity","entity":{"type":"system","name":"alux","sensitivity":"normal"}}`,
		`{"op":"add","kind":"claim","subject":"alux","title":"Written in an ended session","sensitivity":"normal","source":{"type":"conversation","ref":"t"}}`,
	} {
		res := write(agent.WithRecordSession(t.Context(), "ended"), agent.Call{ID: "c", Input: []byte(input)}, agent.Callbacks{})
		if res.OfToolResult.IsError.Or(false) {
			t.Fatalf("%s: %+v", input, res.OfToolResult.Content)
		}
	}
	dir := agent.SessionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ended.jsonl"), []byte(`{"kind":"user","text":"hi"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := curatorBacklog("current"); err != nil || n != 1 {
		t.Fatalf("backlog = %d, %v; want the one gistable session", n, err)
	}
	if n, err := curatorBacklog("ended"); err != nil || n != 0 {
		t.Fatalf("backlog from the session itself = %d, %v; a session is never gistable by its own curator", n, err)
	}
}

func TestRunIfDueCountsTheBacklogForItsOwnSession(t *testing.T) {
	def := upkeep(t)
	var asked string
	def.Backlog = func(session string) (int, error) { asked = session; return 0, nil }
	m, _ := manager(t, def)
	m.Session = "this-session"
	if _, _, err := runIfDue(m, agent.Callbacks{}, filepath.Join(t.TempDir(), "tidy-job.last"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if asked != "this-session" {
		t.Fatalf("backlog counted for session %q, want the dispatching session", asked)
	}
}
