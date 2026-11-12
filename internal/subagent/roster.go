package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/awake"
	"github.com/alexbieg/alux/internal/skills"
)

// The roster is the answer to "how many subagents are running and what are they doing". It is
// read by the dot row, by /agents and by the transcript box each run owns, so it carries enough
// for all three: what kind of agent, what it was asked, where it is against its budget, and what
// it is doing right now.

type State string

const (
	Running State = "running"
	// Waiting is a run parked on an approval. Nothing produces it in the read-only agents that
	// ship today; it exists because an editing subagent will, and the alternative — parking on
	// the session's single approval channel — deadlocks the moment two runs need it at once.
	Waiting State = "waiting"
	// Idle is a run that exists but is not working: a pair's navigator between consultations,
	// which is live for as long as its driver is and costs nothing in between. A state rather
	// than an absence because the row is what tells the user a pair is running, and a navigator
	// that vanished whenever it stopped talking would flicker once per consultation.
	Idle   State = "idle"
	Done   State = "done"
	Failed State = "failed"
)

// kept bounds the roster's history. /agents wants more than the live runs, but a long session
// should not accumulate every dispatch it ever made.
const kept = 50

// Run is one dispatch, live or finished.
type Run struct {
	ID int
	// Dispatch is the ID its dispatch record and transcript are logged under. A pair's navigator
	// has a transcript under its driver's ID plus its agent name, and no dispatch record.
	Dispatch string
	// Call is the tool call that dispatched it, empty when it was dispatched some other way.
	Call  string
	Agent string
	// Model is what the run was sent to, empty until the run starts.
	Model string
	// Tier is guardian's size for the task when routing chose Model, empty otherwise.
	Tier string
	// Name is the short label the run is known by. A pair's navigator shares its driver's.
	Name string
	// Task is the one-line form, which a run with no Name is labelled from.
	Task     string
	State    State
	Step     int
	MaxSteps int
	// Extensions is how many extra budgets the run has been granted.
	Extensions int
	// Activity is the label of the tool the run is in, so the box says "reading foo.go" rather
	// than only counting steps.
	Activity string
	Started  time.Time
	Ended    time.Time
	// LastStep is when the run last began a step, which is how a stuck run is told from a slow one.
	LastStep time.Time
	// LastActive is when a tool call last finished.
	LastActive time.Time
	Result     string
	Err        string
	// Trail is what the run did, one entry per tool call the definition thought worth keeping.
	Trail []Entry
	// Background runs report into the transcript rather than into the tool result that started
	// them, which is what makes their outcome worth queueing for the next turn.
	Background bool
	// Seen marks a failure the user has already been shown. Unseen failures hold their dot in
	// the top bar; everything else leaves the row the moment it ends.
	Seen bool
	// Parent is the run this one belongs to, or 0 when it stands alone. A pair's navigator is
	// the only thing that sets it: it is not a dispatch of its own and cannot be started or
	// cancelled on its own, so everything that draws it draws it as hanging off its driver.
	Parent int
	// Kept is the files of the run's change while it waits for land_run, empty once landed or dropped.
	Kept []string
}

func (r Run) Live() bool { return r.State == Running || r.State == Waiting || r.State == Idle }

// Elapsed is how long the run took, or has been going.
func (r Run) Elapsed() time.Duration {
	if r.Ended.IsZero() {
		return time.Since(r.Started)
	}
	return r.Ended.Sub(r.Started)
}

// Headline is the one line a run is known by, in the transcript box that owns it and in
// /agents. It is rebuilt from live state on every render, so a box's head tracks its run rather
// than freezing at whatever was true when the box was created.
func (r Run) Headline() string {
	lead, activity, tail := r.HeadParts()
	if activity != "" {
		lead += " · " + activity
	}
	if tail != "" {
		lead += " " + tail
	}
	return lead
}

