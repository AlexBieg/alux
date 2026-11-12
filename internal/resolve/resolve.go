// Package resolve names the PR numbers, ticket keys and record-store ids prose mentions, for display only.
package resolve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/vault"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/charmbracelet/x/ansi"
)

var (
	recordID  = regexp.MustCompile(`\b(?:rec|ent|src)_[0-9a-f]{16}\b`)
	ticketKey = regexp.MustCompile(`\b[A-Z]{2,8}-\d+\b`)
	// RE2 has no lookbehind, so the character before the # is matched and left out of the span.
	prNumber = regexp.MustCompile(`(?:^|[^\w&/#])(#\d{4,})\b`)
)

// Find is the span of every id in text, in order and never overlapping.
func Find(text string) [][]int {
	var spans [][]int
	for _, re := range []*regexp.Regexp{recordID, ticketKey} {
		spans = append(spans, re.FindAllStringIndex(text, -1)...)
	}
	for _, m := range prNumber.FindAllStringSubmatchIndex(text, -1) {
		spans = append(spans, m[2:4])
	}
	slices.SortFunc(spans, func(a, b []int) int { return a[0] - b[0] })
	out := spans[:0]
	for _, s := range spans {
		if len(out) == 0 || s[0] >= out[len(out)-1][1] {
			out = append(out, s)
		}
	}
	return out
}

// IDs is every distinct id in text, in order of first mention.
func IDs(text string) []string {
	var ids []string
	for _, s := range Find(text) {
		if id := text[s[0]:s[1]]; !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

type State int

const (
	Pending State = iota + 1
	Known
	None
)

type Entry struct {
	Name  string
	State State
}

// Labels is the record store's no-taint naming.
type Labels interface {
	Label(id string) (string, bool, error)
	TicketLabel(key string) (string, bool, error)
}

// MCP is the slice of mcpclient.Manager the ticket lookup needs.
type MCP interface {
	Inventory() []mcpclient.ToolInfo
	Tools() []anthropic.ToolUnionParam
	Call(ctx context.Context, name string, input json.RawMessage) mcpclient.Result
}

const (
	maxBackground = 4
	timeout       = 4 * time.Second
)

// Resolver is safe for concurrent use. A nil source is skipped.
type Resolver struct {
	Records func() (Labels, error)
	Vault   string
	// TicketsDir is the vault folder of ticket folders; empty skips the vault for ticket keys.
	TicketsDir string
	// TicketServer is the MCP server whose read-only issue tool names ticket keys; empty skips it.
	TicketServer string
	Queue        func() []prs.RepoState
	Gh           prs.Gh
	// Repo is where a bare #number is looked up with gh; empty skips gh.
	Repo string
	MCP  MCP
	// Remote, when set, answers every lookup in place of the sources above: a TUI's resolver, asking the session that holds them.
	Remote func(ctx context.Context, id string) string
	// OnName fires from a lookup goroutine whenever a name becomes known.
	OnName func()

	mu    sync.Mutex
	table map[string]Entry
	slots chan struct{}
	wg    sync.WaitGroup
}

func (r *Resolver) init() {
	if r.table == nil {
		r.table = map[string]Entry{}
		r.slots = make(chan struct{}, maxBackground)
	}
}

// Name never blocks: an id seen for the first time is queued for lookup and reported unknown.
func (r *Resolver) Name(id string) (string, bool) {
	r.mu.Lock()
	r.init()
	e, ok := r.table[id]
	if !ok {
		r.table[id] = Entry{State: Pending}
		r.wg.Add(1)
		go r.lookup(id)
	}
	r.mu.Unlock()
	return e.Name, e.State == Known
}

// Known is a copy of every entry looked up so far.
func (r *Resolver) Known() map[string]Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Entry, len(r.table))
	for id, e := range r.table {
		out[id] = e
	}
	return out
}

// Learn records a name seen elsewhere, such as a PR title in a fetched detail.
func (r *Resolver) Learn(id, name string) {
	name = clean(name)
	if name == "" {
		return
	}
	r.mu.Lock()
	r.init()
	changed := r.table[id] != Entry{Name: name, State: Known}
	r.table[id] = Entry{Name: name, State: Known}
	r.mu.Unlock()
	if changed && r.OnName != nil {
		r.OnName()
	}
}

// Lookup is Name that waits for the answer, "" when no source knows it. A name learned or found
// before is answered from the table.
func (r *Resolver) Lookup(id string) string {
	r.mu.Lock()
	r.init()
	e, ok := r.table[id]
	r.mu.Unlock()
	if ok && e.State == Known {
		return e.Name
	}
	name := clean(r.find(id))
	if name != "" {
		r.Learn(id, name)
	}
	return name
}

// Wait blocks until every queued lookup has finished.
func (r *Resolver) Wait() { r.wg.Wait() }

func (r *Resolver) lookup(id string) {
	defer r.wg.Done()
	name := clean(r.find(id))
	r.mu.Lock()
	if r.table[id].State == Known {
		r.mu.Unlock()
		return
	}
	e := Entry{State: None}
	if name != "" {
		e = Entry{Name: name, State: Known}
	}
	r.table[id] = e
	r.mu.Unlock()
	if e.State == Known && r.OnName != nil {
		r.OnName()
	}
}

