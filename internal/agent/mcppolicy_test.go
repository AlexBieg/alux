package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/anthropics/anthropic-sdk-go"
)

// A server with a read tool and a write tool, each labelled as the real ones are: the reads
// declare readOnlyHint, the writes say nothing or say false.
func linearish(t *testing.T) *mcpclient.Manager {
	t.Helper()
	m := mcpclient.New(nil)
	m.AddForTest("linear", "list_issues", true)
	m.AddForTest("linear", "get_issue", true)
	m.AddForTest("linear", "update_issue", false)
	m.AddForTest("linear", "create_comment", false)
	return m
}

func readOnlyLinear() *Rules {
	return &Rules{Tools: []Rule{
		{Pattern: "mcp__linear__list_", Verdict: VerdictAllow},
		{Pattern: "mcp__linear__get_", Verdict: VerdictAllow},
	}}
}

func names(tools []anthropic.ToolUnionParam) []string {
	var out []string
	for _, tool := range tools {
		if tool.OfTool != nil {
			out = append(out, tool.OfTool.Name)
		}
	}
	return out
}

// The case this was built for: an agent that may read a ticket and may not change it. MCP tool
// names are self-describing enough that a prefix rule per read family does the whole job.
func TestMcpReadsAreAllowedAndWritesAreNot(t *testing.T) {
	mcp, rules := linearish(t), readOnlyLinear()
	a := &Agent{Policy: rules, Fallback: McpHandler(mcp)}

	for _, name := range []string{"mcp__linear__list_issues", "mcp__linear__get_issue"} {
		if v, reason := a.Decide(Call{ID: "t1", Name: name}); v != VerdictAllow {
			t.Errorf("%s: got %v (%s), want allow", name, v, reason)
		}
	}
	for _, name := range []string{"mcp__linear__update_issue", "mcp__linear__create_comment"} {
		if v, _ := a.Decide(Call{ID: "t1", Name: name}); v != VerdictDeny {
			t.Errorf("%s: got %v, want deny", name, v)
		}
	}
}

// Refusing the call is not enough. An agent shown a tool will try it, so a tool it may not call
// should not be in its list at all — otherwise every run spends a turn discovering by refusal
// what the list could have said for free.
func TestOnlyPermittedMcpToolsAreOffered(t *testing.T) {
	mcp, rules := linearish(t), readOnlyLinear()

	offered := names(McpTools(mcp, rules))
	want := []string{"mcp__linear__get_issue", "mcp__linear__list_issues"}
	if strings.Join(offered, ",") != strings.Join(want, ",") {
		t.Errorf("offered %v, want %v", offered, want)
	}
}

// The main agent has no policy and must keep every tool it had before any of this existed.
func TestNilRulesOfferEveryMcpTool(t *testing.T) {
	mcp := linearish(t)
	if got, want := len(McpTools(mcp, nil)), 4; got != want {
		t.Errorf("offered %d tools, want all %d", got, want)
	}
}

// An agent with no servers configured gets no tools and no panic, which is every subagent that
// is not the coder.
func TestNoManagerOffersNothing(t *testing.T) {
	if got := McpTools(nil, readOnlyLinear()); got != nil {
		t.Errorf("offered %v, want nothing", names(got))
	}
}

// Ask has to leave the tool in the list. Withholding it would turn "check with me first" into a
// silent no, which is a different rule than the one that was written.
func TestAnAskedForToolIsStillOffered(t *testing.T) {
	mcp := linearish(t)
	rules := &Rules{Tools: []Rule{{Pattern: "mcp__linear__update_", Verdict: VerdictAsk, Reason: "it changes the ticket"}}}

	if offered := names(McpTools(mcp, rules)); len(offered) != 1 || offered[0] != "mcp__linear__update_issue" {
		t.Errorf("offered %v, want the asked-for tool", offered)
	}
}

