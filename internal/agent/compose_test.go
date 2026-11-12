package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestAComposeStepThePolicyRefusesStopsTheStepsAfterIt(t *testing.T) {
	var ran []string
	a := &Agent{
		Policy: editorRules(t.TempDir()),
		Screen: &fakeScreen{verdict: passing},
		Handlers: map[string]Handler{"bash": func(_ context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
			var in struct{ Command string }
			_ = json.Unmarshal(call.Input, &in)
			ran = append(ran, in.Command)
			return ok(call.ID, "")
		}},
	}
	steps := []map[string]any{}
	for _, c := range []string{"go test ./...", "jj git push", "jj diff"} {
		steps = append(steps, map[string]any{"tool": "bash", "input": map[string]any{"command": c}})
	}
	input, _ := json.Marshal(map[string]any{"steps": steps})
	cb := Callbacks{
		Approve:      func(context.Context, string, string) bool { return true },
		Require:      func(context.Context, string, string, string) bool { return false },
		OnToolResult: func(string, string, string, ToolKind) {},
	}

	text, isError := resultOf(a.runTool(context.Background(), Call{ID: "t1", Name: Compose, Input: input}, cb))
	if !slices.Equal(ran, []string{"go test ./..."}) {
		t.Errorf("ran %q, want only the step before the refused one", ran)
	}
	if !isError || !strings.Contains(text, "2. bash failed") {
		t.Errorf("result (error %v) does not name the refused step:\n%s", isError, text)
	}
}
