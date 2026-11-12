package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

// RequestKind is what a detached run is blocked on the main agent for; foreground runs never ask.
type RequestKind string

const (
	KindQuestion  RequestKind = "question"
	KindExtension RequestKind = "extension"
)

const (
	AskTool    = "ask_main"
	ReplyTool  = "agent_reply"
	ExtendTool = "agent_extend"
)

// userEvery is how often an extension needs the user rather than the main agent: every sixth.
const userEvery = 6

// askWithin bounds how long a run waits on the main agent before carrying on without an answer.
var askWithin = 15 * time.Minute

const goneAnswer = "The conversation that dispatched you has been cleared, so nobody will answer. Use your own judgement, and say in your report what you assumed."

// Request is one thing a run is waiting on the main agent for.
type Request struct {
	Run   int
	Label string
	Agent string
	Kind  RequestKind
	Text  string
	// Steps is what the run has used so far, for an extension.
	Steps int
}

type answer struct {
	text  string
	grant bool
}

type pendingRequest struct {
	Request
	delivered bool
	reply     chan answer
}

// post registers a run's request and wakes the session; refused once its conversation is cleared.
func (r *Roster) post(h *handle, kind RequestKind, text string) (<-chan answer, error) {
	r.mu.Lock()
	if h.generation != r.generation {
		r.mu.Unlock()
		return nil, fmt.Errorf("the conversation that dispatched this run has been cleared")
	}
	if r.requests == nil {
		r.requests = map[int]*pendingRequest{}
	}
	p := &pendingRequest{
		Request: Request{Run: h.run.ID, Label: h.run.Label(), Agent: h.run.Agent, Kind: kind, Text: text, Steps: h.run.Step},
		reply:   make(chan answer, 1),
	}
	r.requests[h.run.ID] = p
	wake := r.OnOutcome
	r.mu.Unlock()
	r.changed()
	if wake != nil {
		wake()
	}
	return p.reply, nil
}

func (r *Roster) withdraw(run int) {
	r.mu.Lock()
	delete(r.requests, run)
	r.mu.Unlock()
}

// TakeRequests returns the requests the main agent has not been shown yet, oldest run first.
func (r *Roster) TakeRequests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Request
	for _, run := range r.runs {
		if p, ok := r.requests[run.ID]; ok && !p.delivered {
			p.delivered = true
			out = append(out, p.Request)
		}
	}
	return out
}

// waiting returns the run's outstanding request of kind, if it has one.
func (r *Roster) waiting(run int, kind RequestKind) (Request, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.requests[run]
	if !ok || p.Kind != kind {
		return Request{}, false
	}
	return p.Request, true
}

func (r *Roster) respond(run int, kind RequestKind, a answer) error {
	r.mu.Lock()
	p, ok := r.requests[run]
	if !ok || p.Kind != kind {
		r.mu.Unlock()
		return fmt.Errorf("run %d is not waiting on a %s", run, kind)
	}
	delete(r.requests, run)
	if kind == KindExtension && a.grant {
		for _, candidate := range r.runs {
			if candidate.ID == run {
				candidate.Extensions++
			}
		}
	}
	r.mu.Unlock()
	p.reply <- a
	r.changed()
	return nil
}

// dropRequests answers everything outstanding with goneAnswer. Called with the roster locked.
func (r *Roster) dropRequests() {
	for id, p := range r.requests {
		p.reply <- answer{text: goneAnswer}
		delete(r.requests, id)
	}
}

// RequestBlock is how waiting requests are put to the main agent.
func RequestBlock(requests []Request) string {
	if len(requests) == 0 {
		return ""
	}
	parts := make([]string, 0, len(requests))
	for _, q := range requests {
		who := fmt.Sprintf("run %d (%s · %s)", q.Run, q.Label, q.Agent)
		switch q.Kind {
		case KindExtension:
			parts = append(parts, fmt.Sprintf("%s has used its %d turns and asks for more. Its account:\n%s\nGrant or refuse with %s.",
				who, q.Steps, strings.TrimSpace(q.Text), ExtendTool))
		default:
			parts = append(parts, fmt.Sprintf("%s asks:\n%s\nAnswer with %s.", who, strings.TrimSpace(q.Text), ReplyTool))
		}
	}
	return "<subagent-requests>\nThese detached runs are blocked until you answer. One left unanswered for " +
		short(askWithin) + " carries on without you (a question) or stops (more turns).\n\n" +
		strings.Join(parts, "\n\n") + "\n</subagent-requests>"
}

func askMainTool() anthropic.ToolUnionParam {
	return strict(AskTool,
		"Ask the agent that dispatched you a question, and wait for its answer. It holds context you do not: the wider "+
			"plan, the user's preferences, what other runs are doing. Ask only when you are blocked on something you cannot "+
			"find out yourself and guessing wrong would waste the work. It does not spend your turns. If no answer comes "+
			"you are told so, and should carry on with your best judgement.",
		map[string]any{"question": map[string]any{"type": "string",
			"description": "The question, standing on its own: what you are deciding, the options, and what you would do by default."}},
		[]string{"question"})
}

