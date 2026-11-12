package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// recorder collects everything a turn pushed at the pane.
type recorder struct {
	mu        sync.Mutex
	text      strings.Builder
	thinking  strings.Builder
	ends      int
	results   []string
	kinds     []ToolKind
	approvals []string
	bodies    []string
	usage     []Usage

	approve func(string) bool
	answer  *ask.Answer
	// answers is a series' worth of replies, consumed in order; running out stands for the user
	// backing out of the rest. questions records what each one was asked.
	answers   []*ask.Answer
	questions []ask.Question
}

func (r *recorder) reply(q ask.Question) *ask.Answer {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.questions = append(r.questions, q)
	if r.answers == nil {
		return r.answer
	}
	if len(r.questions) > len(r.answers) {
		return nil
	}
	return r.answers[len(r.questions)-1]
}

func (r *recorder) callbacks() Callbacks {
	return Callbacks{
		OnText:        func(d string) { r.mu.Lock(); r.text.WriteString(d); r.mu.Unlock() },
		OnThinking:    func(d string) { r.mu.Lock(); r.thinking.WriteString(d); r.mu.Unlock() },
		OnThinkingEnd: func() { r.mu.Lock(); r.ends++; r.mu.Unlock() },
		OnUsage:       func(u Usage) { r.mu.Lock(); r.usage = append(r.usage, u); r.mu.Unlock() },
		Approve: func(_ context.Context, label, body string) bool {
			r.mu.Lock()
			r.approvals = append(r.approvals, label)
			r.bodies = append(r.bodies, body)
			r.mu.Unlock()
			if r.approve != nil {
				return r.approve(label)
			}
			return true
		},
		Ask: func(_ context.Context, q ask.Question) *ask.Answer { return r.reply(q) },
		OnToolResult: func(_, label, output string, kind ToolKind) {
			r.mu.Lock()
			r.results = append(r.results, label+"\n"+output)
			r.kinds = append(r.kinds, kind)
			r.mu.Unlock()
		},
	}
}

func testAgent(t *testing.T, turns ...mockapi.Turn) (*Agent, *mockapi.Server) {
	t.Helper()
	server := mockapi.New(turns...)
	t.Cleanup(server.Close)
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	return &Agent{
		Client:   &client,
		Model:    "mock",
		System:   "test",
		Standing: NewRuleInjector(""),
		Tools:    BuiltinTools,
		Handlers: BuiltinHandlers(),
		Fallback: RunBash,
		Screen:   &fakeScreen{verdict: passing},
	}, server
}

func user(text string) []anthropic.MessageParam {
	return []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(text))}
}

func TestPlainReplyStreamsAndStops(t *testing.T) {
	a, _ := testAgent(t, mockapi.Turn{Text: "hello there"})
	r := &recorder{}

	res := a.RunTurn(context.Background(), user("hi"), r.callbacks())

	if res.StopReason != "end_turn" {
		t.Errorf("stop reason = %q", res.StopReason)
	}
	if got := r.text.String(); got != "hello there" {
		t.Errorf("streamed text = %q", got)
	}
	if res.Text != "hello there" {
		t.Errorf("result text = %q", res.Text)
	}
}

func TestAPausedTurnIsResentAsIsAndCarriesOn(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Text: "Searching.", Stop: "pause_turn"},
		mockapi.Turn{Text: "done"},
	)

	res := a.RunTurn(context.Background(), user("hi"), (&recorder{}).callbacks())

	if res.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", res.StopReason)
	}
	if len(server.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(server.Requests))
	}
	var sent struct {
		Messages []struct{ Role string } `json:"messages"`
	}
	if err := json.Unmarshal(server.Requests[1], &sent); err != nil {
		t.Fatal(err)
	}
	if n := len(sent.Messages); n != 2 || sent.Messages[n-1].Role != "assistant" {
		t.Errorf("resumed request should end on the paused assistant reply, got %+v", sent.Messages)
	}
}

// A reply cut off at max_tokens partway through a tool call must not leave that call in history:
// it has no result, so every request after it would be rejected until the conversation is cleared.
func TestAToolCallCutOffAtMaxTokensDoesNotPoisonHistory(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	a, server := testAgent(t,
		mockapi.Turn{
			Thinking: "planning",
			Text:     "Writing it now.",
			Tools:    []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "touch " + marker}}},
			Stop:     "max_tokens",
		},
		mockapi.Turn{Text: "picked up the background result"},
		mockapi.Turn{Text: "answering the next prompt"},
	)
	landed := false
	a.Pending = func() string {
		if landed {
			return ""
		}
		landed = true
		return "BACKGROUND-RESULT"
	}

	res := a.RunTurn(context.Background(), user("write the file"), (&recorder{}).callbacks())
	if res.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	next := append(res.History, anthropic.NewUserMessage(anthropic.NewTextBlock("and now?")))
	a.RunTurn(context.Background(), next, (&recorder{}).callbacks())

	if _, err := os.Stat(marker); err == nil {
		t.Error("the cut-off tool call ran")
	}
	if len(server.Requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(server.Requests))
	}
	for i, body := range server.Requests[1:] {
		if strings.Contains(string(body), `"tool_use"`) {
			t.Errorf("request %d replayed the cut-off tool call:\n%s", i+2, body)
		}
		if !strings.Contains(string(body), "Writing it now.") || !strings.Contains(string(body), "never ran") {
			t.Errorf("request %d lost what the model said or that its call did not run:\n%s", i+2, body)
		}
	}
}

