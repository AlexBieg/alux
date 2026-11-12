package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

const home = "/home/t"

var started = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const runDir = home + "/" + paths.State + "/skills/deep-research/20261014T120000Z"

func call(tool string, input map[string]any) Event {
	data, _ := json.Marshal(input)
	return Event{Tool: tool, Input: data}
}

// world is an honest deep-research run: the log, the files it wrote and the pages it fetched.
type world struct {
	log   []Event
	files map[string]string
	pages map[string]string
	tools []string
}

func honest() *world {
	w := &world{files: map[string]string{}, pages: map[string]string{}, tools: []string{"vault_search", "vault_note", "web_search", "web_fetch"}}
	write := func(name, body string) {
		w.files[runDir+"/"+name] = body
		w.log = append(w.log, call(editorTool, map[string]any{"command": "create", "path": runDir + "/" + name, "file_text": body}))
	}
	fetchURL := func(u string) { w.log = append(w.log, call(fetchTool, map[string]any{"url": u})) }

	write("scope.md", "Q: one\nQ: two\nQ: three\n")
	w.log = append(w.log, call("vault_search", map[string]any{"query": "alux search"}))
	for _, q := range []string{"first query", "second query", "third query"} {
		w.log = append(w.log, call(searchTool, map[string]any{"query": q}))
	}
	sources := []string{"https://a.com/1", "https://b.org/2", "https://c.net/3", "https://d.io/4"}
	var terms []string
	for i := 1; i <= 6; i++ {
		terms = append(terms, fmt.Sprintf("Lead%d", i))
	}
	w.pages[sources[0]] = "This page mentions " + strings.Join(terms, ", ") + "."
	for _, u := range sources {
		fetchURL(u)
		if w.pages[u] == "" {
			w.pages[u] = "page " + u
		}
	}
	for _, u := range sources {
		w.log = append(w.log, call(editorTool, map[string]any{"command": "view", "path": cachePath(u)}))
	}
	var leads strings.Builder
	for i, term := range terms {
		w.log = append(w.log, call(searchTool, map[string]any{"query": "what is " + strings.ToLower(term)}))
		follow := fmt.Sprintf("https://e%d.com/page", i)
		fetchURL(follow)
		w.pages[follow] = "about " + term
		fmt.Fprintf(&leads, "- %s | %s | it widens the answer\n", term, sources[0])
	}
	write("leads.md", leads.String())
	write("report.md", "The answer [1](https://a.com/1), and Lead1 widens it (https://e0.com/page).\n")
	w.log = append(w.log, call("vault_note", map[string]any{"task": "file it"}))
	return w
}

func cachePath(u string) string {
	return home + "/" + paths.Cache + "/web/" + strings.NewReplacer("https://", "", "/", "_").Replace(u) + "/page.md"
}

func (w *world) env() Env {
	return Env{
		Tools: w.tools,
		ReadFile: func(p string) ([]byte, error) {
			body, ok := w.files[p]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(body), nil
		},
		CachedURL: func(p string) (string, bool) {
			for u := range w.pages {
				if cachePath(u) == p {
					return u, true
				}
			}
			return "", false
		},
		PageText: func(u string) (string, bool) { t, ok := w.pages[u]; return t, ok },
	}
}

func (w *world) drop(keep func(Event) bool) {
	w.log = slices.DeleteFunc(w.log, func(e Event) bool { return !keep(e) })
}

func check(t *testing.T, w *world, inputs map[string]any) Result {
	t.Helper()
	s, err := Load("testdata/deep-research.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if inputs == nil {
		inputs = map[string]any{"question": "how should a harness check research?"}
	}
	r, err := Bind(s, inputs, started, home)
	if err != nil {
		t.Fatal(err)
	}
	return Check(r, w.log, w.env())
}

func node(t *testing.T, res Result, name string) NodeResult {
	t.Helper()
	for _, n := range res.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node %s in %+v", name, res.Nodes)
	return NodeResult{}
}

func mentions(t *testing.T, n NodeResult, want string) {
	t.Helper()
	if !slices.ContainsFunc(append(n.Missing, n.Order...), func(m string) bool { return strings.Contains(m, want) }) {
		t.Errorf("%s says %q / %q, want something mentioning %q", n.Name, n.Missing, n.Order, want)
	}
}

