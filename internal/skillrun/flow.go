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

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/flow"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/anthropics/anthropic-sdk-go"
)

// startFlow opens a flow once every checklist it names loads and can run here, and takes it to its first checklist stage.
// Opening run stages wait for lead, so they run inside a turn that can be stopped and can ask the user.
func (h *Host) startFlow(bound *skills.Run, by Starter, offered, tools []string) (string, error) {
	s := bound.Skill
	lists, err := h.loadChecklists(s, bound, offered)
	if err != nil {
		return "", err
	}
	r := &run{
		skill:      s,
		flow:       flow.New(s, bound.Inputs, func(t string) bool { return slices.Contains(tools, t) }, agent.CommandRefusal),
		bound:      bound,
		checklists: lists,
		tools:      tools,
		by:         by,
		env:        skills.LiveEnv(tools),
	}
	bound.Stage = r.flow.Value
	h.mu.Lock()
	if h.open != nil {
		h.mu.Unlock()
		return "", fmt.Errorf("skill %s is still running", h.open.skill.Name)
	}
	h.open = r
	h.mu.Unlock()
	if k := r.flow.Next().Kind; k == flow.Run || k == flow.Spawn || k == flow.SpawnEach {
		h.mu.Lock()
		r.leading = true
		h.mu.Unlock()
		h.changed()
		return flowBrief(bound), nil
	}
	brief := flowBrief(bound) + "\n\n" + h.advance(context.Background(), r, agent.Callbacks{})
	h.settle(r)
	return brief, nil
}

// lead makes the open flow's opening run stages under the turn's ctx and callbacks, once, and returns what happened.
func (h *Host) lead(ctx context.Context, cb agent.Callbacks) string {
	h.mu.Lock()
	r := h.open
	if r == nil || !r.leading {
		h.mu.Unlock()
		return ""
	}
	r.leading = false
	notes := r.landed
	r.landed = nil
	h.mu.Unlock()
	text := h.advance(ctx, r, cb)
	h.settle(r)
	return strings.Join(append(notes, text), "\n\n")
}

// Leading reports whether the open flow waits for a turn to move on in, as it does once a spawn lands.
func (h *Host) Leading() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.open != nil && h.open.leading
}

// spawnItem is the parallel foreach a spawn call runs one item of.
type spawnItem struct {
	each *skills.Stage
	item map[string]any
}

func (it spawnItem) id() string {
	id, _ := it.item["id"].(string)
	return id
}

// startSpawn starts a spawn stage's subagent in the background; Land moves the flow on when it ends.
func (h *Host) startSpawn(ctx context.Context, r *run, st *skills.Stage, cb agent.Callbacks) error {
	return h.dispatchSpawn(ctx, r, st, nil, cb)
}

// dispatchSpawn starts st's subagent, for one item of a parallel foreach when it is non-nil.
func (h *Host) dispatchSpawn(ctx context.Context, r *run, st *skills.Stage, it *spawnItem, cb agent.Callbacks) error {
	if h.Dispatch == nil {
		return errors.New("this agent cannot start subagents")
	}
	bound := r.bound
	if it != nil {
		bound = bound.WithItem(it.each.Foreach.As, it.item)
	}
	inputs, change, err := bound.FillSpawn(st.Spawn)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.stepN++
	id := fmt.Sprintf("%s%d", stepPrefix, h.stepN)
	if r.spawned == nil {
		r.spawned = map[string]int{}
	}
	r.spawned[id] = 0
	if it != nil {
		if r.items == nil {
			r.items = map[string]spawnItem{}
		}
		r.items[id] = *it
	}
	h.mu.Unlock()
	if it == nil {
		r.flow.Started()
	}
	task := fmt.Sprintf("Run the %s skill. alux started it for stage %s of the %s flow.", st.Spawn.Run, st.Name, r.skill.Name)
	runID, err := h.Dispatch(ctx, agent.Call{ID: id, Name: "dispatch"}, st.Spawn.Agent, st.Spawn.Run, inputs, change, task, cb)
	h.mu.Lock()
	if err != nil {
		delete(r.spawned, id)
		delete(r.items, id)
	} else if _, live := r.spawned[id]; live {
		r.spawned[id] = runID
	}
	h.mu.Unlock()
	return err
}

