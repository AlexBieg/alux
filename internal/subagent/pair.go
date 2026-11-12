package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/alexbieg/alux/internal/vcs"
	"github.com/anthropics/anthropic-sdk-go"
)

// A pair is two agents on one piece of work. The driver has the editing tools. The peer reads the
// repository to plan, and its reviews carry on that same conversation, so what it read stays in
// context and in the prompt cache. At each review alux tells it which files changed and
// what the driver ran; it reads the files itself.
//
// Every exchange with the peer is a blocking call made from inside the driver's own loop — its
// Pending hook or its ask tool — rather than a channel between two running agents. That is the
// whole reason this has no deadlock to reason about: there is only ever one of them live, and the
// single approval slot the session owns is never contended. See Definition.validatePeer for the
// rules that keep it true.
//
// What the peer is, is a Definition like any other. It is simply never registered with the
// manager, so it has no name in the dispatch enum and cannot be started on its own.

const (
	// defaultRounds bounds the driver's questions, and defaultSteers the peer's interventions.
	// Each exchange resends the peer's whole conversation, so the spend is quadratic in both.
	defaultRounds = 6
	defaultSteers = 4

	// checkpointEvery is how many editing steps pass between reviews.
	checkpointEvery = 8
	// endReviews is how many times the turn's end is reviewed. One: a steer there reopens the
	// turn, and what the driver does with it is judged by the closing view rather than by a
	// second steer that could reopen it again.
	endReviews = 1

	bashTail = 30
	maxDiff  = 30000
)

const peerContract = `How this conversation works: each message comes either from alux, showing you what the driver has done since your last look, or from the driver itself, asking you something. Every reply you send is read. Answer the message in front of you and stop.

If what you can read and what you are shown is not enough to answer, say what you would need rather than guessing at it.

Keep each reply to a few sentences. The driver is in the middle of a piece of work and a long answer costs it the context this arrangement exists to save.`

// PeerSpec gives a definition a counterpart: a planner that runs before it starts, a reviewer
// that alux consults at checkpoints, and one extra tool for the driver to ask it things.
type PeerSpec struct {
	// Def is the reviewer. It is not registered with the manager and never appears in the
	// dispatch enum.
	Def *Definition
	// Planner plans before the driver starts, and reviews carry on its conversation. Nil means no plan.
	Planner *Definition
	// Tool is what the driver calls to ask the reviewer something. It has to be permitted by the
	// driver's own policy, which validatePeer checks.
	Tool        string
	Description string
	// Rounds caps the driver's questions. Zero takes defaultRounds.
	Rounds int
	// Steers caps the reviewer's interventions. Zero takes defaultSteers.
	Steers int
	// Root is where the end-of-turn diff is taken. Empty, or outside a repository, sends the
	// driver's edits instead.
	Root string
	// Seeded is the driver's system prompt when its history already holds seeded reads; nil keeps the definition's.
	Seeded func() string
}

func (s *PeerSpec) rounds() int {
	if s.Rounds > 0 {
		return s.Rounds
	}
	return defaultRounds
}

func (s *PeerSpec) steers() int {
	if s.Steers > 0 {
		return s.Steers
	}
	return defaultSteers
}

// exchanges is the most the peer can be spoken to in a run of the given budget: the plan, every
// checkpoint, the end of the turn and every question.
func (s *PeerSpec) exchanges(steps int) int {
	n := 1 + endReviews + s.rounds()
	if s.Planner == nil {
		n--
	}
	return n + steps/checkpointEvery
}

func (s *PeerSpec) tool() anthropic.ToolUnionParam {
	return strict(s.Tool, s.Description,
		map[string]any{
			"question": map[string]any{
				"type": "string",
				"description": "What you want to ask or dispute, stated so that someone who has not seen your tool results " +
					"can answer it.",
			},
			"work": map[string]any{
				"type": "string",
				"description": "The code, output or error that bears on it — quoted, not described. It sees nothing of " +
					"what you have read.",
			},
		},
		[]string{"question"})
}

