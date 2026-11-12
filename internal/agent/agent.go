// Package agent owns the turn loop: it streams a reply, runs whatever tools the model asks for,
// and hands the conversation back. It knows nothing about the terminal.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/scrub"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	maxTokens = 64000
	// Cancelled is returned as the stop reason when the user interrupted the turn.
	Cancelled = "cancelled"
	// StepLimit is returned when MaxSteps ran out with the model still asking for tools. Only a
	// subagent sets a budget, so only a subagent sees this.
	StepLimit = "step_limit"
	// ContextFull is returned when an agent that clears tool results still filled its window.
	ContextFull = "context_full"
	interrupted = "Interrupted by the user."

	// Load-bearing: without this header context_management is rejected outright.
	contextManagementBeta = "context-management-2025-06-27"

	// Replayed thinking is billed its original tokens, and the API strips none of it by itself.
	// keep counts the in-progress turn as turn 1, so 2 keeps the live turn's blocks, which a tool
	// loop requires, plus the last answered turn's reasoning for follow-ups. 0 is rejected.
	keepThinkingTurns = 2

	// Tool-result clearing, as fractions of the window so they hold at 200k and at 1M alike.
	clearTriggerFraction = 0.6
	clearAtLeastFraction = 0.05
	keepToolUses         = 20
	contextFullFraction  = 0.9
)

// Applied server-side, which is what keeps stored history replayable verbatim. Injected with
// WithJSONSet because ContextManagement is on the SDK's Beta params only.
//
// For the main agent, clearing thinking is the whole of it, deliberately. clear_tool_uses_20250919
// was wired here and removed: it cleared everything but the last three tool results once the
// prompt passed 100k, which on the default model is a tenth of the window, and three results is
// less than one view-edit-test cycle. The damage was not the re-reads. It was that an obligation
// the agent had taken on — a pending revert, a standing instruction read at session start — lived
// only in a cleared result, so the agent stopped knowing it owed anything. A tool result is
// recomputable; knowing you have to recompute it is not. ClearToolResults is the late backstop
// sized against the real window, and only a subagent, whose history is bounded by its task, sets it.
//
// The API requires clear_thinking first and rejects any other order, so a second edit appends.
func (a *Agent) contextEdits() anthropic.BetaContextManagementConfigParam {
	edits := []anthropic.BetaContextManagementConfigEditUnionParam{
		{
			OfClearThinking20251015: &anthropic.BetaClearThinking20251015EditParam{
				Keep: anthropic.BetaClearThinking20251015EditKeepUnionParam{
					OfThinkingTurns: &anthropic.BetaThinkingTurnsParam{Value: keepThinkingTurns},
				},
			},
		},
	}
	if a.ClearToolResults {
		window := float64(a.window())
		edits = append(edits, anthropic.BetaContextManagementConfigEditUnionParam{
			OfClearToolUses20250919: &anthropic.BetaClearToolUses20250919EditParam{
				Trigger: anthropic.BetaClearToolUses20250919EditTriggerUnionParam{
					OfInputTokens: &anthropic.BetaInputTokensTriggerParam{Value: int64(window * clearTriggerFraction)},
				},
				Keep:         anthropic.BetaToolUsesKeepParam{Value: keepToolUses},
				ClearAtLeast: anthropic.BetaInputTokensClearAtLeastParam{Value: int64(window * clearAtLeastFraction)},
				ExcludeTools: a.ExcludeTools,
			},
		})
	}
	return anthropic.BetaContextManagementConfigParam{Edits: edits}
}

// ToolKind is what the transcript colours a box by. The agent says what happened; the UI picks
// the colour, so a new tone is one map entry there and one constant here.
type ToolKind string

const (
	KindBash  ToolKind = "bash"
	KindMCP   ToolKind = "mcp"
	KindEdit  ToolKind = "edit"
	KindVault ToolKind = "vault"
	// KindRecord is a record-store answer the pane draws as markdown; the rest stay KindVault.
	KindRecord ToolKind = "record"
	KindSearch ToolKind = "search"
	KindWeb    ToolKind = "web"
	KindAsk    ToolKind = "ask"
	KindAgent  ToolKind = "agent"
	// KindCalendar is the one builtin whose effect leaves the machine.
	KindCalendar ToolKind = "calendar"
	KindSkill    ToolKind = "skill"
	// KindPoll is a check rerun on an interval: shell underneath, but a wait rather than one command.
	KindPoll ToolKind = "poll"
)

// Usage is how much of the window one request occupied. Total is the conversation's size as of
// that request, not a cost to be added to the last one.
type Usage struct {
	Prompt int64
	Output int64
	Window int64
	// CacheRead and CacheWrite are the parts of Prompt billed at cache rates.
	CacheRead  int64
	CacheWrite int64
	// Counted is a baseline from count_tokens: no request has run, so there is no cache to report.
	Counted bool
}

func (u Usage) Total() int64 { return u.Prompt + u.Output }

// Known separates "not measured yet" from an empty context.
func (u Usage) Known() bool { return u.Window > 0 && u.Total() > 0 }

// Fraction is how full the window is, clamped: an assumed window can be overshot.
func (u Usage) Fraction() float64 {
	if !u.Known() {
		return 0
	}
	return min(float64(u.Total())/float64(u.Window), 1)
}

