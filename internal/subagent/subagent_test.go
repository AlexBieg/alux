package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// echo is a definition with one tool, enough to drive the loop without touching the disk.
func echo(background bool) *Definition {
	return &Definition{
		Name:        "echo",
		Description: "test agent",
		System:      func() string { return "be a test" },
		Tools: func() []anthropic.ToolUnionParam {
			return []anthropic.ToolUnionParam{strict("note", "record something",
				map[string]any{"text": map[string]any{"type": "string"}}, []string{"text"})}
		},
		Handlers: map[string]agent.Handler{
			"note": func(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
				var in struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(call.Input, &in)
				cb.OnToolResult(call.ID, "note "+in.Text, "noted "+in.Text, agent.KindVault)
				return anthropic.NewToolResultBlock(call.ID, "ok", false)
			},
		},
		Model:      func() string { return "mock" },
		MaxSteps:   5,
		Background: background,
		Commands:   lookOnly("echo"),
	}
}

func lookOnly(name string) *guardian.Criteria {
	return &guardian.Criteria{Agent: name, Allowed: []guardian.Category{guardian.Read}, Threshold: 0.5}
}

// passAll stands in for guardian in tests about something else.
type passAll struct{}

func (passAll) Screen(context.Context, string) guardian.Verdict {
	return guardian.Verdict{Agent: "test", Top: guardian.Read, Threshold: 0.5}
}
func (passAll) Record(string, guardian.Verdict, string) {}
func (passAll) Suggest(context.Context, string, []string) guardian.Suggestion {
	return guardian.Suggestion{}
}
func (passAll) RecordSuggested(string, guardian.Verdict, guardian.Suggestion, string, string) {}

func manager(t *testing.T, def *Definition, turns ...mockapi.Turn) (*Manager, *mockapi.Server) {
	t.Helper()
	return managerWith(t, []*Definition{def}, turns...)
}

func managerWith(t *testing.T, defs []*Definition, turns ...mockapi.Turn) (*Manager, *mockapi.Server) {
	t.Helper()
	server := mockapi.New(turns...)
	t.Cleanup(server.Close)
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	m, err := NewManager(NewRoster(nil), defs...)
	if err != nil {
		t.Fatal(err)
	}
	m.Client = &client
	m.screener = func(guardian.Criteria, func() bool) agent.Screener { return passAll{} }
	return m, server
}

// The main agent gets the subagent's own words back, and nothing else: not its transcript, not
// its tool calls. This is the whole return path for a foreground agent, so if it regresses the
// feature is silently useless.
func TestForegroundDispatchReturnsTheReport(t *testing.T) {
	m, _ := manager(t, echo(false),
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}}},
		mockapi.Turn{Text: "The answer is 42."},
	)

	report, err := m.Dispatch(context.Background(), "echo", "find the answer", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if report != "The answer is 42." {
		t.Errorf("report = %q, want the subagent's final text", report)
	}
	if strings.Contains(report, "noted a") {
		t.Error("the subagent's tool output leaked into the report")
	}
}

// A subagent that thinks aloud on its way to the answer reports only the answer. The narration
// is the main cost of delegating to a chatty agent — it lands in the context the delegation
// existed to protect — so the return path takes the last message rather than joining them all.
func TestReportIsTheFinalMessageNotEveryMessage(t *testing.T) {
	m, _ := manager(t, echo(false),
		mockapi.Turn{
			Text:  "I'll start by recording a note.",
			Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "a"}}},
		},
		mockapi.Turn{
			Text:  "Still looking — one more check.",
			Tools: []mockapi.ToolUse{{ID: "t2", Name: "note", Input: map[string]any{"text": "b"}}},
		},
		mockapi.Turn{Text: "The answer is 42."},
	)

	report, err := m.Dispatch(context.Background(), "echo", "find the answer", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if report != "The answer is 42." {
		t.Errorf("report = %q, want only the final message", report)
	}
}

