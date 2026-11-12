package compact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/alexbieg/alux/internal/mine"
)

var (
	exitRe        = regexp.MustCompile(`\[exit (\d+)`)
	identRe       = regexp.MustCompile(`\b[a-z][a-z0-9]*[A-Z][A-Za-z0-9]{2,}\b|\b[A-Z][a-z0-9]+[A-Z][A-Za-z0-9]{2,}\b|\b[A-Za-z]+_[A-Za-z0-9_]{3,}\b`)
	itemRe        = regexp.MustCompile(`(?m)^\s*([-*•]|\d+[.)])\s+\S`)
	tokenRe       = regexp.MustCompile(`rec_[0-9a-f]{16}|[\w.~-]*/[\w./-]*\.[a-z]{1,5}\b`)
	refusalLineRe = regexp.MustCompile(`(?i)\b(refused|is blocked|not allowed)\b`)
	fetchedRe     = regexp.MustCompile(`(?m)^(https?://\S+) → \S*/(web/[0-9a-f]+/page\.md)`)
	cachedPageRe  = regexp.MustCompile(`/\.cache/alux/web/[0-9a-f]+/page\.md$`)
)

// liveLine is one explore or read call (or same-target calls merged) whose output a later step used.
type liveLine struct {
	at         []string
	tool, rest string
}

// writeRuns writes live lines, folding consecutive lines of one tool under a line naming only the tool.
func writeRuns(b *strings.Builder, lines []liveLine) {
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j].tool == lines[i].tool {
			j++
		}
		if j-i == 1 {
			fmt.Fprintf(b, "    @%s %s %s\n", strings.Join(lines[i].at, ","), lines[i].tool, lines[i].rest)
			i = j
			continue
		}
		fmt.Fprintf(b, "    %s\n", lines[i].tool)
		for _, l := range lines[i:j] {
			fmt.Fprintf(b, "        - @%s %s\n", strings.Join(l.at, ","), l.rest)
		}
		i = j
	}
}

// turnHead is a turn's heading; the item count and status appear only when they say something.
func turnHead(at, items int, status string) string {
	h := fmt.Sprintf("\n## Turn @%d", at)
	if items > 0 {
		h += fmt.Sprintf(" (%d listed items)", items)
	}
	if status != "" {
		h += " — status: " + status
	}
	return h + "\n"
}

func distinctTargets(ls []lstep) int {
	seen := map[string]bool{}
	for _, l := range ls {
		if l.target != "" {
			seen[l.target] = true
		}
	}
	return len(seen)
}

// editResult writes an editor call kept whole as its command, path and output; the output's diff already
// shows what the input's old and new strings would.
func editResult(input json.RawMessage, output string) string {
	return fmt.Sprintf("TOOL %s %s result:\n%s", inputField(input, "command"), inputField(input, "path"), strings.TrimSpace(output))
}

// pageTargets names a cached web page by the URL it was fetched from, where the history fetched it.
func pageTargets(ev []Event, ls []lstep) {
	url := map[string]string{}
	for _, e := range ev {
		if e.Name == "web_fetch" {
			for _, m := range fetchedRe.FindAllStringSubmatch(e.Output, -1) {
				url[m[2]] = m[1]
			}
		}
	}
	for i := range ls {
		if m := cachedPageRe.FindString(ls[i].target); m != "" {
			if u := url[strings.TrimPrefix(m, "/.cache/alux/")]; u != "" {
				ls[i].target = u
			}
		}
	}
}

// tailBudget is how many characters of the newest tool results stay whole: facts nothing has used yet live there.
const tailBudget = 8000

var classOf = map[string]string{
	"grep": "explore", "glob": "explore", "vault_search": "explore", "vault_read": "explore",
	"vault_record_read": "explore", "web_search": "explore", "web_fetch": "explore", "read_rule": "explore",
	"pr_detail": "explore", "pr_diff": "explore", "pr_threads": "explore", "agent_status": "explore",
	"mcp__linear__list_issues": "explore", "mcp__linear__get_issue": "explore", "view": "read",
	"vault_record": "record", "vault_migration": "record",
	"task": "delegate", "land_run": "delegate", "agent_reply": "delegate", "kill_shell": "delegate",
	"browser": "browser", "poll": "wait", "run_background": "wait",
	"pr_edit": "github", "pr_create": "github", "pr_review_bot_run": "github", "land_session": "vcs",
	"start_skill": "skill", "finish_skill": "skill", "fail_run": "skill", RecallTool: "explore",
}

