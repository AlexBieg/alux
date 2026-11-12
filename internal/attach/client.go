package attach

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/ui"
)

// callTimeout bounds a quick op, which the TUI makes from its event loop; model calls wait on their context instead.
const callTimeout = 5 * time.Second

// ErrDetached is what every call returns once the server has gone.
var ErrDetached = errors.New("detached from the session")

// Client is a TUI's end of an attached session. Snapshot reads a local mirror the server keeps current.
type Client struct {
	c  net.Conn
	ev Events

	wmu sync.Mutex
	enc *json.Encoder

	mu      sync.Mutex
	snap    session.Snapshot
	nextID  int
	waiting map[int]chan frame
	viewing bool
	done    chan struct{}
	// pane is what the pane last reported, which a viewer holds back until it takes the session.
	pane  *Pane
	hello Hello
}

// Events are what the session tells the attached TUI unasked; any may be nil. Each is called from
// the client's own goroutine, so none may block on the caller of a Client method, whose reply comes
// through that same goroutine.
type Events struct {
	// Changed follows every snapshot, and once more on detaching.
	Changed func()
	Notice  func(string)
	PRs     func([]prs.RepoState)
	Review  func(reviewstore.Review)
	// Guardian is guardian's startup check, nil when it is ready.
	Guardian func(error)
	// Demoted follows another TUI taking the session from this one, which stays attached as a viewer.
	Demoted func()
}

// Dial attaches to the session at path, returning once its first snapshot has arrived.
func Dial(path string, ev Events) (*Client, error) {
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	c := &Client{c: nc, ev: ev, enc: json.NewEncoder(nc), waiting: map[int]chan frame{}, done: make(chan struct{})}
	first := make(chan struct{})
	go c.read(first)
	select {
	case <-first:
		return c, nil
	case <-c.done:
		return nil, ErrDetached
	case <-time.After(callTimeout):
		nc.Close()
		return nil, errors.New("the session sent no snapshot")
	}
}

// Done is closed when the client stops being attached.
func (c *Client) Done() <-chan struct{} { return c.done }

// Viewing reports whether another TUI drives the session; this one takes it on its first call that changes the session.
func (c *Client) Viewing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.viewing
}

// Close detaches without stopping the session.
func (c *Client) Close() error { return c.c.Close() }

func (c *Client) read(first chan struct{}) {
	defer func() {
		c.mu.Lock()
		close(c.done)
		for id, ch := range c.waiting {
			close(ch)
			delete(c.waiting, id)
		}
		c.mu.Unlock()
		c.c.Close()
		if c.ev.Changed != nil {
			c.ev.Changed()
		}
	}()
	sc := bufio.NewScanner(c.c)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		var f frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			return
		}
		switch {
		case f.Viewing != nil:
			c.mu.Lock()
			demoted := *f.Viewing && !c.viewing && first == nil
			c.viewing = *f.Viewing
			c.mu.Unlock()
			if demoted && c.ev.Demoted != nil {
				c.ev.Demoted()
			}
		case f.Hello != nil:
			c.mu.Lock()
			c.hello = *f.Hello
			c.mu.Unlock()
		case f.Notice != "":
			if c.ev.Notice != nil {
				c.ev.Notice(f.Notice)
			}
		case f.PRs != nil:
			if c.ev.PRs != nil {
				c.ev.PRs(f.PRs.states())
			}
		case f.Review != nil:
			if c.ev.Review != nil {
				c.ev.Review(*f.Review)
			}
		case f.Guardian != nil:
			if c.ev.Guardian != nil {
				var err error
				if f.Guardian.Err != "" {
					err = errors.New(f.Guardian.Err)
				}
				c.ev.Guardian(err)
			}
		case f.Snap != nil:
			c.mu.Lock()
			items := c.snap.Items
			if f.From > len(items) {
				c.mu.Unlock()
				return
			}
			snap := *f.Snap
			snap.Items = append(items[:f.From:f.From], f.Snap.Items...)
			c.snap = snap
			c.mu.Unlock()
			if first != nil {
				close(first)
				first = nil
			}
			if c.ev.Changed != nil {
				c.ev.Changed()
			}
		case f.Reply != 0:
			c.mu.Lock()
			ch := c.waiting[f.Reply]
			delete(c.waiting, f.Reply)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		}
	}
}

// call sends op and waits for its reply, decoding the result into out when out is not nil.
func (c *Client) call(ctx context.Context, op string, a args, out any) error {
	ch := make(chan frame, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return ErrDetached
	default:
	}
	c.nextID++
	id := c.nextID
	c.waiting[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}

	c.wmu.Lock()
	err := c.enc.Encode(request{ID: id, Op: op, Args: a})
	c.wmu.Unlock()
	if err != nil {
		forget()
		return err
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return ErrDetached
		}
		if f.Err != "" {
			return errors.New(f.Err)
		}
		if out != nil && len(f.Result) > 0 {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	case <-ctx.Done():
		forget()
		return ctx.Err()
	}
}