func (r *Resolver) find(id string) string {
	if r.Remote != nil {
		r.slots <- struct{}{}
		defer func() { <-r.slots }()
		// Twice a local lookup's bound, which the session's own lookup runs under.
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		return r.Remote(ctx, id)
	}
	switch {
	case strings.HasPrefix(id, "#"):
		n, err := strconv.Atoi(id[1:])
		if err != nil {
			return ""
		}
		if title := r.queued(n); title != "" {
			return title
		}
		return r.gh(n)
	case recordID.MatchString(id):
		return r.label(func(l Labels) (string, bool, error) { return l.Label(id) })
	default:
		if title, ok := vault.TicketTitle(r.Vault, r.TicketsDir, id); ok {
			return title
		}
		if label := r.label(func(l Labels) (string, bool, error) { return l.TicketLabel(id) }); label != "" {
			return label
		}
		return r.ticket(id)
	}
}

func (r *Resolver) label(read func(Labels) (string, bool, error)) string {
	if r.Records == nil {
		return ""
	}
	l, err := r.Records()
	if err != nil || l == nil {
		return ""
	}
	name, ok, err := read(l)
	if err != nil || !ok {
		return ""
	}
	return name
}

func (r *Resolver) queued(n int) string {
	if r.Queue == nil {
		return ""
	}
	for _, state := range r.Queue() {
		for _, it := range append(slices.Clone(state.Queue.Mine), state.Queue.Review...) {
			if it.Number == n {
				return it.Title
			}
		}
	}
	return ""
}

// background runs one network lookup, at most maxBackground at a time.
func (r *Resolver) background(call func(ctx context.Context) string) string {
	r.slots <- struct{}{}
	defer func() { <-r.slots }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return call(ctx)
}

func (r *Resolver) gh(n int) string {
	if r.Gh == nil || r.Repo == "" {
		return ""
	}
	return r.background(func(ctx context.Context) string {
		out, err := r.Gh(ctx, "pr", "view", strconv.Itoa(n), "--repo", r.Repo, "--json", "title")
		if err != nil {
			return ""
		}
		var pr struct{ Title string }
		if json.Unmarshal([]byte(out), &pr) != nil {
			return ""
		}
		return pr.Title
	})
}

func (r *Resolver) ticket(key string) string {
	if r.MCP == nil || r.TicketServer == "" {
		return ""
	}
	tool, ok := issueTool(r.MCP.Inventory(), r.TicketServer)
	if !ok {
		return ""
	}
	input, err := json.Marshal(map[string]string{issueArg(r.MCP.Tools(), tool): key})
	if err != nil {
		return ""
	}
	return r.background(func(ctx context.Context) string {
		res := r.MCP.Call(ctx, tool, input)
		if res.IsError {
			return ""
		}
		return issueTitle(res.Display)
	})
}

// issueTool is a tool of server that reads one issue and claims to write nothing.
func issueTool(tools []mcpclient.ToolInfo, server string) (string, bool) {
	var found string
	for _, t := range tools {
		name := strings.ToLower(t.Name)
		if !t.ReadOnly || t.Server != server || !strings.Contains(name, "get") || !strings.Contains(name, "issue") {
			continue
		}
		if strings.HasSuffix(name, "get_issue") {
			return t.Name, true
		}
		if found == "" && !strings.Contains(name, "issues") && !strings.Contains(name, "comment") {
			found = t.Name
		}
	}
	return found, found != ""
}

// issueArg is whichever key name the tool's schema declares, falling back to "id".
func issueArg(defs []anthropic.ToolUnionParam, tool string) string {
	for _, def := range defs {
		if def.OfTool == nil || def.OfTool.Name != tool {
			continue
		}
		props, _ := def.OfTool.InputSchema.Properties.(map[string]any)
		for _, arg := range []string{"id", "issueId", "identifier", "issue_id"} {
			if _, ok := props[arg]; ok {
				return arg
			}
		}
	}
	return "id"
}

var titleLine = regexp.MustCompile(`(?im)^\W*title\W*:\s*(.+)$`)

func issueTitle(display string) string {
	var issue struct {
		Title string
		Issue struct{ Title string }
	}
	if json.Unmarshal([]byte(display), &issue) == nil {
		if issue.Title != "" {
			return issue.Title
		}
		return issue.Issue.Title
	}
	if m := titleLine.FindStringSubmatch(display); m != nil {
		return m[1]
	}
	return ""
}

// TicketServer is settings.json's tickets.mcp_server, or "" when it is unset or unreadable.
func TicketServer() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return ""
	}
	var settings struct {
		Tickets struct {
			MCPServer string `json:"mcp_server"`
		} `json:"tickets"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return ""
	}
	return settings.Tickets.MCPServer
}

func clean(name string) string {
	return strings.Join(strings.Fields(ansi.Strip(name)), " ")
}
