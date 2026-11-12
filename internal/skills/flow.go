package skills

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/google/jsonschema-go/jsonschema"
)

// Stage is one named stage of a flow: a checklist file the model works through, one tool call alux makes,
// a subagent alux starts, or a list of stages run per item or per round.
type Stage struct {
	Name      string   `json:"-"`
	Checklist string   `json:"checklist"`
	Run       *Step    `json:"run"`
	Spawn     *Spawn   `json:"spawn"`
	Foreach   *Foreach `json:"foreach"`
	Repeat    *Repeat  `json:"repeat"`
	// Stages is what a foreach runs per item and a repeat runs per round.
	Stages []Stage `json:"stages"`
	// Inputs fills a checklist stage's inputs, with the same refs a run's input takes.
	Inputs map[string]any      `json:"inputs"`
	Out    map[string]OutField `json:"out"`
	When   *StageCondition     `json:"when"`
	OnFail string              `json:"on_fail"`
	Max    int                 `json:"max"`
	// Strict fails the flow when this run or spawn stage fails, rather than stopping for a rerun or waiver.
	Strict bool `json:"strict"`
}

func (st *Stage) UnmarshalJSON(b []byte) error {
	var one map[string]json.RawMessage
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	if len(one) != 1 {
		return fmt.Errorf("a stage is its name mapped to its fields, but this one has %d keys", len(one))
	}
	type plain Stage
	for name, body := range one {
		var p plain
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
		*st = Stage(p)
		st.Name = name
	}
	return nil
}

// Spawn is a subagent pinned to a skill, started in the background; its stage ends when the run lands.
type Spawn struct {
	Agent  string         `json:"agent"`
	Run    string         `json:"run"`
	Inputs map[string]any `json:"inputs"`
	// Change is the jj change the subagent edits in place; "" starts a new one.
	Change string `json:"change"`
	// Report is where alux writes the run's final report when it lands complete, under the skill runs' state dir.
	Report string `json:"report"`
}

// ReportRoot is the directory a spawn's report path must be under, relative to the home directory.
var ReportRoot = filepath.Join(paths.State, "skills")

// Foreach runs its stages once per item of the list In names, one at a time unless Parallel, with the item as ${<As>.<field>}.
type Foreach struct {
	In string `json:"in"`
	As string `json:"as"`
	// Parallel starts every item at once; its stages must be one spawn stage.
	Parallel bool `json:"parallel"`
}

// Repeat runs its stages until Until holds after a round, at most Max rounds before it asks the user.
type Repeat struct {
	Max   Count           `json:"max"`
	Until *StageCondition `json:"until"`
}

// OutField is one field of a checklist stage's out: a type name, a list of allowed strings, or a small JSON schema.
type OutField struct {
	Type     string    `json:"type"`
	Enum     []any     `json:"enum"`
	Format   string    `json:"format"`
	Items    *OutField `json:"items"`
	MinItems *int      `json:"minItems"`
	// Properties are an object's fields, every one required.
	Properties map[string]OutField `json:"properties"`
}

// FormatCommand marks a string field that must be a command alux's shell rules would run.
const FormatCommand = "command"

func (f *OutField) UnmarshalJSON(b []byte) error {
	var typ string
	if json.Unmarshal(b, &typ) == nil {
		*f = OutField{Type: typ}
		return nil
	}
	var enum []any
	if json.Unmarshal(b, &enum) == nil {
		*f = OutField{Type: "string", Enum: enum}
		return nil
	}
	type plain OutField
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*f = OutField(p)
	return nil
}

// StageCondition is an earlier stage's out field or an input equal to a value, or a tool being available.
type StageCondition struct {
	Field     string          `json:"field"`
	Input     string          `json:"input"`
	Equals    json.RawMessage `json:"equals"`
	Available string          `json:"available"`
}

// Describe is the condition in words, such as "when work.outcome is "changed"".
func (c *StageCondition) Describe() string {
	switch {
	case c.Field != "":
		return fmt.Sprintf("when %s is %s", c.Field, c.Equals)
	case c.Input != "":
		return fmt.Sprintf("when input %s is %s", c.Input, c.Equals)
	}
	return "when " + c.Available + " is available"
}

