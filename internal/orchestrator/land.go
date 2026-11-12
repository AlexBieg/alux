package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/vcs"
	"github.com/anthropics/anthropic-sdk-go"
)

// LandSessionTool is the main agent's way to land pool work, on the user's ask.
const LandSessionTool = "land_session"

// ledger lists, one change id a line, the described work sessions left in their slots.
const ledger = "changes"

const described = ` & ~empty() & ~description(exact:"")`

// sessionChanges is the described, non-empty work in s since its session started.
func sessionChanges(ctx context.Context, s *Slot) ([]string, error) {
	rev := "@"
	if base, err := os.ReadFile(filepath.Join(s.Dir, baseMark)); err == nil && len(base) > 0 {
		rev = "(" + string(base) + "..@) ~ immutable()"
	}
	out, err := jjStdout(ctx, s.Dir, "log", "-r", "("+rev+")"+described, "--no-graph", "-T", `change_id ++ "\n"`)
	return lines(out), err
}

// record appends the slot's session work to the ledger, so /land finds it after the session is gone.
func (p *Pool) record(ctx context.Context, s *Slot) error {
	changes, err := sessionChanges(ctx, s)
	if err != nil || len(changes) == 0 {
		return err
	}
	f, err := os.OpenFile(filepath.Join(p.Root, ledger), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(changes, "\n") + "\n")
	return err
}

func (p *Pool) ledgerIDs() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(p.Root, ledger))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return lines(strings.TrimSpace(string(data))), nil
}

// unlandedRev is the described, mutable changes among ids not yet under the default workspace's @.
func unlandedRev(ids []string) string {
	terms := make([]string, len(ids))
	for i, id := range ids {
		terms[i] = "present(change_id(" + id + "))"
	}
	return "(" + strings.Join(terms, " | ") + ")" + described + " & mutable() & ~::default@"
}

// Unlanded is one change of pool work not yet under the default workspace's @.
type Unlanded struct {
	Change, Age, Description string
	// With is the other listed changes that land with it, newest first: its ancestors, and for this session's own work its whole stack.
	With []string
}

// Unlanded lists, newest first, the described work sessions made in slots that is not yet under the default workspace's @, own included.
func (p *Pool) Unlanded(ctx context.Context, own *Slot) ([]Unlanded, error) {
	ids, err := p.ledgerIDs()
	if err != nil {
		return nil, err
	}
	var mine []string
	if own != nil {
		if mine, err = sessionChanges(ctx, own); err != nil {
			return nil, err
		}
		ids = append(ids, mine...)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", unlandedRev(ids), "--no-graph", "-T",
		`change_id.shortest(8) ++ "\t" ++ change_id ++ "\t" ++ parents.map(|c| c.change_id()).join(",") ++ "\t" ++ committer.timestamp().ago() ++ "\t" ++ description.first_line() ++ "\n"`)
	if err != nil {
		return nil, err
	}
	var list []Unlanded
	full := map[string]int{}
	var parents [][]string
	for _, line := range lines(out) {
		if f := strings.SplitN(line, "\t", 5); len(f) == 5 {
			full[f[1]] = len(list)
			parents = append(parents, strings.Split(f[2], ","))
			list = append(list, Unlanded{Change: f[0], Age: f[3], Description: f[4]})
		}
	}
	for i := range list {
		seen := map[int]bool{}
		next := append([]string{}, parents[i]...)
		if slices.ContainsFunc(mine, func(id string) bool { j, ok := full[id]; return ok && j == i }) {
			next = append(next, mine...)
		}
		for len(next) > 0 {
			id := next[0]
			next = next[1:]
			if j, ok := full[id]; ok && !seen[j] {
				seen[j] = true
				next = append(next, parents[j]...)
			}
		}
		for j := range list {
			if seen[j] && j != i {
				list[i].With = append(list[i].With, list[j].Change)
			}
		}
	}
	return list, nil
}

