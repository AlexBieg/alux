// Package skillrun holds one agent's open skill run: its tools, its call log and what it preapproves.
package skillrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/flow"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/anthropics/anthropic-sdk-go"
)

// Starter is who opened a run, which decides what it preapproves.
type Starter string

const (
	ByUser  Starter = "user"
	ByModel Starter = "model"
)

const (
	StartTool       = "start_skill"
	FinishTool      = "finish_skill"
	FinishStageTool = flow.Tool
	FailTool        = "fail_run"
)

// Finished is how a run closed by finish_skill ends.
const Finished = "finished"

// View is the open run as a pane draws it.
type View struct {
	Name     string
	Nodes    []skills.NodeResult
	Edges    []skills.Edge
	Complete bool
	Waivers  []skills.Waiver
}

// Host is one agent's skill runs, at most one open at a time.
type Host struct {
	// Changed reports the open run after every change, and nil once it closes.
	Changed func(*View)
	// Closed reports a run's final check once it ends; how says who ended it.
	Closed func(s *skills.Skill, res skills.Result, how string)
	// Dir is where skill files live; nil means skills.Dir.
	Dir func() (string, error)
	// Now and Home bind a run's ${now_utc} and ~/; nil means the clock and the user's home.
	Now  func() time.Time
	Home func() string
	// Pinned never offers start_skill, for an agent whose owner opens its one run with Start.
	Pinned bool
	// Lands names the dispatch tools, which Observe skips because Land logs them when their run ends.
	Lands func(tool string) bool
	// PollOf is the process id a poll call started, 0 for none, which PollEnded's id is matched against.
	PollOf func(call string) int
	// Dispatch starts a detached subagent running skill pinned for a spawn stage, editing jj change in place when it is set;
	// Land hears call when it ends. Nil means this agent cannot run a flow with spawn stages.
	Dispatch func(ctx context.Context, call agent.Call, agent, skill string, inputs map[string]any, change, task string, cb agent.Callbacks) (int, error)
	// Stop cancels the subagent run with that id, for a flow that ends while a spawn it started is running.
	Stop func(run int) error
	// CheckDispatch says why agent cannot run skill, or nil.
	CheckDispatch func(agent, skill string) error

	mu    sync.Mutex
	open  *run
	base  func() []anthropic.ToolUnionParam
	agent *agent.Agent
	stepN int

	startOnce   sync.Once
	startOffer  anthropic.ToolUnionParam
	offersStart bool

	// asked is the user's latest message, which lets the model start a user-only skill it names.
	asked string
}

type run struct {
	// run is the checklist being checked: the skill itself, or a flow's open checklist stage.
	run *skills.Run
	// skill is the file that was started, which a flow's checklists are not.
	skill *skills.Skill
	// flow is set for a flow, whose own inputs bound fills run stages and its checklists.
	flow       *flow.Flow
	bound      *skills.Run
	checklists map[string]*skills.Skill
	tools      []string
	by         Starter
	log        []skills.Event
	env        skills.Env
	// closing is how the run ends, set by finish_skill or fail_run and acted on by Observe.
	closing string
	// leading is set while a flow's opening run stages wait for a turn to run in.
	leading bool
	// spawned maps each running spawn stage's call id to its subagent run id, for Land and for stopping it.
	spawned map[string]int
	// items maps a parallel foreach's spawn call id to the item it runs.
	items map[string]spawnItem
	// landed is what alux tells the model about spawns that landed, sent with the next brief.
	landed []string
	// unscreened is each step call whose command an input supplied whole, which is never preapproved.
	unscreened map[string]bool
	// noticed is each node and pending predecessor pair whose progress notice was sent this run.
	noticed map[[2]string]bool
	// waivers is every step or stage the model waived, each named as the report shows it.
	waivers []skills.Waiver
}

// Wire offers finish_skill, fail_run and, unless Pinned, start_skill for the whole session, so the tool list never changes as runs open and close.
func (h *Host) Wire(a *agent.Agent) {
	h.mu.Lock()
	h.base = a.Tools
	h.agent = a
	h.mu.Unlock()
	a.Tools = h.tools
	a.OnTurnStart = h.lead
	a.SkillNotices = h.progress
	if a.Handlers == nil {
		a.Handlers = map[string]agent.Handler{}
	}
	a.Handlers[StartTool] = h.startTool
	a.Handlers[FinishTool] = h.finish
	a.Handlers[FinishStageTool] = h.finishStage
	a.Handlers[FailTool] = h.fail
	a.Preapproved = h.Preapproved
	a.PreapprovedStep = h.PreapprovedStep
}