// TextField is a run stage's whole result, as ${<stage>.text}.
const TextField = "text"

// RoundField is a repeat's current round from 1, as ${<repeat>.round} inside it.
const RoundField = "round"

var (
	flowOnce   sync.Once
	flowSchema schemas
	flowErr    error
)

func parseFlow(name string, normal []byte, instance any) (*Skill, error) {
	flowOnce.Do(func() {
		var s jsonschema.Schema
		if err := json.Unmarshal(schemaFlow, &s); err != nil {
			flowErr = fmt.Errorf("embedded flow schema: %w", err)
			return
		}
		if flowSchema.root, flowErr = s.Resolve(nil); flowErr != nil {
			flowErr = fmt.Errorf("embedded flow schema: %w", flowErr)
		}
	})
	if flowErr != nil {
		return nil, flowErr
	}
	if err := flowSchema.root.Validate(instance); err != nil {
		return nil, Problems{readable("", err)}
	}
	var s Skill
	dec := json.NewDecoder(bytes.NewReader(normal))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, Problems{err.Error()}
	}
	if len(s.Starts) == 0 {
		s.Starts = []string{"user", "model"}
	}
	if problems := s.checkFlow(name); len(problems) > 0 {
		return nil, problems
	}
	return &s, nil
}

// checkFlow finds what the flow schema cannot: stage references, conditions against out:, and run inputs' refs.
func (s *Skill) checkFlow(name string) Problems {
	var p Problems
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	if name != "" && s.Name != name {
		add("skill: %s does not match the file name %s.yaml", s.Name, name)
	}
	for _, in := range slices.Sorted(maps.Keys(s.Inputs)) {
		s.checkInput(in, add)
	}
	for _, v := range slices.Sorted(maps.Keys(s.Vars)) {
		s.checkTemplate("vars."+v, s.Vars[v], false, add)
	}
	s.checkStages("stages", s.Stages, scope{}, add)
	return p
}

// scope is what a stage may refer to: the stages before it in its own list and in every list around it,
// and each enclosing foreach's item. Those stages are also the ones its on_fail may name.
type scope struct {
	stages map[string]*Stage
	items  map[string]OutField
}

func (sc scope) withStage(st *Stage) scope {
	out := scope{stages: maps.Clone(sc.stages), items: sc.items}
	if out.stages == nil {
		out.stages = map[string]*Stage{}
	}
	out.stages[st.Name] = st
	return out
}

func (sc scope) withItem(as string, item OutField) scope {
	out := scope{stages: sc.stages, items: maps.Clone(sc.items)}
	if out.items == nil {
		out.items = map[string]OutField{}
	}
	out.items[as] = item
	return out
}

// lookup is the out field ${stage.field} reads, or why it reads nothing here.
func (sc scope) lookup(stage, field string) (OutField, string) {
	if item, ok := sc.items[stage]; ok {
		if f, ok := item.Properties[field]; ok {
			return f, ""
		}
		return OutField{}, fmt.Sprintf("item %s has no field %s", stage, field)
	}
	st, ok := sc.stages[stage]
	switch {
	case !ok:
		return OutField{}, fmt.Sprintf("names stage %s, which is not earlier", stage)
	case st.Run != nil:
		if field == TextField {
			return OutField{Type: "string"}, ""
		}
		c, ok := st.Run.Capture[field]
		if !ok {
			return OutField{}, fmt.Sprintf("run stage %s has %s and no capture %s", stage, TextField, field)
		}
		if c.Type == "int" {
			return OutField{Type: "integer"}, ""
		}
		return OutField{Type: "string"}, ""
	case st.Checklist != "":
		if f, ok := st.Out[field]; ok {
			return f, ""
		}
		return OutField{}, fmt.Sprintf("stage %s has no out field %s", stage, field)
	}
	return OutField{}, fmt.Sprintf("stage %s puts out nothing", stage)
}

