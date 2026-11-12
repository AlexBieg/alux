// Package procs runs shell commands that outlive the turn that started them. A process is given
// its own group so cancelling takes its children with it, and its output is buffered for a reader
// that arrives later.
package procs

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/awake"
	"github.com/alexbieg/alux/internal/clip"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/shell"
)

// killGrace is how long a signalled group gets to die politely before it is killed outright.
const killGrace = 2 * time.Second

// Proc is one command running in its own process group.
type Proc struct {
	ID      int
	Command string
	Started time.Time
	// Call is the tool call that started it, which is how its dot finds the box in the transcript.
	Call      string
	Sandboxed bool
	Poll      bool
	wrap      *sandbox.Wrap

	out *ring
	// spill holds a command's whole output for Take and Final; a poll has none, and reads the ring.
	spill      *clip.Spill
	head, tail int
	done       chan struct{}
	// signal reaches whatever does the work: a command's process group, or a poll's loop.
	signal func(syscall.Signal)
	// detach frees the agent's live shell from a held command, false once it has finished; nil for a fresh process.
	detach func() bool
	// checked closes once a poll's first check is logged without ending it; unmet, set before, says why.
	checked chan struct{}
	unmet   string

	mu     sync.Mutex
	status string
	// exit is how it ended alone, such as "exit 0", without the shell trailer's directory or notes.
	exit   string
	stop   Stop
	killed bool

	// Guarded by the registry's mu; only a backgrounded, undelivered exit is queued.
	backgrounded bool
	delivered    bool
	// disarm stops the wake Arm set, so arming again replaces it.
	disarm chan struct{}
}

// Start runs command detached from any turn, so ctx must outlive the turn or a backgrounded command dies with it.
// A non-nil w runs it in the sandbox.
func Start(ctx context.Context, command string, w *sandbox.Wrap) (*Proc, error) {
	return StartAt(ctx, command, w, Where{})
}

// Where is the directory and environment a fresh process starts in; zero is the project's, in the base environment.
type Where struct {
	Dir string
	Env []string
}

func (at Where) apply(cmd *exec.Cmd) {
	cmd.Dir = project.Cwd()
	if at.Dir != "" {
		cmd.Dir = at.Dir
	}
	if at.Env != nil {
		cmd.Env = at.Env
	}
}

// StartAt is Start in at's directory and environment.
func StartAt(ctx context.Context, command string, w *sandbox.Wrap, at Where) (*Proc, error) {
	cmd := sandbox.Shell(command, w)
	at.apply(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &ring{}
	spill := clip.NewSpill(takeMaxChars)
	both := io.MultiWriter(out, spill)
	cmd.Stdout, cmd.Stderr = both, both
	// stdin is left nil so commands cannot steal keystrokes from the TUI.
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &Proc{
		Command:   command,
		Started:   time.Now(),
		Sandboxed: w != nil,
		wrap:      w,
		out:       out,
		spill:     spill,
		head:      clip.DefaultHead,
		tail:      clip.DefaultTail,
		done:      make(chan struct{}),
		signal:    func(s syscall.Signal) { _ = syscall.Kill(-cmd.Process.Pid, s) },
	}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.status = describeExit(cmd, err)
		p.exit = p.status
		p.mu.Unlock()
		spill.Close()
		out.finish()
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			p.Kill()
		case <-p.done:
		}
	}()
	return p, nil
}

// Attach runs command in the agent's live shell as a Proc. Holding it hands the shell over with
// the command, so the agent's next call gets a new one.
func Attach(ctx context.Context, sh *shell.Shell, command string, w *sandbox.Wrap) (*Proc, error) {
	out := &ring{}
	spill := clip.NewSpill(takeMaxChars)
	c, err := sh.Start(ctx, command, w, io.MultiWriter(out, spill))
	if err != nil {
		return nil, err
	}
	p := &Proc{
		Command:   command,
		Started:   time.Now(),
		Sandboxed: w != nil,
		wrap:      w,
		out:       out,
		spill:     spill,
		head:      clip.DefaultHead,
		tail:      clip.DefaultTail,
		done:      make(chan struct{}),
		signal:    func(s syscall.Signal) { _ = syscall.Kill(-c.Pid(), s) },
		detach:    c.Detach,
	}
	go func() {
		<-c.Done()
		p.mu.Lock()
		res := c.Result()
		p.status = res.Trailer(sh.Root())
		p.exit = res.Status
		p.mu.Unlock()
		spill.Close()
		out.finish()
		close(p.done)
	}()
	return p, nil
}