// HeadParts is the headline in pieces, so a live head can shorten its activity alone.
func (r Run) HeadParts() (lead, activity, tail string) {
	lead = r.Agent
	if label := r.Label(); label != "" {
		lead = label + " · " + r.Agent
	}
	if r.Model != "" {
		lead += " · " + shortModel(r.Model)
		if r.Tier != "" {
			lead += " (" + r.Tier + ")"
		}
	}
	switch r.State {
	case Idle:
		// Counted rather than timed. A navigator's elapsed time is its driver's, which says
		// nothing about it; what the user wants to know is how much of the conversation is left.
		if r.Step == 0 {
			return lead + " · not yet consulted", "", ""
		}
		return fmt.Sprintf("%s · consulted %d of %d", lead, r.Step, r.MaxSteps), "", ""
	case Running, Waiting:
		if r.State == Waiting {
			lead += " · waiting"
		}
		activity = r.Activity
		if activity == "" {
			activity = "thinking"
		}
		return lead, activity, fmt.Sprintf("(%d/%d)", r.Step, r.MaxSteps)
	case Failed:
		return lead + " · failed — " + firstLine(r.Err), "", ""
	default:
		return fmt.Sprintf("%s · done, %s", lead, short(r.Elapsed())), "", ""
	}
}

// Label is the run's name, or the first words of its task when it was given none.
func (r Run) Label() string {
	if r.Name != "" {
		return r.Name
	}
	return runName("", r.Task)
}

const maxName = 32

func runName(name, task string) string {
	if name = firstLine(name); name != "" {
		if runes := []rune(name); len(runes) > maxName {
			return string(runes[:maxName-1]) + "…"
		}
		return name
	}
	words := strings.Fields(firstLine(task))
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.TrimRight(strings.Join(words, " "), ".,:;…")
}

// short is a duration a glance can read: a run's exact milliseconds are noise.
func short(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Outcome is a finished background run as the main agent is told about it.
//
// Summary and Report are both kept because they go to different places. Summary is one line, for
// the notice strip, where a paragraph would not fit. Report is everything the run said, which is
// the whole product of a detached research agent — reducing that to its first line, as the strip
// does, would hand back a headline and throw away the findings.
type Outcome struct {
	Dispatch string
	Agent    string
	Summary  string
	Report   string
	Err      string
}

// Message is the outcome as the agent reads it. A failure or an empty report is one line because
// there is nothing else to say; anything else is reproduced whole.
func (o Outcome) Message() string {
	switch {
	case o.Err != "":
		return o.Agent + ": failed — " + o.Err
	case strings.TrimSpace(o.Report) == "" || strings.TrimSpace(o.Report) == none:
		return o.Agent + ": nothing to add"
	default:
		return o.Agent + ":\n" + strings.TrimSpace(o.Report)
	}
}

// OutcomeBlock is how a batch of finished runs is put to the main agent. One shape wherever they
// arrive — folded into the next prompt, injected mid-turn, or carried by a turn alux
// started itself — because the agent should not have to read three framings of the same event.
//
// The wording names no arrival moment for that reason: "since your last turn" is a lie to a turn
// that is still running.
func OutcomeBlock(outcomes []Outcome) string {
	if len(outcomes) == 0 {
		return ""
	}
	messages := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		messages = append(messages, o.Message())
	}
	return "<background-agents>\nThese background runs you dispatched have finished.\n\n" +
		strings.Join(messages, "\n\n") + "\n</background-agents>"
}

