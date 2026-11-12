package skills

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/rulecel"
	"github.com/alexbieg/alux/internal/web"
)

// Event is one answered tool call from alux's own log, in order; a dispatch is logged when its run ends, with its pinned skill.
type Event struct {
	Tool   string
	Input  json.RawMessage
	Failed bool
	// Commands is what a call rule's when reads as call.commands: the commands a bash call runs, as hold rules see them.
	Commands []string
	Skill    *SkillCheck
	// Proc is the process a poll call started; an event with Ended set is alux's note that it stopped, with no Tool.
	Proc  int
	Ended PollEnd
}

// PollEnd is how a poll stopped, worded to follow "poll 3".
type PollEnd string

const (
	PollMet      PollEnd = "met its condition"
	PollTimedOut PollEnd = "timed out"
	PollFailed   PollEnd = "failed its check"
	PollKilled   PollEnd = "was killed"
)

// SkillCheck is how a dispatched subagent's pinned skill ended; Finished means finish_skill closed it complete.
type SkillCheck struct {
	Name     string
	Finished bool
	// Waivers is every step or stage the run waived on its way to that end.
	Waivers []Waiver
	Report  string
}

// Env is what the checker reads besides the log: files the agent wrote, and the web cache.
type Env struct {
	Tools     []string
	ReadFile  func(path string) ([]byte, error)
	CachedURL func(path string) (string, bool)
	PageText  func(url string) (string, bool)
}

// LiveEnv reads the real disk and web cache; tools is what this session can call.
func LiveEnv(tools []string) Env {
	return Env{
		Tools:    tools,
		ReadFile: os.ReadFile,
		CachedURL: func(path string) (string, bool) {
			o, ok := web.Cached(path)
			return o.URL, ok && o.URL != ""
		},
		PageText: web.CachedPage,
	}
}

type State string

const (
	Pending    State = "pending"
	Done       State = "done"
	OutOfOrder State = "out of order"
	Skipped    State = "skipped"
	Running    State = "running"
	Waived     State = "waived"
)

// unfinished is a state a required node cannot be left in at finish.
func (s State) unfinished() bool { return s == Pending || s == Running }

type NodeResult struct {
	Name     string
	Optional bool
	State    State
	// At is the index of the event that completed the node, or -1.
	At      int
	Missing []string
	Order   []string
	// Waived is how many waivers were made in a flow stage, Fails how many times it failed, Stopped why it waits for a rerun or waiver.
	Waived, Fails int
	Stopped       string
	// Item of Items is a running foreach's current item, Round of Rounds a running repeat's round; zero otherwise.
	Item, Items, Round, Rounds int
}

type Result struct {
	Nodes []NodeResult
	// Edges is every order pair once, or a flow's stage graph.
	Edges    []Edge
	Complete bool
	// Waivers is every step the model closed without its evidence or its run passing, and why.
	Waivers []Waiver
}

type Waiver struct {
	Step   string
	Reason string
}

// Waive marks each required node in steps that is still unfinished as Waived, leaving a node with evidence as it is.
func Waive(res Result, steps map[string]bool) Result {
	res.Nodes = slices.Clone(res.Nodes)
	res.Complete = true
	for i := range res.Nodes {
		n := &res.Nodes[i]
		if steps[n.Name] && !n.Optional && n.State.unfinished() {
			n.State = Waived
		}
		if !n.Optional && n.State.unfinished() {
			res.Complete = false
		}
	}
	return res
}

// Unfinished reports whether node is a required node of res still without evidence.
func (res Result) Unfinished(node string) bool {
	for _, n := range res.Nodes {
		if n.Name == node {
			return !n.Optional && n.State.unfinished()
		}
	}
	return false
}

// Edge is one arrow between two node names; a dashed one is a path taken only sometimes, a skip or an on_fail.
type Edge struct {
	From, To string
	Label    string
	Dashed   bool
}

