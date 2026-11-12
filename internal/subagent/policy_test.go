package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/anthropics/anthropic-sdk-go"
)

// approveInHandler makes the definition's one tool take an approval before it runs, which is what
// an editing subagent's handlers do and what the test definition otherwise never exercises.
func approveInHandler(def *Definition) {
	inner := def.Handlers["note"]
	def.Handlers["note"] = func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		cb.Approve(ctx, "write a file", "")
		return inner(ctx, call, cb)
	}
}

// A background agent has nobody to ask: its run outlives the turn that dispatched it, so an
// approval raised from it parks on a session whose turn has ended and races whatever the current
// turn puts in the same single slot. That is a hang, not a wrong answer, and the only place it can
// be caught is before anything runs.
func TestABackgroundAgentCannotHaveARuleThatAsks(t *testing.T) {
	def := echo(true)
	def.Policy = &agent.Rules{Bash: []agent.Rule{{Pattern: "jj describe", Verdict: agent.VerdictAsk}}}

	_, err := NewManager(NewRoster(nil), def)
	if err == nil {
		t.Fatal("a background agent that asks was accepted")
	}
	if !strings.Contains(err.Error(), "background") || !strings.Contains(err.Error(), "echo") {
		t.Errorf("error %q does not say which agent or why", err)
	}

	// The same rules on a foreground agent are fine: there the dispatching turn is still live and
	// the mutex queues the prompt.
	fg := echo(false)
	fg.Policy = def.Policy
	if _, err := NewManager(NewRoster(nil), fg); err != nil {
		t.Errorf("a foreground agent that asks was rejected: %v", err)
	}
}

// A Default of Ask reaches the user just as surely as a rule that asks, so it has to be caught by
// the same check. Easy to miss, since it is not in any of the three lists.
func TestABackgroundAgentCannotDefaultToAsking(t *testing.T) {
	def := echo(true)
	def.Policy = &agent.Rules{Default: agent.VerdictAsk}
	if _, err := NewManager(NewRoster(nil), def); err == nil {
		t.Fatal("a background agent defaulting to ask was accepted")
	}
}

// A rule set that does not mean what it says stops alux at boot. Validate's own reasoning
// is tested in the agent package; what matters here is that a Definition is actually run through
// it rather than carrying it unexamined.
func TestAnInvalidRuleSetIsRejectedAtConstruction(t *testing.T) {
	def := echo(false)
	def.Policy = &agent.Rules{Bash: []agent.Rule{
		{Pattern: "jj", Verdict: agent.VerdictAllow},
		{Pattern: "jj git push", Verdict: agent.VerdictDeny},
	}}
	_, err := NewManager(NewRoster(nil), def)
	if err == nil {
		t.Fatal("an evadable deny was accepted")
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Errorf("error %q does not name the agent", err)
	}
}

// Two agents answering to one name means one of them is unreachable, and which one depends on map
// iteration order.
func TestTwoAgentsCannotShareAName(t *testing.T) {
	if _, err := NewManager(NewRoster(nil), echo(false), echo(false)); err == nil {
		t.Fatal("a duplicate name was accepted")
	}
}

// A nil definition is how an agent declines to exist rather than a configuration error.
func TestANilDefinitionIsSkippedNotRejected(t *testing.T) {
	m, err := NewManager(NewRoster(nil), echo(false), nil)
	if err != nil {
		t.Fatalf("a nil definition was rejected: %v", err)
	}
	if len(m.Names()) != 1 {
		t.Errorf("names %v, want just the one real agent", m.Names())
	}
}

// The definition's rules have to reach the loop that runs the tools. Without this the field is
// decoration: every test above would still pass and nothing would be gated.
func TestTheDefinitionsPolicyReachesTheInnerAgent(t *testing.T) {
	def := echo(false)
	def.Policy = &agent.Rules{Tools: agent.AllowTools("nothing-matching")}

	m, server := manager(t, def,
		// The agent asks for its one tool, which its rules do not permit.
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}}},
		mockapi.Turn{Text: "could not"},
	)
	if _, err := m.Dispatch(context.Background(), "echo", "try it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}

	// The refusal has to come back as the tool's result, which is the only way the subagent learns
	// it may not do this. Reading it off the last request the mock received is what proves the gate
	// ran inside the loop rather than somewhere decorative.
	if server.RequestCount() < 2 {
		t.Fatalf("got %d requests, want the refusal to have produced a second", server.RequestCount())
	}
	last, err := json.Marshal(server.LastRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(last), "Refused") {
		t.Errorf("the follow-up request carries no refusal:\n%s", last)
	}
}