func (c *Client) quick(op string, a args, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.call(ctx, op, a, out)
}

// drive makes a call that changes the session, taking it first when viewing. A refusal means another
// TUI took it between the two, so it takes again once.
func (c *Client) drive(ctx context.Context, op string, a args, out any) error {
	if c.Viewing() {
		if err := c.take(ctx); err != nil {
			return err
		}
	}
	err := c.call(ctx, op, a, out)
	if err == nil || err.Error() != errViewing {
		return err
	}
	if err := c.take(ctx); err != nil {
		return err
	}
	return c.call(ctx, op, a, out)
}

func (c *Client) quickDrive(op string, a args, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.drive(ctx, op, a, out)
}

// take makes this TUI the driver and hands the session the pane it held back while viewing.
func (c *Client) take(ctx context.Context) error {
	if err := c.call(ctx, opTake, args{}, nil); err != nil {
		return err
	}
	c.mu.Lock()
	var pane *Pane
	if c.pane != nil {
		p := *c.pane
		pane = &p
	}
	c.mu.Unlock()
	if pane != nil {
		c.call(ctx, opPane, args{Pane: pane}, nil)
	}
	return nil
}

func (c *Client) Snapshot() session.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.snap
	snap.Items = snap.Items[:len(snap.Items):len(snap.Items)]
	return snap
}

func (c *Client) LiveProcs() []procs.Info {
	var out []procs.Info
	if c.quick(opLiveProcs, args{}, &out) != nil {
		return c.Snapshot().Procs
	}
	return out
}

func (c *Client) FileDiff(path string) (string, error) {
	var out string
	err := c.quick(opFileDiff, args{Path: path}, &out)
	return out, err
}

func (c *Client) Submit(text string)     { c.quickDrive(opSubmit, args{Text: text}, nil) }
func (c *Client) Show(text string)       { c.quickDrive(opShow, args{Text: text}, nil) }
func (c *Client) Steer(id int)           { c.quickDrive(opSteer, args{ID: id}, nil) }
func (c *Client) Cancel()                { c.quickDrive(opCancel, args{}, nil) }
func (c *Client) SetMode(m session.Mode) { c.quickDrive(opSetMode, args{Mode: m}, nil) }

func (c *Client) Unqueue(id int) string {
	var out string
	c.quickDrive(opUnqueue, args{ID: id}, &out)
	return out
}

func (c *Client) DropQueued() int {
	var out int
	c.quickDrive(opDropQueued, args{}, &out)
	return out
}

func (c *Client) RunSkill(name string, inputs map[string]any) {
	c.quickDrive(opRunSkill, args{Name: name, Inputs: inputs}, nil)
}

func (c *Client) ApproveID(seq int, okay bool) bool {
	var out bool
	c.quickDrive(opApprove, args{ID: seq, Okay: okay}, &out)
	return out
}

func (c *Client) AnswerID(seq int, a *ask.Answer) bool {
	var out bool
	c.quickDrive(opAnswer, args{ID: seq, Answer: a}, &out)
	return out
}