const (
	editorTool = "str_replace_based_edit_tool"
	grepTool   = "grep"
	searchTool = "web_search"
	fetchTool  = "web_fetch"
	pollTool   = "poll"
)

// Check matches every node's evidence against alux's log, files on disk and the web cache, never the agent's claims.
func Check(r *Run, log []Event, env Env) Result {
	c := checker{run: r, log: log, env: env, fetched: map[string]fetch{}, window: wholeLog}
	c.indexFetches()
	results := map[string]*NodeResult{}
	polls := map[string]func() *NodeResult{}
	chains := r.Skill.Order
	for _, name := range slices.Sorted(maps.Keys(r.Skill.Nodes)) {
		node := r.Skill.Nodes[name]
		results[name] = c.node(name, node)
		if pollsStopped(node) {
			polls[name] = func() *NodeResult { return c.node(name, node) }
		}
	}
	c.preds = predecessors(chains)
	order := c.orderNodes(chains, slices.Sorted(maps.Keys(results)))
	ceilings := map[string]int{}
	for _, name := range order {
		if redo, ok := polls[name]; ok {
			c.window = c.pollFloor(results, name)
			c.window.earliest = true
			results[name] = redo()
			if !c.window.waiting {
				c.eachBefore(results, name, func(p string, _ *NodeResult) {
					if at, ok := ceilings[p]; !ok || c.window.floor < at {
						ceilings[p] = c.window.floor
					}
				})
			}
		}
	}
	for _, name := range order {
		if redo, ok := polls[name]; ok {
			c.window = c.pollFloor(results, name)
			if at, ok := ceilings[name]; ok {
				c.window.ceiling = at
			}
			results[name] = redo()
		}
		c.window = wholeLog
		c.checkOrder(results, name)
	}
	out := Result{Edges: edges(chains), Complete: true}
	for _, name := range order {
		res := results[name]
		out.Nodes = append(out.Nodes, *res)
		if !res.Optional && res.State.unfinished() {
			out.Complete = false
		}
	}
	return out
}

// Behind is a done node and the nodes before it still pending.
type Behind struct {
	Node    string
	Pending []string
}

// NodesBehind names each node checkOrder finds done while a node before it is still pending, ignoring done times.
func NodesBehind(r *Run, log []Event, env Env) []Behind {
	res := Check(r, log, env)
	results := map[string]*NodeResult{}
	for i := range res.Nodes {
		results[res.Nodes[i].Name] = &res.Nodes[i]
	}
	c := checker{preds: map[string][]string{}}
	for _, e := range res.Edges {
		c.preds[e.To] = append(c.preds[e.To], e.From)
	}
	var out []Behind
	for _, n := range res.Nodes {
		if n.State != OutOfOrder {
			continue
		}
		b := Behind{Node: n.Name}
		c.eachBefore(results, n.Name, func(p string, pr *NodeResult) {
			if pr.State != Done && pr.State != OutOfOrder {
				b.Pending = append(b.Pending, p)
			}
		})
		if len(b.Pending) > 0 {
			out = append(out, b)
		}
	}
	return out
}

type checker struct {
	run     *Run
	log     []Event
	env     Env
	fetched map[string]fetch
	preds   map[string][]string
	window  pollWindow
}

// pollWindow is the span of the log whose polls poll_stopped counts for the node being checked.
type pollWindow struct {
	// floor is the predecessor by's At, or past the log's end while by is waiting.
	floor   int
	by      string
	waiting bool
	// ceiling is where a later poll_stopped node starts counting polls as its own, or -1.
	ceiling int
	// earliest meets the rule at its min-th stopped poll, ignoring later ones.
	earliest bool
}

var wholeLog = pollWindow{floor: -1, ceiling: -1}

