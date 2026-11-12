package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/llm"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The local mock validates no request shapes, so the exact request this agent sends — adaptive
// thinking with display:summarized, plus the two server-side tool types — has to be confirmed
// against the real API once. Skipped unless ALUX_REAL_API is set.
func TestRealAPIAcceptsTheRequestShape(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run one real request")
	}
	a := &Agent{
		Standing: NewRuleInjector(""),
		System:   "Answer in exactly one short sentence.",
		Tools:    BuiltinTools,
		Handlers: BuiltinHandlers(),
		Fallback: RunBash,
	}
	var text, thinking strings.Builder
	cb := Callbacks{
		OnText:        func(d string) { text.WriteString(d) },
		OnThinking:    func(d string) { thinking.WriteString(d) },
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		Ask:           func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}

	var kinds []ToolKind
	cb.OnToolResult = func(_, _, _ string, k ToolKind) { kinds = append(kinds, k) }

	// Forces a tool round-trip as well as the request shape: the tool_result format and the
	// replay of the assistant message that asked for it are the parts a mock cannot check.
	res := a.RunTurn(context.Background(),
		user("Run the shell command `echo alux-probe-ok` and then tell me what it printed."), cb)

	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("the real API rejected the request: %s", res.StopReason)
	}
	t.Logf("stop=%s tools=%v text=%q thinking=%d chars", res.StopReason, kinds, text.String(), thinking.Len())
	if len(kinds) == 0 {
		t.Error("no tool ran, so the tool round-trip was not exercised")
	}
	if !strings.Contains(text.String(), "alux-probe-ok") {
		t.Errorf("the tool result did not make it back into the reply: %q", text.String())
	}
}

// Property names like -A are unusual in a tool schema, so the API must accept them and the model must use them.
func TestRealAPIAcceptsAndUsesTheGrepFlags(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the grep-schema probe")
	}
	var inputs []string
	a := &Agent{
		Standing: NewRuleInjector(""),
		System:   "You are a coding agent. Use the grep tool to look things up; you cannot read files any other way.",
		Tools:    func() []anthropic.ToolUnionParam { return []anthropic.ToolUnionParam{{OfTool: &grepTool}} },
		Handlers: map[string]Handler{"grep": func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
			inputs = append(inputs, string(call.Input))
			return RunSearch(ctx, call, cb)
		}},
	}
	var text strings.Builder
	cb := Callbacks{
		OnText:        func(d string) { text.WriteString(d) },
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}
	res := a.RunTurn(context.Background(), user(
		"In ../tools/search.go, the function clipResults starts by calling another function on its first line. "+
			"In one grep call, find the line declaring clipResults together with the two lines after it, and tell me "+
			"which function it calls first."), cb)

	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("the real API rejected the grep schema: %s", res.StopReason)
	}
	t.Logf("stop=%s inputs=%v text=%q", res.StopReason, inputs, text.String())
	usedContext := false
	for _, in := range inputs {
		usedContext = usedContext || strings.Contains(in, `"-A"`) || strings.Contains(in, `"-C"`)
	}
	if !usedContext {
		t.Errorf("the model never asked for context lines: %v", inputs)
	}
	if !strings.Contains(text.String(), "entries") {
		t.Errorf("the context lines did not reach the answer: %q", text.String())
	}
}

// /context counts tokens with the tool list and the context edits pushed in through WithJSONSet,
// which is a request shape no mock validates. Counting is not billed, so this probe is free.
func TestRealAPIMeasuresTheContextBreakdown(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the count_tokens probe")
	}
	a := &Agent{System: "You are a coding agent.", Tools: BuiltinTools, Standing: NewRuleInjector("")}

	report, err := a.Breakdown(context.Background(), user("hello"))
	if err != nil {
		t.Fatalf("the real API rejected the count request: %v", err)
	}
	t.Logf("\n%s", report)

	if report.Total <= 0 {
		t.Fatalf("total = %d, want a measured prompt", report.Total)
	}
	// The tool schemas are the largest fixed cost in the prompt, so a zero here means the
	// WithJSONSet injection silently did not reach the request.
	for _, row := range report.Rows {
		if strings.HasSuffix(row.Label, " tools") && row.Tokens <= 0 {
			t.Errorf("row %q measured %d tokens, so the tools never reached the request", row.Label, row.Tokens)
		}
	}
}