func (h *Host) tools() []anthropic.ToolUnionParam {
	offered := h.offered()
	if !h.Pinned {
		h.startOnce.Do(func() { h.startOffer, h.offersStart = h.startParam(names(offered)) })
		if h.offersStart {
			offered = append(offered, h.startOffer)
		}
	}
	return append(offered, finishParam(), flow.Param(), failParam())
}

func (h *Host) offered() []anthropic.ToolUnionParam {
	h.mu.Lock()
	base := h.base
	h.mu.Unlock()
	if base == nil {
		return nil
	}
	return base()
}

func names(tools []anthropic.ToolUnionParam) []string {
	var out []string
	for _, t := range tools {
		if n := t.GetName(); n != nil {
			out = append(out, *n)
		}
	}
	return out
}

func closed(tool string) bool { return taint.Tainted() && agent.ClosedByTaint(tool) }

// Preapproved is true for a tool the open run declares, and only for a run the user started.
func (h *Host) Preapproved(name string) bool {
	h.mu.Lock()
	r := h.open
	h.mu.Unlock()
	if r == nil || r.by != ByUser {
		return false
	}
	req := r.skill.Requires
	return slices.Contains(req.Tools, name) || slices.Contains(req.OptionalTools, name)
}

// PreapprovedStep is true for a step call alux issued for a run the user started; the model cannot choose a call id.
func (h *Host) PreapprovedStep(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.open
	return r != nil && r.by == ByUser && strings.HasPrefix(id, stepPrefix) && !r.unscreened[id]
}

// Heard records the user's latest message, replacing the one before.
func (h *Host) Heard(text string) {
	h.mu.Lock()
	h.asked = text
	h.mu.Unlock()
}

// named reports whether text names the skill as a run of whole words, ignoring case and whether
// its parts are joined by a hyphen, an underscore, a space or nothing.
func named(text, skill string) bool {
	want := strings.FieldsFunc(strings.ToLower(skill), notWord)
	if len(want) == 0 {
		return false
	}
	words := strings.FieldsFunc(strings.ToLower(text), notWord)
	joined := strings.Join(want, "")
	for i, w := range words {
		if w == joined || i+len(want) <= len(words) && slices.Equal(words[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func notWord(r rune) bool {
	return !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r > 127)
}

// Open reports whether a run is open, and its skill's name.
func (h *Host) Open() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open == nil {
		return "", false
	}
	return h.open.skill.Name, true
}

// Stage names the open flow's current stage, else the open skill, else "".
func (h *Host) Stage() string {
	h.mu.Lock()
	r := h.open
	h.mu.Unlock()
	if r == nil {
		return ""
	}
	if r.flow != nil {
		if st := r.flow.Stage(); st != "" {
			return st
		}
	}
	return r.skill.Name
}

// StartText is /skills run: the text after the name fills the skill's one required string input.
// It returns the brief that starts the agent on the run.
func (h *Host) StartText(name, text string) (string, error) {
	return h.start(name, ByUser, func(s *skills.Skill) (*skills.Run, error) {
		return skills.BindText(s, text, h.now(), h.home())
	})
}

// StartInputs opens a run the user asked for with every input already bound, and returns its brief.
func (h *Host) StartInputs(name string, inputs map[string]any) (string, error) {
	return h.start(name, ByUser, func(s *skills.Skill) (*skills.Run, error) {
		return skills.Bind(s, inputs, h.now(), h.home())
	})
}

// Start opens a run on the agent's behalf, as start_skill does, and returns its brief.
func (h *Host) Start(name string, inputs map[string]any) (string, error) {
	return h.start(name, ByModel, func(s *skills.Skill) (*skills.Run, error) {
		return skills.Bind(s, inputs, h.now(), h.home())
	})
}

func (h *Host) start(name string, by Starter, bind func(*skills.Skill) (*skills.Run, error)) (string, error) {
	if open, ok := h.Open(); ok {
		closer := "/skills end closes it"
		if by == ByModel {
			closer = "finish it or fail it first"
		}
		return "", fmt.Errorf("skill %s is still running; %s", open, closer)
	}
	skill, err := h.load(name)
	if err != nil {
		return "", fmt.Errorf("skill %s: %w", name, err)
	}
	if by == ByModel && !slices.Contains(skill.Starts, string(ByModel)) {
		h.mu.Lock()
		asked := h.asked
		h.mu.Unlock()
		if !slices.Contains(skill.Starts, string(ByUser)) || !named(asked, name) {
			return "", fmt.Errorf("skill %s starts only when the user names it in their latest message or runs it with /skills run", name)
		}
	}
	r, err := bind(skill)
	if err != nil {
		return "", fmt.Errorf("skill %s: %w", name, err)
	}
	offered := names(h.offered())
	if problems := skills.Unavailable(skill, offered, closed); len(problems) > 0 {
		return "", fmt.Errorf("skill %s cannot start: %w", name, problems)
	}
	tools := slices.DeleteFunc(slices.Clone(offered), closed)
	if skill.Flow() {
		brief, err := h.startFlow(r, by, offered, tools)
		if err != nil {
			return "", fmt.Errorf("skill %s cannot start: %w", name, err)
		}
		return brief, nil
	}
	h.mu.Lock()
	if h.open != nil {
		h.mu.Unlock()
		return "", fmt.Errorf("skill %s is still running", h.open.skill.Name)
	}
	h.open = &run{run: r, skill: skill, by: by, env: skills.LiveEnv(tools)}
	h.mu.Unlock()
	h.changed()
	return skills.Brief(r, tools), nil
}

func (h *Host) load(name string) (*skills.Skill, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return nil, errors.New("not a skill name")
	}
	dir, err := h.dir()
	if err != nil {
		return nil, err
	}
	return skills.Load(filepath.Join(dir, name+".yaml"))
}

func (h *Host) dir() (string, error) {
	if h.Dir != nil {
		return h.Dir()
	}
	return skills.Dir()
}

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Host) home() string {
	if h.Home != nil {
		return h.Home()
	}
	home, _ := os.UserHomeDir()
	return home
}