func (c *checker) node(name string, node Node) *NodeResult {
	res := &NodeResult{Name: name, Optional: node.Optional, State: Pending, At: -1}
	if !c.applies(node.When) {
		res.State = Skipped
		return res
	}
	times := 1
	if node.Loop != nil {
		times = max(c.run.count(node.Loop.Times), 1)
	}
	at := -1
	for _, rule := range node.Evidence {
		ruleAt, missing := c.rule(rule, times)
		if missing != nil {
			res.Missing = append(res.Missing, missing...)
			continue
		}
		at = max(at, ruleAt)
	}
	if len(res.Missing) == 0 {
		res.State, res.At = Done, at
	}
	return res
}

// applies reports whether a node's condition holds.
func (c *checker) applies(when *Condition) bool {
	switch {
	case when == nil:
		return true
	case when.Available != "":
		return slices.Contains(c.env.Tools, when.Available)
	}
	bound := c.run.Inputs[when.Input]
	if when.Out != "" {
		if c.run.Out == nil {
			return false
		}
		bound = c.run.Out[when.Out]
	}
	if when.Equals != nil {
		var v any
		_ = json.Unmarshal(when.Equals, &v)
		return same(v, bound)
	}
	return slices.ContainsFunc(when.In, func(v any) bool { return same(v, bound) })
}

// rule returns the event index that satisfied it, or why it is not satisfied.
func (c *checker) rule(r Rule, times int) (int, []string) {
	switch {
	case r.Call != nil:
		return c.call(r.Call)
	case r.FetchesOpened != nil:
		return c.fetchesOpened(c.run.count(r.FetchesOpened.Min))
	case r.ArtifactLines != nil:
		return c.artifactLines(r.ArtifactLines)
	case r.CitationsFetched != nil:
		return c.citationsFetched(r.CitationsFetched)
	case r.LeadsFollowed != nil:
		return c.leadsFollowed(r.LeadsFollowed, times)
	case r.SubagentSkill != nil:
		return c.subagentSkill(r.SubagentSkill)
	case r.PollStopped != nil:
		return c.pollStopped(r.PollStopped)
	}
	return -1, []string{"an evidence rule of no known type"}
}

func ruleMin(r Rule) Count {
	switch {
	case r.Call != nil && r.Call.Min != nil:
		return *r.Call.Min
	case r.FetchesOpened != nil:
		return r.FetchesOpened.Min
	case r.ArtifactLines != nil:
		return r.ArtifactLines.Min
	case r.CitationsFetched != nil && r.CitationsFetched.Min != nil:
		return *r.CitationsFetched.Min
	case r.LeadsFollowed != nil:
		return r.LeadsFollowed.Min
	case r.SubagentSkill != nil && r.SubagentSkill.Min != nil:
		return *r.SubagentSkill.Min
	case r.PollStopped != nil && r.PollStopped.Min != nil:
		return *r.PollStopped.Min
	}
	return Count{}
}

// pollStopped counts poll calls whose process alux logged as ending by its condition;
// the latest poll in the window decides, so a later round's poll still running leaves the rule unmet.
func (c *checker) pollStopped(p *PollStopped) (int, []string) {
	want := 1
	if p.Min != nil {
		want = c.run.count(*p.Min)
	}
	ends := map[int]int{}
	for i, e := range c.log {
		if _, seen := ends[e.Proc]; e.Ended != "" && !seen {
			ends[e.Proc] = i
		}
	}
	w := c.window
	early := 0
	var stopped []int
	latest := ""
	for i, e := range c.log {
		if e.Tool != pollTool || e.Failed || e.Proc == 0 || w.ceiling >= 0 && i >= w.ceiling {
			continue
		}
		if i < w.floor {
			early = e.Proc
			continue
		}
		end, ok := ends[e.Proc]
		switch {
		case !ok:
			latest = fmt.Sprintf("poll %d still running", e.Proc)
		case c.log[end].Ended != PollMet:
			latest = fmt.Sprintf("poll %d %s", e.Proc, c.log[end].Ended)
		default:
			latest = ""
			stopped = append(stopped, max(i, end))
		}
	}
	slices.Sort(stopped)
	switch {
	case w.earliest && len(stopped) >= want && want > 0:
		return stopped[want-1], nil
	case latest == "" && len(stopped) >= want && want > 0:
		return stopped[len(stopped)-1], nil
	}
	var short []string
	switch {
	case latest != "":
		short = []string{latest}
	case len(stopped) > 0:
	case w.waiting:
		short = []string{fmt.Sprintf("waiting on %s before a poll counts", w.by)}
	case early != 0:
		short = []string{fmt.Sprintf("poll %d started before %s finished, so it does not count", early, w.by)}
	case want == 1:
		short = []string{"no poll started in this step has stopped"}
	}
	if want > 1 {
		short = append([]string{fmt.Sprintf("%d of %d polls stopped on their own", len(stopped), want)}, short...)
	}
	return -1, short
}