func describeExit(cmd *exec.Cmd, err error) string {
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return fmt.Sprintf("exit %d", code)
	}
	if err != nil {
		return "killed by " + fmt.Sprint(cmd.ProcessState.Sys())
	}
	return "exited"
}

// Done closes once the process has been reaped.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Exited reports whether the process is finished.
func (p *Proc) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Status is "running", "killed", or how it ended.
func (p *Proc) Status() string {
	if !p.Exited() {
		return "running"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed {
		return "killed"
	}
	return p.status
}

// Exit is how a finished process ended, as agent.SetExit takes it: "exit 0", "killed", and so on.
func (p *Proc) Exit() string {
	if !p.Exited() {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed {
		return "killed"
	}
	return p.exit
}

// Stopped is how a poll ended; zero while it runs, and for a command.
func (p *Proc) Stopped() Stop {
	if !p.Exited() {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed {
		return StopKilled
	}
	return p.stop
}

// Elapsed is how long the process has been going.
func (p *Proc) Elapsed() time.Duration { return time.Since(p.Started) }

// Silent is how long since the process last wrote anything, counting from its start if it never has.
func (p *Proc) Silent() time.Duration {
	if last := p.out.last(); !last.IsZero() {
		return time.Since(last)
	}
	return time.Since(p.Started)
}

// Kill signals the whole group, so npm and docker grandchildren go too.
func (p *Proc) Kill() {
	if p.Exited() {
		return
	}
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()

	p.signal(syscall.SIGTERM)
	go func() {
		select {
		case <-time.After(killGrace):
			p.signal(syscall.SIGKILL)
		case <-p.done:
		}
	}()
}

// SetLines is how many lines from each end of long output Take and Final keep.
func (p *Proc) SetLines(head, tail int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.head, p.tail = head, tail
}

func (p *Proc) lines() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.head, p.tail
}

// Take is the output written since the last call. A command's is cut to its first and last lines
// with the whole kept in a spill file; a poll's is shaped to the same budget from its buffer, with
// a notice when the buffer overflowed.
func (p *Proc) Take() string {
	out, missed := p.out.take()
	if p.spill != nil {
		head, tail := p.lines()
		return p.spill.Take(head, tail)
	}
	out = clip.Middle(clip.CollapseCRs(out), takeMaxChars)
	if missed > 0 {
		return fmt.Sprintf("[%d earlier bytes dropped — output outran the buffer]\n", missed) + out
	}
	return out
}

// Final is the whole output as the transcript keeps it once the process has exited, with its status.
func (p *Proc) Final() string {
	if p.spill != nil {
		head, tail := p.lines()
		return strings.TrimSpace(strings.TrimSpace(p.spill.Whole(head, tail)) + "\n[" + p.Status() + "]")
	}
	out, dropped := p.out.whole()
	out = clip.Middle(clip.CollapseCRs(out), takeMaxChars)
	if dropped > 0 {
		out = fmt.Sprintf("[%d earlier bytes dropped — output outran the buffer]\n", dropped) + out
	}
	return strings.TrimSpace(strings.TrimSpace(out) + "\n[" + p.Status() + "]")
}

// Buffered is everything still held, without consuming it.
func (p *Proc) Buffered() string { return p.out.all() }

// Tail is the last n lines held, without consuming them: the pane's reader, where Take is the
// agent's.
func (p *Proc) Tail(n int) string { return p.out.tail(n) }

// TailLines is how much of a live process's output rides along on Info. Kept small because Live
// runs on every snapshot, and a snapshot is taken on every change for any reason.
const TailLines = 8

// Info is one process as the pane draws it, read from the snapshot rather than the live registry
// so a mirroring pane sees exactly what this one does.
type Info struct {
	ID      int
	Command string
	Elapsed time.Duration
	// Call is the tool call whose box in the transcript this process belongs to.
	Call string
	// Tail is the last [TailLines] lines of output, which is what that box shows while the
	// process is still going.
	Tail string
	// Foreground is a bash call still inside its time limit: tailed in its box, but not yet a proc.
	Foreground bool
	Poll       bool
}

// Exit is news of a background command as the agent is told about it: its exit, or a wake it asked for.
type Exit struct {
	ID      int
	Command string
	Status  string
	Output  string
	// Event is empty for an exit; a wake leaves the process running.
	Event Event
	// Matched is the line an EventMatched wake matched.
	Matched string
	Elapsed time.Duration
	Silent  time.Duration
	// Next is how soon a repeating check-in comes round again; zero when the wake is spent.
	Next time.Duration
}

// Event is why a still-running process woke the agent.
type Event string

const (
	EventMatched Event = "matched"
	EventCheckIn Event = "check-in"
)

// Message is one piece of news as the agent reads it.
func (e Exit) Message() string {
	var head string
	switch {
	case e.Event == EventMatched && e.Next > 0:
		head = fmt.Sprintf("proc %d (%s) — until matched %s after %s; still running. Check-ins carry on every %s "+
			"until it exits. background_proc(%d, check_in) replaces them, kill_shell(%d) stops it", e.ID, e.Command,
			strconv.Quote(clip.Middle(e.Matched, 300)), short(e.Elapsed), short(e.Next), e.ID, e.ID)
	case e.Event == EventMatched:
		head = fmt.Sprintf("proc %d (%s) — until matched %s after %s; still running. You will be told when it exits. "+
			"background_proc(%d, check_in) sets a new wake, kill_shell(%d) stops it", e.ID, e.Command,
			strconv.Quote(clip.Middle(e.Matched, 300)), short(e.Elapsed), e.ID, e.ID)
	case e.Event == EventCheckIn && e.Next > 0:
		head = fmt.Sprintf("proc %d (%s) — check-in: still running after %s, last output %s ago. The next check-in "+
			"comes in %s with no call from you; background_proc(%d, check_in) replaces it, kill_shell(%d) if it looks "+
			"stuck", e.ID, e.Command, short(e.Elapsed), short(e.Silent), short(e.Next), e.ID, e.ID)
	case e.Event == EventCheckIn:
		head = fmt.Sprintf("proc %d (%s) — check-in: still running after %s, last output %s ago. Its wake is spent: "+
			"background_proc(%d, check_in) to keep it running and set the next one, kill_shell(%d) if it looks stuck",
			e.ID, e.Command, short(e.Elapsed), short(e.Silent), e.ID, e.ID)
	default:
		head = fmt.Sprintf("proc %d (%s) — %s", e.ID, e.Command, e.Status)
	}
	if strings.TrimSpace(e.Output) == "" {
		return head + ", no further output"
	}
	return head + ":\n" + strings.TrimSpace(e.Output)
}

// ExitBlock is how background news is put to the agent, in one framing wherever it arrives.
func ExitBlock(exits []Exit) string {
	if len(exits) == 0 {
		return ""
	}
	messages := make([]string, 0, len(exits))
	for _, e := range exits {
		messages = append(messages, e.Message())
	}
	return "<background-commands>\nNews of background commands you started: an exit, or a wake you set.\n\n" +
		strings.Join(messages, "\n\n") + "\n</background-commands>"
}

// Wake wakes the agent about a running backgrounded process at Until's first match or CheckIn, and every CheckIn with Repeat.
type Wake struct {
	CheckIn time.Duration
	Until   *regexp.Regexp
	Repeat  bool
}

// Head is the wake as a box's head shows it.
func (w Wake) Head() string {
	head := "check-in " + brief(w.CheckIn)
	if w.Repeat {
		head = "check-in every " + brief(w.CheckIn)
	}
	if w.Until != nil {
		head += " or /" + w.Until.String() + "/"
	}
	return head
}

// Arm sets p's wake, replacing any it had. It reports false if p is not backgrounded or has exited.
func (r *Registry) Arm(p *Proc, w Wake) bool {
	r.mu.Lock()
	if !p.backgrounded || p.Exited() {
		r.mu.Unlock()
		return false
	}
	if p.disarm != nil {
		close(p.disarm)
	}
	stop := make(chan struct{})
	p.disarm = stop
	gen := r.generation
	r.mu.Unlock()

	var hit <-chan string
	var unwatch func()
	if w.Until != nil {
		lw, stopWatch := p.out.watch(w.Until)
		hit, unwatch = lw.hit, stopWatch
	}
	go func() {
		if unwatch != nil {
			defer unwatch()
		}
		timer := time.NewTicker(w.CheckIn)
		defer timer.Stop()
		var next time.Duration
		if w.Repeat {
			next = w.CheckIn
		}
		for {
			select {
			case <-p.done:
				return
			case <-stop:
				return
			case line := <-hit:
				hit = nil
				if !r.wake(p, gen, stop, Exit{Event: EventMatched, Matched: line, Next: next}) || !w.Repeat {
					return
				}
			case <-timer.C:
				if !r.wake(p, gen, stop, Exit{Event: EventCheckIn, Next: next}) || !w.Repeat {
					return
				}
			}
		}
	}()
	return true
}

// wake queues news of a running process and reports whether it did: not once cleared, exited, or re-armed.
func (r *Registry) wake(p *Proc, gen int, stop chan struct{}, e Exit) bool {
	r.mu.Lock()
	if gen != r.generation || p.Exited() || p.disarm != stop {
		r.mu.Unlock()
		return false
	}
	if e.Next == 0 {
		p.disarm = nil
	}
	e.ID, e.Command, e.Status = p.ID, p.Command, "running"
	e.Output, e.Elapsed, e.Silent = p.Take(), p.Elapsed(), p.Silent()
	r.queueWake(e)
	r.mu.Unlock()
	if r.OnExit != nil {
		r.OnExit()
	}
	return true
}

// queueWake folds a wake into one the agent has not read yet for the same process; called under r.mu.
func (r *Registry) queueWake(e Exit) {
	for i, was := range r.exits {
		if was.ID != e.ID || was.Event == "" {
			continue
		}
		e.Output = was.Output + e.Output
		if was.Event == EventMatched && e.Event == EventCheckIn {
			e.Event, e.Matched = EventMatched, was.Matched
		}
		r.exits[i] = e
		return
	}
	r.exits = append(r.exits, e)
}

// Backgrounded reports whether any backgrounded process is still running, so its news is still to come.
func (r *Registry) Backgrounded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.procs {
		if p.backgrounded && !p.Exited() {
			return true
		}
	}
	return false
}

// Registry is every background process of one session: the sibling of the subagent roster, with
// the same queued results, waker, and generation that a cleared conversation bumps.
type Registry struct {
	mu         sync.Mutex
	procs      map[int]*Proc
	foreground map[string]*Proc
	nextID     int
	exits      []Exit
	generation int
	// checkInUnit is how long one second of check_in lasts; zero is a second.
	checkInUnit time.Duration

	// OnExit fires once a process has queued its exit, for a session that wants to go and read it
	// rather than wait to be asked. It must not block.
	OnExit func()

	// OnChange fires whenever the set of live processes changes, so a pane drawing them redraws.
	// The screen's channel where OnExit is the model's. It must not block.
	OnChange func()

	// OnFinish hands a registered process's final output to the box of the call that started it.
	OnFinish func(call, output string)

	// OnPollEnd reports how a registered poll ended, by its id.
	OnPollEnd func(id int, how Stop)

	// Awake is held for every process meant to finish, until it exits; nil holds nothing.
	Awake *awake.Keeper

	shutdownOnce sync.Once
	shutdown     context.Context
	stop         context.CancelFunc
}

func NewRegistry() *Registry {
	return &Registry{procs: map[int]*Proc{}, foreground: map[string]*Proc{}}
}

// keepAwake counts p as work in flight until it exits.
func (r *Registry) keepAwake(p *Proc) {
	h := r.Awake.Hold()
	go func() {
		<-p.done
		h.Release()
	}()
}

func (r *Registry) watch(call string, p *Proc) {
	r.mu.Lock()
	r.foreground[call] = p
	r.mu.Unlock()
	r.changed()
}

func (r *Registry) unwatch(call string) {
	r.mu.Lock()
	delete(r.foreground, call)
	r.mu.Unlock()
	r.changed()
}

// Context is what every process runs under: live for the whole session, cancelled when it ends.
// On a turn's context instead, an adopted process would die with the turn that handed it over.
func (r *Registry) Context() context.Context {
	r.shutdownOnce.Do(func() {
		r.shutdown, r.stop = context.WithCancel(context.Background())
	})
	return r.shutdown
}

// Adopt registers a running process whose exit wakes the session, and returns its id.
func (r *Registry) Adopt(p *Proc, call string) int { return r.register(p, call, true) }

// Hold registers a running process that only a wait reports on, and that KillHeld stops.
func (r *Registry) Hold(p *Proc, call string) int { return r.register(p, call, false) }

func (r *Registry) register(p *Proc, call string, backgrounded bool) int {
	r.mu.Lock()
	r.nextID++
	p.ID = r.nextID
	p.Call = call
	p.backgrounded = backgrounded
	r.procs[p.ID] = p
	gen := r.generation
	r.mu.Unlock()

	r.changed()
	go r.collect(p, gen)
	return p.ID
}

// Background moves a held process to the background, or reports false if it already exited.
func (r *Registry) Background(p *Proc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.Exited() {
		return false
	}
	p.backgrounded = true
	return true
}

// Claim makes the caller report p's exit, returning output an already queued exit had taken.
func (r *Registry) Claim(p *Proc) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.delivered = true
	for i, e := range r.exits {
		if e.ID == p.ID {
			r.exits = slices.Delete(r.exits, i, i+1)
			return e.Output
		}
	}
	return ""
}