// A bad thinking replay fails with a 400, which the mock cannot show. Covers both places thinking
// is load-bearing: a tool loop, and a later turn whose earlier blocks are cleared. Skipped unless
// ALUX_REAL_API is set.
func TestRealAPIAcceptsClearingThinkingAcrossTurns(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run two real requests")
	}
	a := &Agent{
		Standing: NewRuleInjector(""),
		System:   "Be brief.",
		Tools:    BuiltinTools,
		Handlers: BuiltinHandlers(),
		Fallback: RunBash,
	}
	cb := Callbacks{
		OnText:        func(string) {},
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		Ask:           func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}

	// Something worth thinking about, so there is a thinking block to clear on the turn after.
	first := a.RunTurn(context.Background(),
		user("Run `echo 41` and then reason carefully about whether the number it printed is prime."), cb)
	if strings.HasPrefix(first.StopReason, "error:") {
		t.Fatalf("first turn rejected: %s", first.StopReason)
	}
	t.Logf("turn 1: stop=%s prompt=%d output=%d", first.StopReason, first.Usage.Prompt, first.Usage.Output)

	// The replay is the probe: this request carries turn one's thinking and asks the server to drop
	// all but the live turn's.
	second := a.RunTurn(context.Background(),
		append(first.History, user("Now run `echo 42` and do the same for that number.")...), cb)
	if strings.HasPrefix(second.StopReason, "error:") {
		t.Fatalf("second turn rejected with thinking cleared from history: %s", second.StopReason)
	}
	t.Logf("turn 2: stop=%s prompt=%d output=%d", second.StopReason, second.Usage.Prompt, second.Usage.Output)
}

// Skipped unless ALUX_REAL_API is set. Two billed requests.
func TestRealAPISeesAnImageThroughView(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run one real request")
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "swatch.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{System: "Be brief.", Tools: BuiltinTools, Handlers: BuiltinHandlers(), Fallback: RunBash, Standing: NewRuleInjector("")}
	var text strings.Builder
	cb := Callbacks{
		OnText:        func(s string) { text.WriteString(s) },
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return false },
		Ask:           func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}
	run := a.RunTurn(context.Background(),
		user("View "+path+" with the editor's view command and tell me, in one word, its colour."), cb)
	if strings.HasPrefix(run.StopReason, "error:") {
		t.Fatalf("rejected: %s", run.StopReason)
	}
	if !strings.Contains(strings.ToLower(text.String()), "red") {
		t.Errorf("the model did not see red: %q", text.String())
	}
}

