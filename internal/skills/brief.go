package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Unavailable says why each required tool cannot be used: not offered at all, or offered but closed.
func Unavailable(s *Skill, offered []string, closed func(string) bool) Problems {
	var p Problems
	for _, t := range s.Requires.Tools {
		switch {
		case !slices.Contains(offered, t):
			p = append(p, fmt.Sprintf("it needs %s, which this session does not offer", t))
		case closed(t):
			p = append(p, fmt.Sprintf("it needs %s, which is closed for the rest of this session because it read a sensitive note", t))
		}
	}
	return p
}

// BindText binds a run from the text typed after the skill's name, which fills its one required string input.
func BindText(s *Skill, text string, now time.Time, home string) (*Run, error) {
	given := map[string]any{}
	text = strings.TrimSpace(text)
	var free []string
	for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
		if in := s.Inputs[name]; in.Type == "string" && in.Default == nil {
			free = append(free, name)
		}
	}
	switch {
	case text == "":
	case len(free) == 1:
		given[free[0]] = text
	case len(free) == 0:
		return nil, Problems{s.Name + " takes no text after its name"}
	default:
		return nil, Problems{fmt.Sprintf("%s has %d required text inputs (%s), so the text cannot say which it fills",
			s.Name, len(free), strings.Join(free, ", "))}
	}
	return Bind(s, given, now, home)
}

// Brief is the message that starts a run: every step at once, with what will be checked, so the agent can plan.
func Brief(r *Run, tools []string) string {
	s := r.Skill
	var b strings.Builder
	fmt.Fprintf(&b, "Run the skill **%s** (v%d). Use it when %s.\n\n", s.Name, s.Version, s.When)
	r.briefSteps(&b, tools)
	b.WriteString("\nThe run stays open across turns, so you may stop to ask the user something. When every required step " +
		"is done, call finish_skill; it is refused while one has no evidence. If a required step has no evidence, do the missing " +
		"work, waive the step in waive with a reason, or call fail_run if it cannot honestly be done.")
	return b.String()
}

// StageBrief is a flow's checklist stage worded as Brief words a skill, under head and closing with tail.
func StageBrief(r *Run, tools []string, head, tail string) string {
	var b strings.Builder
	b.WriteString(head + "\n\n")
	r.briefSteps(&b, tools)
	b.WriteString("\n" + tail)
	return b.String()
}

func (r *Run) briefSteps(b *strings.Builder, tools []string) {
	s := r.Skill
	if len(r.Inputs) > 0 {
		b.WriteString("Inputs:\n")
		for _, name := range slices.Sorted(maps.Keys(r.Inputs)) {
			fmt.Fprintf(b, "- %s = %v\n", name, r.Inputs[name])
		}
		b.WriteString("\n")
	}
	if dir, ok := r.Vars["RUN"]; ok {
		fmt.Fprintf(b, "Write this run's files under %s with the editor tool, not through bash.\n\n", dir)
	}
	b.WriteString("alux checks each step against its own record of your tool calls, the files on disk " +
		"and the web cache. What you say about a step is not evidence. Do the steps in order; " +
		"plan ahead across them as you see fit.\n\n")
	res := Check(r, nil, Env{Tools: tools, ReadFile: noFile, CachedURL: noURL, PageText: noURL})
	step := 0
	for _, n := range res.Nodes {
		node, ok := s.Nodes[n.Name]
		if !ok {
			continue
		}
		step++
		tag := ""
		switch {
		case node.When != nil && node.When.Out != "":
			tag = " (checked at finish, " + node.When.Describe() + ")"
		case n.State == Skipped:
			fmt.Fprintf(b, "%d. **%s**: skipped, its condition does not hold in this session.\n", step, n.Name)
			continue
		case node.Optional:
			tag = " (optional)"
		}
		fmt.Fprintf(b, "%d. **%s**%s: %s\n", step, n.Name, tag, strings.TrimSpace(r.Expand(node.Say)))
		r.briefChecks(b, "   ", node)
	}
}

