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

// DispatchTool is the generic one. Aliases sit beside it, never instead of it.
const DispatchTool = "task"

const (
	StatusTool = "agent_status"
	CancelTool = "agent_cancel"
)

// recentTrail is how many trail lines a single-run status carries.
const recentTrail = 5

// backgroundArg is the override, offered on the generic tool and on every alias so the choice
// does not depend on which door was used.
//
// Written around false rather than true, because true is now the default for every agent,
// and an argument you would only ever set to its default value is an argument nobody reads.
// What the model actually has to decide is when to wait.
var backgroundArg = map[string]any{
	"type": "boolean",
	"description": "Whether to run this detached. Every agent detaches by default: the call returns at once and the " +
		"report reaches you when it lands, mid-turn if you are still working and in a turn of its own if you have stopped. " +
		"Set it to false to wait for the report in the tool result instead. Wait when the next thing you do depends on the " +
		"answer, when you want to see an edit land before building on it, or when there is nothing else you could usefully " +
		"do meanwhile — waiting is a normal choice, not a failure to parallelise.",
}

func strict(name, description string, properties map[string]any, required []string) anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        name,
		Description: anthropic.String(description),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties:  properties,
			Required:    required,
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}
}

// SystemNote is the main agent's guidance on when to delegate, empty when no agent is registered.
func (m *Manager) SystemNote() string {
	if len(m.order) == 0 {
		return ""
	}
	note := "Delegate with the `" + DispatchTool + "` tool when work splits into independent pieces: several questions, a " +
		"search across many files or pages, review from more than one angle, or a change you can state completely. " +
		"Dispatch the pieces in one reply so they run at once, then carry on with your own part while they run. Keep " +
		"work in your own hands when each step depends on the last, as in a debugging loop, or when it takes only a few " +
		"tool calls. Give each agent its own slice, and never split one change between two coders: they don't see each " +
		"other's work."
	if _, ok := m.defs["verifier"]; ok {
		note += " Browser work beyond one quick look goes to the verifier, since snapshots and screenshots fill your context."
	}
	return note
}

// catalogueLine reads "- name (model; smaller tasks on medium or small): description", routing included only when on.
func (m *Manager) catalogueLine(def *Definition) string {
	cost := def.model()
	if def.Explore && m.Routing != nil && m.Tier != nil {
		if r, ok := m.Routing(); ok {
			cost += fmt.Sprintf("; smaller tasks on %s or %s", r.Medium, r.Small)
		}
	}
	if def.Peer != nil {
		cost += ", plus a " + def.Peer.Def.Name + " on " + def.Peer.Def.model()
	}
	return "- " + def.Name + " (" + cost + "): " + def.Description
}