// Can a subagent's closing call carry a JSON schema? This began as a spike and the answer is
// now load-bearing: the vault filing agent's report is a schema, and its filed boolean is what
// decides whether a filing reports NONE. The research agent was tried this way and went back to
// prose, so what these probes defend is the mechanism, not any one agent's use of it.
//
// The question is whether output_config survives the one request shape a structured report would
// have to use — a history full of tool_use and tool_result blocks, the tool list still present
// because the API rejects that history without it, and tool_choice:none to stop another call.
// That combination is the whole design, and no mock can tell us whether the API takes it.
//
// Skipped unless ALUX_REAL_API is set. Two billed requests.
func TestRealAPIAcceptsAStructuredClosingCall(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the structured-output spike")
	}
	a := &Agent{
		Standing: NewRuleInjector(""),
		System:   "You are a research subagent. Be brief.",
		Tools:    BuiltinTools,
		Handlers: BuiltinHandlers(),
		Fallback: RunBash,
	}
	cb := Callbacks{
		OnText:        func(string) {},
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		Ask:           func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}

	// A real tool round-trip, so the history carries the blocks that make this shape awkward.
	run := a.RunTurn(context.Background(),
		user("Run `echo spike-probe-ok` and say what it printed."), cb)
	if strings.HasPrefix(run.StopReason, "error:") {
		t.Fatalf("the setup turn was rejected: %s", run.StopReason)
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"finding":  map[string]any{"type": "string"},
			"files":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"complete": map[string]any{"type": "boolean"},
		},
		"required":             []string{"finding", "files", "complete"},
		"additionalProperties": false,
	}

	messages := append([]anthropic.MessageParam(nil), run.History...)
	messages = append(messages, user("Report what you found.")...)

	client := llm.Client()
	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model(llm.Model()),
		MaxTokens: 1000,
		System:    []anthropic.TextBlockParam{{Text: a.System}},
		// Both present deliberately: this is exactly what Finalize has to send.
		Tools:        BuiltinTools(),
		ToolChoice:   anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}},
		Messages:     messages,
		OutputConfig: anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: schema}},
	})
	if err != nil {
		t.Fatalf("output_config with tools and tool_choice:none was rejected: %v", err)
	}

	// Shape of the answer matters as much as its acceptance: the report path has to know which
	// block to read and whether it needs parsing.
	for i, block := range msg.Content {
		t.Logf("block %d: type=%s text=%q", i, block.Type, block.Text)
	}
	t.Logf("stop_reason=%s output_tokens=%d", msg.StopReason, msg.Usage.OutputTokens)

	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text.String())), &got); err != nil {
		t.Fatalf("the reply did not parse as JSON: %v\nreply: %q", err, text.String())
	}
	t.Logf("parsed: %#v", got)
	for _, key := range []string{"finding", "files", "complete"} {
		if _, ok := got[key]; !ok {
			t.Errorf("required key %q missing from the structured reply", key)
		}
	}
}

// SPIKE part two: can the schema ride along on every request instead of a closing one?
//
// If a schema and an ordinary tool_choice coexist — the model still calling tools, and only its
// eventual text constrained — then a structured report costs no extra request at all, and the
// whole design collapses into one field on the loop's params. If instead the schema suppresses
// tool calls, the closing call is mandatory. This decides between the two.
//
// Skipped unless ALUX_REAL_API is set. One billed request.
func TestRealAPISchemaAlongsideToolUse(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the structured-output spike")
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"answer": map[string]any{"type": "string"}},
		"required":             []string{"answer"},
		"additionalProperties": false,
	}

	client := llm.Client()
	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model(llm.Model()),
		MaxTokens: 1000,
		System:    []anthropic.TextBlockParam{{Text: "You are a research subagent."}},
		Tools:     BuiltinTools(),
		// Left at its default, unlike the closing call: the point is whether the model may still
		// reach for a tool while a schema is in force.
		Messages:     user("Run `echo schema-and-tools` and report what it printed."),
		OutputConfig: anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: schema}},
	})
	if err != nil {
		t.Fatalf("output_config alongside an open tool_choice was rejected: %v", err)
	}

	var kinds []string
	for _, block := range msg.Content {
		kinds = append(kinds, string(block.Type))
	}
	t.Logf("stop_reason=%s blocks=%v", msg.StopReason, kinds)
	for i, block := range msg.Content {
		t.Logf("block %d: type=%s name=%s text=%q", i, block.Type, block.Name, block.Text)
	}
	if msg.StopReason == anthropic.StopReasonToolUse {
		t.Log("VERDICT: tools still work under a schema — the report can ride the loop's own requests")
	} else {
		t.Log("VERDICT: the schema suppressed the tool call — a separate closing call is required")
	}
}

