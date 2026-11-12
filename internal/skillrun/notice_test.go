package skillrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const ordered = `schema: 2
skill: ordered
version: 1
when: testing progress notices
requires:
  tools: [glob, grep]
nodes:
  look:
    say: Look first.
    evidence:
      - call: { tool: glob }
  write:
    say: Then write.
    evidence:
      - call: { tool: grep }
order:
  - [look, write]
`

const wantNotice = "Skill ordered: `write` counts as done, but `look` comes before it and is still pending. Do it now, then redo `write` if it depended on it."

func answer(text string) agent.Handler {
	return func(_ context.Context, c agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		return anthropic.NewToolResultBlock(c.ID, text, false)
	}
}

// ran answers like bash: text for the model, and exit as how the command ended.
func ran(exit, text string) agent.Handler {
	return func(ctx context.Context, c agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		agent.SetExit(ctx, exit)
		return anthropic.NewToolResultBlock(c.ID, text, false)
	}
}

func uses(names ...string) mockapi.Turn {
	var tools []mockapi.ToolUse
	for i, n := range names {
		tools = append(tools, mockapi.ToolUse{ID: fmt.Sprintf("t%d", i), Name: n, Input: map[string]any{}})
	}
	return mockapi.Turn{Tools: tools}
}

// talking points a at a mock API scripted with turns and returns a function running one user turn.
func talking(t *testing.T, h *Host, a *agent.Agent, turns ...mockapi.Turn) (*mockapi.Server, func(ctx context.Context)) {
	t.Helper()
	t.Setenv("ALUX_RECORDS", "off")
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	server := mockapi.New(turns...)
	t.Cleanup(server.Close)
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	a.Client, a.Model, a.System, a.Standing = &client, "mock", "test", agent.NewRuleInjector("")
	approve := func(context.Context, string, string) bool { return true }
	return server, func(ctx context.Context) {
		res := a.RunTurn(ctx, []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("go"))},
			agent.Callbacks{OnCall: h.Observe, Approve: approve})
		if strings.HasPrefix(res.StopReason, "error") {
			t.Fatal(res.StopReason)
		}
	}
}

type sentContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// lastMessage is the last message of request i to the mock API.
func lastMessage(t *testing.T, server *mockapi.Server, i int) []sentContent {
	t.Helper()
	var sent struct {
		Messages []struct {
			Content []sentContent `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(server.Requests[i], &sent); err != nil {
		t.Fatal(err)
	}
	return sent.Messages[len(sent.Messages)-1].Content
}

// noticeIn is the text of the skill-notice block in request i's last message, or "".
func noticeIn(t *testing.T, server *mockapi.Server, i int) string {
	t.Helper()
	for _, c := range lastMessage(t, server, i) {
		if _, rest, ok := strings.Cut(c.Text, "<skill-notice "); ok {
			_, body, _ := strings.Cut(rest, ">\n")
			return strings.TrimSuffix(body, "\n</skill-notice>")
		}
	}
	return ""
}

func TestANodeDoneAheadOfAPendingPredecessorIsToldOncePerPair(t *testing.T) {
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("- one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paired := fmt.Sprintf(`schema: 2
skill: paired
version: 1
when: testing progress notices
requires:
  tools: [glob, grep]
nodes:
  p:
    say: Look first.
    evidence:
      - call: { tool: glob }
  q:
    say: Keep notes.
    evidence:
      - artifact_lines: { path: %q, pattern: "^- ", min: 1 }
  write:
    say: Then write.
    evidence:
      - call: { tool: grep }
order:
  - [p, write]
  - [q, write]
`, notes)
	h, a := host(t, map[string]string{"paired": paired}, "glob", "grep")
	a.Handlers["glob"] = answer("found")
	a.Handlers["grep"] = answer("nothing")
	server, turn := talking(t, h, a, uses("grep"), mockapi.Turn{Text: "ok"}, uses("grep"), mockapi.Turn{Text: "ok"}, uses("glob"), mockapi.Turn{Text: "ok"})
	if _, err := h.StartText("paired", ""); err != nil {
		t.Fatal(err)
	}
	turn(t.Context())
	want := "Skill paired: `write` counts as done, but `p` comes before it and is still pending. Do it now, then redo `write` if it depended on it."
	if got := noticeIn(t, server, 1); got != want {
		t.Fatalf("the turn that did write ahead of p ended with %q, want %q", got, want)
	}
	turn(t.Context())
	if got := noticeIn(t, server, 3); got != "" {
		t.Fatalf("a later turn repeated the sent pair: %q", got)
	}
	if err := os.WriteFile(notes, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	turn(t.Context())
	want = "Skill paired: `write` counts as done, but `q` comes before it and is still pending. Do it now, then redo `write` if it depended on it."
	if got := noticeIn(t, server, 5); got != want {
		t.Fatalf("write falling behind q ended the turn with %q, want %q", got, want)
	}
}

func TestANoticeComputedInACancelledTurnIsSentInTheNext(t *testing.T) {
	h, a := host(t, map[string]string{"ordered": ordered}, "glob", "grep")
	ctx, cancel := context.WithCancel(t.Context())
	a.Handlers["grep"] = func(_ context.Context, c agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		cancel()
		return anthropic.NewToolResultBlock(c.ID, "nothing", false)
	}
	server, turn := talking(t, h, a, uses("grep"), uses("grep"), mockapi.Turn{Text: "ok"})
	if _, err := h.StartText("ordered", ""); err != nil {
		t.Fatal(err)
	}
	turn(ctx)
	if n := len(server.Requests); n != 1 {
		t.Fatalf("the cancelled turn made %d requests, want 1", n)
	}
	turn(t.Context())
	if got := noticeIn(t, server, 2); got != wantNotice {
		t.Fatalf("the turn after the cancelled one ended with %q, want %q", got, wantNotice)
	}
}

func TestAPredecessorDoneLaterInTheSameTurnGivesNoNotice(t *testing.T) {
	h, a := host(t, map[string]string{"ordered": ordered}, "glob", "grep")
	a.Handlers["glob"] = answer("found")
	a.Handlers["grep"] = answer("nothing")
	server, turn := talking(t, h, a, uses("grep", "glob"), mockapi.Turn{Text: "ok"})
	if _, err := h.StartText("ordered", ""); err != nil {
		t.Fatal(err)
	}
	turn(t.Context())
	if got := noticeIn(t, server, 1); got != "" {
		t.Fatalf("a turn that ends with look and write both done sent %q", got)
	}
}

func TestTheLoopSendsANoticeAsANonceMarkedBlockAfterTheToolResults(t *testing.T) {
	h, a := host(t, map[string]string{"ordered": ordered}, "glob", "grep")
	a.Handlers["grep"] = answer("nothing")
	server, turn := talking(t, h, a, uses("grep"), mockapi.Turn{Text: "done"})
	if _, err := h.StartText("ordered", ""); err != nil {
		t.Fatal(err)
	}
	turn(t.Context())
	last := lastMessage(t, server, 1)
	if len(last) != 2 || last[0].Type != "tool_result" || last[1].Type != "text" {
		t.Fatalf("want the tool result then one text block, got %+v", last)
	}
	after := last[1].Text
	nonce, rest, ok := strings.Cut(strings.TrimPrefix(after, `<skill-notice from="harness" nonce="`), `">`)
	if !strings.HasPrefix(after, `<skill-notice from="harness" nonce="`) || !ok || nonce == "" {
		t.Fatalf("the block after the results is not nonce-marked:\n%s", after)
	}
	if rest != "\n"+wantNotice+"\n</skill-notice>" {
		t.Fatalf("the notice block reads:\n%s", after)
	}
	inside := last[0].Content[0].Text
	if strings.Contains(inside, "counts as done") || strings.Contains(inside, nonce) {
		t.Fatalf("the notice or its nonce is inside the tool result:\n%s", inside)
	}
}