// Every subagent is told how its work is returned, whatever its own prompt says. Without this the
// contract is a silent property of the loop, and a subagent that does not know it will write its
// findings into a message that gets thrown away.
func TestSubagentIsToldOnlyItsFinalMessageIsReturned(t *testing.T) {
	m, server := manager(t, echo(false), mockapi.Turn{Text: "done"})

	if _, err := m.Dispatch(context.Background(), "echo", "do a thing", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}

	// Printed rather than decoded: the SDK may send system as a string or as text blocks, and
	// the assertion is about the words reaching the model either way.
	system := fmt.Sprint(server.LastRequest()["system"])
	if !strings.Contains(system, "be a test") {
		t.Errorf("the definition's own prompt was lost: %q", system)
	}
	if !strings.Contains(system, "FINAL message") {
		t.Errorf("the subagent was not told how its work is returned: %q", system)
	}
}

// reporter is echo with a schema, for the structured-report path.
func reporter(render func(json.RawMessage) string) *Definition {
	def := echo(false)
	def.Report = &ReportSchema{
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"answer": map[string]any{"type": "string"}},
			"required":             []string{"answer"},
			"additionalProperties": false,
		},
		Render: render,
	}
	return def
}

// A schema'd agent's reply reaches the main agent as the rendered sentence, not as the JSON.
// Forwarding raw JSON would make every caller parse it, and the notice strip show braces.
func TestStructuredReportIsRenderedNotForwarded(t *testing.T) {
	m, server := manager(t, reporter(func(raw json.RawMessage) string {
		var in struct {
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Errorf("render got unparseable JSON: %q", raw)
		}
		return "rendered: " + in.Answer
	}), mockapi.Turn{Text: `{"answer":"42"}`})

	report, err := m.Dispatch(context.Background(), "echo", "find the answer", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if report != "rendered: 42" {
		t.Errorf("report = %q, want the rendered sentence", report)
	}

	// The schema has to actually reach the API, or the rendering above is only working because
	// the mock happened to reply with JSON.
	req := server.LastRequest()
	config, ok := req["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("no output_config on the request: %v", req)
	}
	if _, ok := config["format"]; !ok {
		t.Errorf("output_config carried no format: %v", config)
	}
}

// An unparseable reply is passed through as prose rather than failing the run. A filing truncated
// at max_tokens still knows what it wrote, and that account is worth more than an error.
func TestUnparseableStructuredReportFallsBackToProse(t *testing.T) {
	m, _ := manager(t, reporter(func(json.RawMessage) string {
		t.Error("render was called on a reply that is not JSON")
		return ""
	}), mockapi.Turn{Text: `{"answer":"truncated here`})

	report, err := m.Dispatch(context.Background(), "echo", "find the answer", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if report != `{"answer":"truncated here` {
		t.Errorf("report = %q, want the raw reply passed through", report)
	}
}

// A prose agent sends no schema at all. The coding loop and the research agent share this path,
// so a stray output_config here would constrain replies nobody wanted constrained.
func TestProseAgentSendsNoSchema(t *testing.T) {
	m, server := manager(t, echo(false), mockapi.Turn{Text: "plain prose"})

	if _, err := m.Dispatch(context.Background(), "echo", "do a thing", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.LastRequest()["output_config"]; ok {
		t.Error("an unconstrained agent sent an output_config")
	}
}

// The two contracts are mutually exclusive, and a schema'd agent must be told to write the object
// rather than a report — under a schema, narration is malformed output rather than mere noise.
func TestSchemaAgentGetsTheSchemaContract(t *testing.T) {
	m, server := manager(t, reporter(func(json.RawMessage) string { return "x" }),
		mockapi.Turn{Text: `{"answer":"42"}`})

	if _, err := m.Dispatch(context.Background(), "echo", "do a thing", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	system := fmt.Sprint(server.LastRequest()["system"])
	if !strings.Contains(system, "single JSON object") {
		t.Errorf("the schema contract did not reach the subagent: %q", system)
	}
	if strings.Contains(system, "that string is the last thing you write") {
		t.Errorf("the prose contract was sent to a schema'd agent as well: %q", system)
	}
}

// A background dispatch must not block the turn that started it, and its outcome must reach the
// next turn. The second half is the one that matters: an outcome that only ever reaches the
// transcript reaches an agent that has stopped reading, which is how a failed filing went
// unnoticed by the one party able to re-dispatch it.
func TestBackgroundDispatchReturnsAtOnceAndQueuesItsOutcome(t *testing.T) {
	m, _ := manager(t, echo(true), mockapi.Turn{Text: "Filed it."})

	report, err := m.Dispatch(context.Background(), "echo", "file this", "", agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "background") {
		t.Errorf("the main agent was not told to carry on: %q", report)
	}
	if got := m.Roster.TakeOutcomes(); len(got) != 0 {
		t.Errorf("the outcome was queued before the run finished: %q", got)
	}

	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the background run never finished")
	}
	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Report, "Filed it.") {
		t.Fatalf("outcomes = %+v, want the run's report", outcomes)
	}
	if got := m.Roster.TakeOutcomes(); len(got) != 0 {
		t.Errorf("outcomes were delivered twice: %q", got)
	}
}

// What a detached research agent is for is its findings, and those are paragraphs. The notice
// strip can only show a line, so the queue has to carry both — reducing the queued outcome to the
// strip's line would hand the agent a headline and drop the work it paid for.
func TestABackgroundOutcomeCarriesTheWholeReportNotJustItsFirstLine(t *testing.T) {
	report := "Found three callers.\n\nThe first is in index.go.\nThe second is in links.go."
	m, _ := manager(t, echo(true), mockapi.Turn{Text: report})

	if _, err := m.Dispatch(context.Background(), "echo", "look into it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the background run never finished")
	}

	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want one", outcomes)
	}
	got := outcomes[0]
	if !strings.Contains(got.Report, "links.go") {
		t.Errorf("the body past the first line was dropped: %q", got.Report)
	}
	if strings.Contains(got.Summary, "links.go") {
		t.Errorf("the notice strip got the whole report, not a line: %q", got.Summary)
	}
	if message := got.Message(); !strings.Contains(message, "links.go") || !strings.Contains(message, "echo") {
		t.Errorf("the message the agent reads = %q, want the agent named and the whole report", message)
	}
}