// The whole claim, through the real loop: OutputSchema set on an Agent, over a turn that calls a
// tool, carrying everything alux always sends and the hand-rolled spikes above left out —
// adaptive thinking, the context-management beta, cache control.
//
// Those omissions are the point. A schema that the API accepts on a bare request but rejects
// beside a thinking config would pass every probe and fail on the first real filing.
//
// Skipped unless ALUX_REAL_API is set. Two billed requests.
func TestRealAPISchemaSurvivesTheActualLoop(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the structured-output spike")
	}
	a := &Agent{
		Standing: NewRuleInjector(""),
		System:   "You are a research subagent. Write nothing but the closing JSON object.",
		Tools:    BuiltinTools,
		Handlers: BuiltinHandlers(),
		Fallback: RunBash,
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer":  map[string]any{"type": "string"},
				"ranTool": map[string]any{"type": "boolean"},
			},
			"required":             []string{"answer", "ranTool"},
			"additionalProperties": false,
		},
	}

	// Text is collected through OnText, so this also confirms the JSON arrives as ordinary
	// text_delta rather than materialising only in the final snapshot — anything rendering a
	// live view reads these deltas.
	var streamed strings.Builder
	cb := Callbacks{
		OnText:        func(d string) { streamed.WriteString(d) },
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		Ask:           func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}

	var tools int
	cb.OnToolResult = func(string, string, string, ToolKind) { tools++ }

	res := a.RunTurn(context.Background(),
		user("Run `echo loop-schema-ok` and report what it printed."), cb)
	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("the real API rejected a schema beside thinking and context management: %s", res.StopReason)
	}
	t.Logf("stop=%s tools=%d final=%q", res.StopReason, tools, res.Final)

	if tools == 0 {
		t.Error("no tool ran under a schema, so the loop was never exercised")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Final)), &got); err != nil {
		t.Fatalf("Final did not parse as JSON: %v\nfinal: %q", err, res.Final)
	}
	for _, key := range []string{"answer", "ranTool"} {
		if _, ok := got[key]; !ok {
			t.Errorf("required key %q missing: %#v", key, got)
		}
	}
	if strings.TrimSpace(streamed.String()) == "" {
		t.Error("nothing arrived through OnText, so a live view would show an empty reply")
	}
}

// Two things only the real API can answer about a series of questions. First, whether it accepts
// the nested schema at all — questions[] carries objects holding their own options[] and a when
// object, and the local mock validates no schema, so a rejection would only ever show up live.
// Second, and the reason the feature exists: whether the model actually reaches for questions[]
// instead of welding several decisions into one consolidated question, which is what it did when
// a series had no way to be expressed.
//
// Skipped unless ALUX_REAL_API is set. One billed request.
func TestRealAPIAcceptsAndUsesASeriesOfQuestions(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the ask-series probe")
	}
	// Only the ask tool. With bash and the editor on the table the model goes exploring first,
	// which probes nothing here and once hung the run on a shell command; what is being measured
	// is whether it reaches for a series when a series is the right move.
	a := &Agent{
		Standing: NewRuleInjector(""),
		System: "You are a coding agent. Decisions about what the user wants are theirs to make, not yours to guess. " +
			"You have no access to the project yet, so settle the open questions first.",
		Tools:    func() []anthropic.ToolUnionParam { return []anthropic.ToolUnionParam{{OfTool: &askTool}} },
		Handlers: map[string]Handler{"ask_user": runAsk},
	}

	var asked []ask.Question
	cb := Callbacks{
		OnText:        func(string) {},
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		Ask: func(_ context.Context, q ask.Question) *ask.Answer {
			asked = append(asked, q)
			// Answer with the first option where there is one, so a gated follow-up has something
			// to match on, and never back out — the walk itself is what is being probed.
			if len(q.Options) > 0 {
				return &ask.Answer{Label: q.Options[0].Label}
			}
			return &ask.Answer{Label: "Other", Note: "whichever you would pick", Custom: true}
		},
		OnToolResult: func(string, string, string, ToolKind) {},
	}

	res := a.RunTurn(context.Background(), user(
		"I want a cache added to my project. Before anything else, settle what I want with me: where it lives, "+
			"how entries expire, and what happens on a miss — and if I pick an in-memory cache, whether it has to "+
			"survive a restart. Ask me, do not guess."), cb)

	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("the real API rejected the ask_user schema: %s", res.StopReason)
	}
	t.Logf("stop=%s questions=%d", res.StopReason, len(asked))
	for _, q := range asked {
		t.Logf("  %d of %d (approx=%v, prior=%v): %s %v", q.Step, q.Total, q.Approx, q.Prior, q.Question, q.Options)
	}

	if len(asked) < 2 {
		t.Fatalf("asked %d questions, want a series — the model consolidated instead", len(asked))
	}
	// Step and Total are what the panel draws, and they come from the walk rather than the model,
	// so a series that reports itself as a lone question would mean the walk was bypassed.
	if asked[0].Total < 2 || asked[1].Step != 2 {
		t.Errorf("series presented as %d of %d then %d of %d", asked[0].Step, asked[0].Total, asked[1].Step, asked[1].Total)
	}
}