func (s *Skill) checkStages(prefix string, list []Stage, sc scope, add func(string, ...any)) {
	seen := map[string]bool{}
	for i := range list {
		st := &list[i]
		at := fmt.Sprintf("%s[%d].%s", prefix, i, st.Name)
		if seen[st.Name] {
			add("%s: another stage in this list is already called %s", at, st.Name)
		}
		gatedOn := s.checkStageWhen(at+".when", st.When, sc, add)
		kinds := 0
		for _, set := range []bool{st.Checklist != "", st.Run != nil, st.Spawn != nil, st.Foreach != nil, st.Repeat != nil} {
			if set {
				kinds++
			}
		}
		container := st.Foreach != nil || st.Repeat != nil
		if kinds != 1 {
			add("%s: a stage has exactly one of checklist, run, spawn, foreach or repeat", at)
		}
		if container != (len(st.Stages) > 0) {
			add("%s: a foreach or repeat stage, and only one, has stages", at)
		}
		if st.Out != nil && st.Checklist == "" {
			add("%s.out: only a checklist stage has out", at)
		}
		if st.Inputs != nil && st.Checklist == "" {
			add("%s.inputs: only a checklist stage takes inputs; a spawn takes them under spawn", at)
		}
		switch {
		case kinds != 1:
		case st.Foreach != nil:
			item := s.checkForeach(at+".foreach", st.Foreach, sc, add)
			s.checkStages(at+".stages", st.Stages, sc.withItem(st.Foreach.As, item), add)
			if st.Foreach.Parallel {
				if one := st.Stages; len(one) != 1 || one[0].Spawn == nil || one[0].When != nil || one[0].OnFail != "" {
					add("%s.foreach.parallel: a parallel foreach runs one spawn stage, with no when or on_fail", at)
				}
			}
		case st.Repeat != nil:
			s.checkCount(at+".repeat.max", st.Repeat.Max, add)
			if st.Repeat.Max.Ref == "" && st.Repeat.Max.N < 1 {
				add("%s.repeat.max: a repeat runs at least once, so max is 1 or more", at)
			}
			inner := sc
			for j := range st.Stages {
				inner = inner.withStage(&st.Stages[j])
			}
			if st.Repeat.Until == nil || st.Repeat.Until.Available != "" {
				add("%s.repeat.until: needs field: <stage>.<out field> or input: <name> with equals: <value>", at)
			} else {
				s.checkStageWhen(at+".repeat.until", st.Repeat.Until, inner, add)
			}
			round := OutField{Type: "object", Properties: map[string]OutField{RoundField: {Type: "integer"}}}
			s.checkStages(at+".stages", st.Stages, sc.withItem(st.Name, round), add)
		case st.Spawn != nil:
			if st.Spawn.Run == s.Name {
				add("%s.spawn.run: a flow cannot spawn itself", at)
			}
			for _, k := range slices.Sorted(maps.Keys(st.Spawn.Inputs)) {
				s.checkRunInput(at+".spawn.inputs."+k, st.Spawn.Inputs[k], sc, add)
			}
			s.checkRunInput(at+".spawn.change", st.Spawn.Change, sc, add)
			if st.Spawn.Report != "" {
				s.checkRunInput(at+".spawn.report", st.Spawn.Report, sc, add)
				s.checkReport(at+".spawn.report", st.Spawn.Report, add)
			}
		case st.Run != nil:
			s.checkRequired(at+".run", st.Run.Tool, gatedOn, add)
			for _, k := range slices.Sorted(maps.Keys(st.Run.Capture)) {
				s.checkStageCapture(at+".run.capture."+k, k, st.Run.Capture[k].Pattern, add)
			}
			for _, k := range slices.Sorted(maps.Keys(st.Run.Input)) {
				s.checkRunInput(at+".run.input."+k, st.Run.Input[k], sc, add)
			}
			if script, ok := st.Run.Input["command"].(string); ok && st.Run.Tool == "bash" {
				checkScript(at+".run.input.command", script, add)
			}
		default:
			for _, k := range slices.Sorted(maps.Keys(st.Out)) {
				checkOutField(at+".out."+k, st.Out[k], add)
			}
			for _, k := range slices.Sorted(maps.Keys(st.Inputs)) {
				s.checkRunInput(at+".inputs."+k, st.Inputs[k], sc, add)
			}
		}
		if st.OnFail != "" && st.Run == nil && st.Spawn == nil {
			add("%s.on_fail: only a run or spawn stage fails; the others end when their work is in", at)
		}
		if _, ok := sc.stages[st.OnFail]; st.OnFail != "" && !ok {
			add("%s.on_fail: %s is not an earlier stage in this list or one around it", at, st.OnFail)
		}
		if st.Strict && st.Run == nil && st.Spawn == nil {
			add("%s.strict: only a run or spawn stage fails; the others end when their work is in", at)
		}
		if st.Max != 0 && st.OnFail == "" {
			add("%s.max: max limits on_fail, which this stage does not have", at)
		}
		if !seen[st.Name] {
			sc = sc.withStage(st)
		}
		seen[st.Name] = true
	}
}

