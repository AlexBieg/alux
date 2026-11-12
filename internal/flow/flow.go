// Package flow keeps a skill flow's cursor: which stage is open, what each stage put out, and where a failed run sends it.
package flow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/skills"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/jsonschema-go/jsonschema"
)

// Tool ends the open checklist stage.
const Tool = "finish_stage"

// Kind is what the stage at the cursor needs next.
type Kind int

const (
	// Open is a checklist stage for the model to work through.
	Open Kind = iota
	// Run is a stage whose one call alux makes itself.
	Run
	// Spawn is a stage whose subagent alux starts; Waiting is one whose subagent has not landed yet.
	Spawn
	Waiting
	// SpawnEach is a parallel foreach whose Items each need their spawn started; Stage is the foreach.
	SpawnEach
	// Paused is a repeat that ran max rounds without its until holding; Extend or Fail decides.
	Paused
	Done
	Failed
	// Stopped is a run or spawn stage that failed where it would have failed the flow; Retry or Waive moves it on.
	Stopped
)

type Action struct {
	Kind  Kind
	Stage *skills.Stage
	// Reason is why the flow failed, for Failed, why the stage stopped, for Stopped, and how many rounds ran, for Paused.
	Reason string
	Items  []map[string]any
}

// frame is one list of stages being run: the flow's own, or a foreach's for one item, or a repeat's for one round.
type frame struct {
	stages []skills.Stage
	cursor int
	// owner is the foreach or repeat stage running this list; nil for the flow's own.
	owner *skills.Stage
	// items is a foreach's items still to run when it opened, in order; item indexes the current one.
	items []map[string]any
	item  int
	// round is a repeat's current round, from 1.
	round int
}

// Flow is one run of a flow file. It does no I/O: the caller opens checklists, makes run calls and starts spawns.
type Flow struct {
	skill     *skills.Skill
	inputs    map[string]any
	available func(tool string) bool
	refuse    func(command string) string

	mu    sync.Mutex
	stack []*frame
	state map[*skills.Stage]skills.State
	out   map[string]map[string]any
	fails map[*skills.Stage]int
	// done is the ids each foreach has finished; a reopened foreach never runs them again.
	done map[*skills.Stage]map[string]bool
	// extra is the rounds past max the user granted each repeat.
	extra map[*skills.Stage]int
	// running is the item ids of a parallel foreach whose spawns have not landed; missed the ones that landed incomplete.
	running  map[*skills.Stage]map[string]bool
	missed   map[*skills.Stage][]string
	failed   string
	failedAt *skills.Stage
	// stopped is the stage at the cursor that failed and waits for a rerun or waiver.
	stopped    *skills.Stage
	stopReason string
	waivers    []waiver
}

// waiver is one recorded waiver with the stage at each depth of the stack, and each list's pass, when it was made.
type waiver struct {
	path []*skills.Stage
	// passes[d] is frame d's foreach item id or repeat round; passes[0] is the flow's own list, always "".
	passes []string
	skills.Waiver
}

func pass(fr *frame) string {
	switch {
	case fr.owner == nil:
		return ""
	case fr.owner.Foreach != nil:
		id, _ := fr.items[fr.item]["id"].(string)
		return id
	}
	return strconv.Itoa(fr.round)
}

// record keeps a waiver of the stage at the cursor, named by its path with each item as [id] and each round as #n, then step.
func (f *Flow) record(step, reason string) {
	f.recordItem("", step, reason)
}

// recordItem is record for item id of the parallel foreach at the cursor, under its spawn stage; "" is the cursor's own.
func (f *Flow) recordItem(id, step, reason string) {
	w := waiver{Waiver: skills.Waiver{Reason: reason}}
	var name []string
	for d, fr := range f.stack {
		st := &fr.stages[fr.cursor]
		w.path, w.passes = append(w.path, st), append(w.passes, pass(fr))
		part := st.Name
		if d+1 < len(f.stack) {
			if next := f.stack[d+1]; st.Foreach != nil {
				part += "[" + pass(next) + "]"
			} else {
				part += "#" + pass(next)
			}
		}
		name = append(name, part)
	}
	if id != "" {
		inner := &w.path[len(w.path)-1].Stages[0]
		w.path, w.passes = append(w.path, inner), append(w.passes, id)
		name[len(name)-1] += "[" + id + "]"
		name = append(name, inner.Name)
	}
	if step != "" {
		name = append(name, step)
	}
	w.Step = strings.Join(name, ".")
	f.waivers = append(f.waivers, w)
}