// subagentSkill counts dispatched runs that ended with their pinned skill finished complete.
func (c *checker) subagentSkill(t *SubagentSkill) (int, []string) {
	want := 1
	if t.Min != nil {
		want = c.run.count(*t.Min)
	}
	seen := 0
	for i, e := range c.log {
		if e.Failed || e.Skill == nil || e.Skill.Name != t.Skill || !e.Skill.Finished {
			continue
		}
		if seen++; seen >= want {
			return i, nil
		}
	}
	return -1, []string{fmt.Sprintf("subagent skill %s: %d of %d finished", t.Skill, seen, want)}
}

// call counts successful calls to its tools whose when holds; a call whose when errors does not count.
func (c *checker) call(t *CallRule) (int, []string) {
	want := 1
	if t.Min != nil {
		want = c.run.count(*t.Min)
	}
	firstErr := ""
	seen := map[string]bool{}
	for i, e := range c.log {
		if e.Failed || !slices.Contains(t.Tool, e.Tool) {
			continue
		}
		if t.When != "" {
			holds, err := c.holds(t.When, e)
			if err != nil && firstErr == "" {
				firstErr = err.Error()
			}
			if !holds {
				continue
			}
		}
		key := callKey(t, e, i)
		if key == "" {
			continue
		}
		seen[key] = true
		if len(seen) >= want {
			return i, nil
		}
	}
	missing := fmt.Sprintf("%s: %d of %d %s", t.label(), len(seen), want, counted[t.Distinct])
	if firstErr != "" {
		missing += "; first error: " + firstErr
	}
	return -1, []string{missing}
}

// callKey is what a call rule's distinct counts e as, the call's index i without one; "" never counts.
func callKey(t *CallRule, e Event, i int) string {
	switch t.Distinct {
	case "query":
		return strings.ToLower(strings.Join(strings.Fields(field(e.Input, "query")), " "))
	case "url":
		return normalURL(field(e.Input, "url"))
	case "domain":
		return domain(field(e.Input, "url"))
	}
	return fmt.Sprint(i)
}

// holds evaluates when over one logged call, with the run's inputs and vars as data.
func (c *checker) holds(when string, e Event) (bool, error) {
	ev, err := evidenceFor(when)
	if err != nil {
		return false, err
	}
	args := map[string]any{}
	if len(e.Input) > 0 {
		if err := json.Unmarshal(e.Input, &args); err != nil {
			return false, fmt.Errorf("the call's input is not an object: %w", err)
		}
	}
	return ev.Eval(rulecel.Call{Tool: e.Tool, Args: args, Commands: e.Commands}, c.run.homeInputs(), c.run.Vars, nil)
}

// homeInputs is the bound inputs with a leading ~/ in a string value expanded, as vars already are.
func (r *Run) homeInputs() map[string]any {
	out := make(map[string]any, len(r.Inputs))
	for k, v := range r.Inputs {
		if s, ok := v.(string); ok {
			v = r.expandHome(s)
		}
		out[k] = v
	}
	return out
}

// label is the rule's tools, joined with "or", and its condition.
func (t *CallRule) label() string {
	s := strings.Join(t.Tool, " or ")
	if t.When != "" {
		s += " where " + t.When
	}
	return s
}