// stepCall is a run stage's call as the brief shows it: a bash command as written, any other tool with its input.
func (r *Run) stepCall(st *Step) string {
	tool := st.Tool
	in, err := r.fillStep(st)
	if err != nil {
		in, _ = json.Marshal(st.Input)
	}
	var fields map[string]any
	_ = json.Unmarshal(in, &fields)
	if cmd, ok := fields["command"].(string); ok && tool == "bash" {
		return fmt.Sprintf("bash `%s`", cmd)
	}
	return fmt.Sprintf("%s with input %s", tool, in)
}

func (r *Run) briefChecks(b *strings.Builder, indent string, node Node) {
	times := 1
	if node.Loop != nil {
		times = max(r.count(node.Loop.Times), 1)
		fmt.Fprintf(b, "%sRepeat this step %d times.\n", indent, times)
	}
	for _, rule := range node.Evidence {
		fmt.Fprintf(b, "%sChecked: %s\n", indent, r.describe(rule, times))
	}
}

func (r *Run) describe(rule Rule, times int) string {
	switch {
	case rule.Call != nil:
		t := rule.Call
		n := 1
		if t.Min != nil {
			n = r.count(*t.Min)
		}
		where := ""
		if t.When != "" {
			where = " where " + t.When
		}
		tools := strings.Join(t.Tool, " or ")
		if t.Distinct != "" {
			return fmt.Sprintf("at least %d successful %s calls with %s%s", n, tools, counted[t.Distinct], where)
		}
		return fmt.Sprintf("at least %d successful %s call(s)%s", n, tools, where)
	case rule.FetchesOpened != nil:
		return fmt.Sprintf("at least %d fetched pages later opened with view or grep on their cache path", r.count(rule.FetchesOpened.Min))
	case rule.ArtifactLines != nil:
		a := rule.ArtifactLines
		where := r.Expand(a.Path)
		if a.Section != "" {
			where += fmt.Sprintf(", under the heading %q,", r.Expand(a.Section))
		}
		s := fmt.Sprintf("%s has at least %d lines matching %s", where, r.count(a.Min), a.Pattern)
		if a.Max != nil {
			s += fmt.Sprintf(" and at most %d", r.count(*a.Max))
		}
		if cv := a.Covers; cv != nil {
			s += fmt.Sprintf("; every line of %s matching %s has one of them with the same key, its first non-empty group", r.Expand(cv.Path), cv.Pattern)
		}
		return s
	case rule.CitationsFetched != nil:
		cf := rule.CitationsFetched
		n := 1
		if cf.Min != nil {
			n = r.count(*cf.Min)
		}
		return fmt.Sprintf("%s cites at least %d URL(s), and every URL it cites was fetched", r.Expand(cf.Path), n)
	case rule.LeadsFollowed != nil:
		l := rule.LeadsFollowed
		fresh := ""
		if l.NotInInput != "" {
			fresh = " not already in the question,"
		}
		return fmt.Sprintf("at least %d leads in %s, each on a page you fetched,%s "+
			"then searched for by name, and followed to a page on a new domain that mentions it; "+
			"a lead that search turns up nothing for may carry a fourth column starting \"dead: <what you searched>\", at most %d of them",
			r.count(l.Min)*times, r.Expand(l.Path), fresh, times)
	case rule.SubagentSkill != nil:
		n := 1
		if rule.SubagentSkill.Min != nil {
			n = r.count(*rule.SubagentSkill.Min)
		}
		return fmt.Sprintf("at least %d dispatched subagent(s) whose own %s skill run ended with finish_skill", n, rule.SubagentSkill.Skill)
	case rule.PollStopped != nil:
		n := 1
		if rule.PollStopped.Min != nil {
			n = r.count(*rule.PollStopped.Min)
		}
		return fmt.Sprintf("at least %d poll(s) you started in the background after the steps before this one were done, "+
			"that stopped on their own, by their condition or their time limit; the latest such poll must have stopped, "+
			"so one still running or killed leaves this unmet", n)
	}
	return "an evidence rule of no known type"
}