// The audit is the only thing readOnlyHint is used for, and it must report rather than enforce:
// the hint cannot grant anything, but a server saying "this one writes" about an allowed tool is
// worth telling whoever wrote the rule.
func TestTheAuditReportsAnAllowedToolTheServerCallsAWrite(t *testing.T) {
	mcp := linearish(t)

	// A rule broader than its author meant: the whole server, writes included.
	broad := &Rules{Tools: []Rule{{Pattern: "mcp__linear__", Verdict: VerdictAllow}}}
	found := AuditMcp(mcp, "coder", broad)
	if len(found) != 2 {
		t.Fatalf("audit found %d writes, want 2: %v", len(found), found)
	}
	for _, line := range found {
		if !strings.Contains(line, "coder") {
			t.Errorf("%q does not name the agent", line)
		}
	}
	joined := strings.Join(found, "\n")
	for _, want := range []string{"update_issue", "create_comment"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit does not mention %s:\n%s", want, joined)
		}
	}

	// And the careful rules have nothing to report, so the warning means something when it appears.
	if found := AuditMcp(mcp, "coder", readOnlyLinear()); len(found) != 0 {
		t.Errorf("audit complained about read-only rules: %v", found)
	}
}

// A hint the server never set reads as "may write", so a tool nobody labelled is audited rather
// than waved through. Absence must fail the same way a false would.
func TestAnUnlabelledToolIsAuditedAsAWrite(t *testing.T) {
	mcp := mcpclient.New(nil)
	mcp.AddForTest("mystery", "do_something", false)

	broad := &Rules{Tools: []Rule{{Pattern: "mcp__mystery__", Verdict: VerdictAllow}}}
	if found := AuditMcp(mcp, "coder", broad); len(found) != 1 {
		t.Errorf("audit found %v, want the unlabelled tool reported", found)
	}
}

// The audit never gates. A tool it complains about is still callable, because the hint is
// advisory by specification and the rules are what decide.
func TestTheAuditDoesNotChangeWhatIsAllowed(t *testing.T) {
	mcp := linearish(t)
	broad := &Rules{Tools: []Rule{{Pattern: "mcp__linear__", Verdict: VerdictAllow}}}
	a := &Agent{Policy: broad, Fallback: McpHandler(mcp)}

	if len(AuditMcp(mcp, "coder", broad)) == 0 {
		t.Fatal("expected the audit to have something to say about these rules")
	}
	if v, _ := a.Decide(Call{ID: "t1", Name: "mcp__linear__update_issue"}); v != VerdictAllow {
		t.Error("the audit suppressed a call the rules allow")
	}
	if offered := names(McpTools(mcp, broad)); len(offered) != 4 {
		t.Errorf("the audit removed tools from the list: %v", offered)
	}
}

// The strict handler is what makes "give it Linear" different from "give it a shell". The main
// agent's fallback sends any unrecognised name to bash; a subagent's must not.
func TestTheStrictMcpHandlerRefusesNonMcpNames(t *testing.T) {
	input, err := json.Marshal(map[string]any{"command": "echo pwned"})
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "t1", Name: "shell", Input: input}

	result := McpHandler(nil)(context.Background(), call, Callbacks{
		Approve:      func(context.Context, string, string) bool { return true },
		OnToolResult: func(string, string, string, ToolKind) {},
	})
	if result.OfToolResult == nil || !result.OfToolResult.IsError.Or(false) {
		t.Fatal("a non-MCP name was not refused")
	}

	// And the lenient one still does run it, which is the behaviour the main agent depends on and
	// the reason the two cannot be the same function.
	ran := false
	McpFallback(nil, nil)(context.Background(), call, Callbacks{
		Approve:      func(context.Context, string, string) bool { ran = true; return false },
		OnToolResult: func(string, string, string, ToolKind) {},
	})
	if !ran {
		t.Error("McpFallback stopped sending unknown names to bash")
	}
}

func TestAnUnknownToolNameWithNoCommandIsRefusedByName(t *testing.T) {
	call := Call{ID: "t1", Name: "view", Input: json.RawMessage(`{"path":"a.go","view_range":"[1, 5]"}`)}
	result := McpFallback(nil, func(context.Context, Call, Callbacks) anthropic.ContentBlockParamUnion {
		t.Fatal("a call with no command reached bash")
		return anthropic.ContentBlockParamUnion{}
	})(context.Background(), call, Callbacks{})
	if r := result.OfToolResult; r == nil || !r.IsError.Or(false) || !strings.Contains(r.Content[0].OfText.Text, "No tool named view") {
		t.Errorf("result = %+v, want an error naming the unknown tool", result)
	}
}