// peer is one run's counterpart: its review conversation, and the record of what the driver has
// done since the peer last looked.
type peer struct {
	spec *PeerSpec
	// h is the peer's own roster row; driver is the driver's, which gets a marker per steer.
	h, driver *handle
	agent     *agent.Agent
	planner   *agent.Agent
	maxSteps  int
	mark      vcs.Mark
	marked    bool
	log       func(PeerExchange)
	session   string
	dispatch  string
	// rec is the peer's own transcript: every message it is sent, and its replies and reads.
	rec *recorder
	// shell is the peer's own, so its commands never move the driver's.
	shell *shell.Shell

	// mu guards everything below. The driver is sequential, so it is never contended in
	// practice; it is here because OnCall is reachable from the agent's concurrent tool path.
	mu      sync.Mutex
	history []anthropic.MessageParam
	// preface is the task, sent ahead of the first review when there is no planning conversation to carry on.
	preface string
	// seeds are the dispatch's seeds and the files the planner viewed and the plan names, re-read into the driver's first request.
	seeds   []seedFile
	asked   int
	steered int
	reviews int

	prose strings.Builder
	delta []string
	// touched is the files edited since the last look, sent in place of the edits to a peer that reads.
	touched []string
	reads   bool
	edits   []string
	step    int
	calls   int
	edited  bool
	// editSteps counts steps that edited; reviewedAt is its value at the last checkpoint.
	editSteps   int
	reviewedAt  int
	endReviewed int
	// steps is how many requests the navigator's latest exchange took, for the log.
	steps int
}

func (m *Manager) newPeer(spec *PeerSpec, h, driver *handle, maxSteps int) *peer {
	def := spec.Def
	p := &peer{
		spec:     spec,
		h:        h,
		driver:   driver,
		maxSteps: maxSteps,
		log:      m.PeerLog,
		session:  m.Session,
		dispatch: driver.run.Dispatch,
		rec:      newRecorder(m.Transcripts, h.run.Dispatch),
	}
	reviewer := agent.Agent{
		Client:   m.Client,
		Model:    def.model(),
		System:   def.System() + "\n\n" + peerContract,
		Standing: agent.NewRuleInjector(def.Name),
		Tools:    def.Tools,
		Handlers: def.Handlers,
		Fallback: def.Fallback,
		MaxSteps: def.steps(),
		Policy:   def.Policy,
		Screen:   m.screen(def, func() bool { return true }),
		Sandbox:  m.sandbox(def),
	}
	// With a planner, reviews carry on its conversation, so the planning reads stay in context and cache.
	if pl := spec.Planner; pl != nil {
		reviewer.Model = pl.model()
		reviewer.System = pl.System() + "\n\n" + def.System() + "\n\n" + peerContract
		reviewer.Tools, reviewer.Handlers, reviewer.Fallback = pl.Tools, pl.Handlers, pl.Fallback
		reviewer.Policy, reviewer.Screen, reviewer.Sandbox = pl.Policy, m.screen(pl, func() bool { return true }), m.sandbox(pl)
		planner := reviewer
		planner.MaxSteps = pl.steps()
		p.planner = &planner
	}
	p.agent = &reviewer
	p.reads = offers(p.agent, editorName)
	h.model(reviewer.Model)
	if spec.Root != "" {
		p.mark, p.marked = vcs.Snapshot(spec.Root)
	}
	return p
}

const planOutOfBudget = `You are out of turns. Do not call tools. Write the plan now from what you have read, and name what you did not get to check.`

// plan runs the planner on the task and returns the plan, or "" when there is none. It never
// fails the run: a driver without a plan is a plain coder with a reviewer.
//
// The planner reports through the peer's own row, never the driver's callbacks: those drive the
// driver's row, and the planner's steps would rewrite its step count.
func (p *peer) plan(ctx context.Context, task string, seeds []seedFile) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.seeds = seeds
	plan := ""
	if p.planner != nil {
		p.h.consulting(true)
		p.rec.add(TranscriptEvent{Kind: TranscriptTask, Name: p.spec.Def.Name, Text: task})
		first := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(task))}
		seeded, _ := seedViews(p.own(ctx), p.planner, seeds)
		res := p.planner.RunTurn(p.own(ctx), append(first, seeded...), p.toolTrail())
		p.rec.flush()
		history := res.History
		var forced error
		switch {
		case res.StopReason == agent.StepLimit:
			plan, history, forced = p.settle(ctx, p.planner, res.History, planOutOfBudget)
		case res.StopReason == agent.Cancelled, strings.HasPrefix(res.StopReason, "error: "):
		default:
			plan = res.Final
		}
		p.h.consulting(false)
		plan = strings.TrimSpace(plan)
		p.logExchange(ExchangePlan, task, plan, res, forced)
		if plan == "" {
			why := firstLine(res.StopReason)
			if forced != nil {
				why = "out of turns, and the plan it was then asked for failed: " + firstLine(forced.Error())
			}
			p.h.appendTrail(Entry{Kind: EntryNote, Text: p.spec.Def.Name + " · no plan: " + why})
		} else {
			p.seeds = mergeSeeds(seeds, selectSeeds(res.History, plan, p.spec.Root, tools.Dir(ctx)))
			p.h.appendTrail(Entry{Kind: EntryPlan, From: p.spec.Def.Name, Text: plan})
			p.history = history
			return plan
		}
	}

	p.preface = "The task, as the driver was given it:\n<task>\n" + task + "\n</task>\n\n" +
		"No plan was made; the driver is working without one."
	return plan
}