// Tools is the dispatch tool plus one per alias. Empty when nothing is registered, so the
// feature disappears rather than offering an agent that does not exist.
func (m *Manager) Tools() []anthropic.ToolUnionParam {
	if len(m.order) == 0 {
		return nil
	}

	var work, upkeep []string
	for _, name := range m.order {
		line := m.catalogueLine(m.defs[name])
		if m.defs[name].Upkeep {
			upkeep = append(upkeep, line)
		} else {
			work = append(work, line)
		}
	}
	catalogue := strings.Join(work, "\n")
	if len(upkeep) > 0 {
		catalogue += "\n\nUpkeep agents, which alux usually starts on its own. Dispatch one only for upkeep the user " +
			"asked for, or that a skill step names:\n" + strings.Join(upkeep, "\n")
	}

	tools := []anthropic.ToolUnionParam{
		strict(DispatchTool,
			"Hand a self-contained piece of work to a subagent. It runs with its own context and its own tools, and returns a "+
				"summary — you never see its transcript, which is the point: the work costs you a paragraph instead of twenty tool "+
				"calls. Dispatch several in one reply when the pieces are independent and they will run at the same time.\n\n"+
				"Dispatching does not stop you working. A subagent runs detached unless you ask to wait for it, so the call comes "+
				"back at once and the report arrives later — while you are still working, or in a new turn if you have finished. "+
				"Carry on with something else rather than idling until it lands, and do not re-dispatch work you are already "+
				"waiting on. When the answer is the next thing you need, pass background: false and wait for it. Check on a "+
				"detached run with agent_status and stop one with agent_cancel.\n\n"+
				"Available agents, each with its model:\n"+catalogue+"\n\n"+
				"A foreground subagent cannot ask you anything once it has started; a detached one can ask you a question, and asks "+
				"for more turns when its budget runs out. Neither can dispatch subagents of its own, so the brief has to "+
				"stand on its own. Vague briefs are the usual failure: two agents told \"look into X\" run the same searches, and "+
				"one told \"fix X\" stops at the first plausible patch.",
			map[string]any{
				"agent": map[string]any{
					"type":        "string",
					"enum":        m.order,
					"description": "Which agent to run.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": `A 2–4 word label for the work, shown to the user, e.g. "render flicker" or "pr 412 review".`,
				},
				"objective": map[string]any{
					"type":        "string",
					"description": "What you want and why, in a sentence or two, for someone who cannot see this conversation.",
				},
				"done": map[string]any{
					"type": "string",
					"description": "What counts as finished, ideally a check that can fail: a test or command that passes, or a " +
						"question answered with file and line — and for a search, where it looked when it finds nothing.",
				},
				"scope": map[string]any{
					"type": "string",
					"description": "Where to look and what to use: files, packages or sources to start from, and the tools to " +
						"prefer. Leave it out only when the agent should find its own way in.",
				},
				"boundaries": map[string]any{
					"type": "string",
					"description": "What not to touch, what another agent already owns, and when to stop and report instead of " +
						"pressing on. Leave it out only when there is genuinely nothing to fence off.",
				},
				"report": map[string]any{
					"type":        "string",
					"description": "What the final message should contain, which is all that comes back to you. Keep it short.",
				},
				"context": map[string]any{
					"type": "string",
					"description": "Optional material to hand over verbatim — a snippet, an error, paths, a prior finding. It is " +
						"reproduced as given, so put here what must not be paraphrased.",
				},
				"background": backgroundArg,
				"base": map[string]any{
					"type": "string",
					"enum": []string{BaseParent, BaseTrunk},
					"description": "Where a coder or pair's workspace starts. \"@-\", the default, is the last commit of your working " +
						"copy, and its change lands into @ with land_run. \"main\" is the repo's trunk, fetched first, for work " +
						"that ships as its own PR: its change cannot be landed into @, only pushed or dropped. Other agents take no base.",
				},
				"seed": map[string]any{
					"type": "array",
					"description": "Files or ranges you have viewed this session whose current contents the subagent needs for " +
						"the work. Each is re-read fresh and shown as the subagent's own view. Use view_range when only part of a " +
						"large file matters. Do not list files you only searched or skimmed, or whole directories, and do not " +
						"mention seeds in the brief. Only coder, pair and research take seeds.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":       map[string]any{"type": "string"},
							"view_range": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 2, "maxItems": 2},
						},
						"required":             []string{"path"},
						"additionalProperties": false,
					},
				},
			},
			[]string{"agent", "name", "objective", "done", "report"}),
	}

	runArg := func(description string) map[string]any {
		return map[string]any{"run": map[string]any{"type": "integer", "description": description}}
	}
	tools = append(tools,
		strict(StatusTool,
			"See what your subagents are doing: state, step against budget, how long since the last step, and current "+
				"activity. Without run it lists the live ones; with run it adds that run's recent trail, finished runs included. "+
				"Use it before deciding a long run is stuck, and before re-dispatching work that may still be going.",
			runArg("The run id, from the dispatch result or from a listing. Omit to list every live run."), nil),
		strict(CancelTool,
			"Stop a live subagent run. It stops at once, then writes a closing report of what it finished, what state it "+
				"left behind and what remains; that report reaches you like any other outcome. Use it when a run is stuck, "+
				"going the wrong way, or no longer needed. Check agent_status first if you are not sure.",
			runArg("The run id to stop."), []string{"run"}),
		strict(ReplyTool,
			"Answer a question a detached run put to you with ask_main. The run is blocked until you do.",
			map[string]any{
				"run":    map[string]any{"type": "integer", "description": "The run that asked."},
				"answer": map[string]any{"type": "string", "description": "Your answer, standing on its own."},
			}, []string{"run", "answer"}),
		strict(ExtendTool,
			"Grant or refuse more turns to a detached run that has used its budget. A granted run carries on with a fresh "+
				"budget of the same size; a refused one stops and reports. Grant when its account shows real progress and a "+
				"clear remainder; refuse when it is going in circles. Every sixth grant to one run is put to the user.",
			map[string]any{
				"run":   map[string]any{"type": "integer", "description": "The run asking."},
				"grant": map[string]any{"type": "boolean", "description": "true to grant another budget, false to stop it."},
			}, []string{"run", "grant"}),
	)
	if m.Layers != nil {
		tools = append(tools, m.landTool())
	}

	for _, name := range m.order {
		alias := m.defs[name].Alias
		if alias == nil {
			continue
		}
		tools = append(tools, strict(alias.Name, alias.Description,
			map[string]any{
				"task":       map[string]any{"type": "string", "description": alias.TaskArg},
				"background": backgroundArg,
			},
			[]string{"task"}))
	}
	return tools
}