// verbatim classes are never folded, so an instruction or answer cannot live only in a fold.
var verbatim = map[string]bool{"user": true, "assistant": true, "ask_user": true, "refusal": true}

type lstep struct {
	i      int
	class  string
	tool   string
	target string
	exit   int // -1 when the output carries none
	full   string
}

func inputField(raw json.RawMessage, keys ...string) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func fullText(e Event) string {
	switch e.Kind {
	case "user":
		return "USER: " + e.Text
	case "text":
		return "ASSISTANT: " + e.Text
	}
	return "TOOL " + e.Name + " " + string(e.Input) + "\n" + e.Output
}

func classify(i int, e Event) lstep {
	l := lstep{i: i, exit: -1, full: fullText(e), tool: e.Name}
	switch e.Kind {
	case "user":
		l.class = "user"
		return l
	case "text":
		l.class = "assistant"
		return l
	}
	if m := exitRe.FindStringSubmatch(e.Output); m != nil {
		fmt.Sscan(m[1], &l.exit)
	}
	low := strings.ToLower(e.Output)
	switch {
	case e.Name == "ask_user", e.Name == "decline_suggestion":
		l.class = map[bool]string{true: "ask_user", false: "refusal"}[e.Name == "ask_user"]
	case len(e.Output) < 800 && (strings.Contains(low, "refused") || strings.Contains(low, "is blocked") || strings.Contains(low, "not allowed")):
		l.class = "refusal"
	case e.Name == "str_replace_based_edit_tool":
		l.target = inputField(e.Input, "path")
		l.tool = inputField(e.Input, "command")
		l.class = map[bool]string{true: "read", false: "edit"}[l.tool == "view"]
	case e.Name == "bash":
		l.target = short(inputField(e.Input, "command"), 70)
		l.class = "bash"
		for _, sym := range mine.Encode("bash", e.Input) {
			switch {
			case strings.HasPrefix(sym, "go_test"), strings.HasPrefix(sym, "go_vet"), strings.HasPrefix(sym, "go_build"),
				sym == "pytest", sym == "tsc", strings.HasPrefix(sym, "npm_"), strings.HasPrefix(sym, "pnpm_"):
				l.class = "check"
			case (strings.HasPrefix(sym, "jj_") || strings.HasPrefix(sym, "git_")) && l.class == "bash":
				l.class = "vcs"
			}
		}
	default:
		l.class = classOf[e.Name]
		if l.class == "" {
			l.class = "other"
		}
		l.target = inputField(e.Input, "path", "pattern", "query", "url", "name", "title", "objective")
		if l.class != "read" {
			l.target = short(l.target, 60)
		}
	}
	return l
}

// writeExploreHead writes an explore or read episode's range line, listing only targets no body line names.
func writeExploreHead(b *strings.Builder, rng, class string, ep []lstep, named map[string]bool) {
	var rest []lstep
	for _, x := range ep {
		if !named[fmt.Sprint(x.i)] {
			rest = append(rest, x)
		}
	}
	if fs := filesIn(rest); fs != "" {
		fmt.Fprintf(b, "[%s] %s: %s\n", rng, class, fs)
	} else {
		fmt.Fprintf(b, "[%s] %s\n", rng, class)
	}
}

func filesIn(ls []lstep) string {
	seen := map[string]int{}
	var order []string
	for _, l := range ls {
		if l.target == "" {
			continue
		}
		if seen[l.target] == 0 {
			order = append(order, l.target)
		}
		seen[l.target]++
	}
	var parts []string
	for _, t := range order[:min(6, len(order))] {
		if seen[t] > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", t, seen[t]))
		} else {
			parts = append(parts, t)
		}
	}
	if len(order) > 6 {
		parts = append(parts, fmt.Sprintf("+%d more", len(order)-6))
	}
	return strings.Join(parts, ", ")
}