var compiled sync.Map

// evidenceFor compiles a call rule's condition once per process.
func evidenceFor(when string) (*rulecel.Evidence, error) {
	type result struct {
		ev  *rulecel.Evidence
		err error
	}
	if r, ok := compiled.Load(when); ok {
		return r.(result).ev, r.(result).err
	}
	ev, err := rulecel.CompileEvidence(when)
	compiled.Store(when, result{ev, err})
	return ev, err
}

var counted = map[string]string{"": "calls", "query": "distinct queries", "url": "distinct urls", "domain": "distinct domains"}

func (c *checker) artifactLines(a *ArtifactLines) (int, []string) {
	path := c.run.Expand(a.Path)
	data, err := c.env.ReadFile(path)
	if err != nil {
		return -1, []string{path + " has not been written"}
	}
	lines := fileLines(data)
	if a.Section != "" {
		head := c.run.Expand(a.Section)
		var ok bool
		if lines, ok = section(lines, head); !ok {
			return -1, []string{fmt.Sprintf("%s has no section %q", path, head)}
		}
	}
	re := regexp.MustCompile(a.Pattern)
	var matched []string
	for _, line := range lines {
		if re.MatchString(line) {
			matched = append(matched, line)
		}
	}
	var missing []string
	if want := c.run.count(a.Min); len(matched) < want {
		missing = append(missing, fmt.Sprintf("%s: %d of %d lines match %s", path, len(matched), want, a.Pattern))
	}
	if a.Max != nil {
		if most := c.run.count(*a.Max); len(matched) > most {
			missing = append(missing, fmt.Sprintf("%s: %d lines match %s, and at most %d may: %s", path, len(matched), a.Pattern, most, strings.Join(matched, " | ")))
		}
	}
	if a.Covers != nil {
		missing = append(missing, c.covers(a.Covers, re, matched)...)
	}
	if len(missing) > 0 {
		return -1, missing
	}
	return c.lastTouch(path), nil
}

// covers names every line of cv.Path whose key no matched line shares.
func (c *checker) covers(cv *Covers, re *regexp.Regexp, matched []string) []string {
	path := c.run.Expand(cv.Path)
	data, err := c.env.ReadFile(path)
	if err != nil {
		return []string{path + " has not been written"}
	}
	have := map[string]bool{}
	for _, line := range matched {
		have[lineKey(re, line)] = true
	}
	want := regexp.MustCompile(cv.Pattern)
	var uncovered []string
	for _, line := range fileLines(data) {
		if key := lineKey(want, line); key != "" && !have[key] {
			uncovered = append(uncovered, line)
		}
	}
	if len(uncovered) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d line(s) of %s have no matching line: %s", len(uncovered), path, strings.Join(uncovered, " | "))}
}

// lineKey is the first non-empty group of re's match in line, or "" when it does not match.
func lineKey(re *regexp.Regexp, line string) string {
	m := re.FindStringSubmatch(line)
	for _, g := range m[min(1, len(m)):] {
		if g != "" {
			return g
		}
	}
	return ""
}

func fileLines(data []byte) []string {
	var out []string
	for line := range strings.Lines(string(data)) {
		out = append(out, strings.TrimRight(line, "\r\n"))
	}
	return out
}

// section is the lines after the first line equal to head, up to the next markdown heading of its level or above.
func section(lines []string, head string) ([]string, bool) {
	head = strings.TrimSpace(head)
	start := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == head })
	if start < 0 {
		return nil, false
	}
	level := headingLevel(head)
	rest := lines[start+1:]
	for i, l := range rest {
		if n := headingLevel(l); n > 0 && (level == 0 || n <= level) {
			return rest[:i], true
		}
	}
	return rest, true
}

func headingLevel(line string) int {
	n := len(line) - len(strings.TrimLeft(line, "#"))
	if n == 0 || n == len(line) || line[n] != ' ' {
		return 0
	}
	return n
}