// toolTrail reports the navigator's own reads on its row, never through the driver's callbacks.
func (p *peer) toolTrail() agent.Callbacks {
	p.steps = 0
	cb := agent.Callbacks{
		OnStep: func(n int) { p.steps = n },
		OnToolResult: func(_, label, _ string, kind agent.ToolKind) {
			p.h.activity(label)
			p.h.appendTrail(Entry{Kind: EntryTool, Tool: kind, Text: label})
		},
	}
	p.rec.watch(&cb)
	return cb
}

// settle is settle with the instruction and answer in the peer's transcript, which no callback streams them to.
func (p *peer) settle(ctx context.Context, a *agent.Agent, history []anthropic.MessageParam, instruction string) (string, []anthropic.MessageParam, error) {
	p.rec.add(TranscriptEvent{Kind: TranscriptUser, Text: instruction})
	said, out, err := settle(ctx, a, history, instruction)
	if err == nil {
		p.rec.add(TranscriptEvent{Kind: TranscriptText, Text: said})
	}
	return said, out, err
}

// settleTokens leaves room for a whole plan, which runs to several thousand tokens.
const settleTokens = 12000

// settle asks an agent out of turns for its answer and keeps the exchange so the conversation can go on.
func settle(ctx context.Context, a *agent.Agent, history []anthropic.MessageParam, instruction string) (string, []anthropic.MessageParam, error) {
	said, err := a.FinalizeWith(ctx, history, instruction, settleTokens)
	if err != nil {
		return "", history, err
	}
	out := append(append([]anthropic.MessageParam(nil), history...),
		anthropic.NewUserMessage(anthropic.NewTextBlock(instruction)),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock(said)))
	return said, out, nil
}

// withPlan is the driver's first message with the navigator's plan in it.
func withPlan(text, plan, tool string) string {
	return text + "\n\n<plan from=\"navigator\">\n" + plan + "\n</plan>\n\n" +
		"This plan is from the navigator, which read the repository before you started. Follow it. It was written " +
		"from reading, not running, so where the repository contradicts it, go with the repository and push back with " +
		tool + ", quoting what you found."
}

// watch wraps the driver's callbacks so that what it says and does is recorded for the next
// checkpoint.
func (p *peer) watch(cb *agent.Callbacks) {
	step, text := cb.OnStep, cb.OnText
	cb.OnStep = func(n int) {
		p.mu.Lock()
		p.flushProse()
		p.step, p.calls = n, 0
		p.mu.Unlock()
		if step != nil {
			step(n)
		}
	}
	cb.OnText = func(delta string) {
		p.mu.Lock()
		p.prose.WriteString(delta)
		p.mu.Unlock()
		if text != nil {
			text(delta)
		}
	}
	call := cb.OnCall
	cb.OnCall = func(c agent.Call, res anthropic.ContentBlockParamUnion) {
		p.record(c, res)
		if call != nil {
			call(c, res)
		}
	}
}

func (p *peer) flushProse() {
	if said := strings.TrimSpace(p.prose.String()); said != "" {
		p.delta = append(p.delta, "The driver said: "+said)
	}
	p.prose.Reset()
}