// KillHeld stops every held process still running and returns them.
func (r *Registry) KillHeld() []*Proc {
	var held []*Proc
	r.mu.Lock()
	for _, p := range r.procs {
		if !p.backgrounded && !p.Exited() {
			held = append(held, p)
		}
	}
	r.mu.Unlock()
	slices.SortFunc(held, func(a, b *Proc) int { return a.ID - b.ID })
	for _, p := range held {
		p.Kill()
	}
	return held
}

func (r *Registry) changed() {
	if r.OnChange != nil {
		r.OnChange()
	}
}

// collect queues the exit once the process ends. The generation is checked under the same lock as
// the append, so a conversation cleared mid-exit either keeps the report or refuses it, never both.
func (r *Registry) collect(p *Proc, gen int) {
	<-p.done
	r.mu.Lock()
	report := gen == r.generation && p.backgrounded && !p.delivered
	if report {
		r.exits = append(r.exits, Exit{ID: p.ID, Command: p.Command, Status: p.Status(), Output: p.Take()})
	}
	r.mu.Unlock()
	if r.OnFinish != nil {
		r.OnFinish(p.Call, p.Final())
	}
	if p.Poll && r.OnPollEnd != nil {
		r.OnPollEnd(p.ID, p.Stopped())
	}
	// Announced even when the report is withheld: the process left the row either way.
	r.changed()
	if !report || r.OnExit == nil {
		return
	}
	r.OnExit()
}

