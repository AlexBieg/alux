package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
)

func awaitRequest(t *testing.T, r *Roster) Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.TakeRequests(); len(got) > 0 {
			return got[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no request reached the main agent")
	return Request{}
}

func lastRequestHas(t *testing.T, server *mockapi.Server, want string) bool {
	t.Helper()
	raw, err := json.Marshal(server.LastRequest())
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), want)
}

var askOnce = mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "q1", Name: AskTool, Input: map[string]any{"question": "Which file holds the parser?"}}}}

// A detached run's question reaches the main agent, and the answer reaches the run.
func TestADetachedRunCanAskTheMainAgent(t *testing.T) {
	m, server := manager(t, echo(true), askOnce, mockapi.Turn{Text: "Fixed it in parse.go."})
	if _, err := m.Dispatch(context.Background(), "echo", "fix the parser", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	q := awaitRequest(t, m.Roster)
	if q.Kind != KindQuestion || !strings.Contains(RequestBlock([]Request{q}), "Which file holds the parser?") {
		t.Fatalf("request = %+v, want the run's question", q)
	}
	if text, failed := toolText(t, m.runReply(context.Background(), call(t, ReplyTool, map[string]any{"run": q.Run, "answer": "parse.go"}), agent.Callbacks{})); failed {
		t.Fatalf("reply refused: %s", text)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the run never finished")
	}
	if !lastRequestHas(t, server, "parse.go") {
		t.Error("the answer never reached the run")
	}
}

// The main agent's answer cannot be asked for again, so the server must never clear it.
func TestAnAnswerFromTheMainAgentIsNeverCleared(t *testing.T) {
	m, server := manager(t, echo(true), mockapi.Turn{Text: "Filed it."})
	if _, err := m.Dispatch(context.Background(), "echo", "file it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the run never finished")
	}
	if got := clearedSpares(t, server.Requests[0]); got != "["+AskTool+"]" {
		t.Errorf("exclude_tools = %s, want [%s]", got, AskTool)
	}
}

// A foreground run is not offered ask_main: the main agent is blocked on it and could not answer.
func TestAForegroundRunCannotAskTheMainAgent(t *testing.T) {
	m, server := manager(t, echo(false), mockapi.Turn{Text: "done"})
	if _, err := m.Dispatch(context.Background(), "echo", "look", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if lastRequestHas(t, server, AskTool) {
		t.Error("a foreground run was offered ask_main")
	}
}

// A question outstanding when the conversation is cleared is answered for it, not left hanging.
func TestClearingTheConversationReleasesAWaitingRun(t *testing.T) {
	m, server := manager(t, echo(true), askOnce, mockapi.Turn{Text: "Guessed parse.go."})
	if _, err := m.Dispatch(context.Background(), "echo", "fix the parser", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	awaitRequest(t, m.Roster)
	m.Roster.DiscardOutcomes()
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the run stayed blocked after the clear")
	}
	if !lastRequestHas(t, server, "has been cleared") {
		t.Error("the run was not told why nobody answered")
	}
}

func outOfTurns() *Definition {
	def := echo(true)
	def.MaxSteps = 1
	return def
}

var noteOnce = mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}}}

// A detached run that spends its budget asks for more, and a granted run carries on to finish.
func TestAGrantedExtensionLetsTheRunFinish(t *testing.T) {
	m, _ := manager(t, outOfTurns(), noteOnce,
		mockapi.Turn{Text: "Wrote a. Still unfinished: b, about one turn."},
		mockapi.Turn{Text: "Wrote a and b."})
	if _, err := m.Dispatch(context.Background(), "echo", "write a and b", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	q := awaitRequest(t, m.Roster)
	if q.Kind != KindExtension || !strings.Contains(q.Text, "Still unfinished: b") {
		t.Fatalf("request = %+v, want an extension carrying the run's account", q)
	}
	if text, failed := toolText(t, m.runExtend(context.Background(), call(t, ExtendTool, map[string]any{"run": q.Run, "grant": true}), agent.Callbacks{})); failed {
		t.Fatalf("grant refused: %s", text)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the run never finished")
	}
	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 || outcomes[0].Report != "Wrote a and b." {
		t.Fatalf("outcomes = %+v, want the finished report", outcomes)
	}
	if run, _ := m.Roster.Find(q.Run); run.MaxSteps != 2 || run.Extensions != 1 {
		t.Errorf("run = %d/%d steps, %d extensions; want the budget doubled and one extension counted", run.Step, run.MaxSteps, run.Extensions)
	}
}

// A refused run stops, and its account of what remains is its report.
func TestARefusedExtensionStopsTheRunWithItsAccount(t *testing.T) {
	m, server := manager(t, outOfTurns(), noteOnce, mockapi.Turn{Text: "Wrote a. Still unfinished: b."})
	if _, err := m.Dispatch(context.Background(), "echo", "write a and b", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	q := awaitRequest(t, m.Roster)
	toolText(t, m.runExtend(context.Background(), call(t, ExtendTool, map[string]any{"run": q.Run, "grant": false}), agent.Callbacks{}))
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the run never finished")
	}
	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Report, "not granted") || !strings.Contains(outcomes[0].Report, "Still unfinished: b.") {
		t.Fatalf("outcomes = %+v, want the refusal and the run's account", outcomes)
	}
	if got := server.RequestCount(); got != 2 {
		t.Errorf("requests = %d, want 2: a refused run was asked for its account twice", got)
	}
}

// The main agent grants five extensions to a run; the sixth is put to the user, and a no stops it.
func TestEverySixthExtensionNeedsTheUser(t *testing.T) {
	m, _ := manager(t, echo(true))
	asked := 0
	cb := agent.Callbacks{Require: func(context.Context, string, string, string) bool { asked++; return false }}

	for _, tc := range []struct {
		granted   int
		wantAsked int
		wantGrant bool
	}{{granted: 4, wantAsked: 0, wantGrant: true}, {granted: 5, wantAsked: 1, wantGrant: false}, {granted: 6, wantAsked: 1, wantGrant: true}, {granted: 11, wantAsked: 2, wantGrant: false}} {
		h := m.Roster.start("echo", "", "work", 5, true)
		h.set(func(run *Run) { run.Extensions = tc.granted })
		reply, err := m.Roster.post(h, KindExtension, "more please")
		if err != nil {
			t.Fatal(err)
		}
		m.runExtend(context.Background(), call(t, ExtendTool, map[string]any{"run": h.run.ID, "grant": true}), cb)
		if got := (<-reply).grant; got != tc.wantGrant || asked != tc.wantAsked {
			t.Errorf("after %d grants: granted %v with the user asked %d times; want %v and %d",
				tc.granted, got, asked, tc.wantGrant, tc.wantAsked)
		}
	}
}
