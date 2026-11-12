package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mine"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/subagent"
)

func TestTheMainSessionWritesItsTranscriptToDisk(t *testing.T) {
	note := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(note, []byte("hello from the file"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "str_replace_based_edit_tool", Input: map[string]any{"command": "view", "path": note}}}},
		mockapi.Turn{Text: "It says hello."},
	)
	dir := t.TempDir()
	s.Transcript = subagent.NewTranscript(dir, "sess1")

	s.Submit("what does the file say?")
	settled(t, s, "It says hello.")
	s.NewSession()

	events, err := subagent.ReadTranscript(dir, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{subagent.TranscriptUser, subagent.TranscriptTool, subagent.TranscriptText, subagent.TranscriptReset}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	if events[0].Text != "what does the file say?" || events[2].Text != "It says hello." {
		t.Fatalf("prompt or reply not recorded verbatim: %+v", events)
	}
	if events[1].Name != "str_replace_based_edit_tool" || !strings.Contains(events[1].Output, "hello from the file") {
		t.Fatalf("tool call not recorded with its output: %+v", events[1])
	}
}

func TestEachTurnsToolCallsReachTheSequenceLogAndNewStartsAConversation(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "true && jj log"}}}},
		mockapi.Turn{Text: "done"},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: "bash", Input: map[string]any{"command": "jj status"}}}},
		mockapi.Turn{Text: "again"},
	)
	dir := t.TempDir()
	s.Sequences = mine.NewRecorder(dir, "sess1")
	mined := make(chan struct{}, 1)
	s.Mine = func() { mined <- struct{}{} }

	s.Submit("look")
	settled(t, s, "done")
	s.NewSession()
	<-mined
	s.Submit("look again")
	settled(t, s, "again")

	turns, err := mine.ReadLog(mine.SequencesFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Calls[0] != "jj_log" || turns[1].Conversation != 1 {
		t.Fatalf("turns = %+v, want jj_log in conversation 0 then a turn in conversation 1", turns)
	}
}