// offerAsk gives one run ask_main, never cleared, under a copied policy so other runs are not widened.
func offerAsk(inner *agent.Agent, handler agent.Handler, h *handle) {
	offered := inner.Tools
	inner.Tools = func() []anthropic.ToolUnionParam {
		var tools []anthropic.ToolUnionParam
		if offered != nil {
			tools = offered()
		}
		return append(tools, askMainTool())
	}
	inner.Handlers[AskTool] = announce(handler, h)
	inner.ExcludeTools = append(inner.ExcludeTools, AskTool)
	free := map[string]bool{AskTool: true}
	for name := range inner.Free {
		free[name] = true
	}
	inner.Free = free
	if inner.Policy != nil {
		rules := *inner.Policy
		rules.Tools = append(append([]agent.Rule(nil), rules.Tools...), agent.Rule{Pattern: AskTool, Verdict: agent.VerdictAllow})
		inner.Policy = &rules
	}
}

func (m *Manager) askMain(h *handle) agent.Handler {
	return func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Question string `json:"question"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil || strings.TrimSpace(in.Question) == "" {
			return anthropic.NewToolResultBlock(call.ID, "question is required", true)
		}
		h.activity("asking the main agent")
		ch, err := m.Roster.post(h, KindQuestion, in.Question)
		if err != nil {
			return anthropic.NewToolResultBlock(call.ID, goneAnswer, false)
		}
		said, failed := "", false
		select {
		case a := <-ch:
			said = a.text
		case <-time.After(askWithin):
			m.Roster.withdraw(h.run.ID)
			said, failed = "No answer came within "+short(askWithin)+". Use your own judgement, and say in your report what you assumed.", true
		case <-ctx.Done():
			m.Roster.withdraw(h.run.ID)
			return anthropic.NewToolResultBlock(call.ID, "Interrupted.", true)
		}
		h.appendTrail(Entry{Kind: EntryExchange, From: h.run.Agent, To: "main", Prompt: in.Question, Text: said, Failed: failed})
		return anthropic.NewToolResultBlock(call.ID, said, false)
	}
}

// wantMore asks a run that has spent its budget for the account its extension request carries.
const wantMore = `You have used all your turns and the task is unfinished. Do not call tools. Reply with one short paragraph for the agent that dispatched you, who will decide whether you get more: what you completed, then "Still unfinished:" and what remains, and roughly how many more turns you need.`

// extend asks for another budget, returning the grant and the run's account, its report on a no.
func (m *Manager) extend(ctx context.Context, inner *agent.Agent, history []anthropic.MessageParam, h *handle) (bool, string) {
	account := inner.Finalize(ctx, history, wantMore)
	h.activity("asking for more turns")
	ch, err := m.Roster.post(h, KindExtension, account)
	if err != nil {
		return false, account
	}
	select {
	case a := <-ch:
		return a.grant, account
	case <-time.After(askWithin):
		m.Roster.withdraw(h.run.ID)
		return false, account
	case <-ctx.Done():
		m.Roster.withdraw(h.run.ID)
		return false, account
	}
}

// resumed is the history a granted run carries on from, told it has more turns.
func resumed(history []anthropic.MessageParam, steps int) []anthropic.MessageParam {
	note := anthropic.NewTextBlock(fmt.Sprintf("You have been given %d more turns. Carry on with the task.", steps))
	out := append([]anthropic.MessageParam(nil), history...)
	if n := len(out); n > 0 && out[n-1].Role == anthropic.MessageParamRoleUser {
		out[n-1].Content = append(append([]anthropic.ContentBlockParamUnion(nil), out[n-1].Content...), note)
		return out
	}
	return append(out, anthropic.NewUserMessage(note))
}

func (m *Manager) runReply(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Run    *int   `json:"run"`
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil || in.Run == nil {
		return anthropic.NewToolResultBlock(call.ID, "run and answer are required", true)
	}
	if err := m.Roster.respond(*in.Run, KindQuestion, answer{text: in.Answer}); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("Answered run %d.", *in.Run), false)
}

// runExtend grants or refuses more turns; every sixth grant to one run goes to the user, counted here.
func (m *Manager) runExtend(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Run   *int `json:"run"`
		Grant bool `json:"grant"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil || in.Run == nil {
		return anthropic.NewToolResultBlock(call.ID, "run and grant are required", true)
	}
	q, ok := m.Roster.waiting(*in.Run, KindExtension)
	if !ok {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("run %d is not waiting on an extension", *in.Run), true)
	}
	run, _ := m.Roster.Find(*in.Run)
	if in.Grant && (run.Extensions+1)%userEvery == 0 {
		label := fmt.Sprintf("extend agent %s, run %d (%s) — extension %d", q.Agent, q.Run, q.Label, run.Extensions+1)
		reason := fmt.Sprintf("every %dth extension of one run is yours to approve", userEvery)
		if cb.Require == nil || !cb.Require(ctx, label, q.Text, reason) {
			_ = m.Roster.respond(*in.Run, KindExtension, answer{})
			return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("The user declined extension %d, so run %d stops "+
				"and reports what it finished.", run.Extensions+1, *in.Run), true)
		}
	}
	if err := m.Roster.respond(*in.Run, KindExtension, answer{grant: in.Grant}); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if !in.Grant {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("Refused. Run %d stops and its report reaches you.", *in.Run), false)
	}
	next := run.Extensions + 1
	left := userEvery - 1 - next%userEvery
	return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("Granted extension %d to run %d. You can grant %d more "+
		"before the user has to approve one.", next, *in.Run, left), false)
}