// Two ways to get this wrong: ignoring cache_read_input_tokens, which is where caching puts most
// of a long conversation, and summing the requests in a tool-using turn, which double-counts it.
func TestContextUsedCountsCachedTokensAndDoesNotAccumulate(t *testing.T) {
	a, _ := testAgent(t,
		mockapi.Turn{
			Usage: mockapi.Usage{InputTokens: 9_000, OutputTokens: 1_000},
			Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo hi"}}},
		},
		mockapi.Turn{
			Usage: mockapi.Usage{InputTokens: 2_000, CacheRead: 48_000, OutputTokens: 500},
			Text:  "done",
		},
	)
	a.Window = 200_000
	r := &recorder{}

	res := a.RunTurn(context.Background(), user("go"), r.callbacks())

	if got, want := res.Usage.Total(), int64(50_500); got != want {
		t.Errorf("context used = %d, want %d (the last request's prompt plus its output)", got, want)
	}
	if got, want := res.Usage.Fraction(), 0.2525; got < want-0.001 || got > want+0.001 {
		t.Errorf("fraction of the window = %v, want %v", got, want)
	}

	// One report per request, so the indicator moves during a long turn.
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.usage) != 2 {
		t.Fatalf("usage reports = %d, want one per request", len(r.usage))
	}
	if got, want := r.usage[0].Total(), int64(10_000); got != want {
		t.Errorf("first report = %d, want %d", got, want)
	}
}

// Thinking has to close on the block boundary, not when reply text starts arriving, or a thought
// followed by a tool call never ends and the box streams forever.
func TestThinkingEndsOnItsBlockNotOnTheReply(t *testing.T) {
	a, _ := testAgent(t, mockapi.Turn{
		Thinking: "weighing it up",
		Tools:    []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo hi"}}},
	}, mockapi.Turn{Text: "done"})
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	if got := r.thinking.String(); got != "weighing it up" {
		t.Errorf("thinking = %q", got)
	}
	if r.ends != 1 {
		t.Errorf("OnThinkingEnd fired %d times, want 1", r.ends)
	}
}

// The requirement that usually breaks thinking plus tools: the assistant message has to go back
// verbatim, thinking block and signature included, or the next request is rejected.
func TestThinkingBlocksAreReplayedWithTheirSignature(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Thinking: "hmm", Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "true"}}}},
		mockapi.Turn{Text: "done"},
	)
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	if server.RequestCount() != 2 {
		t.Fatalf("model was called %d times, want 2", server.RequestCount())
	}
	body, _ := json.Marshal(server.LastRequest()["messages"])
	if !strings.Contains(string(body), "mock-signature") {
		t.Errorf("second request dropped the thinking signature: %s", body)
	}
	if !strings.Contains(string(body), `"thinking"`) {
		t.Errorf("second request dropped the thinking block: %s", body)
	}
}

// Both halves matter: the field without the header is a 400, the header without the field clears
// nothing.
func TestEveryRequestAsksTheServerToClearOldThinking(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{Thinking: "hmm", Text: "done"})
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	if beta := server.LastHeaderValues("anthropic-beta"); !slices.Contains(beta, contextManagementBeta) {
		t.Errorf("anthropic-beta = %v, want it to carry %q — the field is rejected without it",
			beta, contextManagementBeta)
	}

	edit := editOfType(t, server, "clear_thinking_20251015")
	keep, _ := edit["keep"].(map[string]any)
	if keep["type"] != "thinking_turns" {
		t.Errorf("keep type = %v, want thinking_turns", keep["type"])
	}
	// JSON numbers decode as float64.
	if keep["value"] != float64(keepThinkingTurns) {
		t.Errorf("keep value = %v, want %d", keep["value"], keepThinkingTurns)
	}
}

func TestEveryRequestAsksForPromptCaching(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{Text: "done"})
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	cc, _ := server.LastRequest()["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Errorf("cache_control = %v, want type ephemeral — without it every request bills full input price",
			server.LastRequest()["cache_control"])
	}
}