// forget drops the waivers made in frame k's current pass for stages of list, which on_fail is about to run again.
// A finished foreach item is not run again, so its waivers stand.
func (f *Flow) forget(k int, list []skills.Stage) {
	f.waivers = slices.DeleteFunc(f.waivers, func(w waiver) bool {
		if len(w.path) <= k || !inList(list, w.path[k]) {
			return false
		}
		for d := 1; d <= k; d++ {
			if w.passes[d] != pass(f.stack[d]) {
				return false
			}
		}
		owner := w.path[k]
		return len(w.path) == k+1 || owner.Foreach == nil || !f.done[owner][w.passes[k+1]]
	})
}

func inList(list []skills.Stage, st *skills.Stage) bool {
	for i := range list {
		if &list[i] == st {
			return true
		}
	}
	return false
}

// New starts s at its first stage with its bound inputs; available says whether a tool is offered, refuse why alux would not run a command.
func New(s *skills.Skill, inputs map[string]any, available func(string) bool, refuse func(string) string) *Flow {
	return &Flow{skill: s, inputs: inputs, available: available, refuse: refuse,
		stack: []*frame{{stages: s.Stages}}, state: map[*skills.Stage]skills.State{}, out: map[string]map[string]any{},
		fails: map[*skills.Stage]int{}, done: map[*skills.Stage]map[string]bool{}, extra: map[*skills.Stage]int{},
		running: map[*skills.Stage]map[string]bool{}, missed: map[*skills.Stage][]string{}}
}

func (f *Flow) top() *frame { return f.stack[len(f.stack)-1] }

// current is the stage at the cursor, or nil once the flow's own list is done.
func (f *Flow) current() *skills.Stage {
	t := f.top()
	if t.cursor >= len(t.stages) {
		return nil
	}
	return &t.stages[t.cursor]
}

// Stage names the stage at the cursor, "" once the flow's own list is done.
func (f *Flow) Stage() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.current(); st != nil {
		return st.Name
	}
	return ""
}

// Value is ${stage.field}: a stage's out field, run text or capture once it is done, or the current item of a foreach.
func (f *Flow) Value(stage, field string) (any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.out[stage][field]
	return v, ok
}

// Took keeps the run stage at the cursor's result as its text and takes its captures; a capture that matches nothing fails the stage.
func (f *Flow) Took(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.current()
	got := map[string]any{skills.TextField: text}
	for _, k := range slices.Sorted(maps.Keys(st.Run.Capture)) {
		c := st.Run.Capture[k]
		m := regexp.MustCompile(c.Pattern).FindStringSubmatch(text)
		if len(m) < 2 || m[1] == "" {
			return fmt.Errorf("capture %s: %s matched nothing in the result", k, c.Pattern)
		}
		got[k] = m[1]
		if c.Type == "int" {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return fmt.Errorf("capture %s: %q is not an int", k, m[1])
			}
			got[k] = n
		}
	}
	f.out[st.Name] = got
	return nil
}

// Next skips each stage whose when does not hold, enters and leaves foreach and repeat lists, and says what the cursor needs.
func (f *Flow) Next() Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		if f.failed != "" {
			return Action{Kind: Failed, Reason: f.failed}
		}
		if f.stopped != nil {
			return Action{Kind: Stopped, Stage: f.stopped, Reason: f.stopReason}
		}
		t := f.top()
		if t.cursor >= len(t.stages) {
			if len(f.stack) == 1 {
				return Action{Kind: Done}
			}
			if act, stop := f.endPass(t); stop {
				return act
			}
			continue
		}
		st := &t.stages[t.cursor]
		if st.Spawn != nil && f.state[st] == skills.Running {
			return Action{Kind: Waiting, Stage: st}
		}
		if !f.holds(st.When) {
			f.state[st] = skills.Skipped
			t.cursor++
			continue
		}
		switch {
		case st.Run != nil:
			return Action{Kind: Run, Stage: st}
		case st.Spawn != nil:
			return Action{Kind: Spawn, Stage: st}
		case st.Checklist != "":
			return Action{Kind: Open, Stage: st}
		case st.Foreach != nil && st.Foreach.Parallel:
			if len(f.running[st]) > 0 {
				return Action{Kind: Waiting, Stage: &st.Stages[0]}
			}
			items := f.items(st)
			if len(items) == 0 {
				f.state[st], f.state[&st.Stages[0]] = skills.Done, skills.Done
				t.cursor++
				continue
			}
			return Action{Kind: SpawnEach, Stage: st, Items: items}
		case st.Foreach != nil:
			items := f.items(st)
			if len(items) == 0 {
				f.state[st] = skills.Done
				t.cursor++
				continue
			}
			f.state[st] = skills.Running
			f.reset(st.Stages, true)
			f.out[st.Foreach.As] = items[0]
			f.stack = append(f.stack, &frame{stages: st.Stages, owner: st, items: items})
		case st.Repeat != nil:
			f.state[st] = skills.Running
			f.reset(st.Stages, true)
			f.out[st.Name] = map[string]any{skills.RoundField: 1}
			f.stack = append(f.stack, &frame{stages: st.Stages, owner: st, round: 1})
		}
	}
}