// Callbacks are how a turn reaches the pane. Approve and Ask block until the user answers or the
// context is cancelled; everything else returns immediately.
type Callbacks struct {
	OnText        func(delta string)
	OnThinking    func(delta string)
	OnThinkingEnd func()
	// OnUsage fires once per request, so a long turn's context growth is visible while it runs.
	OnUsage func(u Usage)
	// OnStep fires before each request with the step about to be taken, counting from 1. It is
	// how a subagent's progress against its budget reaches the roster.
	OnStep func(n int)
	// OnRun announces a dispatched subagent by id, so the pane can show a box that tracks the
	// run while it goes rather than one that appears only once it is over. This package knows
	// nothing about what a run is; the id is a handle the front end resolves.
	OnRun func(id int)
	// body is what a label cannot carry, such as an edit's diff, and is empty when it can.
	Approve func(ctx context.Context, label, body string) bool
	// Require is Approve for a call the policy singled out, and unlike Approve it must not be
	// short-circuited by auto mode. The distinction is the whole point: a rule that says ask is a
	// rule that has already decided the user is the one to decide, so a session that approves
	// everything by default has to stop here anyway. reason is the rule's, and says why this call
	// in particular is being put to them.
	Require func(ctx context.Context, label, body, reason string) bool
	Ask     func(ctx context.Context, q ask.Question) *ask.Answer
	// OnToolStart fires just before a call's work begins, after any approval, so a slow tool's box
	// appears while it runs rather than only once it is over — a bash command that takes a minute
	// must not show nothing for that minute, which reads as nothing happening. id is the call's,
	// and is what ties the box to the result that fills it in.
	OnToolStart func(id, label string, kind ToolKind)
	// OnToolEnd fires once the call is answered, however it ended, so a box whose tool never
	// reported a result — declined, interrupted, refused, failed on its input — is not left
	// claiming to be running. name is the tool's and failed is the answer's is_error.
	OnToolEnd func(id, name string, failed bool)
	// OnToolResult carries the call's id as well as its output, so the result resolves the box the
	// start announced instead of appending a second one beside it.
	OnToolResult func(id, label, output string, kind ToolKind)
	// OnRules names the rules a turn's calls just fired.
	OnRules func(rules []FiredRule)
	// OnCall fires once per tool call with its input and answer, refused or interrupted ones included.
	OnCall func(call Call, result anthropic.ContentBlockParamUnion)
	// OnScreen carries a shell command's verdict, by call id, before the command's box exists.
	OnScreen func(id string, v guardian.Verdict)
	// OnUnsandboxed names, before its box exists, a shell command about to run outside a sandbox the agent has.
	OnUnsandboxed func(id string)

	// suggestions is set by RunTurn, so a suggestion never outlives the turn that made it.
	suggestions *suggestions
	// browserNudged is set by RunTurn, so the delegate-to-verifier reminder comes once per turn.
	browserNudged *atomic.Bool
}

// The three tool callbacks are reached through these helpers rather than called directly. Every
// handler here is shared with subagents, whose callback sets fill in only the fields they read, so
// each one has to be safe to leave nil.
func (cb Callbacks) toolStart(id, label string, kind ToolKind) {
	if cb.OnToolStart != nil {
		cb.OnToolStart(id, label, kind)
	}
}

func (cb Callbacks) toolEnd(id, name string, res anthropic.ContentBlockParamUnion) {
	if cb.OnToolEnd != nil {
		cb.OnToolEnd(id, name, res.OfToolResult != nil && res.OfToolResult.IsError.Or(false))
	}
}

func (cb Callbacks) toolResult(id, label, output string, kind ToolKind) {
	if cb.OnToolResult != nil {
		cb.OnToolResult(id, label, output, kind)
	}
}

// Call is one tool_use block, decoded far enough to dispatch on.
type Call struct {
	ID    string
	Name  string
	Input json.RawMessage
	// Exit is how a command the call ran ended, such as "exit 0" or "timed out", set by the time OnCall sees it.
	Exit string
}

type exitKey struct{}

// SetExit records how the command a handler ran ended, for OnCall to read as the call's Exit.
func SetExit(ctx context.Context, status string) {
	if slot, ok := ctx.Value(exitKey{}).(*string); ok {
		*slot = status
	}
}

// Handler runs one tool and returns the content block that answers it.
type Handler func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion

// Agent is the assembled loop: a system prompt, a tool list that is recomputed per request
// because MCP servers connect late, and a dispatch table. Handlers for MCP and the vault are
// injected by main so this package depends on neither.
type Agent struct {
	// Client and Model are explicit rather than read from a package singleton, so a test can
	// point one agent at a mock without every other agent in the process following it.
	Client *anthropic.Client
	Model  string
	// Window overrides the assumed context window; zero takes the configured default.
	Window   int64
	System   string
	Tools    func() []anthropic.ToolUnionParam
	Handlers map[string]Handler
	// Fallback runs when no handler matches; it is how MCP tools and bash are reached.
	Fallback Handler
	// BeforeTurn runs once before each request, for waiting on MCP startup.
	BeforeTurn func(ctx context.Context)
	// Backgrounded reports a backgrounded process still running, whose news will wake the agent; nil is none.
	Backgrounded func() bool

	// Pending hands back work that finished outside this turn and has not been put to the model
	// yet, as one formatted block, or "" when there is none. It must drain what it returns: it is
	// consulted in a loop, and a source that kept handing back the same block would never let the
	// turn end.
	//
	// A plain string rather than the roster's own type, because this package must not learn about
	// subagents — the whole dispatch mechanism is injected from outside for that reason.
	//
	// Consulted at two points, which are the only two where the conversation is in a state the
	// model can be handed: after a batch of tool results, and where the turn would otherwise end.
	// The second is what stops a finding that landed during the last step from waiting for the
	// user. Neither point waits for a run still going — a turn with nothing left to do ends, and
	// the session starts a new one when the run lands.
	Pending func() string

	// OutputSchema constrains the model's text to a JSON schema, for a caller that needs fields
	// rather than prose. Nil leaves replies unconstrained, which is what the coding loop wants.
	//
	// It rides every request rather than a closing one, which is only sound because the API
	// allows it: probed 2026-10-02, a schema in force does not suppress tool calling, so the loop
	// still works and a structured report costs no extra request. Plain data rather than a func
	// like Tools, because a schema is fixed where the tool list is not.
	OutputSchema map[string]any

	// MaxSteps bounds how many requests one RunTurn may make. Zero is unbounded, which is what
	// the coding loop wants: it stops when the user stops it. A subagent runs unattended, so it
	// gets a budget and a StepLimit stop reason when it runs out.
	MaxSteps int
	// Concurrent names the tools that may run alongside each other within one assistant message.
	// Everything else runs in order, one at a time, because a tool that stops for approval
	// cannot: the session has one approval channel, so two at once would deadlock on it.
	Concurrent map[string]bool

	// Free names the tools whose use does not count against MaxSteps. A turn is refunded only
	// when every call in it was free, so a model cannot smuggle work through alongside one.
	//
	// It exists for a tool that is not progress on the task: asking a paired agent for an
	// opinion. Charging for that sets the budget against the advice — a model short on turns
	// stops consulting precisely when a second opinion is worth most — and the fix has to be
	// here, because MaxSteps is counted here and nowhere else.
	Free map[string]bool

	// Policy bounds what this agent may do with the tools it has. Nil is unbounded, which is what
	// the main loop runs with: the user is sitting in front of it and approves its writes
	// themselves. A subagent runs unattended, so it carries one.
	//
	// It is consulted in runTool rather than in each handler, which is what makes it cover tools
	// this package does not know about — an MCP tool, a tool added later, and the fallback that
	// turns an unrecognised name into a shell command.
	Policy *Rules

	// Screen judges every shell command before it runs; nil refuses them all.
	Screen Screener

	// Sandbox builds the sandbox a shell call running commands runs in, which then skips Screen; nil leaves every command to Screen.
	Sandbox func(ctx context.Context, commands [][]string) (sandbox.Wrap, error)

	// Allows is the session mode, main agent only: with it, gate alone decides shell and network calls.
	Allows func(guardian.Category) bool

	// Preapproved names tools the user approved ahead of time, such as a skill run's declared tools; never shell calls.
	Preapproved func(name string) bool

	// PreapprovedStep is true for a call id the user's skill run issued itself, whose shell command a skill file wrote.
	PreapprovedStep func(id string) bool
	// OnUnprompted hears each command that leaves the sandbox on guardian's pass alone, with nobody asked.
	OnUnprompted func(command string)

	// OnTurnStart runs inside each turn before its first request; text it returns is added to the last user message.
	OnTurnStart func(ctx context.Context, cb Callbacks) string

	// AfterCall runs once a call is answered and OnCall has seen it; the text it returns is appended to the call's result.
	AfterCall func(ctx context.Context, call Call, cb Callbacks) string

	// SkillNotices computes the skill run's progress notices, sent nonce-marked after a turn's tool results; never called once cancelled.
	SkillNotices func() string

	// Standing puts this agent's startup rules after System and attaches the rest to the calls they fire on; required.
	Standing *RuleInjector

	// AskTrailing puts the questions a turn's closing reply asks the user to ask_user; main agent only.
	AskTrailing bool

	// ClearToolResults clears old tool results late in the window, except ExcludeTools; subagents only.
	ClearToolResults bool
	ExcludeTools     []string
}

// toLastUser adds text to the last message when it is the user's, and as a new user message otherwise.
func toLastUser(messages []anthropic.MessageParam, text string) []anthropic.MessageParam {
	block := anthropic.NewTextBlock(text)
	last := len(messages) - 1
	if last < 0 || messages[last].Role != anthropic.MessageParamRoleUser {
		return append(messages, anthropic.NewUserMessage(block))
	}
	messages[last].Content = append(slices.Clone(messages[last].Content), block)
	return messages
}

// ModelID is the model this agent's requests name.
func (a *Agent) ModelID() string {
	if a.Model != "" {
		return a.Model
	}
	return llm.Model()
}

// networkTool reports the calls that reach outside the machine through something other than a shell.
func networkTool(name string) bool {
	return strings.HasPrefix(name, McpPrefix) || name == "calendar_create_event" || name == "calendar_delete_event" ||
		name == "pr_pending_review"
}

// ClosedByTaint reports whether a tool refuses every call once this session has read PHI.
func ClosedByTaint(name string) bool { return name == webSearchTool || name == webFetchTool }

func approveAll(context.Context, string, string) bool { return true }

// Screener is one agent's command criteria. guardian.Gate is the real one.
type Screener interface {
	Screen(ctx context.Context, command string) guardian.Verdict
	Record(command string, v guardian.Verdict, decision string)
	Suggest(ctx context.Context, command string, tools []string) guardian.Suggestion
	RecordSuggested(command string, v guardian.Verdict, s guardian.Suggestion, decision, outcome string)
}