// The model knows whether it has other work to get on with and the definition does not, so the
// default has to be overridable per call.
func TestTheModelCanDetachAnAgentThatRunsInTheForegroundByDefault(t *testing.T) {
	m, _ := manager(t, echo(false), mockapi.Turn{Text: "Looked into it."})
	yes := true

	report, err := m.DispatchWith(context.Background(), "echo", "look into it", "", &yes, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "background") {
		t.Fatalf("the override was ignored and the call blocked: %q", report)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the overridden run never finished")
	}
	if got := m.Roster.TakeOutcomes(); len(got) != 1 {
		t.Fatalf("outcomes = %+v, want the overridden run to report like any other", got)
	}
}

// The mirror of the above: an agent that detaches by default can be waited for when the next
// thing the model does depends on the answer.
func TestTheModelCanWaitForAnAgentThatDetachesByDefault(t *testing.T) {
	m, _ := manager(t, echo(true), mockapi.Turn{Text: "Filed it."})
	no := false

	report, err := m.DispatchWith(context.Background(), "echo", "file this", "", &no, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if report != "Filed it." {
		t.Errorf("report = %q, want the run's own words rather than a promise to report later", report)
	}
}

// The construction check only ever saw the definition's default, so the override is a second way
// in and needs the same refusal. Letting it through would put an approval nobody can answer in
// front of a run whose turn has ended — a hang, and the one this guard was written for.
func TestDetachingAnAgentThatStopsToAskIsRefused(t *testing.T) {
	def := echo(false)
	def.Policy = &agent.Rules{Default: agent.VerdictAsk}
	m, _ := manager(t, def, mockapi.Turn{Text: "done"})
	yes := true

	_, err := m.DispatchWith(context.Background(), "echo", "try it", "", &yes, agent.Callbacks{})
	if err == nil {
		t.Fatal("an agent that stops to ask was detached anyway")
	}
	if !strings.Contains(err.Error(), "nobody to ask") {
		t.Errorf("error = %q, want it to say why", err)
	}
}

// Refused, not quietly downgraded. A model told "started in the background" that was in fact made
// to wait would plan the rest of its turn around a turn it does not have.
func TestARefusedDetachDoesNotSilentlyRunInTheForeground(t *testing.T) {
	def := echo(false)
	def.Policy = &agent.Rules{Default: agent.VerdictAsk}
	m, server := manager(t, def, mockapi.Turn{Text: "done"})
	yes := true

	if _, err := m.DispatchWith(context.Background(), "echo", "try it", "", &yes, agent.Callbacks{}); err == nil {
		t.Fatal("the refusal did not happen")
	}
	if got := server.RequestCount(); got != 0 {
		t.Errorf("requests = %d, want 0 — the refused dispatch ran anyway", got)
	}
}

// gated is a definition whose one tool stops for permission, standing in for the editor tool
// every writing agent reaches the user through. Which callback it uses is the caller's choice,
// because Approve and Require must come out differently when the run is detached.
func gated(use func(context.Context, agent.Callbacks) bool, got *bool, mu *sync.Mutex) *Definition {
	def := echo(false)
	def.Handlers = map[string]agent.Handler{
		"note": func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
			allowed := use(ctx, cb)
			mu.Lock()
			*got = allowed
			mu.Unlock()
			return anthropic.NewToolResultBlock(call.ID, "ok", false)
		},
	}
	return def
}