// Tool-result clearing is off for the main agent on purpose, and silently so — an edit that leaked
// onto it would break nothing any other test can see, it would just start quietly deleting the
// agent's memory of what it owes. Hence an assertion on the exact set for an agent that has not
// opted in: no clear_tool_uses_20250919, and nothing else crept in beside it.
func TestAPlainAgentNeverAsksTheServerToClearToolResults(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{Text: "done"}, mockapi.Turn{Text: "report"})
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	want := []string{"clear_thinking_20251015"}
	if types := editTypes(t, server); !slices.Equal(types, want) {
		t.Errorf("edits = %v, want exactly %v — no tool result may be cleared", types, want)
	}

	a.Finalize(context.Background(), user("go"), "report")
	if cm, sent := server.LastRequest()["context_management"]; sent {
		t.Errorf("a plain agent's Finalize sent context_management %v", cm)
	}
}

func editsSent(t *testing.T, server *mockapi.Server) []any {
	t.Helper()
	cm, ok := server.LastRequest()["context_management"].(map[string]any)
	if !ok {
		t.Fatalf("the request carried no context_management: %v", server.LastRequest()["context_management"])
	}
	edits, _ := cm["edits"].([]any)
	return edits
}

func editOfType(t *testing.T, server *mockapi.Server, want string) map[string]any {
	t.Helper()
	for _, e := range editsSent(t, server) {
		if edit, _ := e.(map[string]any); edit["type"] == want {
			return edit
		}
	}
	t.Fatalf("no %s edit in %v", want, editsSent(t, server))
	return nil
}

func editTypes(t *testing.T, server *mockapi.Server) []string {
	t.Helper()
	var types []string
	for _, e := range editsSent(t, server) {
		edit, _ := e.(map[string]any)
		types = append(types, fmt.Sprint(edit["type"]))
	}
	return types
}

// The order is the API's, which 400s on any other and which the mock does not enforce; the
// trigger is a share of whatever window the agent has, so a 1M window does not clear at 200k.
func TestAnOptedInAgentClearsToolResultsLateInItsOwnWindow(t *testing.T) {
	for _, tc := range []struct {
		window, trigger, atLeast int64
	}{{200_000, 120_000, 10_000}, {1_000_000, 600_000, 50_000}} {
		a, server := testAgent(t, mockapi.Turn{Text: "done"})
		a.Window = tc.window
		a.ClearToolResults = true
		a.ExcludeTools = []string{"ask_main", "consult"}

		a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

		want := []string{"clear_thinking_20251015", "clear_tool_uses_20250919"}
		if got := editTypes(t, server); !slices.Equal(got, want) {
			t.Fatalf("window %d: edits = %v, want %v in that order", tc.window, got, want)
		}
		edit := editOfType(t, server, "clear_tool_uses_20250919")
		trigger, _ := edit["trigger"].(map[string]any)
		if trigger["type"] != "input_tokens" || trigger["value"] != float64(tc.trigger) {
			t.Errorf("window %d: trigger = %v, want %d input tokens", tc.window, trigger, tc.trigger)
		}
		atLeast, _ := edit["clear_at_least"].(map[string]any)
		if atLeast["type"] != "input_tokens" || atLeast["value"] != float64(tc.atLeast) {
			t.Errorf("window %d: clear_at_least = %v, want %d input tokens", tc.window, atLeast, tc.atLeast)
		}
		keep, _ := edit["keep"].(map[string]any)
		if keep["type"] != "tool_uses" || keep["value"] != float64(keepToolUses) {
			t.Errorf("window %d: keep = %v, want %d tool uses", tc.window, keep, keepToolUses)
		}
		if got := fmt.Sprint(edit["exclude_tools"]); got != "[ask_main consult]" {
			t.Errorf("window %d: exclude_tools = %s, want [ask_main consult]", tc.window, got)
		}
	}
}

// A history that fits only because the server clears it overflows a closing request that does not.
func TestFinalizeOnAnOptedInAgentSendsTheSameEdits(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{Text: "Still unfinished: the rest"})
	a.Window = 200_000
	a.ClearToolResults = true
	a.ExcludeTools = []string{"ask_main"}

	if said := a.Finalize(context.Background(), user("go"), "report"); said != "Still unfinished: the rest" {
		t.Fatalf("Finalize said %q", said)
	}
	if beta := server.LastHeaderValues("anthropic-beta"); !slices.Contains(beta, contextManagementBeta) {
		t.Errorf("anthropic-beta = %v, want it to carry %q", beta, contextManagementBeta)
	}
	want := []string{"clear_thinking_20251015", "clear_tool_uses_20250919"}
	if got := editTypes(t, server); !slices.Equal(got, want) {
		t.Errorf("edits = %v, want %v", got, want)
	}
}

