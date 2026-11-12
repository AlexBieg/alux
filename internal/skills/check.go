package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

var builtins = []string{"now_utc"}

var (
	templateRef = regexp.MustCompile(`\$\{([^}]*)\}`)
	// escapedRef is $${...} in a step's input, passed on as a literal ${...} so a script can use shell expansions.
	escapedRef = regexp.MustCompile(`\$\$\{[^}]*\}`)
	fieldName  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// check finds what the schema cannot: references across the file that must agree.
func (s *Skill) check(name string) Problems {
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
	required := 0
	for _, n := range slices.Sorted(maps.Keys(s.Nodes)) {
		node := s.Nodes[n]
		if !node.Optional {
			required++
		}
		s.checkNode("nodes."+n, node, add)
	}
	if required == 0 {
		add("every node is optional, so a run could complete having done nothing")
	}
	s.checkOrder(add)
	return p
}

func (s *Skill) checkInput(name string, add func(string, ...any)) {
	in := s.Inputs[name]
	for i, v := range in.Enum {
		if !fits(in.Type, v) {
			add("inputs.%s.enum[%d]: %v is not %s", name, i, v, article(in.Type))
		}
	}
	if in.Default == nil {
		return
	}
	if !fits(in.Type, in.Default) {
		add("inputs.%s.default: %v is not %s", name, in.Default, article(in.Type))
	} else if len(in.Enum) > 0 && !slices.Contains(in.Enum, in.Default) {
		add("inputs.%s.default: %v is not one of its enum", name, in.Default)
	}
}

// checkScript runs bash -n over a bash step's command, with each ref standing in as a plain word.
func checkScript(at, script string, add func(string, ...any)) {
	text := fillEscaped(script, func(s string) string { return templateRef.ReplaceAllString(s, "x") })
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var exit *exec.ExitError
	if err := cmd.Run(); errors.As(err, &exit) {
		add("%s: bash -n: %s", at, strings.Join(strings.Fields(stderr.String()), " "))
	}
}

// fillEscaped applies fill to the text between $${...} escapes and drops one $ from each escape.
func fillEscaped(text string, fill func(string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range escapedRef.FindAllStringIndex(text, -1) {
		b.WriteString(fill(text[last:loc[0]]))
		b.WriteString(text[loc[0]+1 : loc[1]])
		last = loc[1]
	}
	b.WriteString(fill(text[last:]))
	return b.String()
}

// checkRequired checks that tool is required, or optional with its node gated on it being available.
func (s *Skill) checkRequired(at, tool, gatedOn string, add func(string, ...any)) {
	switch {
	case slices.Contains(s.Requires.Tools, tool):
	case slices.Contains(s.Requires.OptionalTools, tool):
		if gatedOn != tool {
			add("%s: %s is optional, so this node needs when: {available: %s} or it waits forever without it", at, tool, tool)
		}
	default:
		add("%s: tool %s is not listed under requires", at, tool)
	}
}

func (s *Skill) checkNode(at string, node Node, add func(string, ...any)) {
	if node.Say == "" {
		add("%s: needs say and evidence", at)
	}
	s.checkTemplate(at+".say", node.Say, true, add)
	if node.Loop != nil {
		s.checkCount(at+".loop.times", node.Loop.Times, add)
	}
	gatedOn := s.checkWhen(at, node.When, add)
	for i, r := range node.Evidence {
		ra := fmt.Sprintf("%s.evidence[%d]", at, i)
		switch {
		case r.Call != nil:
			t := r.Call
			if t.Min != nil {
				s.checkCount(ra+".call.min", *t.Min, add)
			}
			if t.When != "" {
				s.checkCallWhen(ra+".call.when", t.When, add)
			}
			for _, tool := range t.Tool {
				s.checkRequired(ra, tool, gatedOn, add)
			}
		case r.FetchesOpened != nil:
			s.checkCount(ra+".fetches_opened.min", r.FetchesOpened.Min, add)
		case r.ArtifactLines != nil:
			a := r.ArtifactLines
			s.checkTemplate(ra+".artifact_lines.path", a.Path, true, add)
			s.checkCount(ra+".artifact_lines.min", a.Min, add)
			if a.Max != nil {
				s.checkCount(ra+".artifact_lines.max", *a.Max, add)
			}
			s.checkTemplate(ra+".artifact_lines.section", a.Section, true, add)
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				add("%s.artifact_lines.pattern: %v", ra, err)
			}
			if cv := a.Covers; cv != nil {
				s.checkTemplate(ra+".artifact_lines.covers.path", cv.Path, true, add)
				if err == nil && re.NumSubexp() == 0 {
					add("%s.artifact_lines.pattern: covers keys each line by its first non-empty group, so the pattern needs a group", ra)
				}
				if cre, err := regexp.Compile(cv.Pattern); err != nil {
					add("%s.artifact_lines.covers.pattern: %v", ra, err)
				} else if cre.NumSubexp() == 0 {
					add("%s.artifact_lines.covers.pattern: the pattern needs a group, whose match is the line's key", ra)
				}
			}
		case r.CitationsFetched != nil:
			c := r.CitationsFetched
			s.checkTemplate(ra+".citations_fetched.path", c.Path, true, add)
			if c.Min != nil {
				s.checkCount(ra+".citations_fetched.min", *c.Min, add)
			}
		case r.LeadsFollowed != nil:
			l := r.LeadsFollowed
			s.checkTemplate(ra+".leads_followed.path", l.Path, true, add)
			s.checkCount(ra+".leads_followed.min", l.Min, add)
			if l.NotInInput != "" {
				if in, ok := s.Inputs[l.NotInInput]; !ok {
					add("%s.leads_followed.not_in_input: no input named %s", ra, l.NotInInput)
				} else if in.Type != "string" {
					add("%s.leads_followed.not_in_input: %s is %s input, not a string", ra, l.NotInInput, article(in.Type))
				}
			}
		case r.SubagentSkill != nil:
			if r.SubagentSkill.Min != nil {
				s.checkCount(ra+".subagent_skill.min", *r.SubagentSkill.Min, add)
			}
		case r.PollStopped != nil:
			if r.PollStopped.Min != nil {
				s.checkCount(ra+".poll_stopped.min", *r.PollStopped.Min, add)
			}
			s.checkRequired(ra, pollTool, gatedOn, add)
		}
	}
}

// checkCallWhen checks a call rule's CEL condition and that every input and var it reads exists here.
func (s *Skill) checkCallWhen(at, when string, add func(string, ...any)) {
	if strings.Contains(when, "${") {
		add("%s: a condition reads inputs.<name> and vars.<NAME>, never ${...}", at)
		return
	}
	ev, err := evidenceFor(when)
	if err != nil {
		add("%s: %s", at, strings.Join(strings.Fields(err.Error()), " "))
		return
	}
	for _, ref := range ev.Refs() {
		switch ref.Var {
		case "inputs":
			if _, ok := s.Inputs[ref.Key]; !ok {
				add("%s: inputs.%s is not an input", at, ref.Key)
			}
		case "vars":
			if _, ok := s.Vars[ref.Key]; !ok {
				add("%s: vars.%s is not a var", at, ref.Key)
			}
		case "scenario":
			add("%s: scenario.%s is never set in a checklist; a condition reads inputs.<name> and vars.<NAME>", at, ref.Key)
		}
	}
}

// checkWhen checks a node's condition and returns the tool it gates on, if any.
func (s *Skill) checkWhen(at string, c *Condition, add func(string, ...any)) string {
	gatedOn := ""
	if c != nil {
		gatedOn = c.Available
		hasIn := c.In != nil
		switch {
		case c.Available != "" && (c.Input != "" || c.Out != "" || c.Equals != nil || hasIn):
			add("%s.when: available stands alone; it cannot be combined with input, out, equals or in", at)
		case c.Available == "" && (c.Input == "") == (c.Out == ""):
			add("%s.when: needs available: <tool>, or one of input: <name> or out: <field> with equals or in", at)
		case c.Input != "" && (c.Equals != nil) == hasIn:
			add("%s.when: input: %s needs exactly one of equals or in", at, c.Input)
		case c.Out != "" && (c.Equals != nil) == hasIn:
			add("%s.when: out: %s needs exactly one of equals or in", at, c.Out)
		case c.Out != "":
		case c.Available != "":
			switch {
			case slices.Contains(s.Requires.Tools, c.Available):
				add("%s.when: %s is a required tool, so it is always available; list it under requires.optional_tools", at, c.Available)
			case !slices.Contains(s.Requires.OptionalTools, c.Available):
				add("%s.when: %s is not listed under requires.optional_tools", at, c.Available)
			}
		default:
			s.checkCondition(at+".when", c, add)
		}
	}
	return gatedOn
}

func (s *Skill) checkCondition(at string, c *Condition, add func(string, ...any)) {
	in, ok := s.Inputs[c.Input]
	if !ok {
		add("%s: no input named %s", at, c.Input)
		return
	}
	var values []any
	if c.Equals != nil {
		var v any
		_ = json.Unmarshal(c.Equals, &v)
		values = []any{v}
	} else {
		values = c.In
	}
	for _, v := range values {
		if !fits(in.Type, v) {
			add("%s: %v is not %s, so %s can never match it", at, v, article(in.Type), c.Input)
		} else if len(in.Enum) > 0 && !slices.Contains(in.Enum, v) {
			add("%s: %v is not one of %s's enum, so it can never match", at, v, c.Input)
		}
	}
}

func (s *Skill) checkCount(at string, c Count, add func(string, ...any)) {
	if c.Ref == "" {
		return
	}
	in, ok := s.Inputs[c.Ref]
	switch {
	case !ok:
		add("%s: no input named %s", at, c.Ref)
	case in.Type != "int":
		add("%s: %s is %s input, not an int", at, c.Ref, article(in.Type))
	case in.Default == nil:
		add("%s: input %s has no default, so a run started without it has no count", at, c.Ref)
	}
}

// checkTemplate checks every ${name} in text; allowVars is false inside vars themselves, which may not chain.
func (s *Skill) checkTemplate(at, text string, allowVars bool, add func(string, ...any)) {
	for _, m := range templateRef.FindAllStringSubmatch(text, -1) {
		ref := m[1]
		_, isInput := s.Inputs[ref]
		_, isVar := s.Vars[ref]
		switch {
		case isInput, slices.Contains(builtins, ref):
		case isVar && allowVars:
		case isVar:
			add("%s: ${%s} is a var, and a var may only use inputs and %s", at, ref, strings.Join(builtins, ", "))
		default:
			add("%s: ${%s} is not an input, a var or %s", at, ref, strings.Join(builtins, ", "))
		}
	}
	if strings.Count(text, "${") != len(templateRef.FindAllString(text, -1)) {
		add("%s: an unclosed ${", at)
	}
}

func (s *Skill) checkOrder(add func(string, ...any)) {
	next := map[string][]string{}
	for i, chain := range s.Order {
		for j, n := range chain {
			if _, ok := s.Nodes[n]; !ok {
				add("order[%d][%d]: no node named %s", i, j, n)
			}
			if j > 0 {
				next[chain[j-1]] = append(next[chain[j-1]], n)
			}
		}
	}
	if cycle := findCycle(next); cycle != nil {
		add("order has a cycle: %s", strings.Join(cycle, " -> "))
	}
}

// findCycle returns one cycle in the ordering graph, closed back on its first node, or nil.
func findCycle(next map[string][]string) []string {
	const (
		unseen = iota
		open
		done
	)
	state := map[string]int{}
	var path []string
	var visit func(n string) []string
	visit = func(n string) []string {
		state[n] = open
		path = append(path, n)
		for _, m := range next[n] {
			switch state[m] {
			case open:
				start := slices.Index(path, m)
				return append(slices.Clone(path[start:]), m)
			case unseen:
				if c := visit(m); c != nil {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		state[n] = done
		return nil
	}
	for _, n := range slices.Sorted(maps.Keys(next)) {
		if state[n] == unseen {
			if c := visit(n); c != nil {
				return c
			}
		}
	}
	return nil
}

func article(typ string) string {
	if typ == "int" {
		return "an int"
	}
	return "a " + typ
}

// fits reports whether a value decoded from JSON has an input type's shape.
func fits(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "bool":
		_, ok := v.(bool)
		return ok
	case "int":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	}
	return false
}