// record keeps the files edited (the edits themselves for a peer that cannot read) and command tails.
func (p *peer) record(call agent.Call, res anthropic.ContentBlockParamUnion) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.flushProse()
	output, failed := callOutput(res)
	switch call.Name {
	case "str_replace_based_edit_tool":
		var in tools.EditInput
		if json.Unmarshal(call.Input, &in) != nil || in.Command == "view" {
			return
		}
		entry := describeEdit(in, call.Input)
		if !failed {
			p.edited = true
			p.edits = append(p.edits, entry)
		}
		if p.reads {
			if !failed && !slices.Contains(p.touched, in.Path) {
				p.touched = append(p.touched, in.Path)
			}
			return
		}
		if failed {
			entry = "(failed: " + firstLine(output) + ") " + entry
		}
		p.delta = append(p.delta, entry)
	case "bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(call.Input, &in) != nil || in.Command == "" {
			return
		}
		p.delta = append(p.delta, "$ "+in.Command+"\n"+tail(output, bashTail))
		if fields := strings.Fields(in.Command); len(fields) > 0 && fields[0] == "rm" && !failed {
			p.edited = true
			p.edits = append(p.edits, "$ "+in.Command)
		}
	}
}

func describeEdit(in tools.EditInput, raw json.RawMessage) string {
	head := in.Command + " " + in.Path
	switch in.Command {
	case "create":
		return head + "\n" + in.FileText
	case "str_replace":
		return head + "\n<old>\n" + in.OldStr + "\n</old>\n<new>\n" + in.NewStr + "\n</new>"
	case "insert":
		if in.InsertLine != nil {
			head += " after line " + strconv.Itoa(*in.InsertLine)
		}
		return head + "\n" + in.InsertText
	}
	return head + "\n" + string(raw)
}

func callOutput(res anthropic.ContentBlockParamUnion) (string, bool) {
	if res.OfToolResult == nil {
		return "", false
	}
	var parts []string
	for _, c := range res.OfToolResult.Content {
		if c.OfText != nil {
			parts = append(parts, c.OfText.Text)
		}
	}
	return strings.Join(parts, "\n"), res.OfToolResult.IsError.Or(false)
}

func tail(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return fmt.Sprintf("[%d lines cut]\n", len(lines)-n) + strings.Join(lines[len(lines)-n:], "\n")
}

// pending is the driver's Pending hook, and the checkpoint. The agent consults it after every
// batch of tool results and where the turn would otherwise end; a batch always reaches OnCall
// first, so a call seen since the last step says which of the two this is.
func (p *peer) pending(ctx context.Context) func() string {
	return func() string {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.steered >= p.spec.steers() {
			return ""
		}
		if p.calls > 0 {
			if p.edited {
				p.editSteps++
				p.edited = false
			}
			if p.editSteps-p.reviewedAt < checkpointEvery {
				return ""
			}
			p.reviewedAt = p.editSteps
			return p.review(ctx, ExchangeCheckpoint, "Checkpoint: the driver has made "+strconv.Itoa(checkpointEvery)+
				" more editing steps since your last look.")
		}
		// A steer on the last step would reopen a turn with no budget left to act on it.
		if p.endReviewed >= endReviews || (p.maxSteps > 0 && p.step >= p.maxSteps) {
			return ""
		}
		p.endReviewed++
		return p.review(ctx, ExchangeEnd, "End of turn: the driver has stopped and is about to report.")
	}
}

const reviewAsk = "If nothing here is genuinely wrong, reply with exactly " + none + ". Otherwise reply with one steer."

func (p *peer) review(ctx context.Context, kind, header string) string {
	text := header + "\n\n" + p.takeDelta()
	if kind == ExchangeEnd {
		text += "\n\n" + p.diff()
	}
	res, err := p.exchange(ctx, kind, text+"\n\n"+reviewAsk)
	p.reviews++
	asked := Entry{Kind: EntryExchange, From: "alux", To: p.spec.Def.Name, Prompt: header}
	if err != nil {
		asked.Failed, asked.Text = true, "failed — "+err.Error()
		p.h.appendTrail(asked)
		return ""
	}
	reply := strings.TrimSpace(res.Final)
	switch verdictOf(reply) {
	case VerdictEmpty:
		asked.Text = emptyReply(res.StopReason)
		p.h.appendTrail(asked)
		return ""
	case VerdictNone:
		asked.Text = reply
		p.h.appendTrail(asked)
		return ""
	}
	p.steered++
	asked.Text = reply
	p.h.appendTrail(asked)
	p.driver.appendTrail(Entry{Kind: EntrySteer, From: p.spec.Def.Name, To: "driver", Text: firstLine(reply)})
	return "<navigator-steer>\nFrom the navigator, which can read your edits but cannot build or run anything: a " +
		"claim here about what the code does or how output renders is a hypothesis to check, not a finding. If it is " +
		"unclear or wrong, say so with " + p.spec.Tool + ".\n\n" + reply + "\n</navigator-steer>"
}