// Hello is what the session said on attach.
func (c *Client) Hello() Hello {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// Land is the session's lander, nil when it has none.
func (c *Client) Land() ui.LandSource {
	if !c.Hello().Land {
		return nil
	}
	return lander{c}
}

// lander waits on each reply however long: every call runs off the TUI's event loop, and a push or a rebase fetches.
type lander struct{ c *Client }

func (l lander) do(op string, a args, out any) error {
	return l.c.call(context.Background(), op, a, out)
}

func (l lander) List() ([]orchestrator.Unlanded, error) {
	var out []orchestrator.Unlanded
	err := l.do(opLandList, args{}, &out)
	return out, err
}

func (l lander) Workspaces() (orchestrator.RepoView, error) {
	var out orchestrator.RepoView
	err := l.do(opLandWorkspaces, args{}, &out)
	return out, err
}

func (l lander) text(op string, a args) (string, error) {
	var out string
	err := l.do(op, a, &out)
	return out, err
}

func (l lander) Stat(change string) (string, error) { return l.text(opLandStat, args{Change: change}) }
func (l lander) Diff(change string) (string, error) { return l.text(opLandDiff, args{Change: change}) }

func (l lander) PRFlow() bool { return l.c.Hello().PRFlow }

// LandCandidates is what /land offers to complete; quick, since completion runs on the TUI's event loop.
func (c *Client) LandCandidates() ([]orchestrator.Unlanded, error) {
	var out []orchestrator.Unlanded
	err := c.quick(opLandCandidates, args{}, &out)
	return out, err
}

func (l lander) Push(workspace, change string) (string, error) {
	var out string
	err := l.c.drive(context.Background(), opLandPush, args{Workspace: workspace, Change: change}, &out)
	return out, err
}

func (l lander) Rebase(workspace string) (string, error) {
	var out string
	err := l.c.drive(context.Background(), opLandRebase, args{Workspace: workspace}, &out)
	return out, err
}

func (l lander) Sweep() ([]string, map[string]orchestrator.Held, error) {
	var out sweepResult
	err := l.c.drive(context.Background(), opLandSweep, args{}, &out)
	return out.Said, out.Held, err
}

// PRs is the session's poller, nil when it tracks no pull requests.
func (c *Client) PRs() ui.PRSource {
	if !c.Hello().PRs {
		return nil
	}
	return poller{c}
}

// poller passes each call's context on as its wait for the reply; the server bounds the call itself.
type poller struct{ c *Client }

func (p poller) Detail(ctx context.Context, repo string, number int) (string, error) {
	var out string
	err := p.c.call(ctx, opPRDetail, args{Repo: repo, Number: number}, &out)
	return out, err
}

func (p poller) Files(ctx context.Context, repo string, number int) ([]string, error) {
	var out []string
	err := p.c.call(ctx, opPRFiles, args{Repo: repo, Number: number}, &out)
	return out, err
}

func (p poller) ReviewApp(ctx context.Context, repo string, number int) (prs.ReviewApp, error) {
	var out prs.ReviewApp
	err := p.c.call(ctx, opPRReviewApp, args{Repo: repo, Number: number}, &out)
	return out, err
}

func (p poller) AddLabel(ctx context.Context, repo string, number int, label string) error {
	return p.c.drive(ctx, opPRAddLabel, args{Repo: repo, Number: number, Label: label}, nil)
}

func (p poller) MergeReady(ctx context.Context, repo string, number int) (prs.MergeCheck, error) {
	var out prs.MergeCheck
	err := p.c.call(ctx, opPRMergeReady, args{Repo: repo, Number: number}, &out)
	return out, err
}

func (p poller) AddMergeLabel(ctx context.Context, repo string, number int) (string, error) {
	var out string
	err := p.c.drive(ctx, opPRAddMergeLabel, args{Repo: repo, Number: number}, &out)
	return out, err
}

func (p poller) Sync(ctx context.Context, force bool) {
	p.c.call(ctx, opPRSync, args{Force: force}, nil)
}

// LookupName is a resolve.Resolver's Remote: the session's name for id, "" when it has none or the wait ran out.
func (c *Client) LookupName(ctx context.Context, id string) string {
	var out string
	if c.call(ctx, opName, args{Text: id}, &out) != nil {
		return ""
	}
	return out
}

// Publish is a registry.Attached forward: it sends the session the fields only its pane knows, when they
// changed. It runs unasked, so a viewer holds them back rather than take the session.
func (c *Client) Publish(p registry.Patch) {
	c.mu.Lock()
	pane := Pane{}
	if c.pane != nil {
		pane = *c.pane
	}
	if p.Unseen != nil {
		pane.Unseen = *p.Unseen
	}
	if p.Bookmark != nil {
		pane.Bookmark = *p.Bookmark
	}
	if p.Change != nil {
		pane.Change = *p.Change
	}
	if c.pane != nil && *c.pane == pane {
		c.mu.Unlock()
		return
	}
	c.pane = &pane
	viewing := c.viewing
	c.mu.Unlock()
	if !viewing {
		c.quick(opPane, args{Pane: &pane}, nil)
	}
}

// Recap runs on the pane's return to focus, unasked, so a viewer skips it rather than take the session.
func (c *Client) Recap(ctx context.Context) error {
	if c.Viewing() {
		return nil
	}
	return c.call(ctx, opRecap, args{}, nil)
}

func (c *Client) FindingChat(dir string, brief subagent.FindingBrief) (session.Chat, error) {
	var id int
	if err := c.quickDrive(opChatOpen, args{Path: dir, Brief: &brief}, &id); err != nil {
		return nil, err
	}
	return &chat{c: c, id: id}, nil
}

// chat is a finding chat held by the server; Model is the one its last reply named.
type chat struct {
	c  *Client
	id int

	mu    sync.Mutex
	model string
}

func (ch *chat) Send(ctx context.Context, text string) (string, error) {
	var out chatReply
	if err := ch.c.drive(ctx, opChatSend, args{ID: ch.id, Text: text}, &out); err != nil {
		return "", err
	}
	ch.mu.Lock()
	ch.model = out.Model
	ch.mu.Unlock()
	return out.Reply, nil
}

func (ch *chat) Model() string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.model
}