func writeOnce() []mockapi.Turn {
	return []mockapi.Turn{
		{Tools: []mockapi.ToolUse{{ID: "t1", Name: "note", Input: map[string]any{"text": "x"}}}},
		{Text: "done"},
	}
}

// The approval that stands in for the user's is the one already given: the dispatch. A detached
// editing run cannot raise another — its turn is over and the session's one approval slot
// belongs to whatever is running now — so without this it would write nothing and report that it
// had been declined by a user who was never asked.
func TestADetachedRunApprovesItsOwnWrites(t *testing.T) {
	var mu sync.Mutex
	allowed := false
	def := gated(func(ctx context.Context, cb agent.Callbacks) bool {
		return cb.Approve(ctx, "write a.go", "")
	}, &allowed, &mu)
	m, _ := manager(t, def, writeOnce()...)
	yes := true

	if _, err := m.DispatchWith(context.Background(), "echo", "write it", "", &yes, agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the detached run never finished")
	}

	mu.Lock()
	defer mu.Unlock()
	if !allowed {
		t.Error("a detached run was refused its own write, with nobody having refused it")
	}
}

// Auto-approval is for the case where asking is impossible, so it must not leak into the case
// where it is merely inconvenient. A foreground run with no approver still gets no for an answer.
func TestAForegroundRunStillCannotApproveItself(t *testing.T) {
	var mu sync.Mutex
	allowed := true
	def := gated(func(ctx context.Context, cb agent.Callbacks) bool {
		return cb.Approve(ctx, "write a.go", "")
	}, &allowed, &mu)
	m, _ := manager(t, def, writeOnce()...)

	if _, err := m.Dispatch(context.Background(), "echo", "write it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if allowed {
		t.Error("a foreground run approved its own write")
	}
}

// Taking an approval silently is the version of this that should not ship: the run's box is the
// only place the user can see a write nobody was asked about.
func TestAnAutoApprovalIsVisibleInTheRunsTrail(t *testing.T) {
	var mu sync.Mutex
	allowed := false
	def := gated(func(ctx context.Context, cb agent.Callbacks) bool {
		return cb.Approve(ctx, "write a.go", "")
	}, &allowed, &mu)
	m, _ := manager(t, def, writeOnce()...)
	yes := true

	if _, err := m.DispatchWith(context.Background(), "echo", "write it", "", &yes, agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the detached run never finished")
	}

	runs := m.Roster.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	trail := runs[0].TrailText()
	if !strings.Contains(trail, "auto-approved") || !strings.Contains(trail, "write a.go") {
		t.Errorf("the trail does not record the approval nobody was asked for:\n%s", trail)
	}
}