// The one assumption in mid-turn delivery that only the real API can settle: a user message whose
// content is tool_result blocks followed by a text block. The local mock validates no request
// shapes, so a turn that is in fact malformed passes every test in this package and then fails
// on the first dispatch anyone makes in anger.
//
// If this is ever rejected, the fix is a different carrier for the block, not a different
// ordering — tool_result must open the message.
func TestRealAPIAcceptsPendingWorkBehindToolResults(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run one real request")
	}
	a := &Agent{
		Standing: NewRuleInjector(""),
		System:   "Answer in one short sentence.",
		Tools:    lookupTool,
		Handlers: lookupHandlers("probe"),
		Pending: drainOnce("<background-agents>\nThese background runs you dispatched have finished.\n\n" +
			"research: the cache lives in internal/store.\n</background-agents>"),
	}
	cb := Callbacks{
		OnText:        func(string) {},
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		Ask:           func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}
	var tools int
	cb.OnToolEnd = func(string, string, bool) { tools++ }

	res := a.RunTurn(context.Background(), user(
		"Use the lookup tool for the key alpha, then tell me what you saw."), cb)

	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("the real API rejected a user turn carrying a text block behind its tool results: %s", res.StopReason)
	}
	if tools == 0 {
		t.Fatal("no tool ran, so the block took the end-of-turn path and the shape was never sent")
	}
	t.Logf("stop=%s tools=%d text=%q", res.StopReason, tools, res.Text)
	if !strings.Contains(res.Text, "cache") && !strings.Contains(res.Text, "store") {
		t.Log("the reply does not mention the injected block, though the API accepted it")
	}
}

// lookupTool is a tool that reaches nothing, so a probe can force a tool round-trip without
// handing an auto-approving model a shell.
func lookupTool() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "lookup",
		Description: anthropic.String("Look up the value stored under a key."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{"key": map[string]any{"type": "string"}},
			Required:   []string{"key"},
		},
	}}}
}

func lookupHandlers(value string) map[string]Handler {
	return map[string]Handler{"lookup": func(_ context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
		return ok(call.ID, value)
	}}
}

// rejectingClient keeps the body of any failed response, because Finalize swallows its error and
// a probe that only sees an empty report cannot say what the API objected to.
func rejectingClient(rejected *[]string) *anthropic.Client {
	c := anthropic.NewClient(option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		res, err := next(req)
		if err == nil && res.StatusCode >= 300 {
			body, _ := io.ReadAll(res.Body)
			res.Body = io.NopCloser(bytes.NewReader(body))
			*rejected = append(*rejected, fmt.Sprintf("%d %s", res.StatusCode, body))
		}
		return res, err
	}))
	return &c
}

