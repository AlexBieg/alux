package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestTheReviewerCanOnlyRead(t *testing.T) {
	if Reviewer(nil, nil) != nil {
		t.Fatal("a reviewer was built without PR tools")
	}
	prTool := anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: "pr_diff"}}
	noop := func(context.Context, agent.Call, agent.Callbacks) anthropic.ContentBlockParamUnion {
		return anthropic.ContentBlockParamUnion{}
	}
	def := Reviewer([]anthropic.ToolUnionParam{prTool}, map[string]agent.Handler{"pr_diff": noop})

	if def.Fallback != nil {
		t.Error("the reviewer has a fallback, which reaches a shell or MCP")
	}
	reads := map[string]bool{
		"str_replace_based_edit_tool": true, "glob": true, "grep": true,
		"vault_search": true, "vault_read": true, "pr_diff": true, "bash": true, "decline_suggestion": true,
		"compose": true,
	}
	if !readOnlyShellOf(def) {
		t.Error("the reviewer's shell is not bounded to reading")
	}
	for name := range def.Handlers {
		if !reads[name] {
			t.Errorf("the reviewer has a %s handler", name)
		}
	}
	for _, tool := range def.Tools() {
		if name := *tool.GetName(); !reads[name] {
			t.Errorf("the reviewer is offered %s", name)
		}
	}

	path := filepath.Join(t.TempDir(), "x.go")
	input, _ := json.Marshal(map[string]string{"command": "create", "path": path, "file_text": "x"})
	def.Handlers["str_replace_based_edit_tool"](context.Background(), agent.Call{ID: "1", Input: input}, agent.Callbacks{})
	if _, err := os.Stat(path); err == nil {
		t.Error("the reviewer's editor wrote a file")
	}
	if !strings.Contains(def.System(), "\nStart with pr_detail") {
		t.Error("the reviewer's instructions no longer start with pr_detail")
	}
	if def.At != nil {
		t.Error("the PR reviewer can be pointed at a graph workspace")
	}
}
