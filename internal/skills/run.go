package skills

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Run is a skill with its inputs bound and its vars expanded, ready to be checked against a log.
type Run struct {
	Skill  *Skill
	Inputs map[string]any
	Vars   map[string]string
	now    time.Time
	home   string

	// Stage looks up ${<stage>.<field>} in a flow; nil outside one.
	Stage func(stage, field string) (any, bool)
	// Out is the out a finish_stage call gives, which a node's when: {out: ...} reads; nil skips such nodes.
	Out map[string]any
}

// Now is the time ${now_utc} stands for in this run.
func (r *Run) Now() time.Time { return r.now }

func (r *Run) stageValue(ref string) (any, bool) {
	stage, field, ok := strings.Cut(ref, ".")
	if !ok || r.Stage == nil {
		return nil, false
	}
	return r.Stage(stage, field)
}

// isStageRef reports whether ref names a stage of the flow, filled or not.
func (r *Run) isStageRef(ref string) bool {
	stage, _, ok := strings.Cut(ref, ".")
	return ok && r.Stage != nil && r.Skill.stageNames()[stage]
}

// Bind fills every input from given or its default, and expands vars with now as ${now_utc}.
func Bind(s *Skill, given map[string]any, now time.Time, home string) (*Run, error) {
	r := &Run{Skill: s, Inputs: map[string]any{}, Vars: map[string]string{}, now: now.UTC(), home: home}
	var p Problems
	for name := range given {
		if _, ok := s.Inputs[name]; !ok {
			p = append(p, fmt.Sprintf("%s is not an input of %s", name, s.Name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
		in := s.Inputs[name]
		v, ok := given[name]
		if !ok {
			v = in.Default
		}
		if v == nil {
			p = append(p, fmt.Sprintf("input %s is required", name))
			continue
		}
		v, ok = normal(in.Type, v)
		if !ok {
			p = append(p, fmt.Sprintf("input %s: %v is not %s", name, given[name], article(in.Type)))
			continue
		}
		if len(in.Enum) > 0 && !slices.ContainsFunc(in.Enum, func(e any) bool { return same(e, v) }) {
			p = append(p, fmt.Sprintf("input %s: %v is not one of %v", name, v, in.Enum))
			continue
		}
		r.Inputs[name] = v
	}
	if len(p) > 0 {
		return nil, p
	}
	for name, text := range s.Vars {
		r.Vars[name] = r.Expand(text)
	}
	return r, nil
}

// Expand fills ${input}, ${VAR} and ${now_utc}, and a leading ~/ with the home directory.
func (r *Run) Expand(text string) string {
	return r.expandHome(r.fillRefs(text, func(s string) string { return s }))
}

func (r *Run) expandHome(out string) string {
	if rest, ok := strings.CutPrefix(out, "~/"); ok {
		return filepath.Join(r.home, rest)
	}
	return out
}

// fillRefs fills every ${name} Expand knows, passing each filled value through quote.
func (r *Run) fillRefs(text string, quote func(string) string) string {
	return templateRef.ReplaceAllStringFunc(text, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := r.Inputs[name]; ok {
			return quote(fmt.Sprint(v))
		}
		if v, ok := r.Vars[name]; ok {
			return quote(v)
		}
		if v, ok := r.stageValue(name); ok {
			return quote(fmt.Sprint(v))
		}
		if name == "now_utc" {
			return quote(r.now.Format("20060102T150405Z"))
		}
		return m
	})
}

// fillStep is st's input with ${...} refs filled, as JSON ready to dispatch.
// In a bash command each filled value is shell-quoted, so an input can never inject shell; $${...} becomes ${...}.
// A command that is one whole ${input} is the exception: that input is the command line.
func (r *Run) fillStep(st *Step) (json.RawMessage, error) {
	if err := r.capturesReady(st.Input); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(st.Input))
	for k, v := range st.Input {
		plain := func(s string) string { return s }
		quote, home := plain, r.expandHome
		if st.Tool == "bash" && k == "command" {
			quote, home = shellQuote, plain
			if r.wholeInput(st) {
				quote = plain
			}
		}
		fill := func(s string) string {
			return home(fillEscaped(s, func(s string) string { return r.fillRefs(s, quote) }))
		}
		out[k] = r.fillStrings(v, fill)
	}
	return json.Marshal(out)
}

// wholeInput reports whether st is a bash call whose command is exactly one ${input}, which fills unquoted as a command line.
func (r *Run) wholeInput(st *Step) bool {
	if st.Tool != "bash" {
		return false
	}
	command, _ := st.Input["command"].(string)
	command = strings.TrimSpace(command)
	m := templateRef.FindStringSubmatch(command)
	if m == nil || m[0] != command {
		return false
	}
	_, ok := r.Inputs[m[1]]
	return ok
}

// capturesReady fails when input refers to a stage that has no value yet.
func (r *Run) capturesReady(input map[string]any) error {
	unfilled, _ := json.Marshal(input)
	for _, m := range templateRef.FindAllStringSubmatch(escapedRef.ReplaceAllString(string(unfilled), ""), -1) {
		if r.isStageRef(m[1]) {
			if _, ok := r.stageValue(m[1]); !ok {
				return fmt.Errorf("${%s} has no value: its stage has not run, or was skipped", m[1])
			}
		}
	}
	return nil
}

// intCapture is the number a string that is exactly one int-valued ${stage.field} stands for.
func (r *Run) intCapture(s string) (int, bool) {
	m := templateRef.FindStringSubmatch(s)
	if m == nil || m[0] != s {
		return 0, false
	}
	switch v, _ := r.stageValue(m[1]); n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), n == math.Trunc(n)
	}
	return 0, false
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fillStrings is a deep copy of v with fill applied to every string in it.
func (r *Run) fillStrings(v any, fill func(string) string) any {
	switch v := v.(type) {
	case string:
		if n, ok := r.intCapture(v); ok {
			return n
		}
		return fill(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = r.fillStrings(item, fill)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = r.fillStrings(item, fill)
		}
		return out
	}
	return v
}

func (r *Run) count(c Count) int {
	if c.Ref == "" {
		return c.N
	}
	n, _ := r.Inputs[c.Ref].(int)
	return n
}

// normal turns a given or decoded value into the Go type an input of typ holds: string, int or bool.
func normal(typ string, v any) (any, bool) {
	switch typ {
	case "string":
		s, ok := v.(string)
		return s, ok
	case "bool":
		b, ok := v.(bool)
		return b, ok
	case "int":
		switch n := v.(type) {
		case int:
			return n, true
		case float64:
			return int(n), n == math.Trunc(n)
		}
	}
	return nil, false
}

// same compares a value from the skill file, decoded from JSON, with a bound input.
func same(fromFile, bound any) bool {
	if f, ok := fromFile.(float64); ok {
		n, isInt := bound.(int)
		return isInt && float64(n) == f
	}
	return fromFile == bound
}