// Finalize sends the context edits, clear_thinking among them, on a request that carries no
// thinking config, over a history that holds thinking blocks from the loop. Whether the API takes
// clear_thinking without thinking enabled is what decides whether a subagent's closing report can
// arrive at all once it clears tool results. Two billed requests and a closing one.
func TestRealAPIFinalizesWithClearingAndNoThinkingConfig(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the closing-report probe")
	}
	var rejected []string
	a := &Agent{
		Standing:         NewRuleInjector(""),
		System:           "You are a research subagent. Be brief.",
		Client:           rejectingClient(&rejected),
		Tools:            lookupTool,
		Handlers:         lookupHandlers("the value under alpha is 7319"),
		ClearToolResults: true,
		ExcludeTools:     []string{"ask_main"},
	}
	cb := Callbacks{
		OnText:        func(string) {},
		OnThinking:    func(string) {},
		OnThinkingEnd: func() {},
		Approve:       func(context.Context, string, string) bool { return true },
		Ask:           func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult:  func(string, string, string, ToolKind) {},
	}

	run := a.RunTurn(context.Background(), user(
		"Use the lookup tool for the key alpha, then think about whether that value is prime before answering."), cb)
	if strings.HasPrefix(run.StopReason, "error:") {
		t.Fatalf("the setup turn was rejected: %s %v", run.StopReason, rejected)
	}
	var thinking, toolUses int
	for _, m := range run.History {
		for _, b := range m.Content {
			switch {
			case b.OfThinking != nil || b.OfRedactedThinking != nil:
				thinking++
			case b.OfToolUse != nil:
				toolUses++
			}
		}
	}
	t.Logf("setup: stop=%s thinking blocks=%d tool uses=%d", run.StopReason, thinking, toolUses)
	if toolUses == 0 {
		t.Fatal("the model never called the tool, so the history is not the shape Finalize meets")
	}

	report := a.Finalize(context.Background(), run.History,
		"Your budget is spent. Report what you did and what is still unfinished.")
	if report == "" {
		t.Fatalf("Finalize returned no report; the API said: %v", rejected)
	}
	t.Logf("report: %q", report)
}

// Whether clear_thinking's kept turn moves the cached prefix of a tool loop; about five billed 13k requests.
func TestRealAPICachesASubagentToolLoop(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the prompt-caching probe")
	}
	var system strings.Builder
	system.WriteString("You are a research subagent. Be brief.\n\nReference material, not needed for the task:\n")
	for i := range 400 {
		fmt.Fprintf(&system, "entry %d: amber %d falcon %d quartz %d meridian lantern\n", i, i*3, i*7, i*11)
	}
	var rejected []string
	a := &Agent{
		Standing:         NewRuleInjector(""),
		System:           system.String(),
		Client:           rejectingClient(&rejected),
		Tools:            lookupTool,
		Handlers:         lookupHandlers("the value is 42"),
		ClearToolResults: true,
		ExcludeTools:     []string{"ask_main"},
	}
	var usages []Usage
	cb := Callbacks{
		OnText: func(string) {}, OnThinking: func(string) {}, OnThinkingEnd: func() {},
		Approve:      func(context.Context, string, string) bool { return true },
		Ask:          func(context.Context, ask.Question) *ask.Answer { return nil },
		OnToolResult: func(string, string, string, ToolKind) {},
		OnUsage:      func(u Usage) { usages = append(usages, u) },
	}

	res := a.RunTurn(context.Background(), user(
		"Use the lookup tool for the keys alpha, beta, gamma and delta, one call per reply, "+
			"thinking briefly before each. Then give the four values."), cb)
	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("rejected: %s %v", res.StopReason, rejected)
	}
	if len(usages) < 3 {
		t.Fatalf("only %d requests, too few to see a cache read follow a write", len(usages))
	}
	var read int64
	for i, u := range usages {
		line := fmt.Sprintf("request %d: prompt=%d cacheRead=%d cacheWrite=%d", i+1, u.Prompt, u.CacheRead, u.CacheWrite)
		if i > 0 {
			line += fmt.Sprintf(" readBack=%.0f%% of previous prompt", 100*float64(u.CacheRead)/float64(usages[i-1].Prompt))
		}
		t.Log(line)
		read += u.CacheRead
	}
	if usages[0].CacheWrite == 0 {
		t.Error("the first request wrote nothing to the cache")
	}
	if read == 0 {
		t.Error("no request read from the cache")
	}
}