// checkReport checks that a spawn's report path, with a leading ${VAR} resolved, is under ~/ReportRoot.
func (s *Skill) checkReport(at, path string, add func(string, ...any)) {
	if m := templateRef.FindStringSubmatchIndex(path); m != nil && m[0] == 0 {
		if v, ok := s.Vars[path[m[2]:m[3]]]; ok {
			path = v + path[m[1]:]
		}
	}
	if !strings.HasPrefix(path, "~/"+ReportRoot+"/") || slices.Contains(strings.Split(path, "/"), "..") {
		add("%s: %s is not a path under ~/%s", at, path, ReportRoot)
	}
}

// checkForeach checks that in names a list of objects keyed by a string id, and returns the item's field.
func (s *Skill) checkForeach(at string, f *Foreach, sc scope, add func(string, ...any)) OutField {
	if !fieldName.MatchString(f.As) {
		add("%s.as: %q must be lowercase letters, digits and underscores", at, f.As)
	}
	if _, clash := sc.stages[f.As]; clash {
		add("%s.as: %s is already a stage's name", at, f.As)
	}
	stage, field, _ := strings.Cut(f.In, ".")
	list, problem := sc.lookup(stage, field)
	switch {
	case problem != "":
		add("%s.in: ${%s}: %s", at, f.In, problem)
	case list.Type != "array" || list.Items == nil || list.Items.Type != "object":
		add("%s.in: %s is not a list of objects", at, f.In)
	case list.Items.Properties["id"].Type != "string":
		add("%s.in: the items of %s need a string id, which keys them", at, f.In)
	default:
		return *list.Items
	}
	return OutField{}
}

// stageNames is every stage name and foreach item name in the flow, which a ${name.field} ref may mean.
func (s *Skill) stageNames() map[string]bool {
	out := map[string]bool{}
	var walk func([]Stage)
	walk = func(list []Stage) {
		for _, st := range list {
			out[st.Name] = true
			if st.Foreach != nil {
				out[st.Foreach.As] = true
			}
			walk(st.Stages)
		}
	}
	walk(s.Stages)
	return out
}

func (s *Skill) checkStageCapture(at, name, pattern string, add func(string, ...any)) {
	if name == TextField {
		add("%s: %s is taken by ${<stage>.%s}, the stage's whole result", at, name, TextField)
	}
	if re, err := regexp.Compile(pattern); err != nil {
		add("%s: %v", at, err)
	} else if re.NumSubexp() == 0 {
		add("%s: the pattern needs a group, whose match is the value", at)
	}
}