// A definition's fallback has to reach the loop, or an MCP tool is unreachable from a subagent
// however carefully its rules were written. The field being set is not the property — the loop
// installing it is — and this is the same class of bug as a policy that never reaches the agent
// that runs the tools.
func TestTheDefinitionsFallbackReachesTheInnerAgent(t *testing.T) {
	def := echo(false)
	reached := make(chan string, 1)
	def.Fallback = func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		reached <- call.Name
		return anthropic.NewToolResultBlock(call.ID, "handled", false)
	}

	m, _ := manager(t, def,
		// A name no handler in the table knows, which is how every MCP tool arrives.
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "mcp__linear__list_issues", Input: map[string]any{}}}},
		mockapi.Turn{Text: "done"},
	)
	if _, err := m.Dispatch(context.Background(), "echo", "look it up", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}

	select {
	case name := <-reached:
		if name != "mcp__linear__list_issues" {
			t.Errorf("the fallback got %q", name)
		}
	default:
		t.Fatal("the fallback was never called, so the definition's fallback is not wired into the loop")
	}
}

// A definition with no fallback must not inherit one. Nil is what every read-only subagent
// carries, and an unmatched name there has to error rather than find a shell.
func TestAnAgentWithoutAFallbackCannotReachOne(t *testing.T) {
	def := echo(false)
	m, server := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo pwned"}}}},
		mockapi.Turn{Text: "could not"},
	)
	if _, err := m.Dispatch(context.Background(), "echo", "try it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}

	last, err := json.Marshal(server.LastRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(last), "no handler for tool bash") {
		t.Errorf("an unhandled name did not error; it may have run:\n%s", last)
	}
}