// landSpawn moves the open flow past a spawn stage whose subagent has ended, and reports whether call was one.
// The flow moves on in the next turn, since a landing arrives outside any turn.
func (h *Host) landSpawn(call agent.Call, failed bool, check *skills.SkillCheck) bool {
	h.mu.Lock()
	r := h.open
	if r == nil || r.flow == nil {
		h.mu.Unlock()
		return false
	}
	if _, ok := r.spawned[call.ID]; !ok {
		h.mu.Unlock()
		return false
	}
	delete(r.spawned, call.ID)
	it, each := r.items[call.ID]
	delete(r.items, call.ID)
	h.mu.Unlock()
	if each {
		h.landItem(r, it, failed, check)
		return true
	}
	st := r.flow.Next().Stage
	complete := !failed && check != nil && check.Finished
	var wrote error
	if complete && st.Spawn.Report != "" {
		if wrote = h.writeReport(r.bound.Expand(st.Spawn.Report), check.Report); wrote != nil {
			complete = false
		}
	}
	if complete {
		r.flow.Landed(check.Waivers)
	}
	back, err := r.flow.Ran(complete)
	verdict := "landed complete"
	switch {
	case complete && len(check.Waivers) > 0:
		verdict += withWaivers(check.Waivers)
	case err != nil:
		verdict = "did not land complete, so the flow failed"
	case !complete && back == "":
		verdict = "did not land complete, so the flow stopped at it"
	case back != "":
		verdict = fmt.Sprintf("did not land complete (%d of at most %s), so alux reopened %s", r.flow.Fails(st), most(st.Max), back)
	}
	if wrote != nil {
		verdict += "; alux could not write its report: " + wrote.Error()
	}
	h.mu.Lock()
	r.landed = append(r.landed, fmt.Sprintf("[alux: stage %s (%s running %s) %s]", st.Name, st.Spawn.Agent, st.Spawn.Run, verdict))
	r.leading = true
	h.mu.Unlock()
	h.changed()
	return true
}

// landItem records one landed item of a parallel foreach, and lets the flow move on once the last is in.
func (h *Host) landItem(r *run, it spawnItem, failed bool, check *skills.SkillCheck) {
	st := &it.each.Stages[0]
	complete := !failed && check != nil && check.Finished
	var wrote error
	if complete && st.Spawn.Report != "" {
		if wrote = h.writeReport(r.bound.WithItem(it.each.Foreach.As, it.item).Expand(st.Spawn.Report), check.Report); wrote != nil {
			complete = false
		}
	}
	var waivers []skills.Waiver
	verdict := "did not land complete"
	if complete {
		waivers, verdict = check.Waivers, "landed complete"
		if len(waivers) > 0 {
			verdict += withWaivers(waivers)
		}
	}
	if wrote != nil {
		verdict += "; alux could not write its report: " + wrote.Error()
	}
	last, _ := r.flow.LandedItem(it.id(), complete, waivers)
	h.mu.Lock()
	r.landed = append(r.landed, fmt.Sprintf("[alux: stage %s[%s] (%s running %s) %s]", it.each.Name, it.id(), st.Spawn.Agent, st.Spawn.Run, verdict))
	r.leading = r.leading || last
	h.mu.Unlock()
	h.changed()
}

// startEach starts the spawn of every item a parallel foreach has left, and says what started.
func (h *Host) startEach(ctx context.Context, r *run, act flow.Action, cb agent.Callbacks) string {
	each, st := act.Stage, &act.Stage.Stages[0]
	ids := make([]string, len(act.Items))
	for i, item := range act.Items {
		ids[i] = spawnItem{item: item}.id()
	}
	r.flow.StartedEach(ids)
	var started, notes []string
	for _, item := range act.Items {
		it := spawnItem{each: each, item: item}
		if err := h.dispatchSpawn(ctx, r, st, &it, cb); err != nil {
			_, _ = r.flow.LandedItem(it.id(), false, nil)
			notes = append(notes, fmt.Sprintf("[alux could not start stage %s[%s] (%s running %s): %v]", each.Name, it.id(), st.Spawn.Agent, st.Spawn.Run, err))
			continue
		}
		started = append(started, it.id())
	}
	h.changed()
	if len(started) > 0 {
		notes = append(notes, fmt.Sprintf("[alux started stage %s: %s is running %s in the background for %s]",
			each.Name, st.Spawn.Agent, st.Spawn.Run, strings.Join(started, ", ")))
	}
	return strings.Join(notes, "\n")
}

