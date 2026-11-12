package subagent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
)

func TestAScopedCoderMayWriteOnlyInItsDirectory(t *testing.T) {
	root, ws := repoWith(t, "go.mod"), repoWith(t, "go.mod", "sub/x.go")
	def := coderDef(root, nil).At(ws)
	a := &agent.Agent{Policy: def.Policy, Handlers: def.Handlers}
	write := func(path string) agent.Verdict { return createVerdict(a, path) }
	if v := write(filepath.Join(ws, "a.go")); v != agent.VerdictAllow {
		t.Errorf("write in the workspace: %v, want allow", v)
	}
	if v := write(filepath.Join(root, "a.go")); v == agent.VerdictAllow {
		t.Error("a workspace-scoped coder may write in the repository it was built from")
	}
	if v := write(filepath.Join(ws, ".jj", "x")); v == agent.VerdictAllow {
		t.Error("a workspace-scoped coder may write in the workspace's .jj")
	}
	if v, why := a.Decide(bash(`{"command":"cd ` + filepath.Join(ws, "sub") + ` && go test ./..."}`)); v != agent.VerdictAllow {
		t.Errorf("cd into the workspace: %v (%s), want allow", v, why)
	}
	if prompt := def.System(); !strings.Contains(prompt, "Working directory: "+ws) {
		t.Error("the scoped coder's prompt does not name the workspace as its working directory")
	}
	if coderDef(root, nil).At == nil || pairedCoderDef(root, nil).At == nil {
		t.Error("both coders should be scopable")
	}
}

func TestAScopedPairConfinesTheDriverAndPointsTheNavigatorAtTheWorkspace(t *testing.T) {
	root, ws := repoWith(t, "go.mod"), repoWith(t, "go.mod")
	def := pairedCoderDef(root, nil).At(ws)
	a := &agent.Agent{Policy: def.Policy, Handlers: def.Handlers}
	if v := createVerdict(a, filepath.Join(ws, "a.go")); v != agent.VerdictAllow {
		t.Errorf("write in the workspace: %v, want allow", v)
	}
	if v := createVerdict(a, filepath.Join(root, "a.go")); v == agent.VerdictAllow {
		t.Error("a workspace-scoped pair may write in the repository it was built from")
	}
	if !strings.Contains(def.System(), "Working directory: "+ws) || !strings.Contains(def.Peer.Seeded(), "Working directory: "+ws) {
		t.Error("the scoped driver's prompts do not name the workspace as its working directory")
	}
	if !strings.Contains(def.Peer.Planner.System(), "Repository root: "+ws) || def.Peer.Root != ws {
		t.Error("the navigator still plans and diffs against the original repository")
	}
	if err := def.validate(); err != nil {
		t.Errorf("the scoped pair is invalid: %v", err)
	}
}

func createVerdict(a *agent.Agent, path string) agent.Verdict {
	v, _ := a.Decide(agent.Call{ID: "t1", Name: "str_replace_based_edit_tool",
		Input: []byte(`{"command":"create","path":"` + path + `","file_text":"x"}`)})
	return v
}
