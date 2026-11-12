package subagent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/records"
)

func seedRules(t *testing.T, triggers map[string]map[string]any) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "records.db")
	t.Setenv("ALUX_RECORDS", path)
	s, err := records.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := records.Actor{Kind: "agent", Agent: "test"}
	if _, err := s.AddEntity(records.EntityInput{Type: "system", Name: "harness", Sensitivity: "normal"}, actor); err != nil {
		t.Fatal(err)
	}
	for title, clause := range triggers {
		_, err := s.Add(records.RecordInput{Kind: "rule", Subject: "harness", Title: title, Sensitivity: "normal",
			Attrs:  map[string]any{"mode": "advise", "trigger": []any{clause}},
			Source: records.Source{Type: "conversation", Ref: "test"}}, actor)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestASubagentStartsWithItsOwnRulesAndIsShownCallRulesOnItsCalls(t *testing.T) {
	seedRules(t, map[string]map[string]any{
		"Rule for every coder run":      {"agent": "coder"},
		"Rule for every research run":   {"agent": "research"},
		"Rule for a coder's note call":  {"agent": "coder", "tool": "note"},
		"Rule for anyone's note call":   {"tool": "note"},
		"Rule for a research note call": {"agent": "research", "tool": "note"},
	})
	def := echo(false)
	def.Name, def.Commands = "coder", lookOnly("coder")
	m, server := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}}},
		mockapi.Turn{Text: "done"},
	)
	if _, err := m.Dispatch(t.Context(), "coder", "do a thing", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if len(server.Requests) != 2 {
		t.Fatalf("%d requests, want 2", len(server.Requests))
	}
	first, second := string(server.Requests[0]), string(server.Requests[1])
	if !strings.Contains(first, "Rule for every coder run") {
		t.Errorf("the coder's starting prompt lacks its rule:\n%s", first)
	}
	for _, not := range []string{"Rule for every research run", "Rule for a coder's note call", "Rule for anyone's note call"} {
		if strings.Contains(first, not) {
			t.Errorf("the starting prompt carries %q", not)
		}
	}
	for _, want := range []string{"Rule for a coder's note call", "Rule for anyone's note call"} {
		if !strings.Contains(second, want) {
			t.Errorf("the note result lacks %q", want)
		}
	}
	if strings.Contains(second, "Rule for a research note call") {
		t.Error("a research-only call rule reached the coder")
	}
}