// Checked before the next request, so a window the server could not clear down is never sent again.
func TestAnOptedInAgentStopsWhenItsWindowIsFull(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{
			Usage: mockapi.Usage{InputTokens: 185_000, OutputTokens: 1_000},
			Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "true"}}},
		},
		mockapi.Turn{Text: "never sent"},
	)
	a.Window = 200_000
	a.ClearToolResults = true

	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if res.StopReason != ContextFull {
		t.Errorf("stop reason = %q, want %q", res.StopReason, ContextFull)
	}
	if n := server.RequestCount(); n != 1 {
		t.Errorf("requests = %d, want 1: a full window must not be sent again", n)
	}
}

// Reading a file has to be no more friction than grep, which never prompts — while a write still
// has to stop for one.
func TestViewingSkipsApprovalButWritingDoesNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{
			{ID: "t1", Name: "str_replace_based_edit_tool", Input: map[string]any{"command": "view", "path": path}},
			{ID: "t2", Name: "str_replace_based_edit_tool", Input: map[string]any{
				"command": "str_replace", "path": path, "old_str": "before", "new_str": "after",
			}},
		}},
		mockapi.Turn{Text: "done"},
	)
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	if len(r.approvals) != 1 || !strings.HasPrefix(r.approvals[0], "edit ") {
		t.Errorf("approvals = %v, want the write alone", r.approvals)
	}
	if len(r.results) != 2 {
		t.Errorf("both calls should have run, got %d results", len(r.results))
	}
}

// The prompt can only show what the handler hands it, so an edit's diff has to reach Approve or
// the user is asked to approve a path and nothing more.
func TestEditApprovalCarriesTheDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "str_replace_based_edit_tool", Input: map[string]any{
			"command": "str_replace", "path": path, "old_str": "before", "new_str": "after",
		}}}},
		mockapi.Turn{Text: "done"},
	)
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	if len(r.bodies) != 1 {
		t.Fatalf("approvals = %v", r.approvals)
	}
	if !strings.Contains(r.bodies[0], "- before") || !strings.Contains(r.bodies[0], "+ after") {
		t.Errorf("approval body = %q", r.bodies[0])
	}
}

// A series is one tool call: the model asks for several questions, the user walks them without
// the model in the loop, and every answer comes back in one result.
func TestAskSeriesReturnsEveryAnswerInOneResult(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "ask_user", Input: map[string]any{
			"questions": []any{
				map[string]any{"id": "store", "question": "Where do answers live?",
					"options": []any{map[string]any{"label": "Session state"}, map[string]any{"label": "The snapshot"}}},
				map[string]any{"question": "Which snapshot key?",
					"when": map[string]any{"question": "store", "answered": []any{"The snapshot"}}},
				map[string]any{"question": "Ship it?", "options": []any{map[string]any{"label": "Yes"}}},
			},
		}}}},
		mockapi.Turn{Text: "done"},
	)
	r := &recorder{answers: []*ask.Answer{{Label: "Session state"}, {Label: "Yes"}}}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	if len(r.questions) != 2 {
		t.Fatalf("asked %d questions, want 2 — the gated one should not have been put", len(r.questions))
	}
	if len(r.results) != 1 {
		t.Fatalf("results = %v, want the series to land as one transcript item", r.results)
	}
	if !strings.HasPrefix(r.results[0], "ask: 2 questions") {
		t.Errorf("transcript label = %q", strings.SplitN(r.results[0], "\n", 2)[0])
	}

	// What the model actually gets: both answers, and the gated question named as not asked.
	sent := string(server.Requests[len(server.Requests)-1])
	for _, want := range []string{"Session state", "Ship it?", "Yes", "not asked"} {
		if !strings.Contains(sent, want) {
			t.Errorf("tool result sent to the model is missing %q: %s", want, sent)
		}
	}
	if strings.Contains(sent, "User declined") {
		t.Error("a fully answered series was reported as declined")
	}
}

// Backing out part way through is not a declined call. The answers already given are real, and
// an error result would throw them away and invite the model to ask all over again.
func TestBackingOutOfASeriesKeepsTheAnswersGiven(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "ask_user", Input: map[string]any{
			"questions": []any{
				map[string]any{"question": "One?", "options": []any{map[string]any{"label": "Yes"}}},
				map[string]any{"question": "Two?", "options": []any{map[string]any{"label": "No"}}},
			},
		}}}},
		mockapi.Turn{Text: "done"},
	)
	r := &recorder{answers: []*ask.Answer{{Label: "Yes"}}}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	sent := string(server.Requests[len(server.Requests)-1])
	if !strings.Contains(sent, "Yes") || !strings.Contains(sent, "backed out") {
		t.Errorf("tool result does not carry both the answer and the back-out: %s", sent)
	}
	if strings.Contains(sent, `"is_error":true`) {
		t.Error("a part-answered series came back as an error result")
	}
}

