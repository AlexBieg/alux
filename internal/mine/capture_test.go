package mine

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

func runTurn(r *Recorder, prompt string, calls ...agent.Call) {
	var cb agent.Callbacks
	end := r.Watch(prompt, "claude-test", "abc123", &cb)
	for _, c := range calls {
		cb.OnCall(c, anthropic.ContentBlockParamUnion{})
	}
	end()
}

func call(name string, input any) agent.Call {
	b, _ := json.Marshal(input)
	return agent.Call{Name: name, Input: b}
}

func TestEachTurnIsLoggedWithItsConversationAndSkill(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, "s1")
	runTurn(r, "review it",
		call("start_skill", map[string]string{"skill": "pr-review"}),
		call("pr_detail", nil),
		call("bash", map[string]string{"command": "jj diff && go test ./..."}))
	runTurn(r, "thanks", call("finish_skill", nil))
	r.Reset()
	runTurn(r, "Run the skill **deep-research** (v1).", call("web_search", nil))

	turns, err := ReadLog(SequencesFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("logged %d turns, want 2 (a turn with no kept call is skipped): %+v", len(turns), turns)
	}
	first, second := turns[0], turns[1]
	if first.Skill != "pr-review" || !slices.Equal(first.Calls, []string{"pr_detail", "jj_diff", "go_test"}) || first.Conversation != 0 {
		t.Errorf("first turn = %+v", first)
	}
	if second.Skill != "deep-research" || second.Conversation != 1 || second.Key() == first.Key() {
		t.Errorf("after /new, turn = %+v, want conversation 1 under deep-research", second)
	}
}

func TestALoggedTurnCarriesItsModelAndPromptVersion(t *testing.T) {
	dir := t.TempDir()
	runTurn(NewRecorder(dir, "s1"), "look", call("pr_detail", nil))

	turns, _ := ReadLog(SequencesFile(dir))
	if len(turns) != 1 || turns[0].Model != "claude-test" || turns[0].Prompt != "abc123" {
		t.Errorf("turns = %+v, want one turn on claude-test under prompt abc123", turns)
	}
}

func TestAnAbandonedTurnStartingASkillDoesNotLabelLaterTurns(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, "s1")
	var stale agent.Callbacks
	endStale := r.Watch("first", "", "", &stale)
	var cb agent.Callbacks
	end := r.Watch("second", "", "", &cb)
	stale.OnCall(call("start_skill", map[string]string{"skill": "pr-review"}), anthropic.ContentBlockParamUnion{})
	endStale()
	cb.OnCall(call("pr_detail", nil), anthropic.ContentBlockParamUnion{})
	end()

	turns, _ := ReadLog(SequencesFile(dir))
	if len(turns) != 1 || turns[0].Skill != "" || !slices.Equal(turns[0].Calls, []string{"pr_detail"}) {
		t.Errorf("turns = %+v, want one unlabelled pr_detail turn", turns)
	}
}
