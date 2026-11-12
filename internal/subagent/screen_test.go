package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
)

// Every agent alux ships, shell or not, says what its work involves, under its own name.
func TestEveryShippedAgentDeclaresItsOwnCommandCriteria(t *testing.T) {
	root := t.TempDir()
	defs := []*Definition{Research(), coderDef(root, nil), pairedCoderDef(root, nil), Reviewer(nil, nil), Migrator(), Verifier()}
	for _, def := range defs {
		if def == nil {
			continue
		}
		if err := def.validateCommands(); err != nil {
			t.Errorf("%s: %v", def.Name, err)
		}
		if def.Peer != nil {
			for _, peer := range []*Definition{def.Peer.Def, def.Peer.Planner} {
				if err := peer.validateCommands(); err != nil {
					t.Errorf("%s's peer: %v", def.Name, err)
				}
			}
		}
	}
	for _, name := range []string{"coder", "pair"} {
		for _, def := range defs {
			if def != nil && def.Name == name && slicesHas(def.Commands.Allowed, guardian.Record) {
				t.Errorf("%s may record work, which is the user's call", name)
			}
		}
	}
}

func slicesHas(cats []guardian.Category, c guardian.Category) bool {
	for _, x := range cats {
		if x == c {
			return true
		}
	}
	return false
}

func TestAnAgentWithoutCriteriaStopsTheHarness(t *testing.T) {
	missing := echo(false)
	missing.Commands = nil
	if _, err := NewManager(NewRoster(nil), missing); err == nil || !strings.Contains(err.Error(), "no command criteria") {
		t.Errorf("an agent without criteria was registered: %v", err)
	}
	borrowed := echo(false)
	borrowed.Name = "other"
	if _, err := NewManager(NewRoster(nil), borrowed); err == nil {
		t.Error("an agent carrying another agent's criteria was registered")
	}
	unpeered := paired()
	unpeered.Peer.Def.Commands = nil
	if _, err := NewManager(NewRoster(nil), unpeered); err == nil {
		t.Error("a peer without criteria was registered through its driver")
	}
}

// shellAgent is echo with a bash stand-in, judged by a real gate against a model stand-in.
func shellAgent(background bool) *Definition {
	def := echo(background)
	def.Name = "shell"
	def.Commands = &guardian.Criteria{Agent: "shell", Allowed: []guardian.Category{guardian.Read, guardian.Build, guardian.Edit}, Threshold: 0.32}
	def.Handlers = map[string]agent.Handler{"bash": func(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		cb.OnToolResult(call.ID, "$ ran", "SHELL-RAN", agent.KindBash)
		return anthropic.NewToolResultBlock(call.ID, "SHELL-RAN", false)
	}}
	return def
}

// read build edit record rewrite destroy network system
var pushes = []float64{0.01, 0.02, 0.01, 0.02, 0.01, 0.0, 0.92, 0.01}

func judgedManager(t *testing.T, def *Definition) (*Manager, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := make([]string, len(pushes))
		for i, p := range pushes {
			parts[i] = fmt.Sprintf("%q: %g", fmt.Sprint(i), p)
		}
		fmt.Fprintf(w, `{"answers": {"does": {"probabilities": {%s}}}}`, strings.Join(parts, ", "))
	}))
	t.Cleanup(srv.Close)
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	m, _ := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "git push"}}}},
		mockapi.Turn{Text: "done"},
	)
	m.screener = nil
	m.Guardian = guardian.New(guardian.Conf{URL: srv.URL, Model: "d3:2b", Log: log}, func(string) {})
	return m, log
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastLogged(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no verdict log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var e map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// A background run has nobody to ask, so a command outside its work is refused, not run.
func TestABackgroundSubagentsCommandOutsideItsWorkIsRefused(t *testing.T) {
	m, log := judgedManager(t, shellAgent(true))
	if _, err := m.Dispatch(context.Background(), "shell", "push it", "", agent.Callbacks{
		Approve: func(context.Context, string, string) bool { return true },
		Require: func(context.Context, string, string, string) bool {
			t.Error("a background run asked the user")
			return true
		},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(log); return err == nil })
	e := lastLogged(t, log)
	if e["agent"] != "shell" || e["decision"] != "refused" || e["human"] != nil || e["top"] != "network" {
		t.Errorf("background refusal logged as %v", e)
	}
}

// A foreground run asks the user, under the agent's name, even though Approve would say yes.
func TestAForegroundSubagentsCommandOutsideItsWorkGoesToTheUser(t *testing.T) {
	m, log := judgedManager(t, shellAgent(false))
	var asked []string
	if _, err := m.Dispatch(context.Background(), "shell", "push it", "", agent.Callbacks{
		Approve: func(context.Context, string, string) bool { return true },
		Require: func(_ context.Context, label, _, reason string) bool {
			asked = append(asked, label+" | "+reason)
			return false
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "agent shell — $ git push") || !strings.Contains(asked[0], "network") {
		t.Errorf("asked %v", asked)
	}
	if e := lastLogged(t, log); e["decision"] != "declined" || e["human"] != "unsafe" || e["misclassified"] != false {
		t.Errorf("foreground decline logged as %v", e)
	}
}