// lastTouch is the last call whose input names path, or the end of the log when none does.
func (c *checker) lastTouch(path string) int {
	short := path
	if rest, ok := strings.CutPrefix(path, c.run.home+"/"); ok && c.run.home != "" {
		short = "~/" + rest
	}
	for i := len(c.log) - 1; i >= 0; i-- {
		in := string(c.log[i].Input)
		if strings.Contains(in, path) || strings.Contains(in, short) {
			return i
		}
	}
	return len(c.log)
}

// orderNodes lists nodes so every order edge points forward, ties going to the node named first in the file's order.
func (c *checker) orderNodes(chains [][]string, names []string) []string {
	first := map[string]int{}
	for _, chain := range chains {
		for _, n := range chain {
			if _, ok := first[n]; !ok {
				first[n] = len(first)
			}
		}
	}
	rank := func(n string) int {
		if i, ok := first[n]; ok {
			return i
		}
		return len(first) + slices.Index(names, n)
	}
	placed := map[string]bool{}
	var out []string
	for len(out) < len(names) {
		best := ""
		for _, n := range names {
			if placed[n] || slices.ContainsFunc(c.preds[n], func(p string) bool { return !placed[p] }) {
				continue
			}
			if best == "" || rank(n) < rank(best) {
				best = n
			}
		}
		if best == "" {
			break
		}
		placed[best] = true
		out = append(out, best)
	}
	return out
}

func predecessors(chains [][]string) map[string][]string {
	preds := map[string][]string{}
	for _, chain := range chains {
		for j := 1; j < len(chain); j++ {
			if !slices.Contains(preds[chain[j]], chain[j-1]) {
				preds[chain[j]] = append(preds[chain[j]], chain[j-1])
			}
		}
	}
	return preds
}

// checkOrder flags a done node whose predecessors were not done before it.
// A skipped predecessor, or an optional one never done, is looked through to the nodes before it.
func (c *checker) checkOrder(results map[string]*NodeResult, name string) {
	res := results[name]
	if res.State != Done {
		return
	}
	c.eachBefore(results, name, func(p string, pr *NodeResult) {
		switch {
		case pr.State != Done && pr.State != OutOfOrder:
			res.Order = append(res.Order, fmt.Sprintf("done before %s, which is still pending", p))
		case pr.At > res.At:
			res.Order = append(res.Order, fmt.Sprintf("done before %s finished", p))
		}
	})
	if len(res.Order) > 0 {
		res.State = OutOfOrder
	}
}

// pollFloor floors name's polls at the largest At before it, or past the log's end at the first node before it not done.
func (c *checker) pollFloor(results map[string]*NodeResult, name string) pollWindow {
	w := wholeLog
	c.eachBefore(results, name, func(p string, pr *NodeResult) {
		switch {
		case w.waiting:
		case pr.State != Done && pr.State != OutOfOrder:
			w.floor, w.by, w.waiting = len(c.log), p, true
		case pr.At > w.floor:
			w.floor, w.by = pr.At, p
		}
	})
	return w
}

// eachBefore visits the predecessors order compares name against, looking through the nodes checkOrder does.
func (c *checker) eachBefore(results map[string]*NodeResult, name string, visit func(p string, pr *NodeResult)) {
	visited := map[string]bool{}
	var walk func(n string)
	walk = func(n string) {
		for _, p := range c.preds[n] {
			if visited[p] {
				continue
			}
			visited[p] = true
			pr := results[p]
			done := pr.State == Done || pr.State == OutOfOrder
			if pr.State == Skipped || pr.Optional && !done {
				walk(p)
				continue
			}
			visit(p, pr)
		}
	}
	walk(name)
}

func pollsStopped(n Node) bool {
	return slices.ContainsFunc(n.Evidence, func(r Rule) bool { return r.PollStopped != nil })
}

func field(input json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func normalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}

func domain(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}