// Result is everything the caller needs after a turn. History is a fresh slice: a turn never
// mutates the one it was given, so a disowned turn cannot write into the session's conversation
// after the session has walked away from it.
type Result struct {
	History    []anthropic.MessageParam
	StopReason string
	// Text is every reply the turn produced, in order, blank-line separated. It is the turn as
	// the user watched it happen, narration and all.
	Text string
	// Final is the last reply alone, which is what a caller wanting a report rather than a
	// narration should read: a turn that thinks aloud before each tool call and then answers has
	// its answer here and its throat-clearing only in Text.
	Final string
	// Usage is the last request's, which is the whole conversation's size — not the turn's cost.
	Usage Usage
}

// result packages a turn's outcome. Every exit from the loop goes through it, so Text and Final
// cannot drift apart as the exits multiply.
func result(messages []anthropic.MessageParam, stop string, replies []string, usage Usage) Result {
	return Result{
		History:    messages,
		StopReason: stop,
		Text:       joined(replies),
		Final:      last(replies),
		Usage:      usage,
	}
}

func ok(id, content string) anthropic.ContentBlockParamUnion {
	return anthropic.NewToolResultBlock(id, content, false)
}

func failed(id string, err error) anthropic.ContentBlockParamUnion {
	return anthropic.NewToolResultBlock(id, err.Error(), true)
}

func declined(id, what string) anthropic.ContentBlockParamUnion {
	return anthropic.NewToolResultBlock(id, "User declined this "+what+".", true)
}

func interruptedResult(id string) anthropic.ContentBlockParamUnion {
	return anthropic.NewToolResultBlock(id, interrupted, true)
}

// RunTurn drives the model until it stops asking for tools.
func (a *Agent) RunTurn(ctx context.Context, history []anthropic.MessageParam, cb Callbacks) Result {
	if a.Standing == nil {
		return result(append([]anthropic.MessageParam(nil), history...), "error: "+errNoRules.Error(), nil, Usage{})
	}
	if a.BeforeTurn != nil {
		a.BeforeTurn(ctx)
	}
	cb.suggestions = &suggestions{}
	cb.browserNudged = &atomic.Bool{}
	defer a.lapse(cb.suggestions)

	// Copied once, up front: everything below appends to this slice and only this slice.
	messages := append([]anthropic.MessageParam(nil), history...)
	if a.OnTurnStart != nil {
		if text := a.OnTurnStart(ctx, cb); text != "" {
			messages = toLastUser(messages, text)
		}
	}
	var replies []string
	var usage Usage

	// spent is the budget, which is not the same as the number of requests made: a turn whose
	// only calls were free tools is refunded below. Reported to OnStep in place of the raw count
	// so the progress a run displays is the budget it is being judged against.
	spent := 0
	// Refunds are capped, or a model that only ever calls a free tool never advances and the loop
	// never ends. MaxSteps is the cap because it needs no second budget to configure and no
	// honest run comes near it.
	refunded := 0

	for {
		if ctx.Err() != nil {
			return result(messages, Cancelled, replies, usage)
		}
		// Ahead of the budget, so a full window is never offered more turns it has no room for.
		if a.ClearToolResults && usage.Fraction() >= contextFullFraction {
			return result(messages, ContextFull, replies, usage)
		}
		if a.MaxSteps > 0 && spent >= a.MaxSteps {
			return result(messages, StepLimit, replies, usage)
		}
		spent++
		if cb.OnStep != nil {
			cb.OnStep(spent)
		}

		msg, said, err := a.stream(ctx, messages, cb)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				// Whatever the model had said stays in history as plain text. The thinking and
				// half-built tool_use blocks around it cannot be replayed — no signature, and
				// truncated JSON — but a text-only assistant turn is always valid to send back.
				if said != "" {
					messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(said)))
					replies = append(replies, said)
				}
				return result(messages, Cancelled, replies, usage)
			}
			return result(messages, "error: "+err.Error(), replies, usage)
		}

		if reported := a.usageOf(msg); reported.Known() {
			usage = reported
			if cb.OnUsage != nil {
				cb.OnUsage(usage)
			}
		}

		if said != "" {
			replies = append(replies, said)
		}
		if msg.StopReason != anthropic.StopReasonToolUse && asksForTools(msg) {
			// A truncated tool call has no result and can't be replayed, so keep it as text like a cancelled turn.
			messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(cutOff(said))))
		} else {
			// ToParam preserves thinking blocks and their signatures, which is what makes extended
			// thinking survive a tool call.
			messages = append(messages, msg.ToParam())
		}

		// A paused server-tool loop ends on a server_tool_use with no result yet; anything but
		// resending it as-is, a user message included, is rejected with a 400.
		if msg.StopReason == anthropic.StopReasonPauseTurn {
			continue
		}

		if msg.StopReason != anthropic.StopReasonToolUse {
			// The turn is over unless something landed behind it. Draining here rather than
			// letting the result wait for the user is the difference between an agent that
			// dispatched work and an agent that can act on what the work found.
			//
			// Cancellation is checked first because the drain is destructive: draining into a
			// turn that is about to return would swallow the outcome on the way past.
			if ctx.Err() != nil {
				return result(messages, Cancelled, replies, usage)
			}
			block := a.pending()
			if block == "" {
				block = a.askTrailing(ctx, fmt.Sprintf("trailing-ask-%d", spent), said, cb)
			}
			if ctx.Err() != nil {
				return result(messages, Cancelled, replies, usage)
			}
			if block == "" {
				return result(messages, string(msg.StopReason), replies, usage)
			}
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(block)))
			continue
		}

		content := a.runTools(ctx, msg, cb)
		if note := a.ruleNote(ctx, withSteps(toolCalls(msg)), cb); note != "" {
			content = append(content, anthropic.NewTextBlock(note))
		}
		if notices := a.SkillNotices; notices != nil && ctx.Err() == nil {
			if text := notices(); text != "" {
				content = append(content, anthropic.NewTextBlock(a.Standing.tag("skill-notice")+"\n"+text+"\n</skill-notice>"))
			}
		}
		// Riding the tool-result message rather than following it: tool_result blocks must open
		// the user turn, and a second user message in a row to carry the text would not be sent.
		//
		// Not drained once cancelled, for the same reason as the exit above: this turn's history
		// is the one the user walked away from, so an outcome written into it is an outcome
		// nobody reads.
		if ctx.Err() == nil {
			if block := a.pending(); block != "" {
				content = append(content, anthropic.NewTextBlock(block))
			}
		}
		messages = append(messages, anthropic.MessageParam{
			Role:    anthropic.MessageParamRoleUser,
			Content: content,
		})
		// Refunded after the fact rather than predicted: the turn being refunded is the one whose
		// only product was a question to a peer, and that is not known until its calls are read.
		if a.allFree(msg) && (a.MaxSteps == 0 || refunded < a.MaxSteps) {
			spent--
			refunded++
		}
		if ctx.Err() != nil {
			return result(messages, Cancelled, replies, usage)
		}
	}
}

