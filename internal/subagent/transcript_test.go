package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
)

func TestARunsTranscriptHoldsItsTaskTextToolCallsAndReportInOrder(t *testing.T) {
	m, _ := manager(t, echo(false),
		mockapi.Turn{Text: "Looking first.", Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}}},
		mockapi.Turn{Text: "The answer is 42."},
	)
	m.Transcripts = t.TempDir()
	if _, err := m.Dispatch(context.Background(), "echo", "find the answer", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	runs := m.Roster.Runs()
	events, err := ReadTranscript(m.Transcripts, runs[len(runs)-1].Dispatch)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{TranscriptTask, TranscriptModel, TranscriptText, TranscriptTool, TranscriptReport}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("events %v, want %v: %+v", kinds, want, events)
	}
	task, model, text, tool, report := events[0], events[1], events[2], events[3], events[4]
	if model.Name != "mock" {
		t.Errorf("model %q, want the definition's mock", model.Name)
	}
	if task.Text != "find the answer" || task.Name != "echo" {
		t.Errorf("task %+v, want echo's find the answer", task)
	}
	if text.Text != "Looking first." {
		t.Errorf("text %q, want the words before the call", text.Text)
	}
	if tool.Label != "note a" || tool.Output != "noted a" || tool.Tool != agent.KindVault || string(tool.Input) != `{"text":"a"}` {
		t.Errorf("tool %+v, want note a with its input and shown output", tool)
	}
	if report.Text != "The answer is 42." {
		t.Errorf("report %q, want the final message, once", report.Text)
	}
}

func TestANavigatorKeepsATranscriptOfItsOwn(t *testing.T) {
	m, _ := manager(t, withViewingPlanner(paired()), finishing(
		mockapi.Turn{Tools: []mockapi.ToolUse{viewUse("p1", "a.go")}},
		mockapi.Turn{Text: "PLAN-SENTINEL: change a.go."},
	)...)
	m.Transcripts = t.TempDir()
	dispatch(t, m)

	var driver, nav Run
	for _, run := range m.Roster.Runs() {
		if run.Parent == 0 {
			driver = run
		} else {
			nav = run
		}
	}
	if nav.Dispatch == "" || nav.Dispatch == driver.Dispatch {
		t.Fatalf("the navigator's transcript id is %q, its driver's %q", nav.Dispatch, driver.Dispatch)
	}
	events, err := ReadTranscript(m.Transcripts, nav.Dispatch)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{TranscriptTask, TranscriptTool, TranscriptText, TranscriptUser, TranscriptText, TranscriptUser, TranscriptReport}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("events %v, want %v: %+v", kinds, want, events)
	}
	if events[1].Name != editorName || events[2].Text != "PLAN-SENTINEL: change a.go." || events[4].Text != none ||
		events[6].Text != "Nothing outstanding." {
		t.Errorf("the navigator's transcript lacks its read, plan, review or closing view: %+v", events)
	}
	for _, e := range events {
		if e.Kind != TranscriptUser && strings.Contains(e.Text, "Done.") {
			t.Errorf("the driver's words reached the navigator's transcript: %+v", e)
		}
	}
}

func TestPruningKeepsTheNewestTranscripts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := range 5 {
		path := filepath.Join(dir, strconv.Itoa(i)+".jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	pruneTranscripts(dir, 2)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 || entries[0].Name() != "3.jsonl" || entries[1].Name() != "4.jsonl" {
		t.Errorf("kept %v, want 3 and 4", entries)
	}
}