func (p *peer) takeDelta() string {
	p.flushProse()
	parts := p.delta
	if len(p.touched) > 0 {
		parts = append([]string{"Files edited (read them to see the change):\n- " + strings.Join(p.touched, "\n- ")}, parts...)
	}
	p.delta, p.touched = nil, nil
	if len(parts) == 0 {
		return "<delta>\nNothing since your last look.\n</delta>"
	}
	return "<delta>\n" + strings.Join(parts, "\n\n") + "\n</delta>"
}

// diff is the working copy's change since the run began, as alux sees it.
func (p *peer) diff() string {
	if p.marked {
		if d, err := p.mark.Diff(); err == nil {
			if strings.TrimSpace(d) == "" {
				return "<diff>\nThe working copy is unchanged since the run began.\n</diff>"
			}
			return "<diff>\n" + capped(d) + "\n</diff>"
		}
	}
	return "<diff>\nNo repository diff is available. Every edit the driver made this run:\n\n" +
		capped(strings.Join(p.edits, "\n\n")) + "\n</diff>"
}

func capped(text string) string {
	if len(text) <= maxDiff {
		return text
	}
	return text[:maxDiff] + fmt.Sprintf("\n[%d more bytes cut]", len(text)-maxDiff)
}

const reviewOutOfBudget = `You are out of turns for this message. Do not call tools. Reply now from what you have read.`

