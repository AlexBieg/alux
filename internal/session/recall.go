package session

import (
	"context"
	"encoding/json"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/compact"
	"github.com/anthropics/anthropic-sdk-go"
)

// wireRecall offers the recall tool while the conversation is a compacted one, reading the events its view numbered.
func (s *Session) wireRecall() {
	if s.agent == nil {
		return
	}
	base := s.agent.Tools
	s.agent.Tools = func() []anthropic.ToolUnionParam {
		var out []anthropic.ToolUnionParam
		if base != nil {
			out = base()
		}
		s.mu.Lock()
		on := s.recalled != nil
		s.mu.Unlock()
		if on {
			out = append(out, recallParam())
		}
		return out
	}
	if s.agent.Handlers == nil {
		s.agent.Handlers = map[string]agent.Handler{}
	}
	s.agent.Handlers[compact.RecallTool] = s.recall
}

func recallParam() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: compact.RecallTool,
		Description: anthropic.String("Return steps of the compacted history in full, numbered as the compacted view numbers them: " +
			"user messages, replies, and each tool call with its input and output. Take the range from a view line such as [120-135] or @42. " +
			"One call returns at most 40,000 characters and says where it stopped."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{"steps": map[string]any{"type": "string", "description": "A step number or range, e.g. 42 or 120-135."}},
			Required:   []string{"steps"},
		},
	}}
}

func (s *Session) recall(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Steps string `json:"steps"`
	}
	_ = json.Unmarshal(call.Input, &in)
	s.mu.Lock()
	ev := s.recalled
	s.mu.Unlock()
	if ev == nil {
		return anthropic.NewToolResultBlock(call.ID, "This conversation was not compacted, so there is no history to recall.", true)
	}
	label := "recall " + in.Steps
	if cb.OnToolStart != nil {
		cb.OnToolStart(call.ID, label, agent.KindSearch)
	}
	out, err := compact.Recall(ev, in.Steps)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if cb.OnToolResult != nil {
		cb.OnToolResult(call.ID, label, out, agent.KindSearch)
	}
	return anthropic.NewToolResultBlock(call.ID, out, false)
}