// landLock serialises landings into the default workspace across every session, with coder landings there too.
func (p *Pool) landLock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(p.Origin, ".jj", "alux-land.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Land puts change and its unlanded ancestors under the default workspace's @, freezing own's live change first when it is own's work.
func (p *Pool) Land(ctx context.Context, own *Slot, change string) (string, error) {
	if p.Prepare.PRFlow() {
		return "", ErrPRFlow
	}
	list, err := p.Unlanded(ctx, own)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(list, func(u Unlanded) bool {
		return strings.HasPrefix(u.Change, change) || strings.HasPrefix(change, u.Change)
	})
	if change == "" || i < 0 {
		return "", fmt.Errorf("%q is not unlanded work from this repo's pool; /land lists what is", change)
	}
	target := list[i]
	commits, err := commitsOf(ctx, p.Origin, target.Change)
	if err != nil {
		return "", err
	}
	if len(commits) > 1 {
		return "", &DivergentError{Change: target.Change, Commits: commits}
	}
	head, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", "change_id("+target.Change+")", "--no-graph", "-T", "change_id")
	if err != nil {
		return "", err
	}
	unlock, err := p.landLock()
	if err != nil {
		return "", err
	}
	defer unlock()

	var left string
	if own != nil {
		mine, err := sessionChanges(ctx, own)
		if err != nil {
			return "", err
		}
		if slices.Contains(mine, head) {
			if left, head, err = freezeAll(ctx, own); err != nil {
				return "", err
			}
		}
	}
	moving := "(::change_id(" + head + ") ~ ::@) & mutable()"
	moved, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", moving, "--no-graph", "-T",
		`"- " ++ change_id.shortest(8) ++ " " ++ if(description, description.first_line(), "(no description)") ++ "\n"`)
	if err != nil {
		return "", err
	}
	if _, err := jj(ctx, p.Origin, "rebase", "-r", moving, "--insert-before", "@"); err != nil {
		return "", err
	}
	text := fmt.Sprintf("Landed %s (%s) under your working copy in %s.", target.Change, target.Description, p.Origin)
	if rows := lines(strings.TrimSpace(moved)); len(rows) > 1 {
		text = fmt.Sprintf("Landed %d changes under your working copy in %s, newest first; %s was asked for and the rest went with it:\n%s\n\n",
			len(rows), p.Origin, target.Change, strings.Join(rows, "\n"))
	}
	if left != "" {
		if err := rejoin(ctx, own, head, left); err != nil {
			text += " Moving this session back on top of it failed: " + err.Error() + "."
		} else {
			text += " This session carries on in a new change on top of it, which needs jj describe -m before the next edit."
		}
	}
	conflict, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", "@ | change_id("+head+")", "--no-graph", "-T", `if(conflict, "conflict ")`)
	if err != nil {
		return text + " Checking it for a conflict failed (" + err.Error() + "); run jj status in " + p.Origin + ".", nil
	}
	if strings.Contains(conflict, "conflict") {
		text += " It conflicts with what was already there: jj recorded the conflict, and the markers are in the files. Resolve them before anything else."
	}
	return text, nil
}

// freezeAll commits s's live change when it has edits, and returns the change s was on and the head of its work to land.
func freezeAll(ctx context.Context, s *Slot) (left, head string, err error) {
	at, err := jjStdout(ctx, s.Dir, "log", "-r", "@", "--no-graph", "-T", `change_id ++ " " ++ if(empty, "empty", "work")`)
	if err != nil {
		return "", "", err
	}
	f := strings.Fields(at)
	if len(f) != 2 {
		return "", "", fmt.Errorf("reading this session's @ gave %q", at)
	}
	if f[1] == "work" {
		if _, err := jj(ctx, s.Dir, "new"); err != nil {
			return "", "", err
		}
		left, err = jjStdout(ctx, s.Dir, "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", "change_id")
		return left, f[0], err
	}
	head, err = jjStdout(ctx, s.Dir, "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", "change_id")
	return f[0], head, err
}

// rejoin puts s back on top of the work it just landed, dropping the empty change it was left on.
func rejoin(ctx context.Context, s *Slot, head, left string) error {
	if _, err := jjStdout(ctx, s.Dir, "log", "-r", "@", "--no-graph", "-T", "commit_id"); err != nil {
		if !strings.Contains(err.Error(), "stale") {
			return err
		}
		if _, err := jj(ctx, s.Dir, "workspace", "update-stale"); err != nil {
			return err
		}
	}
	if _, err := jj(ctx, s.Dir, "new", "change_id("+head+")"); err != nil {
		return err
	}
	_, err := jj(ctx, s.Dir, "abandon", "present(change_id("+left+")) & empty()")
	return err
}

// Lander is /land and the main agent's land tool for one session.
type Lander struct {
	Pool *Pool
	Slot *Slot

	mu      sync.Mutex
	cached  []Unlanded
	fetched time.Time
}

// candidatesTTL keeps one listing for the keystrokes of a typed change id, since listing runs jj.
const candidatesTTL = 30 * time.Second