// Roster holds every run of this session. One per session, handed to the manager.
type Roster struct {
	mu     sync.Mutex
	runs   []*Run
	nextID int
	notify func()

	// outcomes are finished background runs the main agent has not been told about. A filing that
	// failed would otherwise announce itself only into a transcript the agent has stopped reading;
	// draining these into the next turn is what makes it something the agent can act on.
	outcomes []Outcome

	// OnOutcome fires once a run has queued its outcome, for a session that wants to go and read
	// it rather than wait to be asked. Distinct from notify, which fires on every step of every
	// run: a waker hung off that would fire thousands of times a run to no purpose.
	//
	// It must not block. Set before the first dispatch and not afterwards.
	OnOutcome func()

	// generation is which conversation the runs belong to, bumped when one is cleared. A run
	// carries the generation it was dispatched in and reports into no other, so a report cannot
	// arrive in a conversation that has no memory of asking for it.
	generation int

	// kept holds finished layered runs' changes, by run id, until they are landed or dropped.
	kept map[int]Work

	// wg tracks background runs so quitting can wait for them, the way filings already do.
	wg sync.WaitGroup

	// shutdown is the context every detached run is given. Cancelled once, by Drain, when the
	// session is going away — never by a turn ending, which is the whole point of detaching.
	// Built lazily so a zero Roster is still usable.
	shutdownOnce sync.Once
	shutdown     context.Context
	stop         context.CancelFunc

	// stops cancels one run, keyed by run id, for as long as that run is executing.
	stops map[int]context.CancelCauseFunc

	// requests are what detached runs are blocked on the main agent for, one at most per run.
	requests map[int]*pendingRequest

	// OnCheckIn wakes an idle session for a check-in alert, at most once per checkInWakeGap.
	OnCheckIn func()
	watches   map[int]*watch
	checkins  []CheckIn
	lastWake  time.Time
	watching  sync.Once

	// Awake is held while any run is working; nil holds nothing. Set before the first dispatch.
	Awake *awake.Keeper
}

// errStopped marks a cancel by Stop, the only cancel that earns a closing report.
var errStopped = errors.New("stopped by the main agent")

// Stop cancels one live run. It finishes through its closing report rather than vanishing.
func (r *Roster) Stop(id int) error {
	r.mu.Lock()
	var run *Run
	for _, candidate := range r.runs {
		if candidate.ID == id {
			run = candidate
		}
	}
	stop := r.stops[id]
	switch {
	case run == nil:
		r.mu.Unlock()
		return fmt.Errorf("no run %d", id)
	case run.Parent != 0:
		r.mu.Unlock()
		return fmt.Errorf("run %d is a navigator; stop its driver, run %d", id, run.Parent)
	case !run.Live():
		r.mu.Unlock()
		return fmt.Errorf("run %d has already ended (%s)", id, run.State)
	case stop == nil:
		r.mu.Unlock()
		return fmt.Errorf("run %d cannot be stopped yet; it has not started executing", id)
	}
	run.Activity = "stopping"
	r.mu.Unlock()
	stop(errStopped)
	r.changed()
	return nil
}

// Context is what a detached run runs under: live for the whole session, cancelled when it ends.
//
// A detached run needs a context of its own that can still be cancelled; context.Background()
// cannot be, and that is survivable only for an agent like the vault filer, which takes a lock
// no other subagent wants. Agents that queue behind a per-agent lock detach by default, and
// without a cancellable context a queued run waits on a slot it may never get while the quit
// path waits out its whole timeout.
func (r *Roster) Context() context.Context {
	r.shutdownOnce.Do(func() {
		r.shutdown, r.stop = context.WithCancel(context.Background())
	})
	return r.shutdown
}

// NewRoster builds one. notify is called whenever anything changes; it must not block and must
// be safe from any goroutine. Nil is fine — the roster is then only read by polling.
func NewRoster(notify func()) *Roster {
	return &Roster{notify: notify}
}

func (r *Roster) changed() {
	if r.notify != nil {
		r.notify()
	}
}

// startPeer registers a pair's counterpart, hanging off the run it belongs to. It opens Idle
// rather than Running: a navigator that has not been consulted yet is not working, and saying so
// is how an unused navigator — the likeliest way for a pair to be a waste of money — becomes
// visible while the run is still going rather than only in the report.
func (r *Roster) startPeer(parent int, dispatch, agent, name string, rounds int) *handle {
	h := r.start(agent, name, "waiting to be consulted", rounds, false)
	h.set(func(run *Run) {
		run.State = Idle
		run.Parent = parent
		run.Dispatch = ""
		if dispatch != "" {
			run.Dispatch = dispatch + "-" + agent
		}
		// Steps count consultations for a peer, so the budget in the headline is the one the
		// driver is actually spending against.
		run.Step = 0
	})
	return h
}