// exchange sends one message on the review conversation and returns the reply. The caller holds mu.
//
// The peer's only callbacks put its reads on its own row: the driver's would have its turns
// rewrite the driver's row, and none of them is a route to the user.
func (p *peer) exchange(ctx context.Context, kind, text string) (agent.Result, error) {
	if p.preface != "" {
		text = p.preface + "\n\n" + text
	}
	p.h.consulting(true)
	defer p.h.consulting(false)

	next := append(append([]anthropic.MessageParam(nil), p.history...),
		anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
	p.rec.add(TranscriptEvent{Kind: TranscriptUser, Text: text})
	res := p.agent.RunTurn(p.own(ctx), next, p.toolTrail())
	p.rec.flush()
	var err error
	switch {
	case res.StopReason == agent.Cancelled:
		err = context.Canceled
	case strings.HasPrefix(res.StopReason, "error: "):
		err = fmt.Errorf("%s", strings.TrimPrefix(res.StopReason, "error: "))
	case res.StopReason == agent.StepLimit:
		res.Final, res.History, err = p.settle(ctx, p.agent, res.History, reviewOutOfBudget)
	}
	p.logExchange(kind, text, res.Final, res, err)
	if err != nil {
		return agent.Result{}, err
	}
	p.preface = ""
	p.history = res.History
	return res, nil
}

func verdictOf(reply string) string {
	reply = strings.TrimSpace(reply)
	switch {
	case reply == "":
		return VerdictEmpty
	case strings.EqualFold(strings.Trim(reply, " ."), none):
		return VerdictNone
	}
	return VerdictSaid
}

func emptyReply(stop string) string {
	if stop == "" {
		return "(empty reply)"
	}
	return "(empty reply, stop: " + firstLine(stop) + ")"
}

func (p *peer) logExchange(kind, sent, reply string, res agent.Result, err error) {
	if p.log == nil {
		return
	}
	rec := PeerExchange{
		Time:     time.Now(),
		Session:  p.session,
		Dispatch: p.dispatch,
		Kind:     kind,
		Sent:     sent,
		Reply:    reply,
		Thinking: lastThinking(res.History),
		Verdict:  verdictOf(reply),
		Stop:     res.StopReason,
		Steps:    p.steps,
		Prompt:   res.Usage.Prompt,
		Output:   res.Usage.Output,
	}
	if err != nil {
		rec.Verdict, rec.Err = VerdictFailed, err.Error()
	}
	p.log(rec)
}

// lastThinking is the thinking in the conversation's last message, when that message is a reply.
func lastThinking(history []anthropic.MessageParam) string {
	if len(history) == 0 || history[len(history)-1].Role != anthropic.MessageParamRoleAssistant {
		return ""
	}
	var parts []string
	for _, block := range history[len(history)-1].Content {
		if t := block.OfThinking; t != nil && t.Thinking != "" {
			parts = append(parts, t.Thinking)
		}
	}
	return strings.Join(parts, "\n\n")
}

// exhausted is what the driver is told when its questions run out. Not an error block: there is
// nothing to retry.
const exhausted = `No questions left — the budget for asking the navigator is spent. Carry on with your own judgement, and say in your report where you would have asked.`

// handler is the driver's ask tool. It continues the review conversation, so the peer answers
// with its plan and steers in mind.
func (p *peer) handler() agent.Handler {
	return func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Question string `json:"question"`
			Work     string `json:"work"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
		}
		if strings.TrimSpace(in.Question) == "" {
			return anthropic.NewToolResultBlock(call.ID, "question is required", true)
		}

		p.mu.Lock()
		defer p.mu.Unlock()
		if p.asked >= p.spec.rounds() {
			return anthropic.NewToolResultBlock(call.ID, exhausted, false)
		}
		res, err := p.exchange(ctx, ExchangeQuestion, "The driver asks:\n"+message(in.Question, in.Work))
		if err != nil {
			return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
		}
		p.asked++
		asked := Entry{Kind: EntryExchange, From: "driver", To: p.spec.Def.Name, Prompt: firstLine(in.Question)}
		reply := strings.TrimSpace(res.Final)
		if reply == "" {
			asked.Text = emptyReply(res.StopReason)
			p.h.appendTrail(asked)
			return anthropic.NewToolResultBlock(call.ID, "It had nothing to say.", false)
		}

		asked.Text = reply
		p.h.appendTrail(asked)
		if cb.OnToolResult != nil {
			cb.OnToolResult(call.ID, p.spec.Def.Name+" · "+firstLine(reply), reply, agent.KindAgent)
		}
		return anthropic.NewToolResultBlock(call.ID, reply, false)
	}
}

// closing is what the peer is asked once the driver has stopped, for the dispatching agent: the
// driver writes the report and may have overridden the peer, so the peer gets its own word.
const closing = `The driver has finished and you will not be asked again. In three sentences at most, for the agent that dispatched this pair and saw none of this conversation: what the driver did, and anything you raised that the diff you were shown does not address. If you raised nothing, or everything you raised was dealt with, say exactly that. Judge the diff, not the driver's account of it.`

const closingUnfinished = `The driver ran out of turns before finishing, so there was no end-of-turn review, and the diff below is the first you have seen of the state it left. In three sentences at most, for the agent that dispatched this pair and saw none of this conversation: say that the work is unfinished, what the driver got done, and anything you raised that the diff does not address.`

func (p *peer) own(ctx context.Context) context.Context {
	if p.shell == nil {
		return ctx
	}
	return shell.With(ctx, p.shell)
}

// close asks for the closing view and records it as the peer's result, down every exit path so
// the row never stays live. Anything the driver did after the last review is shown with it.
func (p *peer) close(ctx context.Context, stop string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	summary := ""
	switch {
	case p.reviews == 0:
		summary = "No checkpoint was reached. "
	case p.steered == 0:
		summary = "Stayed silent at every checkpoint. "
	}
	if ctx.Err() != nil || stop == StopCancelled || stop == StopError {
		p.h.finish(strings.TrimSpace(summary))
		return ""
	}

	p.flushProse()
	instruction := closing
	if stop != StopDone {
		instruction = closingUnfinished
	}
	if len(p.delta) > 0 || p.endReviewed == 0 {
		instruction += "\n\nWhat the driver did after your last look:\n" + p.takeDelta() + "\n\n" + p.diff()
	}
	if p.preface != "" {
		instruction = p.preface + "\n\n" + instruction
	}
	p.h.consulting(true)
	p.steps = 0
	p.rec.add(TranscriptEvent{Kind: TranscriptUser, Text: instruction})
	said, err := p.agent.FinalizeWith(ctx, p.history, instruction, 1000)
	p.rec.finish(said, err)
	p.logExchange(ExchangeClosing, instruction, said, agent.Result{}, err)
	p.h.finish(summary + said)
	return said
}

// message is what the peer receives from the driver. The work is fenced so that a long excerpt
// cannot be read as part of the instruction.
func message(question, work string) string {
	text := strings.TrimSpace(question)
	if w := strings.TrimSpace(work); w != "" {
		text += "\n\n<work>\n" + w + "\n</work>"
	}
	return text
}