// Unmet names every required node still without evidence, and what each lacks.
func Unmet(res Result) string {
	var b strings.Builder
	b.WriteString("these required steps have no evidence yet:")
	for _, n := range res.Nodes {
		if n.Optional || !n.State.unfinished() {
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s", n.Name, strings.Join(n.Missing, "; "))
	}
	return b.String()
}

// Summary is the end-of-run report: how the run ended, then each node's state and why.
func Summary(name, how string, res Result) string {
	var b strings.Builder
	verdict := "complete"
	if !res.Complete {
		verdict = "not complete"
	}
	if k := len(res.Waivers); k > 0 {
		noun := "waivers"
		if k == 1 {
			noun = "waiver"
		}
		how = fmt.Sprintf("%s with %d %s", how, k, noun)
	}
	fmt.Fprintf(&b, "Skill **%s** %s, %s.\n\n", name, how, verdict)
	for _, w := range res.Waivers {
		fmt.Fprintf(&b, "- waived %s: %s\n", w.Step, w.Reason)
	}
	if len(res.Waivers) > 0 {
		b.WriteString("\n")
	}
	for _, n := range res.Nodes {
		tag := ""
		if n.Optional {
			tag = " (optional)"
		}
		fmt.Fprintf(&b, "- **%s**%s: %s\n", n.Name, tag, n.State)
		for _, why := range append(n.Missing, n.Order...) {
			fmt.Fprintf(&b, "  - %s\n", why)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Diagram is the run's graph as a transcript mermaid fence, each box naming its node and final state.
func Diagram(res Result) string {
	var b strings.Builder
	b.WriteString("```mermaid\nflowchart LR\n")
	ids := map[string]string{}
	for i, n := range res.Nodes {
		ids[n.Name] = fmt.Sprintf("n%d", i)
		fmt.Fprintf(&b, "  %s[\"%s<br/>%s\"]\n", ids[n.Name], outlineEscapes.Replace(n.Name), outlineEscapes.Replace(string(n.State)))
	}
	writeEdges(&b, ids, res.Edges)
	b.WriteString("```")
	return b.String()
}

func writeEdges(b *strings.Builder, ids map[string]string, list []Edge) {
	for _, e := range list {
		from, okF := ids[e.From]
		to, okT := ids[e.To]
		if !okF || !okT {
			continue
		}
		label := outlineEscapes.Replace(e.Label)
		switch {
		case e.Dashed && label != "":
			fmt.Fprintf(b, "  %s -. %s .-> %s\n", from, label, to)
		case e.Dashed:
			fmt.Fprintf(b, "  %s -.-> %s\n", from, to)
		case label != "":
			fmt.Fprintf(b, "  %s -->|%s| %s\n", from, label, to)
		default:
			fmt.Fprintf(b, "  %s --> %s\n", from, to)
		}
	}
}

var outlineEscapes = strings.NewReplacer("#", "#35;", `"`, "#quot;", "<", "#lt;", ">", "#gt;", "|", "#124;", "\n", " ")

// Outline is a skill's graph before any run, as a transcript mermaid fence: each box says who does the node.
func Outline(s *Skill) string {
	if s.Flow() {
		return stageOutline(s)
	}
	var b strings.Builder
	b.WriteString("```mermaid\nflowchart LR\n")
	ids := map[string]string{}
	for i, name := range slices.Sorted(maps.Keys(s.Nodes)) {
		ids[name] = fmt.Sprintf("n%d", i)
		node := s.Nodes[name]
		who := "model"
		if node.Optional {
			who += " (optional)"
		}
		if node.Loop != nil {
			who += ", repeated " + node.Loop.Times.String() + " times"
		}
		if node.When != nil {
			who += " (" + node.When.Describe() + ")"
		}
		fmt.Fprintf(&b, "  %s[\"%s<br/>%s\"]\n", ids[name], outlineEscapes.Replace(name), outlineEscapes.Replace(who))
	}
	writeEdges(&b, ids, edges(s.Order))
	b.WriteString("```")
	return b.String()
}

// edges is every order pair once, in the order the chains name them.
func edges(chains [][]string) []Edge {
	var out []Edge
	for _, chain := range chains {
		for i := 1; i < len(chain); i++ {
			e := Edge{From: chain[i-1], To: chain[i]}
			if !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	return out
}

func noFile(string) ([]byte, error) { return nil, errNoFile }
func noURL(string) (string, bool)   { return "", false }

var errNoFile = errors.New("no file")