func firstError(out string) string {
	for _, line := range strings.Split(out, "\n") {
		ll := strings.ToLower(line)
		if strings.Contains(ll, "error") || strings.Contains(ll, "fail") || strings.Contains(ll, "panic") || strings.Contains(ll, ".go:") {
			return short(line, 140)
		}
	}
	return short(out, 140)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if k := strings.IndexByte(s, '\n'); k >= 0 {
		return s[:k]
	}
	return s
}

// cited reports, for each read or explore step, the identifiers from its output that a later step uses in its
// input or prose; outputs are not uses, so a read is not kept alive by another read.
func cited(ev []Event, ls []lstep, keep bool) map[int][]string {
	uses := make([]string, len(ev))
	for i, e := range ev {
		uses[i] = e.Text + string(e.Input)
	}
	out := map[int][]string{}
	for i := range ev {
		if keep && (ev[i].Kind != "tool" || verbatim[ls[i].class]) || !keep && ls[i].class != "explore" && ls[i].class != "read" {
			continue
		}
		ids := map[string]bool{}
		for _, t := range tokenRe.FindAllString(ev[i].Output, -1) {
			ids[t] = true
		}
		for _, t := range identRe.FindAllString(ev[i].Output, -1) {
			ids[t] = true
		}
		var hits []string
		for t := range ids {
			for j := i + 1; j < len(ev); j++ {
				if strings.Contains(uses[j], t) {
					hits = append(hits, t)
					break
				}
			}
		}
		sort.Strings(hits)
		out[i] = hits
	}
	return out
}

// minted reports, for each tool step, the identifiers on its output's first line that nothing earlier mentions:
// the call created them (a stored record, an opened PR), so they matter before anything has used them.
func minted(ev []Event) map[int][]string {
	out := map[int][]string{}
	var seen strings.Builder
	for i, e := range ev {
		seen.WriteString(e.Text)
		seen.Write(e.Input)
		if e.Kind == "tool" {
			for _, t := range tokenRe.FindAllString(firstLine(e.Output), -1) {
				if !strings.Contains(seen.String(), t) {
					out[i] = append(out[i], t)
				}
			}
		}
		seen.WriteString(e.Output)
	}
	return out
}

// writeUsed lists, under a folded episode, the identifiers its steps created and those a later step uses, each
// tagged with its step and skipped when the view already holds it.
func writeUsed(b *strings.Builder, ep []lstep, cites, made map[int][]string) {
	so := b.String()
	seen := map[string]bool{}
	pick := func(ids []string, at int, into *[]string) {
		for _, t := range ids {
			if !seen[t] && !strings.Contains(so, t) {
				seen[t] = true
				*into = append(*into, fmt.Sprintf("%s @%d", t, at))
			}
		}
	}
	var created, used []string
	for _, x := range ep {
		pick(made[x.i], x.i, &created)
		c := cites[x.i]
		pick(c[:min(5, len(c))], x.i, &used)
	}
	if len(created) > 0 {
		fmt.Fprintf(b, "    created: %s\n", strings.Join(created, ", "))
	}
	if len(used) > 0 {
		fmt.Fprintf(b, "    used later: %s\n", strings.Join(used, ", "))
	}
}