// checkRunInput checks an input's ${stage.field} refs against sc and its other refs as a step input's.
func (s *Skill) checkRunInput(at string, v any, sc scope, add func(string, ...any)) {
	switch v := v.(type) {
	case string:
		names := s.stageNames()
		rest := templateRef.ReplaceAllStringFunc(escapedRef.ReplaceAllString(v, ""), func(m string) string {
			stage, field, ok := strings.Cut(m[2:len(m)-1], ".")
			if !ok || !names[stage] {
				return m
			}
			if _, problem := sc.lookup(stage, field); problem != "" {
				add("%s: ${%s.%s}: %s", at, stage, field, problem)
			}
			return ""
		})
		s.checkTemplate(at, rest, true, add)
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			s.checkRunInput(at+"."+k, v[k], sc, add)
		}
	case []any:
		for i, item := range v {
			s.checkRunInput(fmt.Sprintf("%s[%d]", at, i), item, sc, add)
		}
	}
}

// checkStageWhen checks a condition against the out: of the stages in sc and returns the tool it gates on, if any.
func (s *Skill) checkStageWhen(at string, c *StageCondition, sc scope, add func(string, ...any)) string {
	if c == nil {
		return ""
	}
	switch {
	case c.Available != "" && (c.Field != "" || c.Input != "" || c.Equals != nil):
		add("%s: available stands alone; it cannot be combined with field, input or equals", at)
	case c.Field != "" && c.Input != "":
		add("%s: a condition has field or input, not both", at)
	case c.Input != "":
		in, ok := s.Inputs[c.Input]
		if !ok {
			add("%s.input: no input named %s", at, c.Input)
			return ""
		}
		var v any
		_ = json.Unmarshal(c.Equals, &v)
		switch {
		case c.Equals == nil:
			add("%s: input %s needs equals: <value>", at, c.Input)
		case !fits(in.Type, v):
			add("%s.equals: %v is not %s, so input %s can never match it", at, v, article(in.Type), c.Input)
		}
	case c.Available != "":
		switch {
		case slices.Contains(s.Requires.Tools, c.Available):
			add("%s: %s is a required tool, so it is always available; list it under requires.optional_tools", at, c.Available)
		case !slices.Contains(s.Requires.OptionalTools, c.Available):
			add("%s: %s is not listed under requires.optional_tools", at, c.Available)
		}
		return c.Available
	case c.Field == "" || c.Equals == nil:
		add("%s: needs field: <stage>.<out field> or input: <name> with equals: <value>, or available: <tool>", at)
	default:
		stage, field, _ := strings.Cut(c.Field, ".")
		out, problem := sc.lookup(stage, field)
		if problem != "" {
			add("%s.field: %s", at, problem)
			return ""
		}
		var v any
		_ = json.Unmarshal(c.Equals, &v)
		switch {
		case out.Type == "array" || out.Type == "object":
			add("%s.equals: %s is %s, which equals cannot compare", at, c.Field, article(out.Type))
		case !fitsOut(out.Type, v):
			add("%s.equals: %v is not %s, so %s can never match it", at, v, article(out.Type), c.Field)
		case len(out.Enum) > 0 && !slices.Contains(out.Enum, v):
			add("%s.equals: %v is not one of %v, so %s can never match it", at, v, out.Enum, c.Field)
		}
	}
	return ""
}

func checkOutField(at string, f OutField, add func(string, ...any)) {
	switch f.Type {
	case "string", "integer", "boolean", "array", "object":
	default:
		add("%s: %q is not a type out takes; use string, integer, boolean, array or object", at, f.Type)
	}
	if (len(f.Properties) > 0) != (f.Type == "object") {
		add("%s: an object field, and only an object field, has properties", at)
	}
	for _, k := range slices.Sorted(maps.Keys(f.Properties)) {
		checkOutField(at+".properties."+k, f.Properties[k], add)
	}
	if f.Format != "" && f.Type != "string" {
		add("%s.format: format %s is for a string field", at, f.Format)
	}
	if (f.Items != nil) != (f.Type == "array") {
		add("%s: an array field, and only an array field, has items", at)
	}
	if f.MinItems != nil && f.Type != "array" {
		add("%s.minItems: minItems is for an array field", at)
	}
	for i, v := range f.Enum {
		if !fitsOut(f.Type, v) {
			add("%s.enum[%d]: %v is not %s", at, i, v, article(f.Type))
		}
	}
	if f.Items != nil {
		checkOutField(at+".items", *f.Items, add)
	}
}