func TestToolResultsCarryTheirKind(t *testing.T) {
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{
			{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo hi"}},
			{ID: "t2", Name: "grep", Input: map[string]any{"pattern": "zzz-nope-zzz"}},
		}},
		mockapi.Turn{Text: "done"},
	)
	r := &recorder{}

	a.RunTurn(context.Background(), user("go"), r.callbacks())

	if len(r.kinds) != 2 || r.kinds[0] != KindBash || r.kinds[1] != KindSearch {
		t.Errorf("kinds = %v, want [bash search]", r.kinds)
	}
}

// A declined tool still has to answer its tool_use block, or the next request is invalid.
func TestDeclinedToolStillAnswersItsBlock(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "rm -rf /"}}}},
		mockapi.Turn{Text: "understood"},
	)
	r := &recorder{approve: func(string) bool { return false }}

	res := a.RunTurn(context.Background(), user("go"), r.callbacks())

	if res.StopReason != "end_turn" {
		t.Errorf("stop reason = %q", res.StopReason)
	}
	body, _ := json.Marshal(server.LastRequest()["messages"])
	if !strings.Contains(string(body), "User declined") {
		t.Errorf("declined tool did not produce a tool_result: %s", body)
	}
	if len(r.results) != 0 {
		t.Errorf("a declined tool should not render a transcript box, got %v", r.results)
	}
}

// cancelOnceRunning wraps command so cancel fires only after its shell has started.
func cancelOnceRunning(t *testing.T, cancel context.CancelFunc, command string) string {
	t.Helper()
	started := filepath.Join(t.TempDir(), "started")
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	return fmt.Sprintf("touch %q; %s", started, command)
}

// Cancelling mid-tool must leave a sendable conversation: the assistant message keeps its
// tool_use blocks (required for thinking replay) and every one of them gets an answer.
func TestCancelMidToolLeavesASendableHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": cancelOnceRunning(t, cancel, "sleep 30")}}}},
		mockapi.Turn{Text: "done"},
	)

	res := a.RunTurn(ctx, user("go"), (&recorder{}).callbacks())

	if res.StopReason != Cancelled {
		t.Errorf("stop reason = %q, want %q", res.StopReason, Cancelled)
	}
	// A turn answers every tool_use it opened before returning, cancelled or not, so the history it hands back is
	// always sendable and the caller has nothing to fix up.
	assertEveryToolUseAnswered(t, res.History)
}

// A turn must never write into the slice it was handed, or a disowned turn races the session
// that walked away from it.
func TestRunTurnDoesNotMutateTheCallersHistory(t *testing.T) {
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "true"}}}},
		mockapi.Turn{Text: "done"},
	)
	history := user("go")
	before := len(history)
	r := &recorder{}

	res := a.RunTurn(context.Background(), history, r.callbacks())

	if len(history) != before {
		t.Errorf("caller's history grew from %d to %d", before, len(history))
	}
	if len(res.History) <= before {
		t.Errorf("returned history did not grow: %d", len(res.History))
	}
}

func assertEveryToolUseAnswered(t *testing.T, messages []anthropic.MessageParam) {
	t.Helper()
	answered := map[string]bool{}
	var opened []string
	for _, m := range messages {
		for _, b := range m.Content {
			if b.OfToolUse != nil {
				opened = append(opened, b.OfToolUse.ID)
			}
			if b.OfToolResult != nil {
				answered[b.OfToolResult.ToolUseID] = true
			}
		}
	}
	if len(opened) == 0 {
		t.Fatal("no tool_use in this history, so the assertion proves nothing")
	}
	for _, id := range opened {
		if !answered[id] {
			t.Errorf("tool_use %q was never answered; the next request would be rejected", id)
		}
	}
}

func TestToolResultSecretsAreScrubbedBeforeHistoryAndCallbacks(t *testing.T) {
	secret := "ghp_" + strings.Repeat("Ab3", 12)
	a, server := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "leak"}}},
		mockapi.Turn{Text: "done"},
	)
	a.Handlers["leak"] = func(_ context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
		cb.OnToolResult(call.ID, "leak", "GITHUB_TOKEN="+secret, KindBash)
		return ok(call.ID, "GITHUB_TOKEN="+secret)
	}
	r := &recorder{}
	cb := r.callbacks()
	var called []string
	cb.OnCall = func(_ Call, res anthropic.ContentBlockParamUnion) {
		called = append(called, res.OfToolResult.Content[0].OfText.Text)
	}

	res := a.RunTurn(context.Background(), user("go"), cb)

	history, err := json.Marshal(res.History)
	if err != nil {
		t.Fatal(err)
	}
	if len(server.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(server.Requests))
	}
	for name, seen := range map[string]string{
		"history":      string(history),
		"request":      string(server.Requests[1]),
		"OnToolResult": strings.Join(r.results, "\n"),
		"OnCall":       strings.Join(called, "\n"),
	} {
		if strings.Contains(seen, secret) || !strings.Contains(seen, "[redacted") {
			t.Errorf("%s was not scrubbed: %s", name, seen)
		}
	}
}