// An approval raised by a subagent must say which agent wants it. In a pane running three agents
// an unattributed prompt asks the user to approve a write without saying whose it is.
func TestASubagentsApprovalNamesTheAgent(t *testing.T) {
	def := echo(false)
	m, _ := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}}},
		mockapi.Turn{Text: "done"},
	)

	// The handler approves, which the echo definition's does not do by itself — so this drives the
	// callback the manager built rather than the one the parent passed.
	approveInHandler(def)

	var labels []string
	var mu sync.Mutex
	_, err := m.Dispatch(context.Background(), "echo", "try it", "", agent.Callbacks{
		Approve: func(_ context.Context, label, _ string) bool {
			mu.Lock()
			labels = append(labels, label)
			mu.Unlock()
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(labels) == 0 {
		t.Fatal("the approval never reached the parent")
	}
	for _, label := range labels {
		if !strings.Contains(label, "echo") {
			t.Errorf("label %q does not name the agent that wants it", label)
		}
	}
}

// A run parked on the user is shown as waiting, which is what the dot row and /agents read to
// distinguish "stopped on you" from "working". Waiting was defined for this and nothing produced
// it until now.
func TestARunParkedOnAnApprovalIsShownAsWaiting(t *testing.T) {
	def := echo(false)
	m, _ := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}}},
		mockapi.Turn{Text: "done"},
	)

	approveInHandler(def)

	seen := make(chan State, 1)
	_, err := m.Dispatch(context.Background(), "echo", "try it", "", agent.Callbacks{
		Approve: func(context.Context, string, string) bool {
			// Read from the roster while the prompt is up, which is the only moment the state is
			// observable.
			for _, run := range m.Roster.Runs() {
				if run.Agent == "echo" {
					select {
					case seen <- run.State:
					default:
					}
				}
			}
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case state := <-seen:
		if state != Waiting {
			t.Errorf("state while parked was %q, want %q", state, Waiting)
		}
	default:
		t.Fatal("the approval never fired")
	}

	// And it goes back to running, so a granted approval does not leave the run looking stuck.
	for _, run := range m.Roster.Runs() {
		if run.Agent == "echo" && run.State == Waiting {
			t.Error("the run was left waiting after the approval was answered")
		}
	}
}

// The names an agent acts through. The editor is on both sides of the separation below, which is
// why it is not enough to find it in a tool list.
const (
	editorTool     = "str_replace_based_edit_tool"
	backgroundTool = "run_background"
)

// webSettings puts a search key in a home of its own, so the web tools are genuinely offered and
// the rule below is checked against a definition that holds them rather than against an absence.
func webSettings(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, paths.Config)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"web": {"provider": "brave", "key": "test-key"}}`)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// editorWrites asks the definition's editor to create a file and reports whether it did. Research
// and the coders offer that tool under the same name and wire it to different handlers — one that
// refuses everything but view, one that writes — so trying it is the only honest way to tell which
// of the two an agent is holding.
func editorWrites(t *testing.T, def *Definition) bool {
	t.Helper()
	handler := def.Handlers[editorTool]
	if handler == nil {
		return false
	}
	path := filepath.Join(t.TempDir(), "written.txt")
	in, err := json.Marshal(map[string]any{"command": "create", "path": path, "file_text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	handler(context.Background(), agent.Call{ID: "t1", Name: editorTool, Input: in},
		agent.Callbacks{Approve: func(context.Context, string, string) bool { return true }})
	_, err = os.Stat(path)
	return err == nil
}

// No subagent may hold both a web tool and an unattended way to act. A fetched page is
// attacker-controlled text, and an agent that can run a command or write a file on the strength of
// what it just read needs no exploit beyond the page itself — nobody is watching a detached run,
// so there is no approval between the two. Research reads the web and cannot act; the coders act
// and cannot reach the web. The test exists to fail when that drifts: web_fetch added to the
// coder, or research's shell allowed more than reading.
//
// A shell counts as reading when guardian may pass only reads and a parser policy sits in front of it.
// That is a judgement, not a guarantee: guardian passes option escapes such as `rg --pre=sh`.
//
// The main agent is out of scope on purpose. It holds both, and what stands between a page and a
// command there is the approval channel, which is a person.
func TestNoSubagentHoldsBothAWebToolAndAWayToAct(t *testing.T) {
	webSettings(t)
	// The roster main.go registers, with the roots supplied so every definition exists whatever
	// directory the test runs in.
	root := t.TempDir()
	defs := []*Definition{Research(), coderDef(root, nil), pairedCoderDef(root, nil), Verifier()}

	offered := 0
	for _, def := range defs {
		var reading, acting []string
		for _, tool := range def.Tools() {
			name := ""
			// The editor and bash are server-side tool types rather than plain tools, so the name
			// is read off whichever variant the union holds.
			if n := tool.GetName(); n != nil {
				name = *n
			}
			switch {
			case strings.HasPrefix(name, "web_"):
				reading = append(reading, name)
			case name == "bash" || name == backgroundTool:
				if !readOnlyShellOf(def) {
					acting = append(acting, name)
				}
			case name == editorTool && editorWrites(t, def):
				acting = append(acting, name+" with write commands")
			}
		}
		if len(reading) > 0 {
			offered++
		}
		if len(reading) > 0 && len(acting) > 0 {
			t.Errorf("agent %s holds the web tools %v and can act with %v. No agent may hold both: a page it "+
				"fetched is attacker-controlled text, and nobody is watching a detached run to refuse what the "+
				"page asks for. Give the reading to research and the acting to a coder, and let them report to "+
				"each other.", def.Name, reading, acting)
		}
	}
	if offered == 0 {
		t.Fatal("no agent was offered a web tool, so this proved nothing: either the tools stopped being " +
			"configurable from the settings file, or the list of definitions has gone stale")
	}
}

func readOnlyShellOf(def *Definition) bool {
	c := def.Commands
	return def.Policy != nil && c != nil && len(c.Allowed) == 1 && c.Allowed[0] == guardian.Read
}

func TestTheReadOnlyShellRefusesWhatTheParserCannotVouchFor(t *testing.T) {
	a := &agent.Agent{Policy: readOnlyShell("research"), Handlers: readOnlyHandlers()}
	bash := func(command string) agent.Call {
		in, _ := json.Marshal(map[string]string{"command": command})
		return agent.Call{ID: "c", Name: "bash", Input: in}
	}
	for _, command := range []string{"rg -n Threshold internal/", "git log --oneline | head -5", "gh pr view 412"} {
		if v, why := a.Decide(bash(command)); v != agent.VerdictAllow {
			t.Errorf("%q was refused: %s", command, why)
		}
	}
	for _, command := range []string{
		"git log > notes.txt", "cat $(ls)", "sh -c id", "rg -l x | xargs cat", "env FOO=1 ls",
		"rm notes.txt", "rm /tmp/x", "cd internal && ls",
	} {
		if v, _ := a.Decide(bash(command)); v == agent.VerdictAllow {
			t.Errorf("%q was allowed", command)
		}
	}
	if a.Policy.Permits(agent.McpPrefix + "linear__create_issue") {
		t.Error("the read-only shell's rules admit an MCP tool")
	}
}

// The catalogue is where a capability gate becomes checkable by the person who configured it.
func TestTheCatalogueStatesWhatEachAgentMayDo(t *testing.T) {
	bounded := echo(false)
	bounded.Name, bounded.Commands = "coder", lookOnly("coder")
	bounded.Policy = &agent.Rules{
		Bash:  []agent.Rule{{Pattern: "go test", Verdict: agent.VerdictAllow}, {Pattern: "jj describe", Verdict: agent.VerdictAsk}},
		Tools: agent.AllowTools("glob"),
	}
	m, err := NewManager(NewRoster(nil), bounded, echo(false))
	if err != nil {
		t.Fatal(err)
	}

	out := m.Catalogue()
	for _, want := range []string{"coder", "go test", "asks for jj describe", "glob", "limited by its tools alone"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalogue does not mention %q:\n%s", want, out)
		}
	}
}