// Handlers is the dispatch table for those tools, merged into the agent's by main.
func (m *Manager) Handlers() map[string]agent.Handler {
	if len(m.order) == 0 {
		return nil
	}
	handlers := map[string]agent.Handler{
		DispatchTool: m.runDispatch, StatusTool: m.runStatus, CancelTool: m.runCancel,
		ReplyTool: m.runReply, ExtendTool: m.runExtend,
	}
	if m.Layers != nil {
		handlers[LandTool] = m.runLand
	}
	for _, name := range m.order {
		if alias := m.defs[name].Alias; alias != nil {
			handlers[alias.Name] = m.aliasHandler(name)
		}
	}
	return handlers
}

// Dispatches reports whether name is a tool that starts a run: the dispatch tool or an agent's alias.
func (m *Manager) Dispatches(name string) bool {
	if name == DispatchTool {
		return true
	}
	for _, n := range m.order {
		if alias := m.defs[n].Alias; alias != nil && alias.Name == name {
			return true
		}
	}
	return false
}

// Concurrent names the tools that may run alongside each other. Every dispatch qualifies: a
// subagent either runs detached or blocks only its own tool call, and none of them stops the
// parent for approval, which is the thing that cannot be done twice at once.
func (m *Manager) Concurrent() map[string]bool {
	out := map[string]bool{DispatchTool: true}
	for _, name := range m.order {
		if alias := m.defs[name].Alias; alias != nil {
			out[alias.Name] = true
		}
	}
	return out
}

// Background is a pointer so that unset is distinguishable from false: unset takes the agent's
// default, where false is the model actively asking to wait for an agent that would not have.
type dispatchInput struct {
	Agent      string `json:"agent"`
	Name       string `json:"name"`
	Objective  string `json:"objective"`
	Done       string `json:"done"`
	Scope      string `json:"scope"`
	Boundaries string `json:"boundaries"`
	Report     string `json:"report"`
	Context    string `json:"context"`
	Background *bool  `json:"background"`
	Seed       []Seed `json:"seed"`
	Base       string `json:"base"`
}

// Objective leads so a run without a name is named by its objective's first words.
func (in dispatchInput) brief() (string, error) {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"objective", in.Objective}, {"done", in.Done}, {"report", in.Report},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("the brief has no %s: a subagent sees only the brief, so say it and dispatch again",
			strings.Join(missing, " and no "))
	}
	parts := []string{strings.TrimSpace(in.Objective), "Done when: " + strings.TrimSpace(in.Done)}
	if s := strings.TrimSpace(in.Scope); s != "" {
		parts = append(parts, "Where to look: "+s)
	}
	if s := strings.TrimSpace(in.Boundaries); s != "" {
		parts = append(parts, "Boundaries: "+s)
	}
	parts = append(parts, "Report: "+strings.TrimSpace(in.Report))
	return strings.Join(parts, "\n\n"), nil
}

func (m *Manager) runDispatch(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in dispatchInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	task, err := in.brief()
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	return m.dispatch(ctx, call, in.Agent, in.Name, task, in.Context, in.Base, in.Background, in.Seed, cb)
}