// items is a foreach's list less the ids it has already finished.
func (f *Flow) items(st *skills.Stage) []map[string]any {
	stage, field, _ := strings.Cut(st.Foreach.In, ".")
	list, _ := f.out[stage][field].([]any)
	var out []map[string]any
	for _, v := range list {
		item, ok := v.(map[string]any)
		if id, _ := item["id"].(string); ok && !f.done[st][id] {
			out = append(out, item)
		}
	}
	return out
}

// endPass ends one item of a foreach or one round of a repeat, moving to the next or out of the list.
// It stops with Paused when a repeat has run every round it may without its until holding.
func (f *Flow) endPass(t *frame) (Action, bool) {
	owner := t.owner
	if owner.Foreach != nil {
		if f.done[owner] == nil {
			f.done[owner] = map[string]bool{}
		}
		id, _ := t.items[t.item]["id"].(string)
		f.done[owner][id] = true
		if t.item++; t.item < len(t.items) {
			t.cursor = 0
			f.reset(t.stages, true)
			f.out[owner.Foreach.As] = t.items[t.item]
			return Action{}, false
		}
		delete(f.out, owner.Foreach.As)
		f.leave()
		return Action{}, false
	}
	if f.holds(owner.Repeat.Until) {
		f.leave()
		return Action{}, false
	}
	if most := f.rounds(owner); t.round >= most {
		return Action{Kind: Paused, Stage: owner, Reason: fmt.Sprintf("%s ran %d of at most %d rounds without %s", owner.Name, t.round, most,
			strings.TrimPrefix(owner.Repeat.Until.Describe(), "when "))}, true
	}
	t.round++
	f.out[owner.Name] = map[string]any{skills.RoundField: t.round}
	t.cursor = 0
	f.reset(t.stages, true)
	return Action{}, false
}

// leave pops the top list and moves its owner's list past the owner.
func (f *Flow) leave() {
	owner := f.top().owner
	f.stack = f.stack[:len(f.stack)-1]
	f.state[owner] = skills.Done
	f.top().cursor++
}

func (f *Flow) rounds(st *skills.Stage) int {
	n := st.Repeat.Max.N
	if ref := st.Repeat.Max.Ref; ref != "" {
		n, _ = f.inputs[ref].(int)
	}
	return n + f.extra[st]
}

// Extend grants the paused repeat one more round.
func (f *Flow) Extend(st *skills.Stage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extra[st]++
}

// Fail ends the flow as failed at the stage at the cursor.
func (f *Flow) Fail(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = reason
	f.failedAt = f.top().owner
	if st := f.current(); st != nil {
		f.failedAt = st
	}
}

// reset marks list's stages and every list inside them pending and forgets their output.
// A foreach under them forgets its finished ids only with clearDone, for a list that runs again for a new item or round.
func (f *Flow) reset(list []skills.Stage, clearDone bool) {
	for i := range list {
		st := &list[i]
		delete(f.state, st)
		delete(f.out, st.Name)
		if clearDone {
			delete(f.done, st)
			delete(f.fails, st)
			delete(f.missed, st)
		}
		f.reset(st.Stages, clearDone)
	}
}

func (f *Flow) holds(c *skills.StageCondition) bool {
	switch {
	case c == nil:
		return true
	case c.Available != "":
		return f.available != nil && f.available(c.Available)
	}
	stage, field, _ := strings.Cut(c.Field, ".")
	v, ok := f.out[stage][field]
	if c.Input != "" {
		v, ok = f.inputs[c.Input], f.inputs[c.Input] != nil
	}
	var got any
	b, _ := json.Marshal(v)
	if !ok || json.Unmarshal(b, &got) != nil {
		return false
	}
	var want any
	_ = json.Unmarshal(c.Equals, &want)
	return reflect.DeepEqual(got, want)
}