// start registers a run and returns the handle its goroutine reports through.
func (r *Roster) start(agent, name, task string, maxSteps int, background bool) *handle {
	r.mu.Lock()
	r.nextID++
	run := &Run{
		ID:         r.nextID,
		Dispatch:   NewID(),
		Agent:      agent,
		Name:       runName(name, task),
		Task:       firstLine(task),
		State:      Running,
		MaxSteps:   maxSteps,
		Started:    time.Now(),
		Background: background,
	}
	r.runs = append(r.runs, run)
	// Trimmed from the front, and only of runs that have ended, so a live one is never dropped
	// out from under the UI reading it.
	for len(r.runs) > kept {
		i := 0
		for i < len(r.runs) && (r.runs[i].Live() || len(r.runs[i].Kept) > 0) {
			i++
		}
		if i == len(r.runs) {
			break
		}
		r.runs = append(r.runs[:i], r.runs[i+1:]...)
	}
	h := &handle{roster: r, run: run, generation: r.generation}
	h.keepAwakeLocked()
	r.mu.Unlock()
	if background {
		r.watching.Do(func() { go r.watchStalls(r.Context()) })
	}
	r.changed()
	return h
}

// Runs is every run this session has made, oldest first.
func (r *Roster) Runs() []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Run, len(r.runs))
	for i, run := range r.runs {
		out[i] = *run
	}
	return out
}

// Active is what the top bar draws: the live runs, plus failures nobody has acknowledged yet. A
// run that finished cleanly leaves the row at once — its result is already a transcript box, and
// a dot that means "something ended a while ago" is a dot that means nothing.
func (r *Roster) Active() []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Run
	for _, run := range r.runs {
		if run.Live() || (run.State == Failed && !run.Seen) {
			out = append(out, *run)
		}
	}
	return out
}

// Find returns one run by id, for the transcript box that owns it.
func (r *Roster) Find(id int) (Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.runs {
		if run.ID == id {
			return *run, true
		}
	}
	return Run{}, false
}

// TakeOutcomes drains what finished in the background since the last call, and acknowledges the
// failures still holding a dot. The session calls it as a turn starts, so the agent learns a
// filing failed at the one moment it can still do something about it.
func (r *Roster) TakeOutcomes() []Outcome {
	r.mu.Lock()
	out := r.outcomes
	r.outcomes = nil
	for _, run := range r.runs {
		if run.State == Failed && !run.Seen {
			run.Seen = true
		}
	}
	r.mu.Unlock()
	// Notified outside the lock. The notifier reaches the UI, which reads the roster back, and
	// holding the lock across that is how a redraw ends up waiting on the thing it is drawing.
	r.changed()
	return out
}

// DiscardOutcomes drops what is waiting to be delivered and stops the runs already in flight
// from delivering theirs. Called when the conversation is cleared.
//
// The runs are not cancelled, and that is the decision rather than an oversight: a coder part
// way through a change is better finishing it than leaving half of one on disk, and quitting
// still waits for them. What ends is their right to speak into a conversation that never asked
// them anything — a report arriving there is addressed to a reader who cannot act on it and
// cannot tell whether it matters.
//
// They stay on the roster while they run, because they are running, and the dot row showing
// them is the only indication left that work from before the clear is still going.
func (r *Roster) DiscardOutcomes() {
	r.mu.Lock()
	r.outcomes = nil
	r.checkins = nil
	r.generation++
	r.dropRequests()
	r.mu.Unlock()
	r.changed()
}

// Drain waits for background runs, so quitting never leaves a note created but unindexed.
// Returns false if the wait timed out.
func (r *Roster) Drain(timeout time.Duration) bool {
	finished := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return true
	case <-time.After(timeout):
		// Cancelled on the way out, so a run still queued behind a per-agent lock stops waiting
		// for a slot nobody is going to hand it. Work already in flight has had the whole timeout
		// and is being abandoned either way; this only decides whether it is abandoned tidily.
		//
		// Through Context() so the once has fired and stop is safe to read, even if no run ever
		// asked for the context.
		r.Context()
		r.stop()
		return false
	}
}