// Tools on the Concurrent list run at the same time. Three research subagents dispatched in one
// reply and executed one after another is the failure this guards: it costs three times the wall
// clock and nothing reports that it happened, so only a timing assertion can catch it.
func TestConcurrentToolsOverlap(t *testing.T) {
	const hold = 300 * time.Millisecond
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{
			{ID: "t1", Name: "slow"},
			{ID: "t2", Name: "slow"},
			{ID: "t3", Name: "slow"},
		}},
		mockapi.Turn{Text: "done"},
	)
	a.Handlers["slow"] = func(_ context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
		time.Sleep(hold)
		return ok(call.ID, "slept")
	}
	a.Concurrent = map[string]bool{"slow": true}

	start := time.Now()
	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())
	elapsed := time.Since(start)

	if res.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if elapsed > 2*hold {
		t.Errorf("three concurrent tools took %v; sequentially they would take %v", elapsed, 3*hold)
	}
	assertEveryToolUseAnswered(t, res.History)
}

// Concurrency is opt-in, and everything left off the list still runs in order. Tools that stop
// for approval are the reason: the session has one approval channel, so two of them parked at
// once is a deadlock rather than a slow turn.
func TestToolsOffTheConcurrentListStillRunInOrder(t *testing.T) {
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{
			{ID: "t1", Name: "ordered"},
			{ID: "t2", Name: "ordered"},
			{ID: "t3", Name: "ordered"},
		}},
		mockapi.Turn{Text: "done"},
	)
	var mu sync.Mutex
	var inFlight, peak int
	a.Handlers["ordered"] = func(_ context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return ok(call.ID, "ran")
	}

	a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if peak != 1 {
		t.Errorf("%d tools ran at once; tools off the Concurrent list must not overlap", peak)
	}
}

// A budget is what makes an unattended subagent safe to run at all. Without it a model that
// keeps asking for tools loops until the context window stops it.
func TestMaxStepsStopsTheLoop(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{
		Tools: []mockapi.ToolUse{{ID: "t1", Name: "grep", Input: map[string]any{"pattern": "again"}}},
	})
	a.MaxSteps = 3

	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if res.StopReason != StepLimit {
		t.Errorf("stop reason = %q, want %q", res.StopReason, StepLimit)
	}
	if got := server.RequestCount(); got != 3 {
		t.Errorf("made %d requests, want the 3 the budget allowed", got)
	}
}

// A free tool does not come out of the budget. The case it exists for is asking a paired agent
// for an opinion: charge for that and the budget argues against the advice, because a run short
// on turns stops consulting exactly when a second opinion is worth most.
func TestFreeToolsDoNotSpendTheBudget(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{
		Tools: []mockapi.ToolUse{{ID: "t1", Name: "grep", Input: map[string]any{"pattern": "again"}}},
	})
	a.MaxSteps = 3
	a.Free = map[string]bool{"grep": true}

	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if res.StopReason != StepLimit {
		t.Errorf("stop reason = %q, want %q", res.StopReason, StepLimit)
	}
	// Refunds are capped at MaxSteps, so a model that only ever calls a free tool still stops:
	// the budget's 3 plus 3 refunded. Without the cap this loops until the context window ends it.
	if got := server.RequestCount(); got != 6 {
		t.Errorf("made %d requests, want 6 — the budget plus its cap of refunds", got)
	}
}

// Free is all or nothing within a turn. A model that could batch its edits alongside a question
// would get the whole turn for nothing, which turns a narrow exemption into a way out of the
// budget altogether.
func TestAFreeToolBatchedWithWorkIsStillCharged(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{
		Tools: []mockapi.ToolUse{
			{ID: "t1", Name: "grep", Input: map[string]any{"pattern": "a"}},
			{ID: "t2", Name: "glob", Input: map[string]any{"pattern": "*.go"}},
		},
	})
	a.MaxSteps = 3
	a.Free = map[string]bool{"grep": true}

	a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if got := server.RequestCount(); got != 3 {
		t.Errorf("made %d requests, want 3 — a free call beside a real one buys nothing", got)
	}
}

// The step a run reports is the budget it is judged against, not the number of requests it has
// made. A refunded turn that still advanced the count would have a box reading 40/40 while the
// run carried on, which is the display saying the opposite of what the loop is doing.
func TestReportedStepsAreTheBudgetNotTheRequests(t *testing.T) {
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "grep", Input: map[string]any{"pattern": "a"}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: "glob", Input: map[string]any{"pattern": "*.go"}}}},
		mockapi.Turn{Text: "done"},
	)
	a.MaxSteps = 10
	a.Free = map[string]bool{"grep": true}

	var steps []int
	cb := (&recorder{}).callbacks()
	cb.OnStep = func(n int) { steps = append(steps, n) }
	a.RunTurn(context.Background(), user("go"), cb)

	// Three requests: the free grep, the charged glob, the reply. The grep's turn is refunded, so
	// the second request reports 1 again rather than 2.
	want := []int{1, 1, 2}
	if len(steps) != len(want) {
		t.Fatalf("reported steps %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("reported steps %v, want %v", steps, want)
		}
	}
}