func fitsOut(typ string, v any) bool {
	if typ == "integer" {
		typ = "int"
	}
	if typ == "boolean" {
		typ = "bool"
	}
	return fits(typ, v)
}

// FillStep is a run stage's input with its refs filled as a step's are, and whether its bash command is one whole input.
func (r *Run) FillStep(st *Step) (json.RawMessage, bool, error) {
	in, err := r.fillStep(st)
	return in, r.wholeInput(st), err
}

// FillSpawn is a spawn stage's inputs and change with their refs filled; it fails while a ref's stage has no value.
func (r *Run) FillSpawn(sp *Spawn) (map[string]any, string, error) {
	if err := r.capturesReady(map[string]any{"change": sp.Change}); err != nil {
		return nil, "", err
	}
	inputs, err := r.FillInputs(sp.Inputs)
	return inputs, strings.TrimSpace(r.Expand(sp.Change)), err
}

// WithItem is r with ${<as>.<field>} reading item, for one item of a parallel foreach while the others run.
func (r *Run) WithItem(as string, item map[string]any) *Run {
	out, stage := *r, r.Stage
	out.Stage = func(name, field string) (any, bool) {
		if name == as {
			v, ok := item[field]
			return v, ok
		}
		if stage == nil {
			return nil, false
		}
		return stage(name, field)
	}
	return &out
}

// FillInputs is a spawn's or checklist stage's inputs with their refs filled; it fails while a ref's stage has no value.
func (r *Run) FillInputs(given map[string]any) (map[string]any, error) {
	if err := r.capturesReady(given); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(given))
	for k, v := range given {
		out[k] = r.fillStrings(v, r.Expand)
	}
	return out, nil
}

// Walk calls fn on every stage of the flow, depth first, with its path: its name, under each container's name and a dot.
func (s *Skill) Walk(fn func(path string, st *Stage)) {
	var walk func(prefix string, list []Stage)
	walk = func(prefix string, list []Stage) {
		for i := range list {
			fn(prefix+list[i].Name, &list[i])
			walk(prefix+list[i].Name+".", list[i].Stages)
		}
	}
	walk("", s.Stages)
}

// Reads is every field of stage that some stage of the flow reads, through a ${stage.field} ref, a condition or a foreach, sorted.
func (s *Skill) Reads(stage string) []string {
	got := map[string]bool{}
	var refs func(v any)
	refs = func(v any) {
		switch v := v.(type) {
		case string:
			for _, m := range templateRef.FindAllString(escapedRef.ReplaceAllString(v, ""), -1) {
				if st, field, ok := strings.Cut(m[2:len(m)-1], "."); ok && st == stage {
					got[field] = true
				}
			}
		case map[string]any:
			for _, item := range v {
				refs(item)
			}
		case []any:
			for _, item := range v {
				refs(item)
			}
		}
	}
	field := func(ref string) {
		if st, f, ok := strings.Cut(ref, "."); ok && st == stage {
			got[f] = true
		}
	}
	s.Walk(func(_ string, st *Stage) {
		refs(st.Inputs)
		if st.Run != nil {
			refs(st.Run.Input)
		}
		if st.Spawn != nil {
			refs(st.Spawn.Inputs)
			refs(st.Spawn.Change)
			refs(st.Spawn.Report)
		}
		if st.When != nil {
			field(st.When.Field)
		}
		if st.Repeat != nil && st.Repeat.Until != nil {
			field(st.Repeat.Until.Field)
		}
		if st.Foreach != nil {
			field(st.Foreach.In)
		}
	})
	return slices.Sorted(maps.Keys(got))
}

// Container says what a foreach or repeat stage runs over, such as "for each change of plan.changes".
func (st *Stage) Container() string {
	switch {
	case st.Foreach != nil && st.Foreach.Parallel:
		return fmt.Sprintf("for each %s of %s, all at once", st.Foreach.As, st.Foreach.In)
	case st.Foreach != nil:
		return fmt.Sprintf("for each %s of %s", st.Foreach.As, st.Foreach.In)
	case st.Repeat != nil:
		return fmt.Sprintf("repeat at most %s rounds until %s", st.Repeat.Max, strings.TrimPrefix(st.Repeat.Until.Describe(), "when "))
	}
	return ""
}