// Finish takes out as the open checklist stage's output, refusing one its out: rejects, records waived, and moves past the stage.
func (f *Flow) Finish(raw json.RawMessage, waived []skills.Waiver) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.current()
	if st == nil || st.Checklist == "" {
		return errors.New("no checklist stage is open")
	}
	var v map[string]any
	if len(st.Out) > 0 {
		if len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("stage %s needs out, matching %s", st.Name, schemaText(st))
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("out is not a JSON object: %w", err)
		}
		if err := validate(st, v); err != nil {
			return fmt.Errorf("out does not fit stage %s's schema %s: %w", st.Name, schemaText(st), err)
		}
		for _, k := range slices.Sorted(maps.Keys(st.Out)) {
			if err := f.screen("out."+k, st.Out[k], v[k]); err != nil {
				return err
			}
		}
	}
	f.out[st.Name] = v
	f.state[st] = skills.Done
	for _, w := range waived {
		f.record(w.Step, w.Reason)
	}
	f.top().cursor++
	return nil
}

func (f *Flow) screen(at string, field skills.OutField, v any) error {
	if field.Format == skills.FormatCommand && f.refuse != nil {
		s, _ := v.(string)
		if why := f.refuse(s); why != "" {
			return fmt.Errorf("%s is a command alux would refuse to run: %s", at, why)
		}
	}
	if items, ok := v.([]any); ok && field.Items != nil {
		for i, item := range items {
			if err := f.screen(fmt.Sprintf("%s[%d]", at, i), *field.Items, item); err != nil {
				return err
			}
		}
	}
	if obj, ok := v.(map[string]any); ok {
		for _, k := range slices.Sorted(maps.Keys(field.Properties)) {
			if err := f.screen(at+"."+k, field.Properties[k], obj[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Started marks the spawn stage at the cursor as waiting for its subagent.
func (f *Flow) Started() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[f.current()] = skills.Running
}

// Landed records waived, the waivers of the subagent run that the spawn stage at the cursor started, under that stage.
func (f *Flow) Landed(waived []skills.Waiver) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range waived {
		f.record(w.Step, w.Reason)
	}
}

// StartedEach marks items ids of the parallel foreach at the cursor as running.
func (f *Flow) StartedEach(ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.current()
	f.running[st] = map[string]bool{}
	for _, id := range ids {
		f.running[st][id] = true
	}
	f.state[st], f.state[&st.Stages[0]] = skills.Running, skills.Running
}

// LandedItem records how item id of the parallel foreach at the cursor went, with its run's waivers when it passed, and
// reports whether it was the last to land. Once all are in, any that failed stop the flow at the foreach for Retry, which
// runs only those again, or Waive; a strict spawn fails the flow, with the error returned.
func (f *Flow) LandedItem(id string, passed bool, waived []skills.Waiver) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.current()
	if st == nil || !f.running[st][id] {
		return false, nil
	}
	delete(f.running[st], id)
	if passed {
		if f.done[st] == nil {
			f.done[st] = map[string]bool{}
		}
		f.done[st][id] = true
		for _, w := range waived {
			f.recordItem(id, w.Step, w.Reason)
		}
	} else {
		f.missed[st] = append(f.missed[st], id)
	}
	if len(f.running[st]) > 0 {
		return false, nil
	}
	missed, inner := f.missed[st], &st.Stages[0]
	delete(f.missed, st)
	if len(missed) == 0 {
		return true, nil
	}
	f.fails[inner]++
	why := fmt.Sprintf("%s did not land complete for %s", inner.Name, strings.Join(missed, ", "))
	if inner.Strict {
		f.failed, f.failedAt = why, inner
		return true, errors.New(why)
	}
	delete(f.state, inner)
	f.stopped, f.stopReason = st, why
	delete(f.state, st)
	return true, nil
}

// Ran records how the run or spawn stage at the cursor went. A failure reopens its on_fail stage, in its own list or one
// around it, returning its name, until the stage has failed max times. Without on_fail, at max, or with on_fail not open,
// the flow stops at the stage for Retry or Waive, returning ("", nil); a strict stage fails the flow with the error returned.
func (f *Flow) Ran(passed bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.current()
	if passed {
		f.state[st] = skills.Done
		f.top().cursor++
		return "", nil
	}
	f.fails[st]++
	why := ""
	switch n := f.fails[st]; {
	case st.OnFail == "":
		why = st.Name + " failed"
	case st.Max > 0 && n >= st.Max:
		why = fmt.Sprintf("%s failed %d times, its max", st.Name, n)
	}
	if why != "" {
		return "", f.stop(st, why)
	}
	for k := len(f.stack) - 1; k >= 0; k-- {
		fr := f.stack[k]
		back := slices.IndexFunc(fr.stages[:fr.cursor+1], func(s skills.Stage) bool { return s.Name == st.OnFail })
		if back < 0 {
			continue
		}
		f.stack = f.stack[:k+1]
		f.forget(k, fr.stages[back:fr.cursor+1])
		f.reset(fr.stages[back:fr.cursor+1], false)
		fr.cursor = back
		return st.OnFail, nil
	}
	return "", f.stop(st, fmt.Sprintf("%s failed and its on_fail %s is not open", st.Name, st.OnFail))
}

// stop fails the flow at a strict stage and otherwise leaves the cursor on st, stopped, returning nil.
func (f *Flow) stop(st *skills.Stage, why string) error {
	if st.Strict {
		f.failed, f.failedAt = why, st
		return errors.New(why)
	}
	f.stopped, f.stopReason = st, why
	// A failed spawn is still marked Running, which would leave a retried one Waiting forever.
	delete(f.state, st)
	return nil
}

// Retry clears a stop so that Next makes the stopped stage again.
func (f *Flow) Retry() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped, f.stopReason = nil, ""
}

// Waive moves past the stopped stage as Waived for reason, with out standing in for every field of it that a later stage reads.
func (f *Flow) Waive(reason string, out map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stopped
	if st == nil {
		return errors.New("no stage is stopped")
	}
	var missing []string
	for _, k := range f.skill.Reads(st.Name) {
		if _, ok := out[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("later stages read stage %s's fields, so waiving it needs out with %s", st.Name, strings.Join(missing, ", "))
	}
	for _, k := range f.skill.Reads(st.Name) {
		v, err := readAs(st, k, out[k])
		if err != nil {
			return err
		}
		out[k] = v
	}
	f.out[st.Name] = out
	f.state[st] = skills.Waived
	f.stopped, f.stopReason = nil, ""
	f.record("", reason)
	f.top().cursor++
	return nil
}

// readAs is v as later stages read st's field: per its out schema, text as a string, an int capture as an integer.
func readAs(st *skills.Stage, field string, v any) (any, error) {
	want := skills.OutField{Type: "string"}
	switch {
	case st.Out[field].Type != "":
		want = st.Out[field]
	case st.Run != nil && st.Run.Capture[field].Type == "int":
		want = skills.OutField{Type: "integer"}
	}
	schema := fieldSchema(want)
	if err := fits(schema, v); err != nil {
		b, _ := json.Marshal(schema)
		return nil, fmt.Errorf("out.%s must fit %s, as later stages read it: %w", field, b, err)
	}
	if n, ok := v.(float64); ok && want.Type == "integer" {
		return int(n), nil
	}
	return v, nil
}

// Fails is how many times stage has failed in this run.
func (f *Flow) Fails(st *skills.Stage) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fails[st]
}

// Result is each stage's state, nested stages named <container>.<stage>; it is complete only once the flow's own list is done.
func (f *Flow) Result() skills.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := skills.Result{Complete: f.failed == "" && len(f.stack) == 1 && f.current() == nil}
	waived := map[*skills.Stage]int{}
	for _, w := range f.waivers {
		res.Waivers = append(res.Waivers, w.Waiver)
		waived[w.path[len(w.path)-1]]++
	}
	progress := map[*skills.Stage]string{}
	frames := map[*skills.Stage]*frame{}
	for _, fr := range f.stack[1:] {
		frames[fr.owner] = fr
		if fr.owner.Foreach != nil {
			id, _ := fr.items[fr.item]["id"].(string)
			progress[fr.owner] = fmt.Sprintf("item %d of %d: %s", fr.item+1, len(fr.items), id)
		} else {
			progress[fr.owner] = fmt.Sprintf("round %d of %d", fr.round, f.rounds(fr.owner))
		}
	}
	var walk func(prefix string, list []skills.Stage)
	walk = func(prefix string, list []skills.Stage) {
		for i := range list {
			st := &list[i]
			state, ok := f.state[st]
			if !ok {
				state = skills.Pending
			}
			n := skills.NodeResult{Name: prefix + st.Name, State: state, At: -1, Waived: waived[st], Fails: f.fails[st]}
			if fr := frames[st]; fr != nil && st.Foreach != nil {
				n.Item, n.Items = fr.item+1, len(fr.items)
			} else if fr != nil {
				n.Round, n.Rounds = fr.round, f.rounds(st)
			}
			if st == f.stopped {
				n.Stopped = f.stopReason
			}
			if p := progress[st]; p != "" {
				n.Missing = append(n.Missing, p)
			}
			if ids := f.running[st]; len(ids) > 0 {
				n.Missing = append(n.Missing, "running: "+strings.Join(slices.Sorted(maps.Keys(ids)), ", "))
			}
			if k := f.fails[st]; k > 0 {
				n.Missing = append(n.Missing, fmt.Sprintf("failed %d time(s)", k))
			}
			if st == f.failedAt {
				n.Missing = append(n.Missing, f.failed)
			}
			if st == f.stopped {
				n.Missing = append(n.Missing, "stopped: "+f.stopReason)
			}
			res.Nodes = append(res.Nodes, n)
			walk(prefix+st.Name+".", st.Stages)
		}
	}
	walk("", f.skill.Stages)
	res.Edges = f.skill.StageEdges()
	return res
}