// Get resolves an id the agent gave back.
func (r *Registry) Get(id int) (*Proc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.procs[id]
	return p, ok
}

// PollOf is the id of the registered poll a tool call started, or 0.
func (r *Registry) PollOf(call string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.procs {
		if p.Poll && p.Call == call {
			return id
		}
	}
	return 0
}

// TakeExits drains what has finished. Destructive: the agent should hear about a process once.
func (r *Registry) TakeExits() []Exit {
	r.mu.Lock()
	defer r.mu.Unlock()
	exits := r.exits
	r.exits = nil
	return exits
}

// DiscardExits drops queued exits and stops live processes reporting, without stopping them.
func (r *Registry) DiscardExits() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exits = nil
	r.generation++
}

// Running is the processes still going, oldest first.
func (r *Registry) Running() []*Proc {
	r.mu.Lock()
	ids := make([]int, 0, len(r.procs))
	for id := range r.procs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	procs := make([]*Proc, 0, len(ids))
	for _, id := range ids {
		if p := r.procs[id]; !p.Exited() {
			procs = append(procs, p)
		}
	}
	r.mu.Unlock()
	return procs
}

// Live is the running processes as the pane draws them, oldest first, then the foreground bash calls.
func (r *Registry) Live() []Info {
	running := r.Running()
	out := make([]Info, 0, len(running))
	for _, p := range running {
		out = append(out, Info{
			ID: p.ID, Command: p.Command, Elapsed: p.Elapsed(), Call: p.Call, Tail: p.Tail(TailLines), Poll: p.Poll,
		})
	}
	r.mu.Lock()
	calls := make([]string, 0, len(r.foreground))
	for call := range r.foreground {
		calls = append(calls, call)
	}
	slices.Sort(calls)
	for _, call := range calls {
		p := r.foreground[call]
		out = append(out, Info{
			Command: p.Command, Elapsed: p.Elapsed(), Call: call, Tail: p.Tail(TailLines), Foreground: true, Poll: p.Poll,
		})
	}
	r.mu.Unlock()
	return out
}

// KillAll stops every live process and reports how many it signalled. Called on quit: waiting is
// wrong for a process, since a dev server never exits on its own.
func (r *Registry) KillAll() int {
	n := 0
	for _, p := range r.Running() {
		p.Kill()
		n++
	}
	// Through Context so the once has run: stop is written under it.
	_ = r.Context()
	r.stop()
	return n
}