// writeReport writes a landed spawn's report to path, which must be under the skill runs' state dir.
func (h *Host) writeReport(path, report string) error {
	root := filepath.Join(h.home(), skills.ReportRoot)
	path = filepath.Clean(path)
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return fmt.Errorf("%s is not under %s", path, root)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(report), 0o644)
}

func withWaivers(ws []skills.Waiver) string {
	noun := "waivers"
	if len(ws) == 1 {
		noun = "waiver"
	}
	var each []string
	for _, w := range ws {
		each = append(each, w.Step+": "+w.Reason)
	}
	return fmt.Sprintf(" with %d %s: %s", len(ws), noun, strings.Join(each, "; "))
}

func (h *Host) stopSpawns(r *run) {
	h.mu.Lock()
	ids := slices.Collect(maps.Values(r.spawned))
	r.spawned = nil
	h.mu.Unlock()
	for _, id := range ids {
		if id != 0 && h.Stop != nil {
			_ = h.Stop(id)
		}
	}
}

// settle ends a flow that advance closed, and otherwise redraws it.
func (h *Host) settle(r *run) {
	h.mu.Lock()
	how := r.closing
	h.mu.Unlock()
	if how != "" {
		h.End(how)
	} else {
		h.changed()
	}
}

func (h *Host) loadChecklists(s *skills.Skill, bound *skills.Run, offered []string) (map[string]*skills.Skill, error) {
	lists := map[string]*skills.Skill{}
	bad := map[string]bool{}
	var problems []error
	s.Walk(func(path string, st *skills.Stage) {
		if sp := st.Spawn; sp != nil {
			switch {
			case h.Dispatch == nil || h.CheckDispatch == nil:
				problems = append(problems, fmt.Errorf("stage %s: this agent cannot start subagents", path))
			default:
				if err := h.CheckDispatch(sp.Agent, sp.Run); err != nil {
					problems = append(problems, fmt.Errorf("stage %s: %w", path, err))
				}
			}
			return
		}
		if st.Checklist == "" || bad[st.Checklist] {
			return
		}
		c := lists[st.Checklist]
		if c == nil {
			var err error
			if c, err = h.load(st.Checklist); err != nil {
				err = fmt.Errorf("checklist %s: %w", st.Checklist, err)
			} else if c.Flow() {
				err = fmt.Errorf("%s is a flow, and a checklist stage names a checklist", st.Checklist)
			} else if p := skills.Unavailable(c, offered, closed); len(p) > 0 {
				err = fmt.Errorf("checklist %s: %w", st.Checklist, p)
			}
			if err != nil {
				bad[st.Checklist] = true
				problems = append(problems, fmt.Errorf("stage %s: %w", path, err))
				return
			}
			lists[st.Checklist] = c
		}
		for _, in := range slices.Sorted(maps.Keys(st.Inputs)) {
			if _, ok := c.Inputs[in]; !ok {
				problems = append(problems, fmt.Errorf("stage %s: checklist %s has no input %s", path, st.Checklist, in))
			}
		}
		for _, in := range slices.Sorted(maps.Keys(c.Inputs)) {
			_, fromFlow := bound.Inputs[in]
			_, fromStage := st.Inputs[in]
			if !fromFlow && !fromStage && c.Inputs[in].Default == nil {
				problems = append(problems, fmt.Errorf("stage %s: checklist %s needs input %s, which the flow does not have", path, st.Checklist, in))
			}
		}
	})
	return lists, errors.Join(problems...)
}