// OutSchema is the JSON schema a checklist stage's out must fit, or nil for a stage with no out.
func OutSchema(st *skills.Stage) map[string]any {
	if len(st.Out) == 0 {
		return nil
	}
	props := map[string]any{}
	for k, field := range st.Out {
		props[k] = fieldSchema(field)
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             slices.Sorted(maps.Keys(st.Out)),
		"additionalProperties": false,
	}
}

func fieldSchema(f skills.OutField) map[string]any {
	m := map[string]any{"type": f.Type}
	if len(f.Enum) > 0 {
		m["enum"] = f.Enum
	}
	if f.Items != nil {
		m["items"] = fieldSchema(*f.Items)
	}
	if f.MinItems != nil {
		m["minItems"] = *f.MinItems
	}
	if len(f.Properties) > 0 {
		props := map[string]any{}
		for k, p := range f.Properties {
			props[k] = fieldSchema(p)
		}
		m["properties"], m["required"], m["additionalProperties"] = props, slices.Sorted(maps.Keys(f.Properties)), false
	}
	return m
}

func schemaText(st *skills.Stage) string {
	b, _ := json.Marshal(OutSchema(st))
	return string(b)
}

// CommandFields names the out fields of st that must be commands alux would run, at any depth.
func CommandFields(st *skills.Stage) []string {
	var out []string
	var walk func(at string, f skills.OutField)
	walk = func(at string, f skills.OutField) {
		if f.Format == skills.FormatCommand {
			out = append(out, at)
		}
		if f.Items != nil {
			walk(at+"[]", *f.Items)
		}
		for _, k := range slices.Sorted(maps.Keys(f.Properties)) {
			walk(at+"."+k, f.Properties[k])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(st.Out)) {
		walk(k, st.Out[k])
	}
	return out
}

func validate(st *skills.Stage, v map[string]any) error {
	return fits(OutSchema(st), v)
}

func fits(schema map[string]any, v any) error {
	data, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return err
	}
	var instance any
	data, _ = json.Marshal(v)
	_ = json.Unmarshal(data, &instance)
	return r.Validate(instance)
}