// Require is the policy saying nothing but a person should answer this. A detached run has no
// person, so the answer is no — never yes, and never a park on a turn that has ended.
func TestADetachedRunIsRefusedWhatOnlyAPersonCouldAllow(t *testing.T) {
	var mu sync.Mutex
	allowed := true
	def := gated(func(ctx context.Context, cb agent.Callbacks) bool {
		return cb.Require(ctx, "push the branch", "", "pushing needs your say-so")
	}, &allowed, &mu)
	m, _ := manager(t, def, writeOnce()...)
	yes := true

	if _, err := m.DispatchWith(context.Background(), "echo", "push it", "", &yes, agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the detached run never finished — it may be parked on an approval nobody can answer")
	}

	mu.Lock()
	defer mu.Unlock()
	if allowed {
		t.Error("a detached run was allowed what the policy reserved for a person")
	}
}

// The posture, asserted through the real dispatch path rather than by reading the field: every
// registered agent detaches unless the caller says otherwise. A definition whose Background was
// set but whose dispatch ignored it would pass a field check and fail here.
func TestEveryAgentDetachesByDefault(t *testing.T) {
	root := t.TempDir()
	for _, def := range []*Definition{Research(), coderDef(root, nil), pairedCoderDef(root, nil)} {
		if def == nil {
			continue
		}
		t.Run(def.Name, func(t *testing.T) {
			def.Model = func() string { return "mock" }
			m, _ := manager(t, def, mockapi.Turn{Text: "done"})

			report, err := m.Dispatch(context.Background(), def.Name, "do the thing", "", agent.Callbacks{
				Approve:      func(context.Context, string, string) bool { return true },
				OnToolResult: func(string, string, string, agent.ToolKind) {},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(report, "background") {
				t.Errorf("%s blocked its caller by default: %q", def.Name, report)
			}
			m.Roster.Drain(5 * time.Second)
		})
	}
}

// A run waiting on its own agent's lock must not be holding a global slot while it waits: the
// lock has to be asked for before the slot is taken, or enough queued runs of one agent occupy
// the whole concurrency budget and agents with no lock at all cannot start.
//
// Filled to maxConcurrent exactly, because the bug is only visible when the budget is exhausted.
func TestRunsQueuedOnTheirOwnLockDoNotStarveEverythingElse(t *testing.T) {
	// Every one of them, not just the first: the bug is that each queued run holds a slot, so the
	// budget is only exhausted once they are all waiting. Waiting on one would let the test pass
	// against the broken ordering purely because the others had not been scheduled yet.
	var held sync.WaitGroup
	held.Add(maxConcurrent)
	release := make(chan struct{})
	defer close(release)

	blocked := echo(true)
	blocked.Name, blocked.Commands = "blocked", lookOnly("blocked")
	blocked.Lock = func(ctx context.Context, job func() error) error {
		held.Done()
		select {
		case <-release:
			return job()
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var freeRan atomic.Bool
	free := echo(true)
	free.Name, free.Commands = "free", lookOnly("free")
	free.Lock = func(_ context.Context, job func() error) error {
		freeRan.Store(true)
		return job()
	}

	m, _ := managerWith(t, []*Definition{blocked, free}, mockapi.Turn{Text: "done"})

	for range maxConcurrent {
		if _, err := m.Dispatch(context.Background(), "blocked", "wait", "", agent.Callbacks{}); err != nil {
			t.Fatal(err)
		}
	}
	held.Wait() // all of them are genuinely waiting on the lock

	if _, err := m.Dispatch(context.Background(), "free", "work", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if freeRan.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("an unlocked agent never started while %d runs waited on an unrelated lock", maxConcurrent)
}

// A failed run has to read as a failure to the agent that could re-dispatch it, not as a report
// that happens to be empty.
func TestAFailedOutcomeReadsAsAFailure(t *testing.T) {
	got := Outcome{Agent: "coder", Err: "the build never passed"}.Message()
	if !strings.Contains(got, "failed") || !strings.Contains(got, "the build never passed") {
		t.Errorf("Message() = %q, want the failure and its reason", got)
	}
}

// Material handed over as context reaches the subagent verbatim. The main agent passes snippets
// and error text this way precisely because paraphrasing them would lose the point.
func TestContextIsHandedOverVerbatim(t *testing.T) {
	m, server := manager(t, echo(false), mockapi.Turn{Text: "done"})

	const snippet = "if err != nil { return nil }   // the exact spacing matters"
	if _, err := m.Dispatch(context.Background(), "echo", "explain this", snippet, agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}

	sent, err := json.Marshal(server.LastRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), snippet) {
		t.Errorf("the context did not reach the subagent verbatim; request was %s", sent)
	}
}

// A subagent cannot dispatch a subagent. Without this the budget is unbounded in a direction
// nobody is watching, and a run's step count stops meaning anything.
func TestSubagentsCannotDispatchSubagents(t *testing.T) {
	def := echo(false)
	m, _ := manager(t, def, mockapi.Turn{Text: "done"})

	for _, tool := range def.Tools() {
		if name := tool.GetName(); name != nil && *name == DispatchTool {
			t.Fatalf("the %s tool is in a subagent's own tool list", DispatchTool)
		}
	}
	if _, ok := m.wrap(def, m.Roster.start("echo", "", "t", 5, false))[DispatchTool]; ok {
		t.Fatalf("a subagent has a handler for %s", DispatchTool)
	}
}

// An unknown agent name is the model's mistake to recover from, not a crash, and the error has
// to say what it could have asked for instead.
func TestUnknownAgentNamesTheAvailableOnes(t *testing.T) {
	m, _ := manager(t, echo(false))

	_, err := m.Dispatch(context.Background(), "nonesuch", "do a thing", "", agent.Callbacks{})
	if err == nil {
		t.Fatal("dispatching an unknown agent succeeded")
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Errorf("the error does not name the available agents: %v", err)
	}
}

// vault_note is kept as a named tool over the generic dispatch because it has been working. It
// has to reach the same place: a wrapper that drifts into a second mechanism is worse than none.
func TestAliasDispatchesTheSameAgent(t *testing.T) {
	def := echo(true)
	def.Alias = &Alias{Name: "echo_note", Description: "d", TaskArg: "t"}
	m, _ := manager(t, def, mockapi.Turn{Text: "Filed it."})

	handler, ok := m.Handlers()["echo_note"]
	if !ok {
		t.Fatal("the alias has no handler")
	}
	handler(context.Background(), agent.Call{
		ID:    "c1",
		Name:  "echo_note",
		Input: json.RawMessage(`{"task":"through the alias"}`),
	}, agent.Callbacks{})

	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the aliased run never finished")
	}
	runs := m.Roster.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want one", len(runs))
	}
	if runs[0].Agent != "echo" {
		t.Errorf("the alias ran %q, not the agent it names", runs[0].Agent)
	}
	if runs[0].Task != "through the alias" {
		t.Errorf("the alias lost the task: %q", runs[0].Task)
	}
}

func TestTheDispatchNamesTheRun(t *testing.T) {
	m, _ := manager(t, echo(false), mockapi.Turn{Text: "one"}, mockapi.Turn{Text: "two"})
	handler := m.Handlers()[DispatchTool]
	for _, input := range []string{
		`{"agent":"echo","name":"render flicker","objective":"Investigate the flicker in render.go","done":"cause found","report":"the cause"}`,
		`{"agent":"echo","objective":"Investigate the flicker in render.go","done":"cause found","report":"the cause"}`,
	} {
		handler(context.Background(), agent.Call{ID: "c", Name: DispatchTool, Input: json.RawMessage(input)},
			agent.Callbacks{})
	}
	runs := m.Roster.Runs()
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	if runs[0].Name != "render flicker" {
		t.Errorf("the named run is called %q", runs[0].Name)
	}
	if runs[1].Name != "Investigate the flicker" {
		t.Errorf("a run dispatched with no name is called %q, want the task's first words", runs[1].Name)
	}
}

func TestABriefMissingARequiredPartIsRefusedByName(t *testing.T) {
	m, server := manager(t, echo(false), mockapi.Turn{Text: "done"})
	handler := m.Handlers()[DispatchTool]
	for _, c := range []struct{ input, missing string }{
		{`{"agent":"echo","name":"n","objective":"  ","done":"d","report":"r"}`, "objective"},
		{`{"agent":"echo","name":"n","objective":"o","done":"","report":"r"}`, "done"},
		{`{"agent":"echo","name":"n","objective":"o","done":"d","report":"\n"}`, "report"},
	} {
		res := handler(context.Background(), agent.Call{ID: "c", Name: DispatchTool, Input: json.RawMessage(c.input)},
			agent.Callbacks{})
		if !res.OfToolResult.IsError.Or(false) {
			t.Errorf("a brief with a blank %s was dispatched", c.missing)
			continue
		}
		if text := res.OfToolResult.Content[0].OfText.Text; !strings.Contains(text, "no "+c.missing) {
			t.Errorf("the refusal for a blank %s does not name it: %q", c.missing, text)
		}
	}
	if n := len(m.Roster.Runs()); n != 0 || server.LastRequest() != nil {
		t.Errorf("a refused brief started %d runs or reached the API", n)
	}
}

func TestTheSubagentReadsEveryPartOfTheBrief(t *testing.T) {
	m, server := manager(t, echo(false), mockapi.Turn{Text: "done"})
	input := `{"agent":"echo","name":"n","objective":"Find the flicker","done":"cause named with file and line",` +
		`"scope":"internal/ui/render.go","boundaries":"edit nothing","report":"the cause in two sentences"}`
	m.Handlers()[DispatchTool](context.Background(), agent.Call{ID: "c", Name: DispatchTool, Input: json.RawMessage(input)},
		agent.Callbacks{})
	sent, err := json.Marshal(server.LastRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Find the flicker", "Done when: cause named with file and line",
		"Where to look: internal/ui/render.go", "Boundaries: edit nothing", "Report: the cause in two sentences"} {
		if !strings.Contains(string(sent), want) {
			t.Errorf("the subagent was never told %q", want)
		}
	}
}

func dispatchDescription(t *testing.T, m *Manager) string {
	t.Helper()
	for _, tool := range m.Tools() {
		if tool.OfTool != nil && tool.OfTool.Name == DispatchTool {
			return tool.OfTool.Description.Value
		}
	}
	t.Fatal("no dispatch tool")
	return ""
}

func TestTheCatalogueGivesEachAgentsModelAndRouting(t *testing.T) {
	cheap := echo(false)
	cheap.Name, cheap.Commands, cheap.Explore = "scout", lookOnly("scout"), true
	cheap.Model = func() string { return "big-model" }
	fixed := echo(false)
	fixed.Name, fixed.Commands = "builder", lookOnly("builder")
	fixed.Model = func() string { return "big-model" }
	m, _ := managerWith(t, []*Definition{cheap, fixed})
	m.Routing = routed
	m.Tier, _ = scoring(nil)

	desc := dispatchDescription(t, m)
	for _, want := range []string{
		"- scout (big-model; smaller tasks on medium-model or small-model): test agent",
		"- builder (big-model): test agent",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("the catalogue lacks %q:\n%s", want, desc)
		}
	}
}

func TestUpkeepAgentsAreListedApartFromTheWorkAgents(t *testing.T) {
	work := echo(false)
	upkeep := echo(false)
	upkeep.Name, upkeep.Commands, upkeep.Upkeep = "tidier", lookOnly("tidier"), true
	m, _ := managerWith(t, []*Definition{upkeep, work})

	desc := dispatchDescription(t, m)
	heading := strings.Index(desc, "Upkeep agents")
	if heading < 0 {
		t.Fatalf("no upkeep section:\n%s", desc)
	}
	if i := strings.Index(desc, "- tidier"); i < heading {
		t.Errorf("the upkeep agent is listed among the work agents:\n%s", desc)
	}
	if i := strings.Index(desc, "- echo"); i < 0 || i > heading {
		t.Errorf("the work agent is not listed before the upkeep section:\n%s", desc)
	}
}

func TestDelegationGuidanceComesOnlyWithAgents(t *testing.T) {
	m, _ := manager(t, echo(false))
	if !strings.Contains(m.SystemNote(), "Delegate with") {
		t.Errorf("with an agent registered the main agent gets no delegation guidance: %q", m.SystemNote())
	}
	empty, err := NewManager(NewRoster(nil))
	if err != nil {
		t.Fatal(err)
	}
	if note := empty.SystemNote(); note != "" {
		t.Errorf("with no agents the main agent is told about subagents anyway: %q", note)
	}
}

func TestARunIsKnownByItsName(t *testing.T) {
	named := Run{Name: "render flicker", Agent: "research", State: Running, Step: 4, MaxSteps: 25, Activity: "reading render.go",
		Task: "Investigate the flicker in render.go"}
	if got, want := named.Headline(), "render flicker · research · reading render.go (4/25)"; got != want {
		t.Errorf("head = %q, want %q", got, want)
	}
	named.Activity = ""
	if got := named.Headline(); strings.Contains(got, named.Task) {
		t.Errorf("between tools the head falls back to the whole task: %q", got)
	}
	unnamed := Run{Agent: "research", State: Done, Task: "Investigate the flicker in render.go"}
	if got := unnamed.Headline(); !strings.HasPrefix(got, "Investigate the flicker · research · done") {
		t.Errorf("an unnamed run's head = %q, want the task's first words", got)
	}
}

// Dispatches run concurrently but ask one at a time. The session parks a single approval, so a
// second prompt raised while the first waits replaces it and the first caller blocks on a
// channel nobody will answer — a hang, not a wrong answer, which is why a plain concurrency
// test would not find it. The stand-in below reproduces that single slot exactly.
func TestConcurrentDispatchesDoNotCollideOnTheApproval(t *testing.T) {
	m, _ := manager(t, echo(false), mockapi.Turn{Text: "done"})

	// One parked approval, as the session has. A second arrival while one is parked strands it.
	var mu sync.Mutex
	parked := false
	stranded := false
	cb := agent.Callbacks{
		Approve: func(context.Context, string, string) bool {
			mu.Lock()
			if parked {
				stranded = true
			}
			parked = true
			mu.Unlock()

			time.Sleep(50 * time.Millisecond) // the user reading the prompt

			mu.Lock()
			parked = false
			mu.Unlock()
			return true
		},
	}

	handler := m.Handlers()[DispatchTool]
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler(context.Background(), agent.Call{
				ID:    "c" + strconv.Itoa(i),
				Name:  DispatchTool,
				Input: json.RawMessage(`{"agent":"echo","objective":"job ` + strconv.Itoa(i) + `","done":"d","report":"r"}`),
			}, cb)
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatches deadlocked on the approval")
	}

	mu.Lock()
	defer mu.Unlock()
	if stranded {
		t.Error("two dispatches were parked on the approval at once; the first would never be answered")
	}
}

// The dot row shows live runs and failures nobody has seen. A finished run leaving at once is
// what keeps the row meaning "this is happening now"; a failure staying is what stops a silent
// one scrolling past.
func TestActiveKeepsFailuresUntilAcknowledgedAndDropsFinishedRuns(t *testing.T) {
	r := NewRoster(nil)
	ok := r.start("echo", "", "worked", 5, true)
	bad := r.start("echo", "", "broke", 5, true)
	running := r.start("echo", "", "going", 5, false)

	ok.finish("fine")
	bad.fail("boom")

	active := r.Active()
	if len(active) != 2 {
		t.Fatalf("active = %d runs, want the failure and the live one", len(active))
	}
	for _, run := range active {
		if run.State == Done {
			t.Error("a finished run is still holding a dot")
		}
	}

	r.TakeOutcomes()
	active = r.Active()
	if len(active) != 1 || active[0].ID != running.run.ID {
		t.Errorf("after acknowledging, active = %v, want only the live run", active)
	}
}
