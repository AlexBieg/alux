package session

import (
	"context"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/awake"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestATurnKeepsTheMacAwakeExceptWhileItWaitsOnTheUser(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "asks", Input: map[string]any{}}}},
		mockapi.Turn{Text: "turn over"},
	)
	s.Awake = awake.CounterForTest()
	working := make(chan struct{})
	s.agent.Handlers["asks"] = func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		working <- struct{}{}
		<-working
		cb.Require(ctx, "$ something", "", "the policy says ask")
		working <- struct{}{}
		<-working
		cb.Ask(ctx, ask.Question{Question: "which?"})
		working <- struct{}{}
		<-working
		return anthropic.NewToolResultBlock(call.ID, "done", false)
	}
	count := func(want int, when string) {
		t.Helper()
		if got := s.Awake.Count(); got != want {
			t.Fatalf("count %d %s, want %d", got, when, want)
		}
	}

	s.Submit("go")
	<-working
	count(1, "while the turn runs a tool")
	working <- struct{}{}
	waitUntil(t, "the approval to park", func(sn Snapshot) bool { return sn.Pending != "" }, s)
	count(0, "while an approval waits on the user")
	s.Approve(true)
	<-working
	count(1, "once the approval was answered")
	working <- struct{}{}
	waitUntil(t, "the question to be asked", func(sn Snapshot) bool { return sn.Question != nil }, s)
	count(0, "while a question waits on the user")
	s.Answer(&ask.Answer{})
	<-working
	count(1, "once the question was answered")
	working <- struct{}{}
	settled(t, s, "turn over")
	count(0, "once the turn ended")
}