// The context_full stop reads Usage, which comes from the Messages response. If usage counts the
// prompt before the server cleared it, a run that clearing keeps comfortably inside its window
// would still be stopped as full. This builds a history big enough to clear, then sets the
// response's usage beside count_tokens with and without the same edits. One billed request of
// about 55k input tokens before clearing; counting is free.
func TestRealAPIReportsUsageAfterClearing(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to run the usage-after-clearing probe")
	}
	var rejected []string
	// A small stated window puts the trigger at 24k, so a 54k history clears without paying for a
	// real 120k prompt.
	a := &Agent{
		Standing:         NewRuleInjector(""),
		System:           "Be brief.",
		Client:           rejectingClient(&rejected),
		Tools:            lookupTool,
		Handlers:         lookupHandlers("unused"),
		ClearToolResults: true,
		Window:           40_000,
		MaxSteps:         1,
	}

	history := user("Look up records 0 to 29.")
	for i := range 30 {
		var filler strings.Builder
		for line := range 60 {
			fmt.Fprintf(&filler, "record %d line %d: amber %d falcon %d quartz %d meridian\n", i, line, i*line, line+i, i*7+line)
		}
		id := fmt.Sprintf("toolu_probe_%02d", i)
		history = append(history,
			anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(id, map[string]any{"key": fmt.Sprint(i)}, "lookup")),
			anthropic.NewUserMessage(anthropic.NewToolResultBlock(id, filler.String(), false)),
		)
	}
	history = append(history,
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("I have all thirty records.")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("Reply with the single word ok.")),
	)

	client := llm.Client()
	count := func(opts ...option.RequestOption) (int64, string) {
		res, err := client.Messages.CountTokens(context.Background(), anthropic.MessageCountTokensParams{
			Model:    anthropic.Model(llm.Model()),
			System:   anthropic.MessageCountTokensParamsSystemUnion{OfTextBlockArray: []anthropic.TextBlockParam{{Text: a.System}}},
			Tools:    []anthropic.MessageCountTokensToolUnionParam{{OfTool: lookupTool()[0].OfTool}},
			Messages: history,
		}, opts...)
		if err != nil {
			t.Fatalf("count_tokens rejected: %v", err)
		}
		return res.InputTokens, res.RawJSON()
	}
	original, _ := count()
	cleared, raw := count(
		option.WithHeaderAdd("anthropic-beta", contextManagementBeta),
		option.WithJSONSet("context_management", a.contextEdits()))
	t.Logf("count_tokens: no edits=%d, with edits=%d (%s)", original, cleared, raw)
	if cleared >= original {
		t.Fatalf("the edits cleared nothing (%d >= %d), so this probe cannot tell the two readings apart", cleared, original)
	}

	res := a.RunTurn(context.Background(), history, Callbacks{
		OnText: func(string) {}, OnThinking: func(string) {}, OnThinkingEnd: func() {},
		OnToolResult: func(string, string, string, ToolKind) {},
	})
	if strings.HasPrefix(res.StopReason, "error:") {
		t.Fatalf("the request was rejected: %s %v", res.StopReason, rejected)
	}
	t.Logf("messages usage: prompt=%d (input+cache) output=%d cacheRead=%d cacheWrite=%d fraction=%.2f stop=%s",
		res.Usage.Prompt, res.Usage.Output, res.Usage.CacheRead, res.Usage.CacheWrite, res.Usage.Fraction(), res.StopReason)

	// Halfway between the two counts is the line: the system prompt and tools are counted the same
	// way on both paths, so only clearing can move usage across it.
	if mid := (original + cleared) / 2; res.Usage.Prompt > mid {
		t.Errorf("usage reports %d, nearer the uncleared %d than the cleared %d: context_full is measured before clearing",
			res.Usage.Prompt, original, cleared)
	} else {
		t.Logf("VERDICT: usage is measured after clearing (%d, cleared count %d, original %d)", res.Usage.Prompt, cleared, original)
	}
}