func asksForTools(msg anthropic.Message) bool {
	for _, block := range msg.Content {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// askTrailing puts the questions a turn's last reply asks the user to them through ask_user, and
// returns their answers as the next message, or "" when there were none or the user backed out.
func (a *Agent) askTrailing(ctx context.Context, id, said string, cb Callbacks) string {
	if !a.AskTrailing || cb.Ask == nil {
		return ""
	}
	input := ask.Find(said)
	if input == nil {
		return ""
	}
	res := a.Run(ctx, Call{ID: id, Name: "ask_user", Input: input}, cb)
	if res.OfToolResult == nil || res.OfToolResult.IsError.Value {
		return ""
	}
	var b strings.Builder
	for _, c := range res.OfToolResult.Content {
		if c.OfText != nil {
			b.WriteString(c.OfText.Text)
		}
	}
	return "The harness put the questions your reply asked to the user through ask_user. Their answers:\n\n" + b.String()
}

// cutOff stands in history for a reply whose tool call was truncated, and says the call never ran.
func cutOff(said string) string {
	const note = "[My tool call was cut off at the output limit and never ran.]"
	if said == "" {
		return note
	}
	return said + "\n\n" + note
}

// pending is Pending with the nil case folded in, so the loop reads the same whether or not this
// agent has a source of background work. Every subagent is in the nil case.
func (a *Agent) pending() string {
	if a.Pending == nil {
		return ""
	}
	return a.Pending()
}

// allFree reports whether every tool this message asked for is one that does not count against
// the budget. False when there are none named, so an agent without Free behaves exactly as it
// did before — and false when a free call is mixed with a real one, which is what stops a model
// from batching its edits alongside a question and getting the turn for nothing.
func (a *Agent) allFree(msg anthropic.Message) bool {
	if len(a.Free) == 0 {
		return false
	}
	found := false
	for _, block := range msg.Content {
		if block.Type != "tool_use" {
			continue
		}
		if !a.Free[block.Name] {
			return false
		}
		found = true
	}
	return found
}

// runTools answers every tool_use block in one assistant message, in block order.
//
// Tools named in Concurrent run together; everything else runs in sequence. The split is not an
// optimisation looking for somewhere to apply itself — it exists because dispatching three
// research subagents in one message and having them run one after another defeats the point of
// asking for three. It is opt-in rather than the default because the tools that are not on the
// list stop for approval, and the session has a single approval channel to park on.
func (a *Agent) runTools(ctx context.Context, msg anthropic.Message, cb Callbacks) []anthropic.ContentBlockParamUnion {
	calls := toolCalls(msg)
	ctx = context.WithValue(ctx, messageCallsKey{}, len(calls))

	// Indexed rather than appended, so a concurrent answer lands in its own slot and the results
	// come back in the order the model asked for them.
	results := make([]anthropic.ContentBlockParamUnion, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		if !a.Concurrent[call.Name] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = a.runTool(ctx, call, cb)
		}()
	}
	for i, call := range calls {
		if a.Concurrent[call.Name] {
			continue
		}
		results[i] = a.runTool(ctx, call, cb)
	}
	wg.Wait()
	return results
}

type messageCallsKey struct{}

// CallsInMessage is how many tool calls the model made in the message a handler is answering; 0 outside a turn.
func CallsInMessage(ctx context.Context) int {
	n, _ := ctx.Value(messageCallsKey{}).(int)
	return n
}

func toolCalls(msg anthropic.Message) []Call {
	var calls []Call
	for _, block := range msg.Content {
		if block.Type == "tool_use" {
			calls = append(calls, Call{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	return calls
}

// Run answers one call the way a turn would, through the policy and the handler, with no model request.
func (a *Agent) Run(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	return a.runTool(ctx, call, cb)
}

func (a *Agent) runTool(ctx context.Context, call Call, cb Callbacks) (res anthropic.ContentBlockParamUnion) {
	// Deferred first so it runs last, with the ctx and callbacks the call arrived with rather than its approval.
	if after := a.AfterCall; after != nil {
		outerCtx, outerCb := ctx, cb
		defer func() { res = withText(res, after(outerCtx, call, outerCb)) }()
	}
	if cb.OnCall != nil {
		defer func() { cb.OnCall(call, res) }()
	}
	defer func() { res = scrubbed(res) }()
	if report := cb.OnToolResult; report != nil {
		cb.OnToolResult = func(id, label, output string, kind ToolKind) {
			report(id, scrub.Text(label), scrub.Text(output), kind)
		}
	}
	// Tools the model asked for together still need an answer each, even once the user has
	// cancelled.
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}
	// Ahead of the policy: the command it puts to the person already passed the policy when first called.
	if call.Name == DeclineSuggestion {
		return a.decline(ctx, call, cb)
	}
	if call.Name == readRuleTool && a.Standing != nil {
		return a.readRule(call, cb)
	}
	if call.Name == Compose {
		return a.compose(ctx, call, cb)
	}
	// Ahead of the gate, so the person is never asked to approve a call the agent may withdraw.
	if held := a.holdForRules(ctx, call); held != nil {
		return *held
	}
	if tool, target := suggestedTool(ctx, call); tool != "" && cb.suggestions != nil {
		if p, found := cb.suggestions.accept(tool, target); found {
			a.recordSuggested(p, "suggested", "accepted")
		}
	}
	handler := a.Handlers[call.Name]
	if handler == nil {
		handler = a.Fallback
	}
	if handler == nil {
		return failed(call.ID, fmt.Errorf("no handler for tool %s", call.Name))
	}
	blocked, cleared, ctx := a.gate(ctx, call, cb)
	if blocked != nil {
		return *blocked
	}
	if cleared {
		cb.Approve = approveAll
	}
	a.markUnsandboxed(ctx, call, cb)
	// However the handler leaves — a result, a decline, an unparseable input — the box it may have
	// announced is no longer running.
	defer func() { cb.toolEnd(call.ID, call.Name, res) }()
	return handler(context.WithValue(ctx, exitKey{}, &call.Exit), call, cb)
}

func withText(res anthropic.ContentBlockParamUnion, text string) anthropic.ContentBlockParamUnion {
	tr := res.OfToolResult
	if text == "" || tr == nil {
		return res
	}
	out := *tr
	out.Content = append(slices.Clone(tr.Content), anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: text}})
	res.OfToolResult = &out
	return res
}

// scrubbed is res with credentials redacted from its text, leaving images and documents as they are.
func scrubbed(res anthropic.ContentBlockParamUnion) anthropic.ContentBlockParamUnion {
	tr := res.OfToolResult
	if tr == nil {
		return res
	}
	out := *tr
	out.Content = slices.Clone(tr.Content)
	for i, c := range out.Content {
		if c.OfText == nil {
			continue
		}
		text := *c.OfText
		text.Text = scrub.Text(text.Text)
		out.Content[i].OfText = &text
	}
	res.OfToolResult = &out
	return res
}

// gate returns the answer for a call that must not run, whether its handler must not ask again, and the ctx it runs under.
func (a *Agent) gate(ctx context.Context, call Call, cb Callbacks) (*anthropic.ContentBlockParamUnion, bool, context.Context) {
	ctx = context.WithValue(ctx, waiverKey{}, false)
	// Kept as one call rather than routed through Decide, because Ask needs the intent's label and
	// body to show the user what it would do, and Decide returns only the verdict.
	in := a.intent(call)
	if in.Class == ClassEdit {
		in.Path = inDir(ctx, in.Path)
	}
	if in.Class == ClassBash {
		in.Dir, in.Root = tools.Dir(ctx), tools.Dir(ctx)
		if sh, ok := shell.From(ctx); ok {
			in.Dir, in.Root = sh.Dir(), sh.Root()
		}
	}
	if why := postsToPullRequest(in, commandOf(call)); why != "" {
		return block(refusedResult(call.ID, why)), false, ctx
	}
	asked := false
	if a.Policy != nil {
		verdict, reason := a.Policy.Decide(in)
		switch verdict {
		case VerdictAsk:
			if blocked := require(ctx, cb, call, in, reason); blocked != nil {
				return blocked, false, ctx
			}
			asked = true
		case VerdictAllow:
		default:
			return block(refusedResult(call.ID, reason)), false, ctx
		}
	}
	// Ahead of the sandbox, which has network in auto mode.
	if why := writesToGitHub(in, commandOf(call)); why != "" && !asked {
		if blocked := require(ctx, cb, call, in, why); blocked != nil {
			return blocked, false, ctx
		}
		asked = true
	}
	if why := touchesSessionDir(in, commandOf(call)); why != "" && !asked {
		if blocked := require(ctx, cb, call, in, why); blocked != nil {
			return blocked, false, ctx
		}
		asked = true
	}
	if in.Class == ClassBash && !asked && a.PreapprovedStep != nil && a.PreapprovedStep(call.ID) && !taint.Tainted() {
		sctx, err := a.shellContext(ctx, call)
		if err != nil {
			return block(refusedResult(call.ID, err.Error())), false, ctx
		}
		return nil, true, sctx
	}
	// A command a rule already put to the person is theirs to decide, not one to redirect to a tool.
	if in.Class == ClassBash && !asked {
		if held := a.holdSleep(call, cb, in); held != nil {
			return held, false, ctx
		}
		if held := a.holdRead(call, cb, in, in.Dir); held != nil {
			return held, false, ctx
		}
	}
	// A network tool still asks once PHI is read: that fall from auto to local is the latch's, not the user's to waive.
	if in.Class != ClassBash && a.Preapproved != nil && a.Preapproved(call.Name) && !(networkTool(call.Name) && taint.Tainted()) {
		return nil, true, WithSkillWaiver(ctx)
	}
	if ghReadPoll(call) {
		return nil, true, ctx
	}
	switch {
	case in.Class == ClassBash && a.Sandbox != nil && !escalates(call):
		sctx, err := a.shellContext(ctx, call)
		if err != nil {
			return block(refusedResult(call.ID, err.Error())), false, ctx
		}
		return nil, a.Allows != nil, sctx
	case in.Class == ClassBash:
		escalated := a.Sandbox != nil
		// A command a rule already put to the person is theirs to decide, not one to redirect to a tool.
		blocked := a.screen(ctx, call, cb, in, !asked, escalated)
		return blocked, blocked == nil && (a.Allows != nil || escalated), ctx
	case a.Allows != nil && networkTool(call.Name):
		if a.Allows(guardian.Network) {
			return nil, true, ctx
		}
		in.Body = string(call.Input)
		return require(ctx, cb, call, in, "it reaches the network, which this mode does not allow unasked"), true, ctx
	}
	return nil, false, ctx
}

// escalates reports a shell call that asks to run outside the sandbox.
func escalates(call Call) bool {
	if !shellTool(call.Name) {
		return false
	}
	var in struct {
		Sandbox *bool `json:"sandbox"`
	}
	return json.Unmarshal(call.Input, &in) == nil && in.Sandbox != nil && !*in.Sandbox
}

// shellContext is the ctx a shell call that already passed the gate runs under: sandboxed unless it escalated.
func (a *Agent) shellContext(ctx context.Context, call Call) (context.Context, error) {
	if a.Sandbox == nil || escalates(call) {
		return ctx, nil
	}
	w, err := a.Sandbox(ctx, a.intent(call).Commands)
	if err != nil {
		return ctx, fmt.Errorf("the sandbox could not be set up, so the command is not run: %w", err)
	}
	return sandbox.With(ctx, w), nil
}

func (a *Agent) markUnsandboxed(ctx context.Context, call Call, cb Callbacks) {
	if a.Sandbox == nil || cb.OnUnsandboxed == nil || a.intent(call).Class != ClassBash {
		return
	}
	if _, ok := sandbox.From(ctx); !ok {
		cb.OnUnsandboxed(call.ID)
	}
}

// screen puts a command outside the criteria, or one guardian could not judge, to the person, or refuses it.
func (a *Agent) screen(ctx context.Context, call Call, cb Callbacks, in Intent, suggest, unsandboxed bool) *anthropic.ContentBlockParamUnion {
	if in.Class != ClassBash {
		return nil
	}
	command := commandOf(call)
	if command == "" {
		return nil
	}
	if a.Screen == nil {
		return block(refusedResult(call.ID, "this agent has no command criteria, so it may not run shell commands"))
	}
	sctx, cancel := context.WithTimeout(ctx, screenWait)
	v := a.Screen.Screen(sctx, command)
	cancel()
	if cb.OnScreen != nil {
		cb.OnScreen(call.ID, v)
	}
	if v.Known() && !v.Flagged {
		a.Screen.Record(command, v, "passed")
		if suggest && escalates(call) && a.OnUnprompted != nil {
			a.OnUnprompted(command)
		}
		return nil
	}
	reason := v.Says()
	if !v.Known() {
		reason = "guardian could not judge this command, so it is not run unchecked"
	}
	if unsandboxed {
		reason = "runs outside the sandbox · " + reason
	}
	var s guardian.Suggestion
	if v.Known() && v.Flagged && suggest {
		var held *anthropic.ContentBlockParamUnion
		if held, s = a.suggest(ctx, call, cb, in, command, reason, v); held != nil {
			return held
		}
	}
	record := func(decision string) { a.Screen.RecordSuggested(command, v, s, decision, "") }
	if cb.Require == nil {
		record("refused")
		if unsandboxed {
			reason += " (and there is nobody to ask)"
		}
		return block(refusedResult(call.ID, reason))
	}
	ok := cb.Require(ctx, in.Label, in.Body, reason)
	switch {
	case ctx.Err() != nil:
		record("interrupted")
		return block(interruptedResult(call.ID))
	case !ok:
		record("declined")
		return block(notRun(call.ID, reason))
	}
	record("approved")
	return nil
}

func notRun(id, reason string) anthropic.ContentBlockParamUnion {
	return anthropic.NewToolResultBlock(id, "Not run: "+reason+". The user, or the absence of one, did not "+
		"allow it. Do the work another way, or stop and report that you could not.", true)
}

// Long enough to wait out a short queue of other agents' screens on one Ollama server.
const screenWait = 10 * time.Second

func commandOf(call Call) string {
	var in struct {
		Command string `json:"command"`
		Restart bool   `json:"restart"`
	}
	if json.Unmarshal(call.Input, &in) != nil || in.Restart {
		return ""
	}
	return strings.TrimSpace(in.Command)
}

// require puts one call to the user, answering it when it must not run and returning nil when it
// may. Deliberately not Approve: a call singled out this way is one something has already decided
// the user should decide, so auto mode must not answer it for them. Nobody to ask is a refusal
// rather than a pass, which is what a subagent — no Require channel — gets.
func require(ctx context.Context, cb Callbacks, call Call, in Intent, reason string) *anthropic.ContentBlockParamUnion {
	if cb.Require == nil {
		return block(refusedResult(call.ID, reason+" (and there is nobody to ask)"))
	}
	if !cb.Require(ctx, in.Label, in.Body, reason) {
		return block(declined(call.ID, "tool call"))
	}
	if ctx.Err() != nil {
		return block(interruptedResult(call.ID))
	}
	return nil
}

func block(b anthropic.ContentBlockParamUnion) *anthropic.ContentBlockParamUnion { return &b }

// refusedResult is a structural refusal, and says so. An agent told only "no" treats it as a
// transient failure and tries the same thing a different way; the one thing it must understand is
// that there is no version of this call that will be permitted and no user to appeal to.
func refusedResult(id, reason string) anthropic.ContentBlockParamUnion {
	return anthropic.NewToolResultBlock(id,
		"Refused: "+reason+". This is a fixed limit on what you are allowed to do, not a prompt you can retry or "+
			"get approved. Do the work within what you have, or stop and report that you could not.", true)
}

// Finalize spends one turn with the tools withheld, and returns what the model said.
//
// It is how a subagent that ran out of budget is made to say what it did not finish: stopping
// mid-task is survivable, but only if the remainder is named precisely enough that another run
// could be handed it. Empty on any error, since a failed report must not read as a finished one.
func (a *Agent) Finalize(ctx context.Context, history []anthropic.MessageParam, instruction string) string {
	said, _ := a.FinalizeWith(ctx, history, instruction, 1000)
	return said
}

// FinalizeWith is Finalize with its output cap given and its error returned.
func (a *Agent) FinalizeWith(ctx context.Context, history []anthropic.MessageParam, instruction string, limit int64) (string, error) {
	if a.Standing == nil {
		return "", errNoRules
	}
	c := a.Client
	if c == nil {
		shared := llm.Client()
		c = &shared
	}
	model := a.Model
	if model == "" {
		model = llm.Model()
	}

	messages := append([]anthropic.MessageParam(nil), history...)
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(instruction)))

	tools := a.tools()
	// A history that only fits because the server clears it would overflow here without the edits.
	var opts []option.RequestOption
	if a.ClearToolResults {
		opts = append(opts,
			option.WithHeaderAdd("anthropic-beta", contextManagementBeta),
			option.WithJSONSet("context_management", a.contextEdits()))
	}
	msg, err := c.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: limit,
		System:    []anthropic.TextBlockParam{{Text: a.system()}},
		// Withheld rather than omitted: a model sent a history full of tool_use blocks with no
		// tool list in the request is being sent something the API does not accept.
		Tools:      tools,
		ToolChoice: anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}},
		Messages:   messages,
	}, opts...)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	said := strings.TrimSpace(b.String())
	if said == "" {
		return "", fmt.Errorf("no text in the reply (stop: %s)", msg.StopReason)
	}
	return said, nil
}