// QueueOutcomeForTest dispatches and finishes a run in one step, firing the waker as a real
// background dispatch does when it lands. Exported for the session's tests, which need a run to
// finish without a model behind it.
func (r *Roster) QueueOutcomeForTest(o Outcome) {
	r.StartForTest(o.Agent).FinishForTest(o)
}

// PendingRun is a dispatched run a test has not finished yet, which is the only way to put
// something between the dispatch and the report — a conversation being cleared, in particular.
type PendingRun struct{ h *handle }

// StartForTest registers a run against the current conversation without executing anything.
func (r *Roster) StartForTest(agent string) *PendingRun {
	r.mu.Lock()
	generation := r.generation
	r.mu.Unlock()
	return &PendingRun{h: &handle{roster: r, run: &Run{Agent: agent}, generation: generation}}
}

// FinishForTest reports the run, which the roster accepts or drops exactly as it would a real
// one — by whether the conversation that dispatched it is still the current one.
func (p *PendingRun) FinishForTest(o Outcome) {
	p.h.queue(o)
}

// AddForTest registers a run in whatever state the caller wants, without executing anything.
// Exported for the front end's tests, which need a populated roster but no model. Returns the
// id it was given.
func (r *Roster) AddForTest(run Run) int {
	r.mu.Lock()
	r.nextID++
	run.ID = r.nextID
	if run.Started.IsZero() {
		run.Started = time.Now()
	}
	r.runs = append(r.runs, &run)
	r.mu.Unlock()
	r.changed()
	return run.ID
}

// SetStateForTest moves a run to state, ending it when that state is not live.
func (r *Roster) SetStateForTest(id int, state State) {
	r.mu.Lock()
	for _, run := range r.runs {
		if run.ID == id {
			run.State = state
			if !run.Live() {
				run.Ended = time.Now()
			}
		}
	}
	r.mu.Unlock()
	r.changed()
}

// UpdateForTest changes a run in place, as a live run's own handle would.
func (r *Roster) UpdateForTest(id int, fn func(*Run)) {
	r.mu.Lock()
	for _, run := range r.runs {
		if run.ID == id {
			fn(run)
		}
	}
	r.mu.Unlock()
	r.changed()
}

// handle is a running subagent's write access to its own row. Every field change goes through
// here so the roster's lock is the only one anybody has to think about.
type handle struct {
	roster *Roster
	run    *Run
	// generation is the conversation this run was dispatched in. Compared against the roster's
	// when the run reports, so a clear in between is what drops the report.
	generation int
	// quiet runs were started by no one's request, so their outcome is shown but never queued for the model.
	quiet bool
	// skill is how the run's pinned skill ended, nil until it does; guarded by the roster's lock.
	skill *skills.SkillCheck
	// hold keeps the Mac awake while the run works; guarded by the roster's lock.
	hold *awake.Hold
}

func (h *handle) set(fn func(*Run)) {
	h.roster.mu.Lock()
	fn(h.run)
	h.keepAwakeLocked()
	h.roster.mu.Unlock()
	h.roster.changed()
}

// keepAwakeLocked holds the Mac awake while the run works, and not while it waits on the user, rests as a peer or has ended.
func (h *handle) keepAwakeLocked() {
	working := h.run.State == Running && h.run.Parent == 0
	switch {
	case working && h.hold == nil:
		h.hold = h.roster.Awake.Hold()
	case !working && h.hold != nil:
		h.hold.Release()
		h.hold = nil
	}
}

// stoppable registers the run's cancel for Stop, and returns the call that withdraws it.
func (h *handle) stoppable(stop context.CancelCauseFunc) func() {
	h.roster.mu.Lock()
	if h.roster.stops == nil {
		h.roster.stops = map[int]context.CancelCauseFunc{}
	}
	h.roster.stops[h.run.ID] = stop
	h.roster.mu.Unlock()
	return func() {
		h.roster.mu.Lock()
		delete(h.roster.stops, h.run.ID)
		h.roster.mu.Unlock()
	}
}