// Param is finish_stage, offered for the whole session so the tool list never changes as stages open and close.
func Param() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: Tool,
		Description: anthropic.String("End the open stage of a skill flow and move to the next. Make it the only tool call in its message. " +
			"alux refuses while a required step of the stage has no evidence from calls made since the stage opened, or while out " +
			"does not fit the stage's out schema. alux then runs the stages it does itself and returns their output with the next " +
			"stage's steps. A step or failed stage whose check is too strict for the case at hand can be waived in waive with a reason, " +
			"which alux records and reports; waiving a stage whose fields later stages read needs out giving those fields. " +
			"Call it only while a flow is open; with none open it returns an error."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"out":   map[string]any{"type": "object", "description": "The stage's output, matching the out schema its steps gave. Leave it out for a stage with none."},
				"waive": WaiveParam(),
			},
		},
	}}
}

// WaiveParam is the waive input finish_skill and finish_stage take.
func WaiveParam() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Steps to close without their evidence, or a failed stage to move past, each with why.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step": map[string]any{"type": "string", "description": "The step's name as the refusal lists it, or the name of the stopped stage."},
				"reason": map[string]any{"type": "string", "description": "Why closing the step without its evidence, or moving past the " +
					"failed stage, is right for this case. alux reports it with the run."},
			},
			"required": []string{"step", "reason"},
		},
	}
}
