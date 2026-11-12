package skills

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// The evidence rules here serve research skills: they read the web cache, not just the call log.

type fetch struct {
	raw    string
	at     int
	domain string
}

func (c *checker) indexFetches() {
	for i, e := range c.log {
		if e.Tool != fetchTool || e.Failed {
			continue
		}
		raw := strings.TrimSpace(field(e.Input, "url"))
		key := normalURL(raw)
		if _, seen := c.fetched[key]; key != "" && !seen {
			c.fetched[key] = fetch{raw: raw, at: i, domain: domain(raw)}
		}
	}
}

// fetchesOpened counts fetched pages whose cache file a later view or grep opened.
func (c *checker) fetchesOpened(want int) (int, []string) {
	opened := map[string]bool{}
	for i, e := range c.log {
		if e.Failed || !(e.Tool == editorTool && field(e.Input, "command") == "view" || e.Tool == grepTool) {
			continue
		}
		u, ok := c.env.CachedURL(field(e.Input, "path"))
		if !ok {
			continue
		}
		if f, ok := c.fetched[normalURL(u)]; ok && f.at < i {
			opened[normalURL(u)] = true
			if len(opened) >= want {
				return i, nil
			}
		}
	}
	return -1, []string{fmt.Sprintf("%d of %d fetched pages were opened with view or grep", len(opened), want)}
}

var citedURL = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `]+`)

func (c *checker) citationsFetched(cf *CitationsFetched) (int, []string) {
	path := c.run.Expand(cf.Path)
	data, err := c.env.ReadFile(path)
	if err != nil {
		return -1, []string{path + " has not been written"}
	}
	want := 1
	if cf.Min != nil {
		want = c.run.count(*cf.Min)
	}
	cited := map[string]bool{}
	var unfetched []string
	for _, u := range citedURL.FindAllString(string(data), -1) {
		u = strings.TrimRight(u, ".,;:!?")
		key := normalURL(u)
		if cited[key] {
			continue
		}
		cited[key] = true
		if _, ok := c.fetched[key]; !ok {
			unfetched = append(unfetched, u)
		}
	}
	var missing []string
	if len(cited) < want {
		missing = append(missing, fmt.Sprintf("%s cites %d of %d URLs", path, len(cited), want))
	}
	if len(unfetched) > 0 {
		missing = append(missing, fmt.Sprintf("%s cites %d URLs never fetched: %s", path, len(unfetched), strings.Join(unfetched, ", ")))
	}
	if missing != nil {
		return -1, missing
	}
	return c.lastTouch(path), nil
}

// leadsFollowed counts leads that came from a page the agent read and were then pursued:
// a later search naming the lead, then a fetch of a page that mentions it on a domain new at that search.
// A lead marked "dead:" in a fourth column needs only the search, and at most one per hop counts.
func (c *checker) leadsFollowed(l *LeadsFollowed, times int) (int, []string) {
	path := c.run.Expand(l.Path)
	data, err := c.env.ReadFile(path)
	if err != nil {
		return -1, []string{path + " has not been written"}
	}
	question := ""
	if l.NotInInput != "" {
		question, _ = c.run.Inputs[l.NotInInput].(string)
	}
	want := c.run.count(l.Min) * times
	var problems []string
	var followed []int
	dead := 0
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "- ")
		if !ok {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		term, src := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		key := looseKey(term)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		isDead := len(parts) > 3 && strings.HasPrefix(strings.ToLower(strings.TrimSpace(parts[3])), "dead:")
		if isDead && dead >= times {
			problems = append(problems, fmt.Sprintf("lead %q: only %d dead end(s) count, one per hop", term, times))
			continue
		}
		at, why := c.lead(term, src, question, isDead)
		if why != "" {
			problems = append(problems, fmt.Sprintf("lead %q: %s", term, why))
			continue
		}
		if isDead {
			dead++
		}
		followed = append(followed, at)
	}
	if len(followed) < want {
		return -1, append([]string{fmt.Sprintf("%s: %d of %d leads followed", path, len(followed), want)}, problems...)
	}
	slices.Sort(followed)
	return max(followed[want-1], c.lastTouch(path)), nil
}

func (c *checker) lead(term, src, question string, dead bool) (int, string) {
	f, ok := c.fetched[normalURL(src)]
	if !ok {
		return -1, "its source " + src + " was never fetched"
	}
	if text, ok := c.env.PageText(f.raw); !ok || !mentionsTerm(text, term) {
		return -1, "its source page does not mention it"
	}
	if question != "" && mentionsTerm(question, term) {
		return -1, "the question already asks about it"
	}
	search := -1
	for i := f.at + 1; i < len(c.log); i++ {
		e := c.log[i]
		if e.Tool == searchTool && !e.Failed && mentionsTerm(field(e.Input, "query"), term) {
			search = i
			break
		}
	}
	if search < 0 {
		return -1, "no search after reading its source names it"
	}
	if dead {
		return search, ""
	}
	known := map[string]bool{}
	for _, other := range c.fetched {
		if other.at < search {
			known[other.domain] = true
		}
	}
	for i := search + 1; i < len(c.log); i++ {
		e := c.log[i]
		if e.Tool != fetchTool || e.Failed {
			continue
		}
		u := field(e.Input, "url")
		if d := domain(u); d == "" || known[d] {
			continue
		}
		if text, ok := c.env.PageText(strings.TrimSpace(u)); ok && mentionsTerm(text, term) {
			return i, ""
		}
	}
	return -1, "no fetch after its search reached a new domain that mentions it"
}

// mentionsTerm matches a lead's name ignoring case, punctuation, spacing and a possessive 's.
func mentionsTerm(s, term string) bool {
	t := looseKey(term)
	return t != "" && strings.Contains(looseKey(s), t)
}

func looseKey(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if (r == '\'' || r == '’') && i+1 < len(rs) && unicode.ToLower(rs[i+1]) == 's' &&
			(i+2 == len(rs) || !unicode.IsLetter(rs[i+2]) && !unicode.IsDigit(rs[i+2])) {
			i++
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}