// OnCall sees every call, the refused ones too: a caller counting what a step did must not be
// told a step that only met refusals did nothing.
func TestOnCallSeesRefusedCalls(t *testing.T) {
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "grep", Input: map[string]any{"pattern": "a"}}}},
		mockapi.Turn{Text: "done"},
	)
	a.Policy = &Rules{DefaultReason: "test"}

	var seen []string
	cb := (&recorder{}).callbacks()
	cb.OnCall = func(call Call, res anthropic.ContentBlockParamUnion) {
		seen = append(seen, fmt.Sprintf("%s %s %v", call.Name, call.Input, res.OfToolResult.IsError.Or(false)))
	}
	a.RunTurn(context.Background(), user("go"), cb)

	if len(seen) != 1 || seen[0] != `grep {"pattern":"a"} true` {
		t.Errorf("OnCall saw %q, want the refused grep with its input", seen)
	}
}

// The read-only agents are read-only because the handler refuses, not because the prompt asks
// nicely. That property is what lets several of them run at once without an answer for where an
// approval prompt would go, so it is worth a test of its own.
func TestRunViewRefusesToWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "untouched.txt")
	r := &recorder{}

	res := RunView(context.Background(), Call{
		ID:    "c1",
		Name:  "str_replace_based_edit_tool",
		Input: json.RawMessage(`{"command":"create","path":"` + path + `","file_text":"written"}`),
	}, r.callbacks())

	if res.OfToolResult == nil || res.OfToolResult.IsError.Or(false) != true {
		t.Fatalf("a write through the read-only editor was not refused: %+v", res)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("the read-only editor wrote %s", path)
	}
}

func TestViewingAnImageSendsTheModelAnImageBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dot.png")
	if err := os.WriteFile(path, onePixelPNG, 0o644); err != nil {
		t.Fatal(err)
	}

	res := RunView(context.Background(), Call{
		ID:    "c1",
		Name:  "str_replace_based_edit_tool",
		Input: json.RawMessage(`{"command":"view","path":"` + path + `"}`),
	}, (&recorder{}).callbacks())

	r := res.OfToolResult
	if r == nil || r.IsError.Or(false) {
		t.Fatalf("view failed: %+v", res)
	}
	i := slices.IndexFunc(r.Content, func(c anthropic.ToolResultBlockParamContentUnion) bool { return c.OfImage != nil })
	if i < 0 {
		t.Fatalf("no image block in %+v", r.Content)
	}
	if src := r.Content[i].OfImage.Source.OfBase64; src == nil || src.MediaType != "image/png" || src.Data == "" {
		t.Errorf("image source is %+v", r.Content[i].OfImage.Source)
	}
}

var onePixelPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\xf0\x1f\x00\x05\x00\x01\xff\x89\x99=\x1d\x00\x00\x00\x00IEND\xaeB`\x82")

// Cancelling while the model asked for several tools at once is the case the single-tool test
// cannot reach: the first tool is answered by its own handler returning early, so only the tools
// queued behind it go through the loop's cancellation guard. Skipping any of them builds a
// conversation the API rejects.
func TestCancelAnswersToolsQueuedBehindTheRunningOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{
			{ID: "t1", Name: "bash", Input: map[string]any{"command": cancelOnceRunning(t, cancel, "sleep 5")}},
			{ID: "t2", Name: "bash", Input: map[string]any{"command": "echo second"}},
			{ID: "t3", Name: "grep", Input: map[string]any{"pattern": "third"}},
		}},
		mockapi.Turn{Text: "done"},
	)

	res := a.RunTurn(ctx, user("go"), (&recorder{}).callbacks())

	if res.StopReason != Cancelled {
		t.Errorf("stop reason = %q, want %q", res.StopReason, Cancelled)
	}
	assertEveryToolUseAnswered(t, res.History)
}

// drainOnce is a Pending that hands its block back exactly once, which is what the real roster
// does. A source that kept returning the same block would spin the loop forever, so a test fake
// that did so would hide the bug rather than find it.
func drainOnce(block string) func() string {
	var mu sync.Mutex
	taken := false
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		if taken {
			return ""
		}
		taken = true
		return block
	}
}

// sent reports whether any request carried this text. The block has to reach the model, not
// merely be appended to a slice the turn then returns.
func sent(server *mockapi.Server, text string) bool {
	for _, body := range server.Requests {
		if strings.Contains(string(body), text) {
			return true
		}
	}
	return false
}

// A finding that lands while the agent is mid-turn has to reach it at the next step, or the agent
// goes on working from what it knew before it dispatched.
func TestPendingWorkIsPutToTheModelAfterToolResults(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "true"}}}},
		mockapi.Turn{Text: "done"},
	)
	a.Pending = drainOnce("RESEARCH-LANDED")

	a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if !sent(server, "RESEARCH-LANDED") {
		t.Error("the pending block never reached the model")
	}
}

// The tool-result blocks have to stay at the head of the user turn: the API rejects a message
// that opens with anything else, so carrying the block as a leading text block would make every
// turn with pending work fail outright.
func TestPendingWorkRidesBehindTheToolResultsNotInFrontOfThem(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "true"}}}},
		mockapi.Turn{Text: "done"},
	)
	a.Pending = drainOnce("RESEARCH-LANDED")

	a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(server.Requests[len(server.Requests)-1], &req); err != nil {
		t.Fatal(err)
	}
	for _, msg := range req.Messages {
		if msg.Role != "user" || len(msg.Content) < 2 {
			continue
		}
		if msg.Content[0].Type == "tool_result" && msg.Content[len(msg.Content)-1].Type == "text" {
			return
		}
	}
	t.Error("no user message carried the block behind its tool results")
}

// The case the whole feature turns on: the agent has stopped asking for tools and the turn is
// about to end, and a run lands in that window. Ending here would hold the finding until the user
// typed something, which is the behaviour this replaces.
func TestWorkLandingAsTheTurnEndsKeepsTheTurnGoing(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Text: "I have dispatched it."},
		mockapi.Turn{Text: "Now I can use the finding."},
	)
	a.Pending = drainOnce("RESEARCH-LANDED")

	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if !sent(server, "RESEARCH-LANDED") {
		t.Fatal("the turn ended without ever putting the finished run to the model")
	}
	if res.Final != "Now I can use the finding." {
		t.Errorf("final reply = %q, want the turn to have carried on past the block", res.Final)
	}
}

func TestAQuestionAReplyEndsOnIsPutToTheUserAndTheAnswerCarriesTheTurnOn(t *testing.T) {
	a, server := testAgent(t,
		mockapi.Turn{Text: "The probe is done.\n\nWant me to build it?\n\nStored rec_1."},
		mockapi.Turn{Text: "Building it."},
	)
	a.AskTrailing = true
	r := &recorder{answer: &ask.Answer{Label: "Yes"}}

	res := a.RunTurn(context.Background(), user("go"), r.callbacks())

	if len(r.questions) != 1 || r.questions[0].Question != "Want me to build it?" {
		t.Fatalf("asked %+v, want the one closing question", r.questions)
	}
	if !sent(server, "Their answers:\\n\\nYes") {
		t.Error("the answer never reached the model")
	}
	if res.Final != "Building it." {
		t.Errorf("final = %q, want the turn to carry on with the answer", res.Final)
	}
}

func TestBackingOutOfATrailingQuestionEndsTheTurn(t *testing.T) {
	a, server := testAgent(t, mockapi.Turn{Text: "Should I push it?"})
	a.AskTrailing = true

	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if len(server.Requests) != 1 || res.StopReason != "end_turn" {
		t.Errorf("requests = %d, stop = %q; want the turn to end on the dismissed question", len(server.Requests), res.StopReason)
	}
}

// A turn with nothing left to do ends, even with runs still going. Waiting for them is what the
// session's wake replaces, and a turn that waited would sit on a sixty-step coder with the input
// box locked.
func TestATurnWithNothingPendingEndsEvenThoughRunsAreStillGoing(t *testing.T) {
	a, _ := testAgent(t, mockapi.Turn{Text: "dispatched, carrying on"})
	a.Pending = func() string { return "" }

	res := a.RunTurn(context.Background(), user("go"), (&recorder{}).callbacks())

	if res.Final != "dispatched, carrying on" {
		t.Errorf("final = %q, want the turn to have ended", res.Final)
	}
}

// The drain is destructive, so order matters: draining before the cancellation check would take
// the outcome off the queue and return without it, and the result would be lost rather than
// delivered late.
func TestACancelledTurnDoesNotSwallowPendingWork(t *testing.T) {
	a, _ := testAgent(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 30"}}}},
		mockapi.Turn{Text: "done"},
	)
	drained := 0
	a.Pending = func() string { drained++; return "RESEARCH-LANDED" }
	ctx, cancel := context.WithCancel(context.Background())
	cb := (&recorder{}).callbacks()
	cb.OnToolStart = func(string, string, ToolKind) { cancel() }

	res := a.RunTurn(ctx, user("go"), cb)

	if res.StopReason != Cancelled {
		t.Fatalf("stop reason = %q, want %q", res.StopReason, Cancelled)
	}
	if drained != 0 {
		t.Errorf("a cancelled turn drained the queue %d times, taking outcomes it then threw away", drained)
	}
}
