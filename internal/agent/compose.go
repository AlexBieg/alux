package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Compose runs tool calls one after another in a single model request; each step passes the
// rules, policy and approval it would alone.
const Compose = "compose"

var composeTool = anthropic.ToolParam{
	Name: Compose,
	Description: anthropic.String("Run several tool calls in order, in one request, when you already know every step and " +
		"none needs a result you have not seen yet: an edit then the tests, or a describe then a log. Each step is checked " +
		"exactly as if you had called it alone. Steps stop at the first refused or erroring step, which comes back in full; " +
		"a command exiting non-zero is output, not an error, so steps after a failing build still run. Join commands with " +
		"&& inside one bash step when the rest must not run after a failure, and keep tail off a test command whose exit " +
		"code you need. Other steps come back as ok unless show is set, so set it on the ones whose output you need to read."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"steps": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool":  map[string]any{"type": "string", "description": "The tool's name, e.g. bash or str_replace_based_edit_tool."},
						"input": map[string]any{"type": "object", "description": "The tool's input, as you would pass it to the tool directly."},
						"show":  map[string]any{"type": "boolean", "description": "Return this step's full output."},
					},
					"required": []string{"tool", "input"},
				},
			},
		},
		Required: []string{"steps"},
	},
}

// ComposeTool is offered only to an agent that lists it; its calls are answered by runTool.
func ComposeTool() *anthropic.ToolParam { return &composeTool }

type composeStep struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	Show  bool            `json:"show"`
}

func (s composeStep) call(parent string, i int) Call {
	return Call{ID: parent + "." + strconv.Itoa(i+1), Name: s.Tool, Input: s.Input}
}

// ComposeParent is the id of the compose call that ran the call id as a step, or "" for a call made directly.
func ComposeParent(id string) string {
	parent, n, found := strings.Cut(id, ".")
	if !found || parent == "" {
		return ""
	}
	if _, err := strconv.Atoi(n); err != nil {
		return ""
	}
	return parent
}

func (a *Agent) compose(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Steps []composeStep `json:"steps"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	if len(in.Steps) == 0 {
		return failed(call.ID, fmt.Errorf("compose needs at least one step"))
	}
	for i, s := range in.Steps {
		if s.Tool == Compose {
			return failed(call.ID, fmt.Errorf("step %d is a compose; steps cannot nest", i+1))
		}
	}
	var b strings.Builder
	for i, s := range in.Steps {
		res := a.Run(ctx, s.call(call.ID, i), cb)
		text, isError := resultOf(res)
		switch {
		case isError:
			fmt.Fprintf(&b, "%d. %s failed:\n%s\n", i+1, s.Tool, text)
			if left := len(in.Steps) - i - 1; left > 0 {
				fmt.Fprintf(&b, "Stopped here; the %d step(s) after it did not run.\n", left)
			}
			return anthropic.NewToolResultBlock(call.ID, b.String(), true)
		case s.Show:
			fmt.Fprintf(&b, "%d. %s:\n%s\n", i+1, s.Tool, text)
		default:
			fmt.Fprintf(&b, "%d. %s: ok\n", i+1, s.Tool)
		}
	}
	return ok(call.ID, b.String())
}

// withSteps is calls with each compose replaced by its steps, so advice rules see what actually ran.
func withSteps(calls []Call) []Call {
	var out []Call
	for _, c := range calls {
		if c.Name != Compose {
			out = append(out, c)
			continue
		}
		var in struct {
			Steps []composeStep `json:"steps"`
		}
		_ = json.Unmarshal(c.Input, &in)
		for i, s := range in.Steps {
			out = append(out, s.call(c.ID, i))
		}
	}
	return out
}

// resultOf is a step's text and error flag; an image or document in it is dropped.
func resultOf(res anthropic.ContentBlockParamUnion) (string, bool) {
	tr := res.OfToolResult
	if tr == nil {
		return "", false
	}
	var parts []string
	for _, c := range tr.Content {
		if c.OfText != nil {
			parts = append(parts, c.OfText.Text)
		}
	}
	return strings.Join(parts, "\n"), tr.IsError.Or(false)
}