const stepPrefix = "alux_step_"

// stepFailed reads bash's exit from the call, since a non-zero exit is not an error result.
func stepFailed(call agent.Call, res anthropic.ContentBlockParamUnion) bool {
	if res.OfToolResult == nil || res.OfToolResult.IsError.Or(false) {
		return true
	}
	return call.Name == "bash" && call.Exit != "exit 0"
}

// fromRoot starts a bash step's command at its shell's root, wherever the agent left that live shell.
func fromRoot(ctx context.Context, input json.RawMessage) json.RawMessage {
	sh, ok := shell.From(ctx)
	if !ok {
		return input
	}
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return input
	}
	command, ok := in["command"].(string)
	if !ok {
		return input
	}
	in["command"] = "cd " + shellQuote(sh.Root()) + " && " + command
	out, err := json.Marshal(in)
	if err != nil {
		return input
	}
	return out
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func resultText(res anthropic.ContentBlockParamUnion) string {
	if res.OfToolResult == nil {
		return ""
	}
	var parts []string
	for _, c := range res.OfToolResult.Content {
		if c.OfText != nil {
			parts = append(parts, c.OfText.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// finishStage ends the open checklist stage once its evidence and out are in, then advances the flow.
func (h *Host) finishStage(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	h.mu.Lock()
	r := h.open
	var log []skills.Event
	closing := ""
	if r != nil {
		log, closing = slices.Clone(r.log), r.closing
	}
	h.mu.Unlock()
	switch {
	case r == nil:
		return anthropic.NewToolResultBlock(call.ID, "No skill run is open.", true)
	case r.flow == nil:
		return anthropic.NewToolResultBlock(call.ID, "The open run is a checklist, not a flow; call "+FinishTool+" once its steps are done.", true)
	case closing != "":
		return anthropic.NewToolResultBlock(call.ID, "The flow is already closed.", true)
	case agent.CallsInMessage(ctx) > 1:
		return anthropic.NewToolResultBlock(call.ID, "Refused: "+FinishStageTool+" must be the only tool call in its message, "+
			"so that no call lands on either side of the stage's edge. Call it again on its own.", true)
	}
	if h.Leading() {
		return anthropic.NewToolResultBlock(call.ID, h.lead(ctx, cb)+"\n\nThe stage you were finishing had already ended; this is where the flow is now.", false)
	}
	var in struct {
		Out json.RawMessage `json:"out"`
	}
	if len(call.Input) > 0 {
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return anthropic.NewToolResultBlock(call.ID, "finish_stage input is not valid: "+err.Error(), true)
		}
	}
	switch act := r.flow.Next(); act.Kind {
	case flow.Waiting:
		return anthropic.NewToolResultBlock(call.ID, "Refused: "+waitingText(act.Stage), true)
	case flow.Open:
		ws, err := parseWaivers(call.Input, r.run.Skill.Nodes, act.Stage.Name)
		if err != nil {
			return anthropic.NewToolResultBlock(call.ID, "Refused: "+err.Error(), true)
		}
		given := *r.run
		given.Out = map[string]any{}
		_ = json.Unmarshal(in.Out, &given.Out)
		res := skills.Check(&given, log, r.env)
		if after := skills.Waive(res, steps(ws)); !after.Complete {
			return anthropic.NewToolResultBlock(call.ID, "Refused: "+skills.Unmet(after)+outSays(&given, after)+
				"\nOnly calls made since this stage opened count. "+unmetTail, true)
		}
		if err := r.flow.Finish(in.Out, waived(res, ws)); err != nil {
			return anthropic.NewToolResultBlock(call.ID, "Refused: "+err.Error(), true)
		}
	case flow.Stopped:
		ws, err := parseWaivers(call.Input, nil, "")
		switch {
		case err != nil:
			return anthropic.NewToolResultBlock(call.ID, "Refused: "+err.Error(), true)
		case len(ws) == 0:
			r.flow.Retry()
		case len(ws) > 1 || ws[0].Step != act.Stage.Name:
			return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("Refused: stage %s is stopped, and waive names it alone: "+
				"[{\"step\": %q, \"reason\": ...}].", act.Stage.Name, act.Stage.Name), true)
		default:
			var out map[string]any
			if len(in.Out) > 0 && string(in.Out) != "null" {
				if err := json.Unmarshal(in.Out, &out); err != nil {
					return anthropic.NewToolResultBlock(call.ID, "Refused: out is not a JSON object: "+err.Error(), true)
				}
			}
			if err := r.flow.Waive(ws[0].Reason, out); err != nil {
				return anthropic.NewToolResultBlock(call.ID, "Refused: "+err.Error(), true)
			}
		}
	}
	return anthropic.NewToolResultBlock(call.ID, h.advance(ctx, r, cb), false)
}

// outSays is the say of each unmet node that holds only for the out just given, since the refusal is about that out.
func outSays(r *skills.Run, res skills.Result) string {
	var b strings.Builder
	for _, n := range res.Nodes {
		if node := r.Skill.Nodes[n.Name]; node.When != nil && node.When.Out != "" && res.Unfinished(n.Name) {
			fmt.Fprintf(&b, "\n%s: %s", n.Name, strings.TrimSpace(r.Expand(node.Say)))
		}
	}
	return b.String()
}

// advance makes every run stage from the cursor until a checklist stage opens or the flow ends, and returns what happened.
func (h *Host) advance(ctx context.Context, r *run, cb agent.Callbacks) string {
	var out []string
	end := func(how, text string) string {
		h.mu.Lock()
		r.closing = how
		h.mu.Unlock()
		return strings.Join(append(out, text), "\n\n")
	}
	for ctx.Err() == nil {
		act := r.flow.Next()
		switch act.Kind {
		case flow.Done:
			return end(Finished, "Every stage is done: the flow is complete and closed.")
		case flow.Failed:
			return end("failed: "+act.Reason, "The flow has failed: "+act.Reason+".")
		case flow.Stopped:
			return strings.Join(append(out, stoppedText(r, act)), "\n\n")
		case flow.Open:
			brief, err := h.openStage(r, act.Stage)
			if err != nil {
				return end("failed: "+err.Error(), "The flow has failed: stage "+act.Stage.Name+" could not open: "+err.Error()+".")
			}
			return strings.Join(append(out, brief), "\n\n")
		case flow.Waiting:
			return strings.Join(append(out, waitingText(act.Stage)), "\n\n")
		case flow.Paused:
			if h.askMore(ctx, r, act, cb) {
				r.flow.Extend(act.Stage)
				out = append(out, "[alux: "+act.Reason+"; the user granted one more round]")
			} else {
				r.flow.Fail(act.Reason + "; the user chose to fail the run")
			}
			h.changed()
			continue
		case flow.SpawnEach:
			out = append(out, h.startEach(ctx, r, act, cb))
			continue
		case flow.Spawn:
			st := act.Stage
			if err := h.startSpawn(ctx, r, st, cb); err != nil {
				back, ferr := r.flow.Ran(false)
				h.changed()
				verdict := "the flow has failed"
				switch {
				case ferr == nil && back != "":
					verdict = "alux reopened " + back
				case ferr == nil:
					verdict = "the flow stopped at it"
				}
				out = append(out, fmt.Sprintf("[alux could not start stage %s (%s running %s): %v; %s]", st.Name, st.Spawn.Agent, st.Spawn.Run, err, verdict))
				continue
			}
			h.changed()
			out = append(out, fmt.Sprintf("[alux started stage %s: %s is running %s in the background]", st.Name, st.Spawn.Agent, st.Spawn.Run))
			return strings.Join(append(out, waitingText(st)), "\n\n")
		}
		st := act.Stage
		text, passed := h.runStage(ctx, r, st, cb)
		if passed {
			if err := r.flow.Took(text); err != nil {
				passed, text = false, text+"\n\n[alux: "+err.Error()+"]"
			}
		}
		back, err := r.flow.Ran(passed)
		h.changed()
		verdict := "passed"
		switch {
		case err != nil, !passed && back == "":
			verdict = "failed"
		case back != "":
			verdict = fmt.Sprintf("failed (%d of at most %s), so alux reopened %s: fix the cause there, then call %s again",
				r.flow.Fails(st), most(st.Max), back, FinishStageTool)
		}
		out = append(out, fmt.Sprintf("[alux ran stage %s (%s): %s]\n%s", st.Name, st.Run.Tool, verdict, text))
	}
	return strings.Join(append(out, "Interrupted before the flow's next stage; call "+FinishStageTool+" to carry on."), "\n\n")
}

// moreRound is the answer that gives a paused repeat another round; any other answer fails the run.
const moreRound = "One more round"

// askMore asks the user, through ask_user, whether a repeat that ran out of rounds gets one more; false when it cannot ask.
func (h *Host) askMore(ctx context.Context, r *run, act flow.Action, cb agent.Callbacks) bool {
	h.mu.Lock()
	a := h.agent
	h.stepN++
	id := fmt.Sprintf("%s%d", stepPrefix, h.stepN)
	h.mu.Unlock()
	if a == nil || !slices.Contains(r.tools, askTool) {
		return false
	}
	input, _ := json.Marshal(map[string]any{
		"question": fmt.Sprintf("The %s flow's %s.", r.skill.Name, act.Reason),
		"options": []map[string]string{
			{"label": moreRound, "detail": "Run " + act.Stage.Name + " once more"},
			{"label": "Fail the run", "detail": "End the flow as failed, leaving its work as it is"},
		},
		"allow_other": false,
	})
	return strings.Contains(resultText(a.Run(ctx, agent.Call{ID: id, Name: askTool, Input: input}, cb)), moreRound)
}

const askTool = "ask_user"

// stoppedText tells the model its three ways on from a failed stage that stopped the flow.
func stoppedText(r *run, act flow.Action) string {
	name := act.Stage.Name
	waive := fmt.Sprintf("call %s with waive: [{\"step\": %q, \"reason\": <why moving on without it is right>}]", FinishStageTool, name)
	if fields := r.skill.Reads(name); len(fields) > 0 {
		waive += fmt.Sprintf(" and out giving %s, which later stages read", listed(fields))
	}
	return fmt.Sprintf("The flow stopped at stage **%s**: %s. Either fix the cause and call %s on its own to run the stage again; "+
		"or, if its check is wrong for this case, %s, and alux records the waiver and moves on; or call %s if the run cannot honestly go on.",
		name, act.Reason, FinishStageTool, waive, FailTool)
}

func waitingText(st *skills.Stage) string {
	return fmt.Sprintf("Stage **%s** waits for %s to finish %s. Do not run it or start it yourself: end your turn, and alux wakes you "+
		"with the next stage once it lands.", st.Name, st.Spawn.Agent, st.Spawn.Run)
}

func most(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

// openStage binds a checklist stage and starts its evidence window afresh.
func (h *Host) openStage(r *run, st *skills.Stage) (string, error) {
	c := r.checklists[st.Checklist]
	given, err := r.bound.FillInputs(st.Inputs)
	if err != nil {
		return "", err
	}
	for name := range c.Inputs {
		if v, ok := r.bound.Inputs[name]; ok && given[name] == nil {
			given[name] = v
		}
	}
	cr, err := skills.Bind(c, given, r.bound.Now(), h.home())
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	r.run, r.log, r.noticed = cr, nil, nil
	h.mu.Unlock()
	h.changed()
	head := fmt.Sprintf("Stage **%s** of the flow **%s** is open: the checklist %s. Only calls made from now on count as its evidence.",
		st.Name, r.skill.Name, c.Name)
	tail := "When every required step of this stage is done, call " + FinishStageTool + " as the only tool call in its message"
	if schema := flow.OutSchema(st); schema != nil {
		b, _ := json.Marshal(schema)
		tail += ", with out matching this JSON schema: " + string(b)
	}
	tail += ". It is refused while a required step has no evidence."
	if fields := flow.CommandFields(st); len(fields) > 0 {
		tail += fmt.Sprintf(" %s must be shell commands alux would run: it checks them against its shell rules without running them, and refuses any those rules would.",
			listed(fields))
	}
	tail += " If a required step has no evidence, do the missing work, waive the step in waive with a reason, or call " + FailTool +
		" if it cannot honestly be done, rather than working around its check."
	return skills.StageBrief(cr, r.tools, head, tail), nil
}

// runStage makes a run stage's one call under the approvals of whoever is advancing the flow.
func (h *Host) runStage(ctx context.Context, r *run, st *skills.Stage, cb agent.Callbacks) (string, bool) {
	input, whole, err := r.bound.FillStep(st.Run)
	h.mu.Lock()
	h.stepN++
	id := fmt.Sprintf("%s%d", stepPrefix, h.stepN)
	if whole {
		if r.unscreened == nil {
			r.unscreened = map[string]bool{}
		}
		r.unscreened[id] = true
	}
	a := h.agent
	h.mu.Unlock()
	var res anthropic.ContentBlockParamUnion
	call := agent.Call{ID: id, Name: st.Run.Tool}
	switch {
	case err != nil:
		res = anthropic.NewToolResultBlock(id, err.Error(), true)
	case a == nil:
		res = anthropic.NewToolResultBlock(id, "no agent is wired to make the call", true)
	default:
		if st.Run.Tool == "bash" {
			input = fromRoot(ctx, input)
		}
		call.Input = input
		observe := cb.OnCall
		cb.OnCall = func(c agent.Call, res anthropic.ContentBlockParamUnion) {
			call = c
			if observe != nil {
				observe(c, res)
			}
		}
		res = a.Run(ctx, call, cb)
	}
	return resultText(res), !stepFailed(call, res)
}

func briefStages(b *strings.Builder, bound *skills.Run, list []skills.Stage, indent string) {
	for i := range list {
		st := &list[i]
		what := "a checklist you work through, ended with " + FinishStageTool
		switch {
		case st.Run != nil:
			what = "alux runs " + bound.StageCall(st.Run) + " itself; do not run it yourself"
		case st.Spawn != nil:
			what = "alux starts " + st.Spawn.Agent + " running " + st.Spawn.Run + " in the background and moves on when it lands; do not dispatch it yourself"
		case st.Foreach != nil && st.Foreach.Parallel:
			what = st.Container() + "; alux starts every item's spawn together and moves on once all have landed"
		case st.Foreach != nil:
			what = st.Container() + ", one at a time; alux moves between them"
		case st.Repeat != nil:
			what = st.Container() + "; alux checks after each round and asks the user at the limit"
		}
		if st.When != nil {
			what += ", only " + st.When.Describe()
		}
		if st.OnFail != "" {
			what += "; if it fails, alux reopens " + st.OnFail
			switch {
			case st.Max > 0 && st.Strict:
				what += fmt.Sprintf(", and the flow fails once it has failed %d times", st.Max)
			case st.Max > 0:
				what += fmt.Sprintf(", and after %d failures the flow stops at it, to be waived with a reason or the run failed", st.Max)
			}
		}
		fmt.Fprintf(b, "%s%d. **%s**: %s.\n", indent, i+1, st.Name, what)
		briefStages(b, bound, st.Stages, indent+"   ")
	}
}

// flowBrief opens a flow's brief: what it is, its inputs, and every stage in order.
func flowBrief(bound *skills.Run) string {
	s := bound.Skill
	var b strings.Builder
	fmt.Fprintf(&b, "Run the flow **%s** (v%d). Use it when %s.\n\n", s.Name, s.Version, s.When)
	if len(bound.Inputs) > 0 {
		b.WriteString("Inputs:\n")
		for _, name := range slices.Sorted(maps.Keys(bound.Inputs)) {
			fmt.Fprintf(&b, "- %s = %v\n", name, bound.Inputs[name])
		}
		b.WriteString("\n")
	}
	b.WriteString("alux takes you through its stages in order and moves between them itself:\n")
	briefStages(&b, bound, s.Stages, "")
	b.WriteString("\nThe run stays open across turns. When a required step has no evidence or a stage fails: " + unmetTail)
	return b.String()
}