// Observe logs an answered call and closes the run once finish_skill or fail_run has; call it after the call's box is drawn.
func (h *Host) Observe(call agent.Call, res anthropic.ContentBlockParamUnion) {
	if call.Name == StartTool || strings.HasPrefix(call.ID, stepPrefix) {
		return
	}
	failed := res.OfToolResult != nil && res.OfToolResult.IsError.Or(false) || call.Exit != "" && call.Exit != "exit 0"
	dispatch := h.Lands != nil && h.Lands(call.Name)
	h.mu.Lock()
	inFlow := h.open != nil && h.open.flow != nil
	h.mu.Unlock()
	logged := call.Name != FinishTool && call.Name != FinishStageTool && call.Name != FailTool && (!dispatch || inFlow)
	proc := 0
	if logged && !failed && h.PollOf != nil {
		proc = h.PollOf(call.ID)
	}
	h.mu.Lock()
	a := h.agent
	h.mu.Unlock()
	var commands []string
	if logged && a != nil {
		commands = a.RuleCommands(call)
	}
	h.mu.Lock()
	r := h.open
	how := ""
	if r != nil {
		if logged {
			r.log = append(r.log, skills.Event{Tool: call.Name, Input: call.Input, Failed: failed, Commands: commands, Proc: proc})
		}
		how = r.closing
	}
	h.mu.Unlock()
	switch {
	case r == nil:
	case how != "":
		h.End(how)
	default:
		h.changed()
	}
}

// Land logs a dispatch once its subagent run has ended, with the run's pinned skill result if it had one.
// A flow counted the call when it was made, so its landing carries no tool and never counts as a second call.
func (h *Host) Land(call agent.Call, failed bool, check *skills.SkillCheck) {
	if h.landSpawn(call, failed, check) {
		return
	}
	h.mu.Lock()
	r := h.open
	if r != nil {
		e := skills.Event{Tool: call.Name, Input: call.Input, Failed: failed, Skill: check}
		if r.flow != nil {
			e.Tool, e.Input = "", nil
		}
		r.log = append(r.log, e)
	}
	h.mu.Unlock()
	if r != nil {
		h.changed()
	}
}

// PollEnded logs how the poll with process id proc ended.
func (h *Host) PollEnded(proc int, how skills.PollEnd) {
	h.mu.Lock()
	r := h.open
	if r != nil {
		r.log = append(r.log, skills.Event{Proc: proc, Ended: how})
	}
	h.mu.Unlock()
	if r != nil {
		h.changed()
	}
}

// End closes the open run and reports its final check; false when none is open.
func (h *Host) End(how string) bool {
	h.mu.Lock()
	r := h.open
	h.open = nil
	var log []skills.Event
	var waivers []skills.Waiver
	if r != nil {
		log, waivers = slices.Clone(r.log), slices.Clone(r.waivers)
	}
	h.mu.Unlock()
	if r == nil {
		return false
	}
	h.stopSpawns(r)
	if h.Changed != nil {
		h.Changed(nil)
	}
	if h.Closed != nil {
		h.Closed(r.skill, r.result(log, waivers), how)
	}
	return true
}