// aliasHandler is the named front door onto one agent. It takes the same path as the generic
// tool — the alias is a description, not a second mechanism.
func (m *Manager) aliasHandler(name string) agent.Handler {
	return func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Task       string `json:"task"`
			Background *bool  `json:"background"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
		}
		return m.dispatch(ctx, call, name, "", in.Task, "", "", in.Background, nil, cb)
	}
}

type runInput struct {
	Run *int `json:"run"`
}

func (m *Manager) runStatus(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in runInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if in.Run == nil {
		var lines []string
		for _, run := range m.Roster.Runs() {
			if run.Live() && run.Parent == 0 {
				lines = append(lines, statusLine(run, time.Now()))
			}
		}
		if len(lines) == 0 {
			return anthropic.NewToolResultBlock(call.ID, "No subagents are running.", false)
		}
		return anthropic.NewToolResultBlock(call.ID, strings.Join(lines, "\n"), false)
	}
	run, ok := m.Roster.Find(*in.Run)
	if !ok {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("no run %d", *in.Run), true)
	}
	return anthropic.NewToolResultBlock(call.ID, statusDetail(run, time.Now()), false)
}

func (m *Manager) runCancel(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in runInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if in.Run == nil {
		return anthropic.NewToolResultBlock(call.ID, "run is required", true)
	}
	if err := m.Roster.Stop(*in.Run); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("Stopping run %d. Its closing report will reach you "+
		"like any other outcome; do not wait on it.", *in.Run), false)
}

func statusLine(run Run, now time.Time) string {
	parts := []string{fmt.Sprintf("run %d", run.ID), run.Label(), run.Agent, string(run.State)}
	if run.Live() {
		parts = append(parts, fmt.Sprintf("step %d/%d", run.Step, run.MaxSteps), short(run.Elapsed()))
		if !run.LastStep.IsZero() {
			parts = append(parts, "last step "+short(now.Sub(run.LastStep))+" ago")
		}
		if run.Activity != "" {
			parts = append(parts, run.Activity)
		}
	} else {
		parts = append(parts, "after "+short(run.Elapsed()))
	}
	return strings.Join(parts, " · ")
}

func statusDetail(run Run, now time.Time) string {
	out := statusLine(run, now)
	if run.Err != "" {
		out += "\nError: " + firstLine(run.Err)
	} else if run.Result != "" {
		out += "\nResult: " + firstLine(run.Result)
	}
	trail := run.Trail
	if len(trail) > recentTrail {
		trail = trail[len(trail)-recentTrail:]
	}
	if len(trail) > 0 {
		out += "\nRecent:"
		for _, e := range trail {
			out += "\n- " + firstLine(e.String())
		}
	}
	return out
}

// label is the run's short name; empty takes the first words of the task.
func (m *Manager) dispatch(ctx context.Context, call agent.Call, name, label, task, extra, base string, detach *bool, seeds []Seed, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	id := call.ID
	if strings.TrimSpace(task) == "" {
		return anthropic.NewToolResultBlock(id, "task is required", true)
	}

	def, ok := m.defs[name]
	if !ok {
		return anthropic.NewToolResultBlock(id,
			fmt.Sprintf("no such agent %q; available: %s", name, strings.Join(m.order, ", ")), true)
	}

	// Approved on the task, not on what the subagent will go on to do. A read-only agent has
	// nothing to approve past this point; an editing one approves its own writes through the
	// callbacks it is handed, under the same mutex this takes.
	//
	// One at a time, even though dispatches themselves run concurrently. The session holds a
	// single parked approval, so a second prompt raised while the first is waiting overwrites
	// it and the first caller waits on a channel nobody will ever answer. Serialising here
	// turns that deadlock into a queue of prompts, which is what the user sees anyway: there is
	// one screen and one y/n.
	if cb.Approve != nil {
		m.approving.Lock()
		approved := cb.Approve(ctx, "agent "+def.Name+" — "+firstLine(task), task)
		m.approving.Unlock()
		if !approved {
			return anthropic.NewToolResultBlock(id, "User declined this dispatch.", true)
		}
	}
	if ctx.Err() != nil {
		return anthropic.NewToolResultBlock(id, "Interrupted by the user.", true)
	}

	report, err := m.dispatchWith(ctx, call, name, label, task, extra, base, detach, seeds, cb)
	if err != nil {
		return anthropic.NewToolResultBlock(id, err.Error(), true)
	}
	if strings.TrimSpace(report) == "" {
		report = "The agent finished without reporting anything."
	}
	return anthropic.NewToolResultBlock(id, report, false)
}