// StageCall is a run stage's call as a brief shows it.
func (r *Run) StageCall(st *Step) string { return r.stepCall(st) }

// StageEdges is a flow's graph over the stage paths Walk names: each stage on to the next, a foreach or repeat into
// its first stage and from its last back to itself, a dashed skip past each run of stages under one when, and a dashed on_fail.
func (s *Skill) StageEdges() []Edge {
	var out []Edge
	var walk func(prefix, owner, again string, list []Stage, outer []map[string]string)
	walk = func(prefix, owner, again string, list []Stage, outer []map[string]string) {
		path := func(i int) string { return prefix + list[i].Name }
		next := func(i int) (string, string) {
			if i+1 < len(list) {
				return path(i + 1), ""
			}
			return owner, again
		}
		if owner != "" && len(list) > 0 {
			out = append(out, Edge{From: owner, To: path(0)})
		}
		earlier := map[string]string{}
		for i := range list {
			st := &list[i]
			if to, label := next(i); to != "" {
				if label == "" && len(st.Stages) > 0 {
					label = "done"
				}
				out = append(out, Edge{From: path(i), To: to, Label: label})
			}
			if st.When != nil && (i == 0 || !sameWhen(list[i-1].When, st.When)) {
				j := i
				for j+1 < len(list) && sameWhen(list[j+1].When, st.When) {
					j++
				}
				from := owner
				if i > 0 {
					from = path(i - 1)
				}
				if to, _ := next(j); from != "" && to != "" && from != to {
					out = append(out, Edge{From: from, To: to, Label: "skip", Dashed: true})
				}
			}
			if to, ok := onFailTarget(st.OnFail, earlier, outer); ok {
				label := "on fail"
				if st.Max > 0 {
					label = fmt.Sprintf("on fail, at most %d", st.Max)
				}
				out = append(out, Edge{From: path(i), To: to, Label: label, Dashed: true})
			}
			if len(st.Stages) > 0 {
				again := "next round"
				if st.Foreach != nil {
					again = "next item"
				}
				walk(path(i)+".", path(i), again, st.Stages, append(slices.Clone(outer), maps.Clone(earlier)))
			}
			earlier[st.Name] = path(i)
		}
	}
	walk("", "", "", s.Stages, nil)
	return out
}

func sameWhen(a, b *StageCondition) bool {
	return a != nil && b != nil && a.Describe() == b.Describe()
}

// onFailTarget is the path of the stage on_fail names: an earlier one in its own list, else in the nearest list around it.
func onFailTarget(name string, earlier map[string]string, outer []map[string]string) (string, bool) {
	if name == "" {
		return "", false
	}
	if p, ok := earlier[name]; ok {
		return p, true
	}
	for k := len(outer) - 1; k >= 0; k-- {
		if p, ok := outer[k][name]; ok {
			return p, true
		}
	}
	return "", false
}

func stageOutline(s *Skill) string {
	var b strings.Builder
	b.WriteString("```mermaid\nflowchart LR\n")
	ids := map[string]string{}
	s.Walk(func(path string, st *Stage) {
		ids[path] = fmt.Sprintf("n%d", len(ids))
		who := "checklist: " + st.Checklist
		switch {
		case st.Run != nil:
			who = "run: alux runs " + st.Run.Tool
		case st.Spawn != nil:
			who = "spawn: " + st.Spawn.Agent + " runs " + st.Spawn.Run
		case st.Foreach != nil || st.Repeat != nil:
			who = st.Container()
		}
		if st.When != nil {
			who += " (" + st.When.Describe() + ")"
		}
		fmt.Fprintf(&b, "  %s[\"%s<br/>%s\"]\n", ids[path], outlineEscapes.Replace(st.Name), outlineEscapes.Replace(who))
	})
	writeEdges(&b, ids, s.StageEdges())
	b.WriteString("```")
	return b.String()
}