// Drop discards the open run without a report, as a cleared conversation does.
func (h *Host) Drop() {
	h.mu.Lock()
	r := h.open
	had := r != nil
	h.open = nil
	h.mu.Unlock()
	if had {
		h.stopSpawns(r)
	}
	if had && h.Changed != nil {
		h.Changed(nil)
	}
}

func (h *Host) check() (*run, skills.Result, bool) {
	h.mu.Lock()
	r := h.open
	var log []skills.Event
	var waivers []skills.Waiver
	if r != nil {
		log, waivers = slices.Clone(r.log), slices.Clone(r.waivers)
	}
	h.mu.Unlock()
	if r == nil {
		return nil, skills.Result{}, false
	}
	return r, r.result(log, waivers), true
}

// result is a flow's stages, or a checklist's nodes checked against log, with the run's waivers.
func (r *run) result(log []skills.Event, waivers []skills.Waiver) skills.Result {
	if r.flow != nil {
		return r.flow.Result()
	}
	res := skills.Waive(skills.Check(r.run, log, r.env), steps(waivers))
	res.Waivers = waivers
	return res
}

func steps(waivers []skills.Waiver) map[string]bool {
	out := map[string]bool{}
	for _, w := range waivers {
		out[w.Step] = true
	}
	return out
}

