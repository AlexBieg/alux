package session

import (
	"context"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestEverySettledPromptReachesOnPromptWithItsReasonAndOutcome(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "asks", Input: map[string]any{}}}},
		mockapi.Turn{Text: "turn over"},
	)
	var got []PromptRecord
	s.OnPrompt = func(r PromptRecord) { got = append(got, r) }
	cancels := make(chan context.CancelFunc)
	s.agent.Handlers["asks"] = func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		cb.Require(ctx, "$ a", "", "it pushes")
		q, cancelQ := context.WithCancel(ctx)
		cancels <- cancelQ
		cb.Ask(q, ask.Question{Question: "gone?"})
		cb.Ask(ctx, ask.Question{Question: "b?"})
		return anthropic.NewToolResultBlock(call.ID, "done", false)
	}

	s.Submit("go")
	seq := waitUntil(t, "approval", func(sn Snapshot) bool { return sn.Pending == "$ a" }, s).PromptSeq
	s.ApproveID(seq, false)
	cancel := <-cancels
	waitUntil(t, "question gone", func(sn Snapshot) bool { return sn.Question != nil && sn.Question.Question == "gone?" }, s)
	cancel()
	seq = waitUntil(t, "question b", func(sn Snapshot) bool { return sn.Question != nil && sn.Question.Question == "b?" }, s).PromptSeq
	s.AnswerID(seq, &ask.Answer{Label: "yes"})
	settled(t, s, "turn over")

	want := []PromptRecord{
		{Kind: "approval", Label: "$ a", Reason: "it pushes", Outcome: "declined"},
		{Kind: "question", Label: "gone?", Outcome: "interrupted"},
		{Kind: "question", Label: "b?", Outcome: "answered"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.Kind || g.Label != w.Label || g.Reason != w.Reason || g.Outcome != w.Outcome || g.Time.IsZero() {
			t.Errorf("record %d: %+v, want %+v", i, g, w)
		}
	}
}

func TestApproveIDAndAnswerIDResolveOnlyTheCurrentPrompt(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "asks", Input: map[string]any{}}}},
		mockapi.Turn{Text: "turn over"},
	)
	cancels := make(chan context.CancelFunc)
	approved := make(chan bool)
	answered := make(chan *ask.Answer)
	s.agent.Handlers["asks"] = func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		a, cancelA := context.WithCancel(ctx)
		cancels <- cancelA
		cb.Require(a, "$ a", "", "")
		approved <- cb.Require(ctx, "$ b", "", "")

		q, cancelQ := context.WithCancel(ctx)
		cancels <- cancelQ
		cb.Ask(q, ask.Question{Question: "a?"})
		answered <- cb.Ask(ctx, ask.Question{Question: "b?"})
		return anthropic.NewToolResultBlock(call.ID, "done", false)
	}

	s.Submit("go")
	cancel := <-cancels
	stale := waitUntil(t, "approval a", func(sn Snapshot) bool { return sn.Pending == "$ a" }, s).PromptSeq
	cancel()
	current := waitUntil(t, "approval b", func(sn Snapshot) bool { return sn.Pending == "$ b" }, s).PromptSeq
	if s.ApproveID(stale, true) {
		t.Fatal("ApproveID resolved a cancelled approval's seq")
	}
	if sn := s.Snapshot(); sn.Pending != "$ b" || sn.PromptSeq != current {
		t.Fatalf("approval b no longer parked after a stale ApproveID: %q seq %d", sn.Pending, sn.PromptSeq)
	}
	if !s.ApproveID(current, true) {
		t.Fatal("ApproveID refused the parked approval")
	}
	if !<-approved {
		t.Fatal("the handler did not see the approval")
	}

	cancel = <-cancels
	stale = waitUntil(t, "question a", func(sn Snapshot) bool { return sn.Question != nil && sn.Question.Question == "a?" }, s).PromptSeq
	cancel()
	current = waitUntil(t, "question b", func(sn Snapshot) bool { return sn.Question != nil && sn.Question.Question == "b?" }, s).PromptSeq
	if s.AnswerID(stale, &ask.Answer{Label: "stale"}) {
		t.Fatal("AnswerID resolved a cancelled question's seq")
	}
	if sn := s.Snapshot(); sn.Question == nil || sn.PromptSeq != current {
		t.Fatalf("question b no longer parked after a stale AnswerID: seq %d", sn.PromptSeq)
	}
	if !s.AnswerID(current, &ask.Answer{Label: "yes"}) {
		t.Fatal("AnswerID refused the parked question")
	}
	if got := <-answered; got == nil || got.Label != "yes" {
		t.Fatalf("the handler got %+v", got)
	}
	if sn := settled(t, s, "turn over"); sn.PromptSeq != 0 {
		t.Fatalf("PromptSeq %d with nothing parked", sn.PromptSeq)
	}
}