// ledger renders the events as the episode ledger. Tool runs fold into one line per episode, an explore
// block's same-tool, same-target calls merge, refusals shrink to one line, and paths go relative to ws.
// reply rewrites the kept assistant reply at step i, given how many user turns follow it.
func ledger(ev []Event, ws string, keep bool, reply func(ev []Event, i, age int) string) string {
	cut := len(ev)
	ls := make([]lstep, cut)
	for i, e := range ev {
		if ws != "" {
			// relative before classify, so the label's length cap is spent on the path inside the workspace
			e.Input = json.RawMessage(strings.ReplaceAll(string(e.Input), ws+"/", ""))
		}
		ls[i] = classify(i, e)
	}
	pageTargets(ev, ls)
	cites, made := cited(ev, ls, keep), map[int][]string{}
	if keep {
		made = minted(ev)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s, steps 0-%d. Each line names a step range; the %s tool returns a range's full text.\n", Marker, cut-1, RecallTool)
	if ws != "" {
		fmt.Fprintf(&b, "Paths are relative to the workspace %s.\n", ws)
	}

	turnStatus := func(from int) string {
		to := cut
		for j := from + 1; j < cut; j++ {
			if ls[j].class == "user" {
				to = j
				break
			}
		}
		var edits []lstep
		lastCheck, lastText := "none", ""
		for _, l := range ls[from+1 : to] {
			switch l.class {
			case "edit":
				edits = append(edits, l)
			case "check":
				lastCheck = map[bool]string{true: "ok", false: "red"}[l.exit == 0]
				lastCheck += fmt.Sprintf(" @%d", l.i)
			case "assistant":
				lastText = ev[l.i].Text
			}
		}
		var parts []string
		if n := distinctTargets(edits); n > 0 {
			parts = append(parts, fmt.Sprintf("edited %d files", n))
		}
		if lastCheck != "none" {
			parts = append(parts, "last check "+lastCheck)
		}
		if to == cut && ls[cut-1].class != "assistant" {
			parts = append(parts, "turn still running at the cut")
		} else if strings.Contains(lastText[max(0, len(lastText)-400):], "?") {
			parts = append(parts, "reply ended by asking the user")
		}
		return strings.Join(parts, "; ")
	}

	finalReply := map[int]bool{}
	for i := 0; i < cut; i++ {
		if ls[i].class != "assistant" {
			continue
		}
		next := cut
		for j := i + 1; j < cut; j++ {
			if ls[j].class == "user" || ls[j].class == "assistant" {
				next = j
				break
			}
		}
		finalReply[i] = next == cut || ls[next].class == "user"
	}
	// the turn still running at the cut keeps all its prose: its plan lives in mid-turn notes
	for i := cut - 1; i >= 0 && ls[i].class != "user"; i-- {
		if ls[i].class == "assistant" {
			finalReply[i] = true
		}
	}
	tail := map[int]bool{}
	left := tailBudget
	for i := cut - 1; keep && i >= 0; i-- {
		if ev[i].Kind != "tool" || verbatim[ls[i].class] {
			continue
		}
		if len(ev[i].Output) > left {
			break
		}
		tail[i] = true
		left -= len(ev[i].Output)
	}
	for i := 0; i < cut; {
		l := ls[i]
		e := ev[i]
		if tail[i] {
			fmt.Fprintf(&b, "@%d %s %s result:\n%s\n", i, e.Name, l.target, strings.TrimSpace(e.Output))
			i++
			continue
		}
		if l.class == "assistant" && !finalReply[i] {
			first := e.Text
			if k := strings.IndexAny(e.Text, ".\n"); k > 0 {
				first = e.Text[:k]
			}
			fmt.Fprintf(&b, "[%d] note: %s\n", i, short(first, 120))
			i++
			continue
		}
		if l.class == "refusal" && (e.Name == "decline_suggestion" || refusalLineRe.MatchString(firstLine(e.Output))) {
			target := inputField(e.Input, "path", "url", "command", "pattern", "query", "id")
			if ws != "" {
				target = strings.ReplaceAll(target, ws+"/", "")
			}
			reason := strings.TrimSpace(e.Output)
			if k := strings.Index(reason, ". "); k > 0 {
				reason = reason[:k]
			}
			fmt.Fprintf(&b, "@%d refused %s %s: %s\n", i, e.Name, short(target, 60), short(reason, 160))
			i++
			continue
		}
		if verbatim[l.class] {
			text := l.full
			if l.class == "assistant" {
				age := 0
				for j := i + 1; j < cut; j++ {
					if ls[j].class == "user" {
						age++
					}
				}
				text = "ASSISTANT: " + reply(ev, i, age)
			}
			if l.class == "user" {
				b.WriteString(turnHead(i, len(itemRe.FindAllString(e.Text, -1)), turnStatus(i)))
			}
			if l.class == "refusal" && e.Name == "str_replace_based_edit_tool" {
				text = editResult(e.Input, e.Output)
			}
			fmt.Fprintf(&b, "@%d %s\n", i, text)
			i++
			continue
		}
		// episode: a maximal run of one class; edits and checks join into one edit-loop
		j := i + 1
		loop := l.class == "edit" || l.class == "check"
		for j < cut && !verbatim[ls[j].class] && !tail[j] {
			c := ls[j].class
			if loop && (c == "edit" || c == "check") || c == l.class {
				j++
				continue
			}
			break
		}
		ep := ls[i:j]
		rng := fmt.Sprintf("%d", i)
		if j-1 > i {
			rng = fmt.Sprintf("%d-%d", i, j-1)
		}
		switch {
		case loop:
			var edits []lstep
			var checks []string
			for _, x := range ep {
				if x.class == "edit" {
					edits = append(edits, x)
					continue
				}
				r := "ok"
				if x.exit != 0 {
					r = fmt.Sprintf("RED exit %d: %s", x.exit, firstError(ev[x.i].Output))
				}
				checks = append(checks, fmt.Sprintf("check@%d %s", x.i, r))
			}
			fmt.Fprintf(&b, "[%s] edit-loop: %d edits (%s)", rng, len(edits), filesIn(edits))
			if len(checks) > 0 {
				fmt.Fprintf(&b, "; %s", strings.Join(checks, "; "))
			}
			b.WriteString("\n")
			writeUsed(&b, ep, cites, made)
		case l.class == "explore" || l.class == "read":
			var live []liveLine
			var dead []string
			type group struct {
				at       []string
				tool, tg string
				ids      map[string]bool
			}
			var groups []*group
			byKey := map[string]*group{}
			for _, x := range ep {
				c := cites[x.i]
				if len(c) == 0 {
					dead = append(dead, fmt.Sprint(x.i))
					continue
				}
				k := x.tool + "\x00" + x.target
				g := byKey[k]
				if g == nil {
					g = &group{tool: x.tool, tg: x.target, ids: map[string]bool{}}
					byKey[k] = g
					groups = append(groups, g)
				}
				g.at = append(g.at, fmt.Sprint(x.i))
				for _, id := range c[:min(5, len(c))] {
					g.ids[id] = true
				}
			}
			so := b.String()
			listed, named := map[string]bool{}, map[string]bool{}
			for _, g := range groups {
				var ids []string
				for id := range g.ids {
					if !keep || !listed[id] && !strings.Contains(so, id) {
						listed[id] = true
						ids = append(ids, id)
					}
				}
				if len(ids) == 0 {
					continue
				}
				sort.Strings(ids)
				for _, at := range g.at {
					named[at] = true
				}
				live = append(live, liveLine{g.at, g.tool, fmt.Sprintf("%s → used later: %s", g.tg, strings.Join(ids, ", "))})
			}
			writeExploreHead(&b, rng, l.class, ep, named)
			writeRuns(&b, live)
			if len(dead) > 0 {
				fmt.Fprintf(&b, "    nothing later used from @%s\n", strings.Join(dead, ","))
			}
		default:
			var res []string
			for _, x := range ep {
				if x.exit > 0 {
					res = append(res, fmt.Sprintf("@%d exit %d: %s", x.i, x.exit, firstError(ev[x.i].Output)))
				}
			}
			fmt.Fprintf(&b, "[%s] %s: %s", rng, l.class, filesIn(ep))
			if len(res) > 0 {
				fmt.Fprintf(&b, "; %s", strings.Join(res, "; "))
			}
			b.WriteString("\n")
			writeUsed(&b, ep, cites, made)
		}
		i = j
	}
	if ws == "" {
		return b.String()
	}
	header, body, _ := strings.Cut(b.String(), "\n")
	header2, body, _ := strings.Cut(body, "\n")
	body = strings.ReplaceAll(strings.ReplaceAll(body, ws+"/", ""), ws, ".")
	return header + "\n" + header2 + "\n" + body
}