// usageOf reads what this request's prompt came to. Cached tokens count with the fresh ones: a
// cached token is cheaper, not absent, and input_tokens alone reads a full window as empty.
func (a *Agent) usageOf(msg anthropic.Message) Usage {
	window := a.Window
	if window <= 0 {
		window = llm.ContextWindow()
	}
	return Usage{
		Prompt:     msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens,
		Output:     msg.Usage.OutputTokens,
		Window:     window,
		CacheRead:  msg.Usage.CacheReadInputTokens,
		CacheWrite: msg.Usage.CacheCreationInputTokens,
	}
}

// stream issues one request and feeds deltas to the callbacks. It returns the accumulated message
// plus the plain text it streamed, which is all that survives a cancellation.
func (a *Agent) stream(ctx context.Context, messages []anthropic.MessageParam, cb Callbacks) (anthropic.Message, string, error) {
	tools := a.tools()

	c := a.Client
	if c == nil {
		shared := llm.Client()
		c = &shared
	}
	model := a.Model
	if model == "" {
		model = llm.Model()
	}
	// Omitted entirely when there is no schema: an empty OutputConfig marshals to nothing, but
	// building it conditionally keeps the unconstrained request byte-identical to before.
	var output anthropic.OutputConfigParam
	if a.OutputSchema != nil {
		output.Format = anthropic.JSONOutputFormatParam{Schema: a.OutputSchema}
	}

	stream := c.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: a.system()}},
		// The SDK documents summarized as the default, but without it the model returns a
		// signature and no thinking text.
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
				Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
			},
		},
		Tools:        tools,
		Messages:     messages,
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
		OutputConfig: output,
	},
		option.WithHeaderAdd("anthropic-beta", contextManagementBeta),
		option.WithJSONSet("context_management", a.contextEdits()),
	)

	var acc anthropic.Message
	var said string
	for stream.Next() {
		event := stream.Current()
		if err := acc.Accumulate(event); err != nil {
			return acc, said, err
		}
		switch event.Type {
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				said += event.Delta.Text
				if cb.OnText != nil {
					cb.OnText(event.Delta.Text)
				}
			case "thinking_delta":
				if cb.OnThinking != nil {
					cb.OnThinking(event.Delta.Thinking)
				}
			}
		case "content_block_stop":
			// Driven by the block that just closed rather than inferred from reply text starting
			// to arrive, so a thought that is followed by a tool call still ends cleanly.
			i := int(event.Index)
			if i >= 0 && i < len(acc.Content) && acc.Content[i].Type == "thinking" {
				if cb.OnThinkingEnd != nil {
					cb.OnThinkingEnd()
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return acc, said, err
	}
	return acc, said, nil
}

// last is the final reply, or "" from a turn that never said anything. Only non-empty replies
// are recorded, so this is the last thing the model actually wrote rather than a trailing blank
// left by a turn that ended on a tool call.
func last(replies []string) string {
	if len(replies) == 0 {
		return ""
	}
	return replies[len(replies)-1]
}

func joined(replies []string) string {
	out := ""
	for i, r := range replies {
		if i > 0 {
			out += "\n\n"
		}
		out += r
	}
	return out
}