// parseWaivers reads finish_skill's or finish_stage's waive, refusing one with no reason or naming a step nodes lacks or marks strict.
// A step may be named <stage>.<step>, as a flow's report names it.
func parseWaivers(input json.RawMessage, nodes map[string]skills.Node, stage string) ([]skills.Waiver, error) {
	var in struct {
		Waive []skills.Waiver `json:"waive"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, fmt.Errorf("input is not valid: %w", err)
		}
	}
	for i, w := range in.Waive {
		in.Waive[i].Reason = strings.TrimSpace(w.Reason)
		node, ok := nodes[w.Step]
		if bare, cut := strings.CutPrefix(w.Step, stage+"."); !ok && stage != "" && cut {
			if node, ok = nodes[bare]; ok {
				in.Waive[i].Step, w.Step = bare, bare
			}
		}
		switch {
		case in.Waive[i].Reason == "":
			return nil, fmt.Errorf("the waiver of %s needs a reason", w.Step)
		case nodes != nil && !ok:
			return nil, fmt.Errorf("there is no step %s to waive", w.Step)
		case node.Strict:
			return nil, fmt.Errorf("step %s is strict and cannot be waived; do its work or call %s", w.Step, FailTool)
		}
	}
	return in.Waive, nil
}

// waived is the waivers that closed a step res still left unfinished.
func waived(res skills.Result, ws []skills.Waiver) []skills.Waiver {
	var out []skills.Waiver
	for _, w := range ws {
		if res.Unfinished(w.Step) {
			out = append(out, w)
		}
	}
	return out
}

func waiverLines(ws []skills.Waiver) string {
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "\n- %s: %s", w.Step, w.Reason)
	}
	return b.String()
}

const unmetTail = "Do the missing work, waive the step in waive with a reason, or call " + FailTool + " if it cannot honestly be done."

func (h *Host) changed() {
	if h.Changed == nil {
		return
	}
	r, res, ok := h.check()
	if !ok {
		return
	}
	h.Changed(&View{Name: r.skill.Name, Nodes: res.Nodes, Edges: res.Edges, Complete: res.Complete, Waivers: res.Waivers})
}

// startParam lists every valid skill the model may start in this session; false when there are none.
func (h *Host) startParam(offered []string) (anthropic.ToolUnionParam, bool) {
	dir, err := h.dir()
	if err != nil {
		return anthropic.ToolUnionParam{}, false
	}
	entries, err := skills.List(dir)
	if err != nil {
		return anthropic.ToolUnionParam{}, false
	}
	var b strings.Builder
	b.WriteString("Start a skill: a procedure whose every step alux checks against your tool calls, files and web cache. " +
		"Start one when the task in front of you matches its \"use when\", and never for its \"not for\". " +
		"The result is the run's steps; do them, then call finish_skill. Only one skill runs at a time; starting one while another is open returns an error.\n\nSkills:")
	staged := map[string]bool{}
	for _, e := range entries {
		if e.Err == nil && e.Skill.Flow() {
			e.Skill.Walk(func(_ string, st *skills.Stage) { staged[st.Checklist] = true })
		}
	}
	var listed []string
	for _, e := range entries {
		s := e.Skill
		if e.Err != nil || staged[e.Name] || len(skills.Unavailable(s, offered, closed)) > 0 {
			continue
		}
		byModel := slices.Contains(s.Starts, string(ByModel))
		listed = append(listed, s.Name)
		fmt.Fprintf(&b, "\n- %s: use when %s.", s.Name, s.When)
		if s.NotFor != "" {
			fmt.Fprintf(&b, " Not for %s.", s.NotFor)
		}
		if !byModel {
			b.WriteString(" Starts only when the user names it in their latest message.")
		}
		if len(s.Inputs) > 0 {
			b.WriteString(" Inputs: " + describeInputs(s) + ".")
		}
	}
	if len(listed) == 0 {
		return anthropic.ToolUnionParam{}, false
	}
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        StartTool,
		Description: anthropic.String(b.String()),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"skill":  map[string]any{"type": "string", "enum": listed},
				"inputs": map[string]any{"type": "object", "description": "The skill's inputs by name. Leave out any with a default you are content with."},
			},
			Required: []string{"skill"},
		},
	}}, true
}

func describeInputs(s *skills.Skill) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
		in := s.Inputs[name]
		part := name + " (" + in.Type
		if in.Default == nil {
			part += ", required"
		} else {
			part += fmt.Sprintf(", default %v", in.Default)
		}
		if len(in.Enum) > 0 {
			part += fmt.Sprintf(", one of %v", in.Enum)
		}
		part += ")"
		if in.Description != "" {
			part += " " + in.Description
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func (h *Host) startTool(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Skill  string         `json:"skill"`
		Inputs map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, "start_skill input is not valid: "+err.Error(), true)
	}
	if h.Pinned {
		return anthropic.NewToolResultBlock(call.ID, "Not started: this agent runs only the skill it was given.", true)
	}
	brief, err := h.Start(strings.TrimSpace(in.Skill), in.Inputs)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, "Not started: "+err.Error(), true)
	}
	if text := h.lead(ctx, cb); text != "" {
		brief += "\n\n" + text
	}
	return anthropic.NewToolResultBlock(call.ID, brief, false)
}

func finishParam() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: FinishTool,
		Description: anthropic.String("End the open skill run as done. alux checks every step against its own record " +
			"and refuses while a required step has no evidence, saying which and why; it never takes your word for a step. " +
			"Do the missing work, waive the step in waive with a reason, or call " + FailTool + " if it cannot honestly be done; " +
			"alux records every waiver and reports it. Call it only while a skill run is open; with none open it returns an error."),
		InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"waive": flow.WaiveParam()}},
	}}
}

func failParam() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: FailTool,
		Description: anthropic.String("End the open skill run as failed, because a required step cannot honestly be done. " +
			"Use it rather than working around a step's check; failing is an acceptable outcome. " +
			"Call it only while a skill run is open; with none open it returns an error."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{"reason": map[string]any{"type": "string", "description": "Why the run cannot be completed."}},
			Required:   []string{"reason"},
		},
	}}
}

func (h *Host) finish(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
	r, res, ok := h.check()
	if !ok {
		return anthropic.NewToolResultBlock(call.ID, "No skill run is open.", true)
	}
	if r.flow != nil {
		return anthropic.NewToolResultBlock(call.ID, "The open run is a flow: end each stage with "+FinishStageTool+", and the flow closes after its last stage.", true)
	}
	ws, err := parseWaivers(call.Input, r.run.Skill.Nodes, "")
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, "Refused: "+err.Error(), true)
	}
	if after := skills.Waive(res, steps(ws)); !after.Complete {
		return anthropic.NewToolResultBlock(call.ID, "Refused: "+skills.Unmet(after)+"\n"+unmetTail, true)
	}
	kept := waived(res, ws)
	h.mu.Lock()
	r.waivers = append(r.waivers, kept...)
	r.closing = Finished
	h.mu.Unlock()
	if len(kept) > 0 {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("The run is closed with %d waived step(s):%s", len(kept), waiverLines(kept)), false)
	}
	return anthropic.NewToolResultBlock(call.ID, "The run is closed; every required step has evidence.", false)
}

func (h *Host) fail(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(call.Input, &in)
	reason := strings.TrimSpace(in.Reason)
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.open
	if r == nil {
		return anthropic.NewToolResultBlock(call.ID, "No skill run is open.", true)
	}
	if reason == "" {
		return anthropic.NewToolResultBlock(call.ID, "fail_run needs a reason.", true)
	}
	r.closing = "failed: " + reason
	return anthropic.NewToolResultBlock(call.ID, "The run is closed as failed.", false)
}
