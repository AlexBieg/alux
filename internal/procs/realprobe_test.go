package procs

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/anthropics/anthropic-sdk-go"
)

// Nothing runs: with no command criteria every shell call is refused, and OnCall still sees the lifetime the model chose.
func TestRealAPIAcceptsAndUsesTheLifetimeEnum(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the run_background lifetime probe")
	}
	var run anthropic.ToolUnionParam
	for _, tool := range NewRegistry().Tools() {
		if tool.OfTool.Name == "run_background" {
			run = tool
		}
	}
	a := &agent.Agent{
		Standing: agent.NewRuleInjector(""),
		System:   "You are a coding agent. Start what the user asks for with run_background, both in one reply.",
		Tools:    func() []anthropic.ToolUnionParam { return []anthropic.ToolUnionParam{run} },
		Handlers: NewRegistry().Handlers(),
	}
	lifetimes := map[string]string{}
	cb := agent.Callbacks{
		OnText: func(string) {}, OnThinking: func(string) {}, OnThinkingEnd: func() {},
		Approve:      func(context.Context, string, string) bool { return true },
		Ask:          func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult: func(string, string, string, agent.ToolKind) {},
		OnCall: func(call agent.Call, _ anthropic.ContentBlockParamUnion) {
			var in struct{ Command, Lifetime string }
			_ = json.Unmarshal(call.Input, &in)
			lifetimes[in.Command] = in.Lifetime
		},
	}
	res := a.RunTurn(context.Background(), []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(
		"Start `go test ./...` and `npm run dev` in the background, then stop."))}, cb)
	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("the real API rejected the run_background schema: %s", res.StopReason)
	}
	t.Logf("stop=%s lifetimes=%v", res.StopReason, lifetimes)
	for command, lifetime := range lifetimes {
		switch {
		case strings.Contains(command, "go test") && lifetime != lifetimeFinite:
			t.Errorf("%q started as %q, want finite", command, lifetime)
		case strings.Contains(command, "npm run dev") && lifetime != lifetimeOngoing:
			t.Errorf("%q started as %q, want ongoing", command, lifetime)
		}
	}
	if len(lifetimes) < 2 {
		t.Errorf("started %v, want both commands", lifetimes)
	}
}