// Candidates is the unlanded work for completing /land, reused for candidatesTTL; a failed listing is not cached.
func (l *Lander) Candidates() ([]Unlanded, error) {
	if l.PRFlow() {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cached == nil || time.Since(l.fetched) > candidatesTTL {
		list, err := l.Pool.Unlanded(context.Background(), l.Slot)
		if err != nil {
			return nil, err
		}
		l.cached, l.fetched = append([]Unlanded{}, list...), time.Now()
	}
	return l.cached, nil
}

// List reads the unlanded work afresh, for the workspaces pane, and keeps it for completion.
func (l *Lander) List() ([]Unlanded, error) {
	if l.PRFlow() {
		return nil, nil
	}
	list, err := l.Pool.Unlanded(context.Background(), l.Slot)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.cached, l.fetched = append([]Unlanded{}, list...), time.Now()
	l.mu.Unlock()
	return list, nil
}

// PRFlow says the repo ships work through pull requests, so nothing lands.
func (l *Lander) PRFlow() bool { return l.Pool.Prepare.PRFlow() }

// StatusScope is what the status line measures this slot's work against.
func (l *Lander) StatusScope() vcs.Scope {
	if !l.PRFlow() {
		return vcs.Scope{Slot: l.Slot.Name, Base: "default@"}
	}
	return vcs.Scope{Slot: l.Slot.Name, Base: "coalesce(present(" + l.Pool.Prepare.TrunkRev() + "), root())", Remote: l.Pool.Prepare.Remote()}
}

// Slug is owner/name of the GitHub repo the pull-request flow pushes to, empty when there is none.
func (l *Lander) Slug() string {
	if !l.PRFlow() {
		return ""
	}
	slug, _ := githubSlug(context.Background(), l.Pool.Origin, l.Pool.Prepare.Remote())
	return slug
}

func (l *Lander) forget() {
	l.mu.Lock()
	l.cached = nil
	l.mu.Unlock()
}

// Command is /land: with no argument the unlanded pool work, with a change id that change landed.
func (l *Lander) Command(arg string) (string, error) {
	if l.PRFlow() {
		return "", ErrPRFlow
	}
	ctx := context.Background()
	if arg = strings.TrimSpace(arg); arg != "" {
		defer l.forget()
		return l.Pool.Land(ctx, l.Slot, arg)
	}
	list, err := l.Pool.Unlanded(ctx, l.Slot)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "Nothing to land: every session's described work is already under your working copy.", nil
	}
	var b strings.Builder
	b.WriteString("Unlanded work from this repo's sessions, newest first. /land <change> puts one under your working copy:\n")
	for _, u := range list {
		fmt.Fprintf(&b, "\n- `%s` %s (%s)%s", u.Change, u.Description, u.Age, withText(u.With))
	}
	return b.String(), nil
}

// withText names the changes that land along with one, empty when none do.
func withText(with []string) string {
	if len(with) == 0 {
		return ""
	}
	return "; lands with " + strings.Join(with, ", ")
}

func (l *Lander) Tools() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name: LandSessionTool,
		Description: anthropic.String("Only when the user asks: land described work from this repo's session workspaces under the " +
			"user's working copy in the default workspace, as commits of their own. With no change it lists the unlanded work, " +
			"newest first. With a change it lands it, stopping for the user's yes; landing this session's own work commits its " +
			"live change first and carries on in a new one, which then needs jj describe -m before the next edit."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"change": str("The change id to land, as the list shows it. Leave it out to list."),
			},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}}
}

func (l *Lander) Handlers() map[string]agent.Handler {
	return map[string]agent.Handler{LandSessionTool: l.run}
}

func (l *Lander) run(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Change string `json:"change"`
	}
	dec := json.NewDecoder(bytes.NewReader(call.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if strings.TrimSpace(in.Change) == "" {
		out, err := l.Command("")
		if err != nil {
			return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
		}
		return anthropic.NewToolResultBlock(call.ID, out, false)
	}
	label := "land " + in.Change + " under your working copy"
	list, err := l.Candidates()
	if err != nil {
		label += "; which changes land with it could not be listed: " + err.Error()
	}
	for _, u := range list {
		if strings.HasPrefix(u.Change, in.Change) || strings.HasPrefix(in.Change, u.Change) {
			label += withText(u.With)
		}
	}
	reason := "landing rewrites your working copy's history"
	if cb.Require == nil {
		return anthropic.NewToolResultBlock(call.ID, "Refused: "+reason+", and there is nobody here to approve it.", true)
	}
	if !cb.Require(ctx, label, "jj rebase -r <its work> --insert-before @ in "+l.Pool.Origin, reason) {
		return anthropic.NewToolResultBlock(call.ID, "The user declined to "+label+"; nothing changed.", true)
	}
	out, err := l.Pool.Land(ctx, l.Slot, in.Change)
	l.forget()
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, "Nothing was landed: "+err.Error(), true)
	}
	return anthropic.NewToolResultBlock(call.ID, out, false)
}