func (h *handle) steps() int {
	h.roster.mu.Lock()
	defer h.roster.mu.Unlock()
	return h.run.Step
}

func (h *handle) step(n int) {
	h.set(func(run *Run) {
		run.Step = n
		run.LastStep = time.Now()
		// The step is the model thinking about what to do next, so whatever tool it last ran is
		// over. Clearing here is what stops a box reading "writing X" long after the write.
		run.Activity = ""
	})
}

func (h *handle) model(model string) {
	h.set(func(run *Run) { run.Model = model })
}

// routed shows the pick's tier only when guardian chose it, not when the run fell back to its own model.
func (h *handle) routed(p Pick) {
	h.set(func(run *Run) {
		run.Model, run.Tier = p.Model, ""
		if p.Tier != TierDefault {
			run.Tier = p.Tier
		}
	})
}

func (h *handle) activity(label string) {
	h.set(func(run *Run) { run.Activity = firstLine(label) })
}

// waiting marks the run as parked on the user, which is what Waiting was defined for. Only a run
// that is still going can be waiting: a cancelled turn can return from an approval after the run
// has already failed, and that must not resurrect it.
func (h *handle) waiting(parked bool) {
	h.set(func(run *Run) {
		if !run.Live() {
			return
		}
		if parked {
			run.State = Waiting
			return
		}
		run.State = Running
	})
}

// consulting moves a peer between answering and resting, counting the exchanges as it goes. A
// peer that has ended — because its driver finished while a reply was in flight — stays ended,
// for the same reason waiting checks: a late callback must not resurrect a closed row.
func (h *handle) consulting(busy bool) {
	h.set(func(run *Run) {
		if !run.Live() {
			return
		}
		if busy {
			run.State = Running
			run.Step++
			run.Activity = "thinking"
			return
		}
		run.State = Idle
		run.Activity = ""
	})
}

func (h *handle) appendTrail(e Entry) {
	h.set(func(run *Run) { run.Trail = append(run.Trail, e) })
}

// detached reports whether this run outlives the turn that dispatched it. Read from the run
// rather than the definition because the model may detach an agent that does not do so by
// default, and it is this run's answer that decides who can be asked anything.
func (h *handle) detached() bool {
	h.roster.mu.Lock()
	defer h.roster.mu.Unlock()
	return h.run.Background
}

// end marks a run done or failed once its workspace is closed, so it never shows finished while teardown still runs.
func (h *handle) end(report string, err error) {
	if err != nil {
		h.fail(err.Error())
		return
	}
	h.finish(report)
}

func (h *handle) finish(result string) {
	h.set(func(run *Run) {
		run.State = Done
		run.Result = result
		run.Ended = time.Now()
		delete(h.roster.watches, run.ID)
	})
}

func (h *handle) fail(err string) {
	h.set(func(run *Run) {
		run.State = Failed
		run.Err = err
		run.Ended = time.Now()
		delete(h.roster.watches, run.ID)
	})
}

// queue records a background run's outcome for the next turn to pick up, unless the conversation
// that dispatched it has been cleared.
//
// The generation check and the append are under one lock, which is what makes a run finishing
// during a clear safe either way: it queued before the bump and DiscardOutcomes drops what it
// wrote, or it queued after and is refused here.
//
// Dropping the outcome is not the same as silencing the run. The notice strip has already been
// written to by the caller, so a coder that edited files while the conversation was being
// cleared still says so to the user. What is withheld is delivery to the model, because that is
// the party with no memory of asking.
func (h *handle) queue(o Outcome) {
	if h.quiet {
		return
	}
	h.roster.mu.Lock()
	if h.generation != h.roster.generation {
		h.roster.mu.Unlock()
		return
	}
	h.roster.outcomes = append(h.roster.outcomes, o)
	wake := h.roster.OnOutcome
	h.roster.mu.Unlock()
	h.roster.changed()
	// Outside the lock, like changed: the waker reaches the session, which reads the roster back.
	if wake != nil {
		wake()
	}
}

func firstLine(s string) string {
	line := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(line) > 90 {
		return line[:89] + "…"
	}
	return line
}