func TestAnHonestRunIsComplete(t *testing.T) {
	res := check(t, honest(), nil)
	var got []string
	for _, n := range res.Nodes {
		got = append(got, n.Name+"="+string(n.State))
	}
	want := "scope=done vault=done glean=skipped sweep=done read=done leads=done synthesis=done file=done"
	if strings.Join(got, " ") != want {
		t.Errorf("states\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if !res.Complete {
		t.Error("an honest run must be complete")
	}
}

func TestFetchedButNeverOpenedPagesDoNotCount(t *testing.T) {
	w := honest()
	w.drop(func(e Event) bool { return field(e.Input, "command") != "view" })
	res := check(t, w, nil)
	read := node(t, res, "read")
	if read.State != Pending {
		t.Errorf("read = %s", read.State)
	}
	mentions(t, read, "0 of 4 fetched pages were opened")
	if res.Complete {
		t.Error("a run that read nothing is not complete")
	}
}

func TestFailedCallsAreNotEvidence(t *testing.T) {
	w := honest()
	for i := range w.log {
		if w.log[i].Tool == searchTool {
			w.log[i].Failed = true
		}
	}
	sweep := node(t, check(t, w, nil), "sweep")
	if sweep.State != Pending {
		t.Errorf("sweep = %s", sweep.State)
	}
	mentions(t, sweep, "web_search: 0 of 3 distinct queries")
}

func TestALeadMustAppearOnItsSourcePage(t *testing.T) {
	w := honest()
	w.pages["https://a.com/1"] = strings.Replace(w.pages["https://a.com/1"], "Lead6", "", 1)
	leads := node(t, check(t, w, nil), "leads")
	mentions(t, leads, "5 of 6 leads followed")
	mentions(t, leads, `lead "Lead6": its source page does not mention it`)
}

func TestALeadTheQuestionAlreadyAsksIsNotAdjacent(t *testing.T) {
	leads := node(t, check(t, honest(), map[string]any{"question": "what is lead3?"}), "leads")
	mentions(t, leads, `lead "Lead3": the question already asks about it`)
}

func TestALeadMustBeSearchedAfterItsSourceWasRead(t *testing.T) {
	w := honest()
	w.drop(func(e Event) bool { return field(e.Input, "query") != "what is lead2" })
	mentions(t, node(t, check(t, w, nil), "leads"), `lead "Lead2": no search after reading its source names it`)
}

func TestALeadMustReachANewDomain(t *testing.T) {
	w := honest()
	w.drop(func(e Event) bool { return field(e.Input, "url") != "https://e4.com/page" })
	mentions(t, node(t, check(t, w, nil), "leads"), `lead "Lead5": no fetch after its search reached a new domain that mentions it`)
}

func TestALeadMatchesDespitePunctuationSpacingAndPossessives(t *testing.T) {
	w := honest()
	w.pages["https://a.com/1"] = "It cites Fitts' law and a ghost-cursor overlay, plus Lead3, Lead4, Lead5, Lead6."
	w.pages["https://fitts.edu/law"] = "about Fitts’s Law"
	w.pages["https://npm.dev/ghost"] = "about ghostcursor"
	w.log = append(w.log,
		call(searchTool, map[string]any{"query": "fitts law cursor timing"}),
		call(fetchTool, map[string]any{"url": "https://fitts.edu/law"}),
		call(searchTool, map[string]any{"query": "ghost cursor npm"}),
		call(fetchTool, map[string]any{"url": "https://npm.dev/ghost"}))
	w.files[runDir+"/leads.md"] = strings.NewReplacer("- Lead1 |", "- Fitts's Law |", "- Lead2 |", "- Ghost Cursor |").Replace(w.files[runDir+"/leads.md"])
	if leads := node(t, check(t, w, nil), "leads"); leads.State != Done {
		t.Errorf("leads = %s %q", leads.State, leads.Missing)
	}
}

func TestADeadEndCountsWithoutANewDomain(t *testing.T) {
	w := honest()
	w.drop(func(e Event) bool { return field(e.Input, "url") != "https://e4.com/page" })
	w.files[runDir+"/leads.md"] = strings.Replace(w.files[runDir+"/leads.md"], "- Lead5 | https://a.com/1 | it widens the answer",
		"- Lead5 | https://a.com/1 | it widens the answer | dead: no page beyond the source", 1)
	if leads := node(t, check(t, w, nil), "leads"); leads.State != Done {
		t.Errorf("leads = %s %q", leads.State, leads.Missing)
	}
}

func TestADeadEndStillNeedsASearch(t *testing.T) {
	w := honest()
	w.drop(func(e Event) bool { return field(e.Input, "query") != "what is lead5" })
	w.files[runDir+"/leads.md"] = strings.Replace(w.files[runDir+"/leads.md"], "- Lead5 | https://a.com/1 | it widens the answer",
		"- Lead5 | https://a.com/1 | it widens the answer | dead: nothing", 1)
	mentions(t, node(t, check(t, w, nil), "leads"), `lead "Lead5": no search after reading its source names it`)
}

func TestOnlyOneDeadEndPerHopCounts(t *testing.T) {
	w := honest()
	w.files[runDir+"/leads.md"] = strings.ReplaceAll(w.files[runDir+"/leads.md"], "widens the answer\n", "widens the answer | dead: nothing\n")
	leads := node(t, check(t, w, nil), "leads")
	mentions(t, leads, "2 of 6 leads followed")
	mentions(t, leads, `lead "Lead3": only 2 dead end(s) count, one per hop`)
}

func TestFewerHopsNeedFewerLeads(t *testing.T) {
	w := honest()
	w.pages["https://a.com/1"] = "This page mentions Lead1, Lead2, Lead3."
	inputs := map[string]any{"question": "q", "hops": 1}
	if leads := node(t, check(t, w, inputs), "leads"); leads.State != Done {
		t.Errorf("one hop of three leads should pass, got %s %q", leads.State, leads.Missing)
	}
}

func TestAReportCitingAnUnfetchedURLFails(t *testing.T) {
	w := honest()
	w.files[runDir+"/report.md"] += "Also see https://never.example/x.\n"
	synthesis := node(t, check(t, w, nil), "synthesis")
	if synthesis.State != Pending {
		t.Errorf("synthesis = %s", synthesis.State)
	}
	mentions(t, synthesis, "1 URLs never fetched: https://never.example/x")
}

func TestEvidenceAheadOfItsPredecessorIsOutOfOrder(t *testing.T) {
	w := honest()
	scope := w.log[0]
	w.log = append(w.log[1:], scope)
	res := check(t, w, nil)
	vault := node(t, res, "vault")
	if vault.State != OutOfOrder {
		t.Errorf("vault = %s", vault.State)
	}
	mentions(t, vault, "done before scope finished")
	if !res.Complete {
		t.Error("out of order is reported, not a failure")
	}
}

func TestOrderLooksThroughASkippedNode(t *testing.T) {
	sweep := node(t, check(t, honest(), nil), "sweep")
	if sweep.State != Done || len(sweep.Order) > 0 {
		t.Errorf("sweep after a skipped glean = %s %q", sweep.State, sweep.Order)
	}
}

func TestAnAvailableOptionalToolLeftUnusedIsPendingNotFailing(t *testing.T) {
	w := honest()
	w.tools = append(w.tools, "mcp__glean__search")
	res := check(t, w, nil)
	if glean := node(t, res, "glean"); glean.State != Pending {
		t.Errorf("glean = %s", glean.State)
	}
	if !res.Complete {
		t.Error("an optional node does not hold up completion")
	}
}

func TestASessionWithoutTheVaultFilerStillRunsToCompletion(t *testing.T) {
	w := honest()
	w.tools = slices.DeleteFunc(w.tools, func(name string) bool { return name == "vault_note" })
	w.drop(func(e Event) bool { return e.Tool != "vault_note" })
	s, err := Load("testdata/deep-research.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p := Unavailable(s, w.tools, func(string) bool { return false }); len(p) > 0 {
		t.Fatalf("refused to start: %q", p)
	}
	res := check(t, w, nil)
	if file := node(t, res, "file"); file.State != Skipped {
		t.Errorf("file = %s", file.State)
	}
	if !res.Complete {
		t.Error("a run without vault_note must still complete")
	}
}

func TestBindFillsDefaultsAndExpandsVars(t *testing.T) {
	s, err := Load("testdata/deep-research.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Bind(s, map[string]any{"question": "q", "hops": float64(3)}, started, home)
	if err != nil {
		t.Fatal(err)
	}
	if r.Inputs["hops"] != 3 || r.Inputs["leads_per_hop"] != 3 {
		t.Errorf("inputs = %v", r.Inputs)
	}
	if r.Vars["RUN"] != runDir {
		t.Errorf("RUN = %s", r.Vars["RUN"])
	}
	if got := r.Expand("${RUN}/scope.md for ${question}"); got != runDir+"/scope.md for q" {
		t.Errorf("Expand = %s", got)
	}
}

func TestBindRefusesBadInputs(t *testing.T) {
	s, err := Load("testdata/deep-research.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		given map[string]any
		want  string
	}{
		{map[string]any{}, "input question is required"},
		{map[string]any{"question": "q", "hops": "two"}, "input hops: two is not an int"},
		{map[string]any{"question": "q", "hops": 1.5}, "input hops: 1.5 is not an int"},
		{map[string]any{"question": "q", "depth": 2}, "depth is not an input"},
	} {
		if _, err := Bind(s, c.given, started, home); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Bind(%v) = %v, want %q", c.given, err, c.want)
		}
	}
}

func TestInputNarrowsWhichCallsCount(t *testing.T) {
	s, err := Parse("", []byte(`schema: 2
skill: narrow
version: 1
when: testing input matching
requires: { tools: [vault_record_read] }
nodes:
  scan:
    say: Read the conflicts twice.
    evidence:
      - call: { tool: vault_record_read, when: 'call.args.op == "conflicts"', min: 2 }
  report:
    say: Say what changed.
    optional: true
    evidence:
      - call: { tool: vault_record_read }
order: [[scan, report]]
`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Bind(s, nil, started, home)
	if err != nil {
		t.Fatal(err)
	}
	log := []Event{
		call("vault_record_read", map[string]any{"op": "conflicts"}),
		call("vault_record_read", map[string]any{"op": "search", "query": "conflicts"}),
	}
	scan := node(t, Check(r, log, Env{}), "scan")
	if scan.State != Pending {
		t.Fatalf("a search call counted as a conflicts read: %+v", scan)
	}
	mentions(t, scan, `vault_record_read where call.args.op == "conflicts": 1 of 2 calls`)
	log = append(log, call("vault_record_read", map[string]any{"op": "conflicts"}))
	if scan := node(t, Check(r, log, Env{}), "scan"); scan.State != Done || scan.At != 2 {
		t.Errorf("two conflicts reads should complete the node at the second: %+v", scan)
	}
}

func callRun(t *testing.T, rule string, inputs map[string]any) *Run {
	t.Helper()
	s, err := Parse("", []byte(`schema: 2
skill: calls
version: 1
when: testing call rules
requires: { tools: [bash, browser, pr_create, pr_edit] }
inputs:
  app_url: { type: string, default: "http://localhost:3000" }
nodes:
  n:
    say: Do it.
    evidence:
      - `+rule+`
  after:
    say: Then.
    optional: true
    evidence:
      - call: { tool: bash }
order: [[n, after]]
`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Bind(s, inputs, started, home)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestACallConditionCountsOnlyTheCallsItHoldsFor(t *testing.T) {
	r := callRun(t, `call: { tool: bash, when: 'call.commands.exists(c, cmd(c, "jj diff"))', min: 2 }`, nil)
	diff := call("bash", map[string]any{"command": "jj diff -r x"})
	diff.Commands = []string{"jj diff -r x"}
	logged := call("bash", map[string]any{"command": "jj log"})
	logged.Commands = []string{"jj log"}
	n := node(t, Check(r, []Event{diff, logged}, Env{}), "n")
	if n.State != Pending {
		t.Fatalf("a jj log counted toward a jj diff rule: %+v", n)
	}
	mentions(t, n, `bash where call.commands.exists(c, cmd(c, "jj diff")): 1 of 2 calls`)
	if n := node(t, Check(r, []Event{diff, logged, diff}, Env{}), "n"); n.State != Done || n.At != 2 {
		t.Errorf("two jj diff calls should complete the node at the second: %+v", n)
	}
}

func TestACallRuleCountsAnyToolItLists(t *testing.T) {
	r := callRun(t, `call: { tool: [pr_create, pr_edit] }`, nil)
	if n := node(t, Check(r, []Event{call("pr_edit", map[string]any{"body": "b"})}, Env{}), "n"); n.State != Done {
		t.Errorf("a pr_edit call should count toward a [pr_create, pr_edit] rule: %+v", n)
	}
}

func TestAnInputIsReadAsDataNeverAsCondition(t *testing.T) {
	r := callRun(t, `call: { tool: browser, when: 'call.args.action == "open" && call.args.url.startsWith(inputs.app_url)' }`,
		map[string]any{"app_url": `") || true || ("`})
	if n := node(t, Check(r, []Event{call("browser", map[string]any{"action": "open", "url": "http://elsewhere/"})}, Env{}), "n"); n.State != Pending {
		t.Errorf("an input holding CEL text changed what counts: %+v", n)
	}
}

func TestAnInputUnderHomeMatchesTheExpandedPath(t *testing.T) {
	r := callRun(t, `call: { tool: browser, when: 'call.args.path == inputs.app_url' }`, map[string]any{"app_url": "~/spec.md"})
	if n := node(t, Check(r, []Event{call("browser", map[string]any{"path": home + "/spec.md"})}, Env{}), "n"); n.State != Done {
		t.Errorf("a ~/ input did not match its expanded path: %+v", n)
	}
}

func TestACallWhoseConditionErrorsDoesNotCountAndSaysWhy(t *testing.T) {
	r := callRun(t, `call: { tool: browser, when: 'call.args.url.startsWith(inputs.app_url)' }`, nil)
	n := node(t, Check(r, []Event{call("browser", map[string]any{"action": "snapshot"})}, Env{}), "n")
	if n.State != Pending {
		t.Fatalf("a call whose condition errored counted: %+v", n)
	}
	mentions(t, n, "; first error: ")
}

const watchSkill = `schema: 2
skill: watch
version: 1
when: testing poll evidence
requires: { tools: [bash, poll] }
nodes:
  push:
    say: Push the change.
    evidence:
      - call: { tool: bash }
  watch:
    say: Wait for CI to finish.
    evidence:
      - poll_stopped: {}
order: [[push, watch]]
`

func TestPollStoppedCountsOnlyAPollThatEndedOnItsOwn(t *testing.T) {
	s, err := Parse("watch", []byte(watchSkill))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Bind(s, nil, started, home)
	if err != nil {
		t.Fatal(err)
	}
	poll := call(pollTool, map[string]any{"command": "gh pr checks", "while": "pending"})
	poll.Proc = 3
	ended := func(how PollEnd) Event { return Event{Proc: 3, Ended: how} }
	push := call("bash", map[string]any{"command": "git push"})
	for _, c := range []struct {
		name string
		log  []Event
		want State
		says string
	}{
		{"none started", []Event{push}, Pending, "no poll started in this step has stopped"},
		{"still running", []Event{push, poll}, Pending, "poll 3 still running"},
		{"killed", []Event{push, poll, ended(PollKilled)}, Pending, "poll 3 was killed"},
		{"check failing", []Event{push, poll, ended(PollFailed)}, Pending, "poll 3 failed its check"},
		{"another proc ended", []Event{push, poll, {Proc: 4, Ended: PollMet}}, Pending, "poll 3 still running"},
		{"condition met", []Event{push, poll, ended(PollMet)}, Done, ""},
		{"timed out", []Event{push, poll, ended(PollTimedOut)}, Pending, "poll 3 timed out"},
		{"ended before its call was logged", []Event{push, ended(PollMet), poll}, Done, ""},
		{"started before push", []Event{poll, ended(PollMet), push}, Pending, "poll 3 started before push finished, so it does not count"},
		{"push not done", []Event{poll, ended(PollMet)}, Pending, "waiting on push before a poll counts"},
	} {
		t.Run(c.name, func(t *testing.T) {
			watch := node(t, Check(r, c.log, Env{}), "watch")
			if watch.State != c.want {
				t.Fatalf("watch is %s, want %s: %+v", watch.State, c.want, watch)
			}
			if c.says != "" {
				mentions(t, watch, c.says)
			}
		})
	}
}

func TestPollStoppedWaitsOnTheLatestRoundsPoll(t *testing.T) {
	s, err := Parse("watch", []byte(watchSkill))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Bind(s, nil, started, home)
	if err != nil {
		t.Fatal(err)
	}
	poll := func(proc int) Event {
		e := call(pollTool, map[string]any{"command": "gh pr checks", "while": "pending"})
		e.Proc = proc
		return e
	}
	log := []Event{call("bash", map[string]any{"command": "git push"}), poll(1), {Proc: 1, Ended: PollMet}, poll(2)}
	watch := node(t, Check(r, log, Env{}), "watch")
	if watch.State != Pending {
		t.Fatalf("watch is %s while the second round's poll runs, want pending: %+v", watch.State, watch)
	}
	mentions(t, watch, "poll 2 still running")
	log = append(log, Event{Proc: 2, Ended: PollMet})
	if watch := node(t, Check(r, log, Env{}), "watch"); watch.State != Done || watch.At != 4 {
		t.Errorf("watch should be done at the second poll's end: %+v", watch)
	}
}

func TestPollStoppedCountsOnlyPollsStartedOnceItsPredecessorsAreDone(t *testing.T) {
	bind := func(t *testing.T, aEvidence string) *Run {
		t.Helper()
		s, err := Parse("chain", []byte(`schema: 2
skill: chain
version: 1
when: testing poll evidence across nodes
requires: { tools: [poll] }
nodes:
  a:
    say: Start or wait on the first poll.
    evidence:
      - `+aEvidence+`
  b:
    say: Wait for the checks.
    evidence:
      - poll_stopped: {}
order: [[a, b]]
`))
		if err != nil {
			t.Fatal(err)
		}
		r, err := Bind(s, nil, started, home)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	poll := func(proc int) Event {
		e := call(pollTool, map[string]any{"command": "check", "while": "pending"})
		e.Proc = proc
		return e
	}

	t.Run("a predecessor's own finished poll", func(t *testing.T) {
		r := bind(t, "poll_stopped: {}")
		log := []Event{poll(1), {Proc: 1, Ended: PollMet}, poll(2)}
		res := Check(r, log, Env{})
		if a := node(t, res, "a"); a.State != Done {
			t.Fatalf("a is %s, want done: %+v", a.State, a)
		}
		b := node(t, res, "b")
		if b.State != Pending {
			t.Fatalf("b is %s while its own poll runs, want pending: %+v", b.State, b)
		}
		mentions(t, b, "poll 2 still running")
		log = append(log, Event{Proc: 2, Ended: PollMet})
		if b := node(t, Check(r, log, Env{}), "b"); b.State != Done || b.At != 3 {
			t.Errorf("b should be done once its own poll ends: %+v", b)
		}
	})

	t.Run("a fix round in the later node", func(t *testing.T) {
		r := bind(t, "poll_stopped: {}")
		log := []Event{poll(1), {Proc: 1, Ended: PollMet}, poll(2), {Proc: 2, Ended: PollMet}, poll(3)}
		res := Check(r, log, Env{})
		if a := node(t, res, "a"); a.State != Done || a.At != 1 {
			t.Errorf("a should stay done at its own poll's end: %+v", a)
		}
		b := node(t, res, "b")
		if b.State != Pending {
			t.Fatalf("b is %s while its second round's poll runs, want pending: %+v", b.State, b)
		}
		mentions(t, b, "poll 3 still running")
	})

	t.Run("the poll call that completed the predecessor", func(t *testing.T) {
		r := bind(t, "call: { tool: poll }")
		log := []Event{poll(1), {Proc: 1, Ended: PollMet}}
		if b := node(t, Check(r, log, Env{}), "b"); b.State != Done || b.At != 1 {
			t.Errorf("b should count the poll a's call started: %+v", b)
		}
	})
}

func TestPollStoppedNeedsPollRequired(t *testing.T) {
	_, err := Parse("watch", []byte(strings.Replace(watchSkill, "[bash, poll]", "[bash]", 1)))
	if err == nil || !strings.Contains(err.Error(), "nodes.watch.evidence[0]: tool poll is not listed under requires") {
		t.Errorf("err = %v, want poll_stopped to need poll under requires", err)
	}
}
