// Package session owns one conversation. Everything that changes it goes through here, whether
// typed locally or, later, sent from another pane.
//
// The UI never touches this state directly: the session notifies, the UI reads a snapshot. That
// indirection is what lets a second pane mirror a session over a socket without the renderer
// knowing the difference.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/awake"
	"github.com/alexbieg/alux/internal/classify"
	"github.com/alexbieg/alux/internal/commands"
	"github.com/alexbieg/alux/internal/compact"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/mine"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/report"
	"github.com/alexbieg/alux/internal/scrub"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/skillrun"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/alexbieg/alux/internal/vault"
	"github.com/alexbieg/alux/internal/workers"
	"github.com/anthropics/anthropic-sdk-go"
)

type Kind string

const (
	KindUser      Kind = "user"
	KindAssistant Kind = "assistant"
	KindThinking  Kind = "thinking"
	KindTool      Kind = "tool"
	KindError     Kind = "error"
	KindInfo      Kind = "info"
	KindNote      Kind = "note"
	// KindReport is text that has already been laid out in columns, such as /context's
	// breakdown. The pane prints it as it stands; markdown would reflow it into a paragraph.
	KindReport Kind = "report"
	// KindTable is a report.Table in Item.Table, drawn at the pane's width.
	KindTable Kind = "table"
)

type Item struct {
	ID   int
	Kind Kind
	Text string
	Body string
	Tool agent.ToolKind
	// Run is the subagent dispatch this box reports, zero when it is not one. The box is drawn
	// from the live run rather than from Text and Body, so its head tracks the agent while it
	// works instead of freezing at whatever was true when the box appeared.
	Run int
	// Call is the tool call this box reports, empty when it is not a live tool box. It is what
	// lets the result that comes back fill in the box the call's start put up, rather than
	// appending a second one beside it.
	Call string
	// Running marks a box whose tool has started and not yet reported. The pane draws it as a
	// spinner and a head: there is no output to count until the call answers, and a call that
	// answers with nothing to show — declined, interrupted — takes the box away again.
	Running bool
	// Writes is the classifier's reading of a bash command, and stays classify.Unknown for every
	// other box and whenever the sidecar is off, slow or wrong-footed. It lands after the box is
	// drawn, so the head retints mid-command rather than waiting for the command to answer.
	Writes classify.Verdict
	// Guardian is the model's reading of whether a bash command is safe to run, landing after the box is drawn.
	Guardian guardian.Verdict
	// Unsandboxed marks a shell command that ran outside the sandbox the session otherwise runs them in.
	Unsandboxed bool
	// Rev counts edits to Body after the box resolved, so a pane caching rendered boxes redraws it.
	Rev int
	// Closed starts the box collapsed however short its body.
	Closed bool
	Table  *report.Table
}

type Mode string

const (
	ModeReadOnly Mode = "read-only"
	ModeLocal    Mode = "local"
	ModeAuto     Mode = "auto"
)

// Modes is the shift+tab cycle, most cautious first.
var Modes = []Mode{ModeReadOnly, ModeLocal, ModeAuto}

// modeAllows is what the main agent may do unasked in each mode; anything else is put to the person.
var modeAllows = map[Mode][]guardian.Category{
	ModeReadOnly: {guardian.Read},
	ModeLocal:    {guardian.Read, guardian.Build, guardian.Edit, guardian.Record},
	ModeAuto:     {guardian.Read, guardian.Build, guardian.Edit, guardian.Record, guardian.Network},
}

// breakdownTimeout bounds /context, whose counts run concurrently but still hit the network.
const breakdownTimeout = 30 * time.Second

// A verdict that has not landed by now is no longer worth drawing: the command it describes has
// usually finished, and the box has been read.
const classifyWait = 5 * time.Second

// Smaller than the channel that wakes the drain, so the queue is the bound that reports overflow.
const maxQueued = 32

// Unannounced, a block of user text arriving among tool results reads as one more tool result.
const steerPreamble = "The user sent this while you were working, after everything above. " +
	"It takes precedence wherever the two conflict. Fold it into what you are already doing " +
	"rather than starting over, and do not redo or re-report work you have finished."

// Queued is a prompt waiting for the running turn to end. The id outlives its position, which
// the drain shortens underneath whoever is holding one.
type Queued struct {
	ID   int
	Text string
	// Command marks a slash command, which runs locally and so cannot be steered into a turn.
	Command bool
}

func clone(q []Queued) []Queued {
	if len(q) == 0 {
		return nil
	}
	return append([]Queued(nil), q...)
}

// LiveThinking carries the id its finished item will get, so a box expanded mid-stream keeps its
// identity — and its expansion — when the thought flushes into the transcript.
type LiveThinking struct {
	ID   int
	Text string
}

type Snapshot struct {
	Items        []Item
	Live         string
	LiveThinking *LiveThinking
	Busy         bool
	// Quiet marks a busy spell other panes and the desktop are not told about, such as a landing.
	Quiet bool
	// Woken marks the latest turn as one a landed run raised rather than one the user started.
	Woken       bool
	Cancelling  bool
	Pending     string // approval label, empty when nothing is parked
	PendingBody string // what the label cannot show, such as the diff an edit would make
	// PendingSummary is a cheap model's sentence on what a parked command does, empty until it answers.
	PendingSummary string
	Question       *ask.Question
	// PromptSeq names the parked question, else the parked approval, for ApproveID and AnswerID; 0 when nothing is parked.
	PromptSeq int
	// ApprovalSeq and QuestionSeq key each kind of prompt on its own, for a client answering one kind; 0 when none of it is parked.
	ApprovalSeq, QuestionSeq int
	Mode                     Mode
	// Usage is how much context this conversation occupies, zero until the model has answered.
	Usage agent.Usage
	// Runs is every subagent this session has dispatched. The dot row reads the live ones and a
	// box reads the one it owns, both from this snapshot rather than from the manager, so a
	// mirroring pane sees exactly what this one does.
	Runs []subagent.Run
	// Procs is the background commands still running, read live like Runs.
	Procs []procs.Info
	// Queued is what runs after the current turn, in order.
	Queued []Queued
	// Steering is sent into the running turn but not yet handed over, which behind a slow tool
	// can be minutes.
	Steering []Queued
	// Steerable is false while the pane is busy with something that is not a turn, such as
	// /context, since nothing would collect a steer.
	Steerable bool
	// Skill is the open skill run, nil when none is.
	Skill *SkillView
	// Awake is whether alux is keeping the Mac from idle-sleeping, read live like Procs.
	Awake awake.State
	// Draft is text for the input box, put there once per ID so the user can edit it before sending.
	Draft Draft
	// Clipboard is text for the system clipboard, copied once per ID.
	Clipboard Draft
	// Files is every path the edit tool created or changed this conversation, in first-touch order.
	Files []File
	// Topic is a cheap model's few-word name for what this conversation is about, empty until it answers.
	Topic string
}

// File is one path the edit tool wrote; Created holds when its first write made the file.
type File struct {
	Path    string
	Created bool
}

type Draft struct {
	ID   int
	Text string
}

// work carries no text: a prompt's lives on the queued list and a wake's on the roster, so the
// channel is a count of things to look at and may overcount.
type work struct {
	wake bool
}

type Session struct {
	agent    *agent.Agent
	notify   func()
	baseline agent.Usage

	mu       sync.Mutex
	snap     Snapshot
	history  []anthropic.MessageParam
	nextID   int
	approval chan bool
	answer   chan *ask.Answer
	// promptSeq counts every park; approvalSeq and answerSeq hold the parked one's, 0 when none.
	promptSeq, approvalSeq, answerSeq int
	cancel                            context.CancelFunc
	abandon                           chan struct{}

	queue chan work
	dirty chan struct{}
	once  sync.Once
	name  string

	queued    []Queued
	steering  []Queued
	nextQueue int

	// workerFinals are claimed worker blocks not yet sent; reset keeps them, since a claim cannot be undone.
	workerFinals []string

	skill *skillrun.Host

	// Roster is the subagent roster, nil when subagents are not configured. Set before Start.
	Roster *subagent.Roster

	// Agents is the subagent manager, read by /agents for the capability catalogue. Optional: the
	// roster alone is enough to report what has run, and only the catalogue needs the
	// definitions. Set before Start.
	Agents *subagent.Manager

	// OnPrompt hears each approval or question once it is settled; nil logs nothing. Set before Start.
	OnPrompt func(PromptRecord)

	// Explain summarizes a shell command parked for approval; nil shows no summary. Set before Start.
	Explain func(ctx context.Context, command string) (string, error)

	// Namer names the conversation from its compacted history; nil names nothing. Set before Start.
	Namer func(ctx context.Context, excerpt string) (string, error)
	// prompts counts user prompts and answered questions this conversation; topicGen discards a name that lands after /new. Held under mu.
	prompts, topicGen int

	// Procs is the background-process registry, nil when none is wired. Set before Start.
	Procs *procs.Registry

	// Awake is held while a turn works; nil holds nothing. Set before Start.
	Awake *awake.Keeper

	// Shell is the main agent's live bash, nil for a fresh shell per call. Set before Start; a
	// turn that is abandoned keeps it, and the session goes on with a fork.
	Shell *shell.Shell

	// Classifier reads bash commands for whether they write to disk, nil when the feature is
	// unconfigured. Set before Start.
	Classifier *classify.Client

	// guardianHeld carries a verdict, by call id, from the gate to the box the command then gets.
	guardianHeld map[string]guardian.Verdict
	// unsandboxedHeld carries, by call id, a command leaving the sandbox from the gate to its box.
	unsandboxedHeld map[string]bool

	// PRs answers /prs, nil when PR tracking is unconfigured. Set before Start.
	PRs func(ctx context.Context, arg string) (string, error)

	// Land answers /land, nil when the session is not in a pool workspace. Set before Start.
	Land func(arg string) (string, error)
	// Resume answers /resume, nil when the session is not in a pool workspace. Set before Start.
	Resume func(arg string) (string, error)

	// Jobs answers /jobs, nil when the session schedules no jobs. Set before Start.
	Jobs func() report.Table

	// Workers answers /workers with this repo's headless workers. Set before Start.
	Workers func() []workers.Worker
	// ClaimWorkers takes this repo's finished workers for the next user turn; nil delivers none. Set before Start.
	ClaimWorkers func() []workers.Finished
	// OnSkillClosed hears how each skill run ended: skillrun.Finished, or "failed: <reason>". Set before the first Submit.
	OnSkillClosed func(how string)

	// Restart runs what quitting and relaunching would do to the workspace, on every NewSession; nil does nothing. Set before Start.
	Restart func() error

	// ID names this process in the logs. Set before Start.
	ID string

	// Followups receives what happens once a subagent's report reaches the agent, nil when
	// nothing is logged. Set before Start.
	Followups func(subagent.Followup)

	// Transcript records every turn's prompt, text and tool calls; nil records nothing. Set before Start.
	Transcript *subagent.Transcript

	// Sequences logs each turn's tool calls as symbols for the workflow miner; nil logs nothing. Set before Start.
	Sequences *mine.Recorder
	// Mine runs the workflow miner in the background after /new; nil skips it.
	Mine func()

	// followMu is held across the write, so the log's order is the order things happened in.
	followMu sync.Mutex
	window   bool
	// reporting maps a foreground dispatch's tool call to the run whose report it will return.
	reporting map[string]string
	labels    map[string]string

	// writing maps an edit-tool call that answered to its file, until its end says whether it failed. Held under mu.
	writing map[string]File

	baselines baselines

	// recalled is the history a compaction's view numbers, which recall reads; nil when not compacted. Held under mu.
	recalled []compact.Event
	// compactions counts /compact runs since the last fresh session; a fork keeps it. Held under mu.
	compactions int
}

// New builds a session. notify is called whenever the snapshot changes, on the session's own
// goroutine rather than the caller's, so it may block.
func New(a *agent.Agent, notify func()) *Session {
	s := &Session{
		agent:  a,
		notify: notify,
		snap:   Snapshot{Mode: ModeAuto},
		queue:  make(chan work, 64),
		dirty:  make(chan struct{}, 1),
	}
	s.skill = s.newSkillHost()
	s.wireRecall()
	if notify != nil {
		go s.notifyPump()
	}
	return s
}

// Start begins draining the prompt queue. Slash commands and prompts share one queue, so work
// sent while a turn is running waits for it rather than racing it.
func (s *Session) Start() {
	s.once.Do(func() { go s.drain() })
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	// Capped so a later append allocates instead of writing into the slice the UI is holding.
	snap.Items = s.snap.Items[:len(s.snap.Items):len(s.snap.Items)]
	// Read live rather than mirrored into the snapshot on every change: a subagent updates its
	// row several times a second, and the roster already notifies when it does.
	if s.Roster != nil {
		snap.Runs = s.Roster.Runs()
	}
	if s.Procs != nil {
		snap.Procs = s.Procs.Live()
	}
	snap.Awake = s.Awake.State()
	return snap
}

// LiveProcs is the running background commands on their own, for a pane refreshing a box that
// tails one without taking a whole new snapshot to do it.
func (s *Session) LiveProcs() []procs.Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Procs == nil {
		return nil
	}
	return s.Procs.Live()
}

// follow logs tool calls and a message only between a report and the user's next message.
func (s *Session) follow(f subagent.Followup) {
	s.followMu.Lock()
	defer s.followMu.Unlock()
	if s.Followups == nil {
		return
	}
	switch f.Event {
	case subagent.EventReport:
		s.window = true
	case subagent.EventTool:
		if !s.window {
			return
		}
	case subagent.EventMessage:
		if !s.window {
			return
		}
		s.window = false
	case subagent.EventReset:
		s.window = false
	}
	f.Time, f.Session = time.Now(), s.ID
	s.Followups(f)
}

func (s *Session) dispatched(id int) {
	if s.Followups == nil || s.Roster == nil {
		return
	}
	run, ok := s.Roster.Find(id)
	if !ok || run.Dispatch == "" || run.Call == "" {
		return
	}
	s.labelTool(run.Call, run.Agent+": "+run.Task)
	if run.Background {
		return
	}
	s.followMu.Lock()
	defer s.followMu.Unlock()
	if s.reporting == nil {
		s.reporting = map[string]string{}
	}
	s.reporting[run.Call] = run.Dispatch
}

func (s *Session) labelTool(id, label string) {
	if s.Followups == nil || id == "" {
		return
	}
	s.followMu.Lock()
	defer s.followMu.Unlock()
	if s.labels == nil {
		s.labels = map[string]string{}
	}
	s.labels[id] = label
}

// toolEnded logs a foreground dispatch's answer as its report, and any other call as a tool call.
func (s *Session) toolEnded(id, name string, failed, live bool) {
	s.followMu.Lock()
	run, reported := s.reporting[id]
	label := s.labels[id]
	delete(s.reporting, id)
	delete(s.labels, id)
	s.followMu.Unlock()
	switch {
	case !live:
	case reported:
		s.follow(subagent.Followup{Event: subagent.EventReport, Run: run, Error: failed})
	default:
		s.follow(subagent.Followup{Event: subagent.EventTool, Tool: name, Label: truncate(label, 300), Error: failed})
	}
}

func (s *Session) noteWrite(id, label string, kind agent.ToolKind) {
	if kind != agent.KindEdit || id == "" {
		return
	}
	verb := tools.EditVerb(label)
	switch verb {
	case "create", "edit", "insert":
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writing == nil {
		s.writing = map[string]File{}
	}
	s.writing[id] = File{Path: tools.EditPath(label), Created: verb == "create"}
}

// wrote adds a finished edit's file to the snapshot's files, once, unless the edit failed.
func (s *Session) wrote(id string, failed bool) {
	s.mu.Lock()
	f, ok := s.writing[id]
	delete(s.writing, id)
	s.mu.Unlock()
	if !ok || failed || f.Path == "" {
		return
	}
	s.update(func(snap *Snapshot) {
		if !slices.ContainsFunc(snap.Files, func(g File) bool { return g.Path == f.Path }) {
			snap.Files = append(slices.Clip(snap.Files), f)
		}
	})
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func (s *Session) update(fn func(*Snapshot)) {
	s.mu.Lock()
	fn(&s.snap)
	s.mu.Unlock()
	s.touch()
}

// touch marks the snapshot changed without blocking, so it is safe from any goroutine.
func (s *Session) touch() {
	select {
	case s.dirty <- struct{}{}:
	default:
		// Already marked; the pump re-checks after notifying, so this change still gets drawn.
	}
}

// notifyPump runs the notifier off the caller's stack. A front end that mutates the session from
// its own event loop would otherwise deadlock against a notifier that wakes that loop.
func (s *Session) notifyPump() {
	for range s.dirty {
		s.notify()
	}
}

func (s *Session) push(kind Kind, text, body string, tool agent.ToolKind) {
	s.update(func(snap *Snapshot) {
		snap.Items = append(snap.Items, Item{ID: s.nextID, Kind: kind, Text: text, Body: body, Tool: tool})
		s.nextID++
	})
}

// resolveTool fills in the box a tool call put up when it started, and reports whether it found
// one. False is the ordinary case for a call that never announced itself, and the caller pushes a
// fresh item for it.
//
// Copy-on-write, like dropRunningTool below: Snapshot hands the UI a capped view of this same
// backing array, so an element edited in place would be rewritten under a renderer reading it.
// One copy per tool completion, which is nothing beside the transcript it is already building.
func (s *Session) resolveTool(id, label, output string, kind agent.ToolKind) bool {
	if id == "" {
		return false
	}
	found := false
	s.update(func(snap *Snapshot) {
		for i, item := range snap.Items {
			if !item.Running || item.Call != id {
				continue
			}
			items := append([]Item(nil), snap.Items...)
			items[i].Text, items[i].Body, items[i].Tool, items[i].Running = label, output, kind, false
			snap.Items = items
			found = true
			return
		}
	})
	return found
}

// ProcFinished puts a process's final output in the box of the call that started it.
func (s *Session) ProcFinished(call, output string) {
	if call == "" {
		return
	}
	s.update(func(snap *Snapshot) {
		for i, item := range snap.Items {
			if item.Call != call || item.Running {
				continue
			}
			items := append([]Item(nil), snap.Items...)
			items[i].Body = output
			items[i].Rev++
			snap.Items = items
			return
		}
	})
}

// classifyTool asks the model whether a bash command writes to disk, off the turn's goroutine so
// a slow or absent sidecar never delays the command it is reporting on. Nothing waits for the
// answer: it lands in the box whenever it arrives, or never.
func (s *Session) classifyTool(id, label string, kind agent.ToolKind) {
	if s.Classifier == nil || kind != agent.KindBash {
		return
	}
	command, ok := classify.Command(label)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), classifyWait)
		defer cancel()
		if verdict := s.Classifier.Classify(ctx, command); verdict != classify.Unknown {
			s.setVerdict(id, verdict)
		}
	}()
}

// setVerdict tints a box after the fact. Unlike resolveTool it does not require the box to still
// be running: a short command outruns the classifier often, and the verdict is worth drawing on a
// box that has already answered.
func (s *Session) setVerdict(id string, verdict classify.Verdict) {
	if id == "" {
		return
	}
	s.update(func(snap *Snapshot) {
		for i, item := range snap.Items {
			if item.Call != id || item.Writes == verdict {
				continue
			}
			items := append([]Item(nil), snap.Items...)
			items[i].Writes = verdict
			snap.Items = items
			return
		}
	})
}

// holdGuardian keeps a command's verdict until the command's box exists; the gate runs before the start.
func (s *Session) holdGuardian(id string, verdict guardian.Verdict) {
	if !verdict.Known() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.guardianHeld == nil {
		s.guardianHeld = map[string]guardian.Verdict{}
	}
	s.guardianHeld[id] = verdict
}

// judgeTool hands the held verdict to the box the command's start just put up.
func (s *Session) judgeTool(id string) {
	s.mu.Lock()
	verdict, held := s.guardianHeld[id]
	delete(s.guardianHeld, id)
	outside := s.unsandboxedHeld[id]
	delete(s.unsandboxedHeld, id)
	s.mu.Unlock()
	if held {
		s.setGuardian(id, verdict)
	}
	if outside {
		s.update(func(snap *Snapshot) {
			for i, item := range snap.Items {
				if item.Call == id {
					items := append([]Item(nil), snap.Items...)
					items[i].Unsandboxed = true
					snap.Items = items
					return
				}
			}
		})
	}
}

func (s *Session) holdUnsandboxed(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsandboxedHeld == nil {
		s.unsandboxedHeld = map[string]bool{}
	}
	s.unsandboxedHeld[id] = true
}

func (s *Session) setGuardian(id string, verdict guardian.Verdict) {
	if id == "" {
		return
	}
	s.update(func(snap *Snapshot) {
		for i, item := range snap.Items {
			if item.Call != id {
				continue
			}
			items := append([]Item(nil), snap.Items...)
			items[i].Guardian = verdict
			snap.Items = items
			return
		}
	})
}

// dropRunningTool takes away a box whose call answered without reporting anything, which is how a
// declined or interrupted call leaves the transcript as it found it.
func (s *Session) dropRunningTool(id string) {
	if id == "" {
		return
	}
	s.update(func(snap *Snapshot) {
		for i, item := range snap.Items {
			if !item.Running || item.Call != id {
				continue
			}
			snap.Items = append(snap.Items[:i:i], snap.Items[i+1:]...)
			return
		}
	})
}

// called gives a decline that ran nothing a box of its own; one whose command ran is shown by that command's box.
func (s *Session) called(call agent.Call, res anthropic.ContentBlockParamUnion) {
	s.skillBox(call, res)
	s.skill.Observe(call, res)
	if call.Name != agent.DeclineSuggestion || s.hasBox(call.ID) {
		return
	}
	var in struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(call.Input, &in)
	body := strings.TrimSpace(in.Reason)
	if r := res.OfToolResult; r != nil && r.IsError.Or(false) {
		for _, c := range r.Content {
			if c.OfText != nil {
				body = strings.TrimSpace(body + "\n\n" + c.OfText.Text)
			}
		}
	}
	s.flush()
	s.push(KindTool, strings.TrimSpace("decline suggestion "+in.ID), body, agent.KindBash)
}

func (s *Session) hasBox(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.snap.Items, func(item Item) bool { return item.Call == id })
}

// Show puts a line in the transcript without involving the model.
func (s *Session) Show(text string) { s.push(KindNote, text, "", "") }

// ShowBlock is Show for text whose own spacing is the point: it reaches the pane unrendered.
func (s *Session) ShowBlock(text string) { s.push(KindReport, text, "", "") }

// ShowTable puts a table in the transcript for the pane to draw at its own width.
func (s *Session) ShowTable(t report.Table) {
	s.update(func(snap *Snapshot) {
		snap.Items = append(snap.Items, Item{ID: s.nextID, Kind: KindTable, Table: &t})
		s.nextID++
	})
}

func (s *Session) SetMode(m Mode) {
	s.update(func(snap *Snapshot) { snap.Mode = m })
}

// EffectiveMode is the mode the gates apply: auto falls to local once sensitive data has been read, for good.
func (s *Session) EffectiveMode() Mode {
	s.mu.Lock()
	m := s.snap.Mode
	s.mu.Unlock()
	return Effective(m)
}

// Effective is EffectiveMode for a mode already read, which is what the pane draws from a snapshot.
func Effective(m Mode) Mode {
	if m == ModeAuto && taint.Tainted() {
		return ModeLocal
	}
	return m
}

// Allowed is the current mode's categories, read on every command so a switch applies to the next one.
func (s *Session) Allowed() []guardian.Category { return modeAllows[s.EffectiveMode()] }

func (s *Session) Allows(c guardian.Category) bool { return slices.Contains(s.Allowed(), c) }

// Submit lines up a prompt: at once when nothing is running, behind the current turn otherwise.
func (s *Session) Submit(text string) {
	full := false
	s.update(func(snap *Snapshot) {
		if len(s.queued) >= maxQueued {
			full = true
			return
		}
		_, _, command := commands.Parse(text)
		s.queued = append(s.queued, Queued{ID: s.nextQueue, Text: text, Command: command})
		s.nextQueue++
		snap.Queued = clone(s.queued)
	})
	if full {
		s.push(KindError, "input queue is full", "", "")
		return
	}
	// A URL the user names is one web_fetch may follow; one a fetched page suggests is not.
	agent.PermitURLsIn(text)
	s.token()
}

// token tells the drain there is something to look at. Safe to drop when full: the queue is the
// shorter of the two, so an unread token is already waiting to reach the item.
func (s *Session) token() {
	select {
	case s.queue <- work{}:
	default:
	}
}

// take pops the next queued prompt. Finding none is ordinary: a steered or dropped item leaves
// its token behind.
func (s *Session) take() (string, bool) {
	var text string
	found := false
	s.update(func(snap *Snapshot) {
		if len(s.queued) == 0 {
			return
		}
		text, found = s.queued[0].Text, true
		s.queued = s.queued[1:]
		snap.Queued = clone(s.queued)
	})
	return text, found
}

// Steer sends a queued prompt into the running turn. The turn hears it at its next step
// boundary, once the tools it already asked for come back, so behind a slow tool it is not quick.
func (s *Session) Steer(id int) {
	s.update(func(snap *Snapshot) {
		if !snap.Steerable {
			return
		}
		for i, q := range s.queued {
			if q.ID != id || q.Command {
				continue
			}
			s.queued = append(s.queued[:i:i], s.queued[i+1:]...)
			s.steering = append(s.steering, q)
			snap.Queued, snap.Steering = clone(s.queued), clone(s.steering)
			return
		}
	})
}

// Unqueue takes a prompt back off the queue, returning "" when it is already gone.
func (s *Session) Unqueue(id int) string {
	text := ""
	s.update(func(snap *Snapshot) {
		for i, q := range s.queued {
			if q.ID != id {
				continue
			}
			text = q.Text
			s.queued = append(s.queued[:i:i], s.queued[i+1:]...)
			snap.Queued = clone(s.queued)
			return
		}
	})
	return text
}

// DropQueued clears everything lined up, undelivered steering included, and reports how much went.
func (s *Session) DropQueued() int {
	n := 0
	s.update(func(snap *Snapshot) {
		n = len(s.queued) + len(s.steering)
		s.queued, s.steering = nil, nil
		snap.Queued, snap.Steering = nil, nil
	})
	return n
}

// PendingBlock is what the running turn has not been told: steering, then landed background runs.
// Destructive, as the agent requires — a block handed back twice would never let the turn end.
func (s *Session) PendingBlock() string {
	parts := []string{}
	for _, block := range []string{s.takeSteering(), s.landed(), s.checkIns()} {
		if block != "" {
			parts = append(parts, block)
		}
	}
	return strings.Join(parts, "\n\n")
}

// landed is everything that finished behind the agent's back: dispatched runs, then background
// commands. Both deliveries read it, so the two sources cannot drift apart.
func (s *Session) landed() string {
	parts := []string{}
	for _, block := range []string{s.requests(), s.outcomes(), s.exits()} {
		if block != "" {
			parts = append(parts, block)
		}
	}
	return scrub.Text(strings.Join(parts, "\n\n"))
}

// takeSteering renders the steering waiting to go and clears it. The transcript gains the message
// at this point, so it reads in the order the model was given things.
func (s *Session) takeSteering() string {
	var sent []Queued
	s.update(func(snap *Snapshot) {
		sent, s.steering = s.steering, nil
		snap.Steering = nil
	})
	if len(sent) == 0 {
		return ""
	}
	var b, heard strings.Builder
	b.WriteString(steerPreamble)
	for _, q := range sent {
		s.push(KindUser, q.Text, "", "")
		s.follow(subagent.Followup{Event: subagent.EventMessage, Text: q.Text, Steer: true})
		b.WriteString("\n\n" + q.Text)
		heard.WriteString(q.Text + "\n\n")
	}
	s.skill.Heard(heard.String())
	return b.String()
}

// restoreSteering re-queues anything a turn ended without being told, which a later turn would
// otherwise collect and act on out of order.
func (s *Session) restoreSteering() {
	n := 0
	s.update(func(snap *Snapshot) {
		if len(s.steering) == 0 {
			return
		}
		n = len(s.steering)
		s.queued = append(clone(s.steering), s.queued...)
		s.steering = nil
		snap.Queued, snap.Steering = clone(s.queued), nil
	})
	for i := 0; i < n; i++ {
		s.token()
	}
}

// Wake tells the session a background run has landed. It goes on the same queue as a prompt, and
// that is the whole of the busy/idle distinction: idle, the drain picks it up at once and starts
// a turn; busy, it waits behind the running turn, by which time that turn's own mid-turn drain
// has usually taken the outcome and the wake finds nothing left to do.
//
// Dropped rather than blocked when the queue is full, because it must be safe to call from a
// finishing run's goroutine. Nothing is lost by dropping one: the outcome stays queued on the
// roster and the next prompt or the next wake collects it.
func (s *Session) Wake() {
	select {
	case s.queue <- work{wake: true}:
	default:
	}
}

// Approve resolves a parked approval. Harmless when nothing is parked.
func (s *Session) Approve(okay bool) {
	s.mu.Lock()
	ch := s.approval
	s.approval, s.approvalSeq = nil, 0
	s.refreshPromptSeq()
	s.mu.Unlock()
	if ch == nil {
		return
	}
	s.update(func(snap *Snapshot) { snap.Pending, snap.PendingBody, snap.PendingSummary = "", "", "" })
	ch <- okay
}

// Answer resolves a parked question. A nil answer means the user backed out, which the tool
// reports as a declined call rather than inventing an answer.
func (s *Session) Answer(a *ask.Answer) {
	s.mu.Lock()
	ch := s.answer
	s.answer, s.answerSeq = nil, 0
	s.refreshPromptSeq()
	s.mu.Unlock()
	if ch == nil {
		return
	}
	s.update(func(snap *Snapshot) { snap.Question = nil })
	ch <- a
}

// ApproveID resolves the parked approval only while seq is still the one parked, reporting whether it did.
func (s *Session) ApproveID(seq int, okay bool) bool {
	s.mu.Lock()
	ch := s.approval
	if ch == nil || s.approvalSeq != seq {
		s.mu.Unlock()
		return false
	}
	s.approval, s.approvalSeq = nil, 0
	s.snap.Pending, s.snap.PendingBody, s.snap.PendingSummary = "", "", ""
	s.refreshPromptSeq()
	s.mu.Unlock()
	s.touch()
	ch <- okay
	return true
}

// AnswerID resolves the parked question only while seq is still the one parked, reporting whether it did.
func (s *Session) AnswerID(seq int, a *ask.Answer) bool {
	s.mu.Lock()
	ch := s.answer
	if ch == nil || s.answerSeq != seq {
		s.mu.Unlock()
		return false
	}
	s.answer, s.answerSeq = nil, 0
	s.snap.Question = nil
	s.refreshPromptSeq()
	s.mu.Unlock()
	s.touch()
	ch <- a
	return true
}

// refreshPromptSeq is called under mu after any park or resolve.
func (s *Session) refreshPromptSeq() {
	s.snap.PromptSeq = s.approvalSeq
	s.snap.ApprovalSeq, s.snap.QuestionSeq = s.approvalSeq, s.answerSeq
	if s.answerSeq != 0 {
		s.snap.PromptSeq = s.answerSeq
	}
}

// Cancel is two-stage. The first press aborts the turn and lets it unwind, so the transcript and
// the history keep what already happened. The second stops waiting on it: the turn keeps running
// but is disowned, because nothing guarantees a hung tool honours a cancellation.
func (s *Session) Cancel() {
	s.mu.Lock()
	if !s.snap.Busy {
		s.mu.Unlock()
		return
	}
	cancelling := s.snap.Cancelling
	abandon := s.abandon
	cancel := s.cancel
	s.mu.Unlock()

	if cancelling {
		if abandon != nil {
			select {
			case <-abandon:
			default:
				close(abandon)
			}
		}
		return
	}
	s.Approve(false) // unblocks a turn parked on an approval
	s.Answer(nil)    // …and one parked on a question
	if cancel != nil {
		cancel()
	}
	s.update(func(snap *Snapshot) { snap.Cancelling = true })
}

// CloseShell ends the main agent's live shell, whichever fork it is by now.
func (s *Session) CloseShell() {
	s.mu.Lock()
	sh := s.Shell
	s.mu.Unlock()
	if sh != nil {
		sh.Close()
	}
}

// NewSession clears the conversation without restarting the process.
//
// Background runs outlive it. They keep running and still report into the notice strip, but
// nothing they say reaches the model any more: a finding delivered here would arrive in a
// conversation with no memory of dispatching it, and the agent would be asked to act on work it
// cannot account for. Worst with a detached coder, whose edits have already landed by then.
func (s *Session) NewSession() { s.reset(false) }

// reset clears the conversation. A fork carries on the same work from a compacted history, so it keeps
// the workspace, the shell and the background runs' reports, which the history still accounts for.
func (s *Session) reset(fork bool) {
	if s.Roster != nil && !fork {
		s.Roster.DiscardOutcomes()
	}
	if s.Procs != nil && !fork {
		s.Procs.DiscardExits()
	}
	s.follow(subagent.Followup{Event: subagent.EventReset})
	s.Transcript.Reset()
	s.Sequences.Reset()
	if s.Mine != nil {
		go s.Mine()
	}
	s.mu.Lock()
	s.history = nil
	s.recalled = nil
	if !fork {
		s.compactions = 0
	}
	s.prompts = 0
	s.topicGen++
	sh := s.Shell
	s.mu.Unlock()
	// A conversation that never ran a command must not inherit a directory it cannot remember.
	if sh != nil && !fork {
		sh.Restart()
	}
	if s.agent.Standing != nil {
		s.agent.Standing.Reset()
	}
	s.skill.Drop()
	s.baselines.reset()
	s.update(func(snap *Snapshot) {
		snap.Skill = nil
		snap.Items = nil
		snap.Live = ""
		snap.LiveThinking = nil
		snap.Files = nil
		snap.Topic = ""
		// The conversation is gone, so the context it filled is too.
		snap.Usage = s.baseline
		// …and so is anything queued, which meant what it meant in that conversation.
		if !fork {
			s.queued, s.steering = nil, nil
			snap.Queued, snap.Steering = nil, nil
		}
	})
	if s.Restart != nil && !fork {
		if err := s.Restart(); err != nil {
			s.push(KindError, "the workspace was not restarted for the new session: "+err.Error(), "", "")
		}
	}
}

func (s *Session) drain() {
	for w := range s.queue {
		if w.wake {
			s.resume()
			continue
		}
		if text, ok := s.take(); ok {
			s.handle(text)
		}
	}
}

func (s *Session) flush() {
	s.update(func(snap *Snapshot) {
		if snap.Live == "" {
			return
		}
		snap.Items = append(snap.Items, Item{ID: s.nextID, Kind: KindAssistant, Text: snap.Live})
		s.nextID++
		snap.Live = ""
	})
}

// think grows the in-flight thought, reserving the id its finished item will take the first time
// it is called.
func (s *Session) think(delta string) {
	s.update(func(snap *Snapshot) {
		if snap.LiveThinking == nil {
			snap.LiveThinking = &LiveThinking{ID: s.nextID}
			s.nextID++
		}
		snap.LiveThinking = &LiveThinking{ID: snap.LiveThinking.ID, Text: snap.LiveThinking.Text + delta}
	})
}

func (s *Session) flushThinking() {
	s.update(func(snap *Snapshot) {
		if snap.LiveThinking == nil {
			return
		}
		snap.Items = append(snap.Items, Item{ID: snap.LiveThinking.ID, Kind: KindThinking, Text: snap.LiveThinking.Text})
		snap.LiveThinking = nil
	})
}

func (s *Session) handle(text string) {
	s.update(func(snap *Snapshot) { snap.Woken = false })
	if name, args, ok := commands.Parse(text); ok {
		s.runCommand(name, args)
		return
	}
	s.push(KindUser, text, "", "")
	s.countPrompt()
	s.follow(subagent.Followup{Event: subagent.EventMessage, Text: text})
	s.skill.Heard(text)
	s.turn(s.withOutcomes(text))
}

// resume is a turn alux starts for itself, because a run it dispatched or a background
// command has finished and nobody is waiting on a prompt to hear about it.
//
// Finding nothing to say is the normal case rather than an error: a wake queued while a turn was
// running arrives after that turn's own drain has already taken the outcome. Checking here is
// what keeps the two deliveries from both reporting the same run.
func (s *Session) resume() {
	block, notes := s.landed(), s.checkIns()
	switch {
	case block != "":
		s.push(KindInfo, "↻ a background run landed — picking it up", "", "")
	case notes != "":
		s.push(KindInfo, "↻ a subagent check-in — picking it up", "", "")
	case s.skill.Leading():
		s.push(KindInfo, "↻ a flow stage landed — picking it up", "", "")
		block = "A stage of the open skill flow has landed."
	default:
		return
	}
	s.update(func(snap *Snapshot) { snap.Woken = true })
	s.turn(strings.TrimSpace(block + "\n\n" + notes))
}

// turn runs one exchange against the model, from a prompt already composed. The caller decides
// what the transcript shows for it, which is the only difference between a prompt the user typed
// and one alux raised for itself.
func (s *Session) turn(prompt string) agent.Result {
	s.mu.Lock()
	history := append([]anthropic.MessageParam(nil), s.history...)
	history = append(history, anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)))
	ctx, cancel := context.WithCancel(agent.WithRecordSession(context.Background(), s.ID))
	sh := s.Shell
	if sh != nil {
		ctx = shell.With(ctx, sh)
	}
	abandon := make(chan struct{})
	s.cancel, s.abandon = cancel, abandon
	s.mu.Unlock()
	defer cancel()

	s.update(func(snap *Snapshot) { snap.Busy, snap.Cancelling, snap.Steerable = true, false, true })

	// live gates every callback. Once the user has given up waiting, the turn goes on running
	// somewhere behind us but none of its output reaches this transcript.
	var liveMu sync.Mutex
	live := true
	isLive := func() bool { liveMu.Lock(); defer liveMu.Unlock(); return live }

	hold := s.Awake.Hold()
	cb := s.callbacks(isLive, hold)
	s.Transcript.User(prompt)
	s.Transcript.Watch(&cb)
	defer s.Transcript.Flush()
	endSequence := s.Sequences.Watch(prompt, s.agent.ModelID(), agent.PromptVersion(s.agent.Standing), &cb)

	done := make(chan agent.Result, 1)
	go func() {
		res := s.agent.RunTurn(ctx, history, cb)
		s.mu.Lock()
		orphaned := sh != nil && s.Shell != sh
		s.mu.Unlock()
		if orphaned {
			sh.Close()
		}
		endSequence()
		hold.Release()
		done <- res
	}()

	var res agent.Result
	select {
	case res = <-done:
		s.finish(res, history)
	case <-abandon:
		liveMu.Lock()
		live = false
		liveMu.Unlock()
		s.flushThinking()
		s.flush()
		// Fork here. The turn we walked away from works on its own copy, so what we keep is
		// simply the conversation as it stood before it started.
		s.mu.Lock()
		s.history = history
		if sh != nil && s.Shell == sh {
			// The abandoned turn still drives sh, so the next one must not queue behind it.
			if fork, err := sh.Fork(); err == nil {
				s.Shell = fork
			}
		}
		s.mu.Unlock()
		s.push(KindInfo, "⊘ abandoned — the turn is still finishing in the background", "", "")
		s.update(func(snap *Snapshot) { snap.Busy, snap.Cancelling, snap.Steerable = false, false, false })
		// Steering held for a disowned turn would reach a conversation this session no longer owns.
		s.restoreSteering()
	}
	s.killHeld()
	return res
}

const HandoffPrompt = `Write a handoff note for a fresh session that will carry on this work with none of this conversation. ` +
	`Your final message must be the note alone, in markdown, under these headings: Goal; Decisions (each with its reason); ` +
	`Current state (what is done and where it lives: jj change, files, records); Open threads; Tried and dropped (each with why); Next step. ` +
	`Point to what jj, the record store or the vault already hold rather than restating it.`

// handoff has the agent write a handoff note, then either copies it to the clipboard and carries on,
// or starts a new session and puts the note in the input box to edit and send.
func (s *Session) handoff(toClipboard bool) {
	s.push(KindInfo, "✎ writing a handoff note", "", "")
	res := s.turn(HandoffPrompt)
	note := strings.TrimSpace(res.Final)
	if res.StopReason != "end_turn" || note == "" {
		s.push(KindError, "no handoff note was written, so this session carries on", "", "")
		return
	}
	if s.Transcript != nil && s.ID != "" {
		log := filepath.Join(subagent.DefaultSessionDir(), s.ID+".jsonl")
		if toClipboard {
			note += fmt.Sprintf("\n\nThe earlier conversation is in %s; grep it when this note is not enough.", log)
		} else {
			note += fmt.Sprintf("\n\nThe earlier conversation is in %s, before the reset event at %s; grep it when this note is not enough.",
				log, time.Now().UTC().Format(time.RFC3339))
		}
	}
	if toClipboard {
		s.update(func(snap *Snapshot) { snap.Clipboard = Draft{ID: snap.Clipboard.ID + 1, Text: note} })
		return
	}
	s.NewSession()
	s.update(func(snap *Snapshot) { snap.Draft = Draft{ID: snap.Draft.ID + 1, Text: note} })
}

const compactCoda = "\n\nThis session was compacted: the history above is everything said before it. " +
	"Carry on where the last turn left off: finish it if it was still running, otherwise wait for the next message."

// compact starts a new session whose first message is the code-built view of this one's transcript.
func (s *Session) compact() {
	log := s.Transcript.Path()
	if log == "" {
		s.push(KindError, "this session keeps no transcript, so there is nothing to compact", "", "")
		return
	}
	s.Transcript.Flush()
	ev, err := compact.Load(log)
	if err != nil {
		s.push(KindError, "the transcript could not be read: "+err.Error(), "", "")
		return
	}
	if len(ev) == 0 {
		s.push(KindError, "nothing has happened since the last reset, so there is nothing to compact", "", "")
		return
	}
	ws, _ := os.Getwd()
	cache := compactCache(log)
	view, err := compact.Build(ev, ws, cache)
	if err != nil {
		s.push(KindInfo, "embedding failed ("+err.Error()+"), so older replies stay whole", "", "")
	}
	s.reset(true)
	s.mu.Lock()
	s.recalled = ev
	s.compactions++
	n := s.compactions
	s.mu.Unlock()
	logCompaction(filepath.Join(filepath.Dir(cache), "log.jsonl"), log, n, len(ev), len(view))
	s.push(KindInfo, fmt.Sprintf("⇣ compacted %d steps into %d characters", len(ev), len(view)), "", "")
	s.turn(view + compactCoda)
}

// compactCache is where a transcript's sentence vectors are kept between builds of its view.
func compactCache(log string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(log)), "compact", strings.TrimSuffix(filepath.Base(log), ".jsonl")+".bin")
}

// logCompaction appends one line per /compact, to see how many a session reaches and how much window the view takes.
func logCompaction(path, transcript string, n, steps, viewChars int) {
	line, _ := json.Marshal(map[string]any{
		"time": time.Now().Format(time.RFC3339), "transcript": filepath.Base(transcript), "n": n,
		"steps": steps, "view_chars": viewChars, "window_tokens": llm.ContextWindow(),
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// killHeld stops the commands that overran the foreground limit and that the turn never claimed.
func (s *Session) killHeld() {
	if s.Procs == nil {
		return
	}
	for _, p := range s.Procs.KillHeld() {
		s.push(KindInfo, fmt.Sprintf("⊘ proc %d killed — not claimed by the end of the turn (%s)", p.ID, p.Command), "", "")
	}
}

// withOutcomes prefixes a prompt with whatever finished while nothing was reading.
//
// The last of the three deliveries to be reached, and the one that needs no turn of its own: a
// run that landed between turns rides in on the next thing the user says. The user's own text is
// unchanged in the transcript; only what is sent to the model carries the prefix.
func (s *Session) withOutcomes(text string) string {
	s.collectWorkers()
	block := strings.TrimSpace(s.landed() + "\n\n" + s.checkIns() + "\n\n" + s.takeWorkerFinals())
	if block == "" {
		return text
	}
	return block + "\n\n" + text
}

// collectWorkers claims finished workers into the queue the next user turn drains.
func (s *Session) collectWorkers() {
	if s.ClaimWorkers == nil {
		return
	}
	for _, f := range s.ClaimWorkers() {
		s.mu.Lock()
		s.workerFinals = append(s.workerFinals, scrub.Text(workers.Block(f)))
		s.mu.Unlock()
		s.push(KindInfo, fmt.Sprintf("worker %s finished: %s", f.Key, f.Final.State), "", "")
	}
}

func (s *Session) takeWorkerFinals() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	blocks := s.workerFinals
	s.workerFinals = nil
	return strings.Join(blocks, "\n\n")
}

// checkIns renders what alux noticed about runs still going; never a reason to wake by itself.
func (s *Session) checkIns() string {
	if s.Roster == nil {
		return ""
	}
	return scrub.Text(subagent.CheckInBlock(s.Roster.TakeCheckIns()))
}

// requests renders what detached runs are blocked on, each shown once.
func (s *Session) requests() string {
	if s.Roster == nil {
		return ""
	}
	return subagent.RequestBlock(s.Roster.TakeRequests())
}

// outcomes drains what has finished and renders it, or "" when nothing is waiting. The drain is
// destructive and there are three callers — a prompt, a mid-turn step, and a resume — so whoever
// gets there first delivers it and the others find it gone. That is the intended behaviour: the
// agent should hear about a run once.
func (s *Session) outcomes() string {
	if s.Roster == nil {
		return ""
	}
	outcomes := s.Roster.TakeOutcomes()
	for _, o := range outcomes {
		s.follow(subagent.Followup{Event: subagent.EventReport, Run: o.Dispatch, Error: o.Err != ""})
	}
	return subagent.OutcomeBlock(outcomes)
}

// exits drains the background commands that have finished, on the same destructive terms.
func (s *Session) exits() string {
	if s.Procs == nil {
		return ""
	}
	return procs.ExitBlock(s.Procs.TakeExits())
}

// Name is the label this session is known by. Set by the registry once there is one; until then
// it is empty and commands that report it say so.
func (s *Session) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

func (s *Session) SetName(name string) {
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
}

func (s *Session) runCommand(name, args string) {
	command, ok := commands.Find(name)
	if !ok {
		var labels []string
		for _, c := range commands.All() {
			labels = append(labels, commands.Label(c))
		}
		s.push(KindError, "unknown command /"+name+". Available: "+strings.Join(labels, " · "), "", "")
		return
	}
	ctx := commands.Context{
		NewSession: s.NewSession,
		Handoff:    s.handoff,
		Compact:    s.compact,
		Show:       s.Show,
		ShowBlock:  s.ShowBlock,
		ShowTable:  s.ShowTable,
		Name:       s.Name,
		Breakdown:  s.breakdown,
		Agents:     s.agents,
		Jobs:       s.Jobs,
		Workers:    s.Workers,
		RunSkill:   s.runSkill,
		EndSkill:   s.endSkill,
		Skills: func() (string, error) {
			dir, err := skills.Dir()
			if err != nil {
				return "", err
			}
			return skills.Report(dir)
		},
		ShowSkill: func(name string) (string, error) {
			if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
				return "", fmt.Errorf("%q is not a skill name", name)
			}
			dir, err := skills.Dir()
			if err != nil {
				return "", err
			}
			s, err := skills.Load(filepath.Join(dir, name+".yaml"))
			if err != nil {
				return "", fmt.Errorf("skill %s: %w", name, err)
			}
			return skills.Outline(s), nil
		},
	}
	// Left nil without a vault, the way the vault's tools decline to exist rather than failing
	// when called. The command then says so itself, in one place.
	if vault.Root() != "" {
		ctx.Vault = s.vault
	}
	if s.PRs != nil {
		ctx.PRs = func(arg string) (string, error) {
			s.update(func(snap *Snapshot) { snap.Busy = true })
			defer s.update(func(snap *Snapshot) { snap.Busy = false })
			return s.PRs(context.Background(), arg)
		}
	}
	if s.Land != nil {
		ctx.Land = func(arg string) (string, error) {
			s.update(func(snap *Snapshot) { snap.Busy, snap.Quiet = true, true })
			defer s.update(func(snap *Snapshot) { snap.Busy, snap.Quiet = false, false })
			return s.Land(arg)
		}
	}
	if s.Resume != nil {
		ctx.Resume = func(arg string) (string, error) {
			s.update(func(snap *Snapshot) { snap.Busy = true })
			defer s.update(func(snap *Snapshot) { snap.Busy = false })
			return s.Resume(arg)
		}
	}
	command.Run(ctx, args)
}

// Chat is a conversation with one agent, one message and reply at a time.
type Chat interface {
	Send(ctx context.Context, text string) (string, error)
	Model() string
}

// FindingChat opens a chat about one review finding, reading code in the PR's review workspace dir.
func (s *Session) FindingChat(dir string, brief subagent.FindingBrief) (Chat, error) {
	if s.Agents == nil {
		return nil, errors.New("subagents are not wired in this session")
	}
	if !isDir(dir) {
		return nil, fmt.Errorf("the review workspace %q is gone; reopen the graph", dir)
	}
	chat, err := s.Agents.FindingChat(dir, brief)
	if err != nil {
		return nil, err
	}
	return chat, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// vault reports on the section index behind vault_search. A rebuild walks every note and an
// alias run is one model call per section that has none, so the pane is marked busy for the
// duration rather than appearing to have ignored the command — the same treatment /context gets
// for the same reason.
func (s *Session) vault(action string) (string, error) {
	root := vault.Root()
	if action == "" {
		return vault.VaultReport(root, "")
	}
	s.update(func(snap *Snapshot) { snap.Busy = true })
	defer s.update(func(snap *Snapshot) { snap.Busy = false })
	return vault.VaultReport(root, action)
}

// agents is the /agents report: what each registered agent may do, then every run this session
// has dispatched. The catalogue comes first because it is the half that answers the question
// worth asking before a dispatch rather than after one.
func (s *Session) agents() report.Table {
	if s.Roster == nil {
		return report.Table{Empty: "no subagents are configured"}
	}
	t := subagent.ReportTable(s.Roster.Runs())
	if s.Agents != nil {
		t.Preamble = s.Agents.Catalogue()
	}
	return t
}

// breakdown measures the conversation as it stands. Several round trips, so the pane is marked
// busy for the duration rather than appearing to have ignored the command.
func (s *Session) breakdown() (string, error) {
	s.mu.Lock()
	history := append([]anthropic.MessageParam(nil), s.history...)
	s.mu.Unlock()

	s.update(func(snap *Snapshot) { snap.Busy = true })
	defer s.update(func(snap *Snapshot) { snap.Busy = false })

	ctx, cancel := context.WithTimeout(context.Background(), breakdownTimeout)
	defer cancel()
	report, err := s.agent.Breakdown(ctx, history)
	if err != nil {
		return "", err
	}
	return report.String(), nil
}

func (s *Session) finish(res agent.Result, sent []anthropic.MessageParam) {
	s.flushThinking()
	s.flush()

	history := res.History
	switch {
	case res.StopReason == agent.Cancelled:
		s.push(KindInfo, "⊘ cancelled", "", "")
	case res.StopReason == "end_turn":
	default:
		s.push(KindError, "stopped: "+res.StopReason, "", "")
	}
	if len(history) == 0 {
		history = sent
	}

	s.mu.Lock()
	s.history = history
	s.cancel, s.abandon = nil, nil
	s.mu.Unlock()
	s.update(func(snap *Snapshot) { snap.Busy, snap.Cancelling, snap.Steerable = false, false, false })
	s.restoreSteering()
}

// PromptRecord is one settled prompt: Kind is approval or question; Outcome approved, declined, answered, dismissed or interrupted.
type PromptRecord struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Label   string    `json:"label"`
	Reason  string    `json:"reason,omitempty"`
	Outcome string    `json:"outcome"`
	Waited  float64   `json:"waited_s"`
}

func (s *Session) logPrompt(ctx context.Context, kind, label, reason, outcome string, start time.Time) {
	if s.OnPrompt == nil {
		return
	}
	if ctx.Err() != nil {
		outcome = "interrupted"
	}
	s.OnPrompt(PromptRecord{Time: start, Kind: kind, Label: label, Reason: reason, Outcome: outcome, Waited: time.Since(start).Seconds()})
}

func verdict(ok bool) string {
	if ok {
		return "approved"
	}
	return "declined"
}

// park shows an approval and blocks until it is answered or the turn is cancelled. Factored out
// because Approve and Require differ only in whether auto mode gets to skip it, and that one
// difference is the point of having both.
func (s *Session) park(ctx context.Context, hold *awake.Hold, label, body string) bool {
	hold.Pause()
	defer hold.Resume()
	s.flush()
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.promptSeq++
	seq := s.promptSeq
	s.approval, s.approvalSeq = ch, seq
	s.snap.Pending, s.snap.PendingBody, s.snap.PendingSummary = label, body, ""
	s.refreshPromptSeq()
	s.mu.Unlock()
	s.touch()
	if command, ok := strings.CutPrefix(label, "$ "); ok && s.Explain != nil {
		explainCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.explain(explainCtx, label, command)
	}
	select {
	case okay := <-ch:
		return okay
	case <-ctx.Done():
		// Only while still parked: a later park may have replaced this one.
		s.update(func(snap *Snapshot) {
			if s.approvalSeq == seq {
				s.approval, s.approvalSeq = nil, 0
				snap.Pending, snap.PendingBody, snap.PendingSummary = "", "", ""
				s.refreshPromptSeq()
			}
		})
		return false
	}
}

func (s *Session) callbacks(isLive func() bool, hold *awake.Hold) agent.Callbacks {
	return agent.Callbacks{
		OnText: func(delta string) {
			if !isLive() {
				return
			}
			s.update(func(snap *Snapshot) { snap.Live += delta })
		},
		OnThinking: func(delta string) {
			if !isLive() {
				return
			}
			s.think(delta)
		},
		OnThinkingEnd: func() {
			if isLive() {
				s.flushThinking()
			}
		},
		// Gated like every other callback: an abandoned turn's figures describe a history this
		// session no longer owns.
		OnUsage: func(u agent.Usage) {
			if !isLive() {
				return
			}
			s.update(func(snap *Snapshot) { snap.Usage = u })
		},
		Approve: func(ctx context.Context, label, body string) bool {
			if !isLive() {
				return false
			}
			// What reaches here is what no gate decided: editor writes, dispatches, process kills.
			if s.EffectiveMode() != ModeReadOnly {
				return true
			}
			start := time.Now()
			ok := s.park(ctx, hold, label, body)
			s.logPrompt(ctx, "approval", label, "", verdict(ok), start)
			return ok
		},
		// Require ignores the mode, the way Ask does: the policy put this call to the user
		// deliberately, and a session that says yes to everything by default must not answer a
		// question that was raised because nothing else should answer it.
		Require: func(ctx context.Context, label, body, reason string) bool {
			if !isLive() {
				return false
			}
			shown := body
			if reason != "" {
				shown = strings.TrimSpace("needs your say-so: " + reason + "\n\n" + body)
			}
			start := time.Now()
			ok := s.park(ctx, hold, label, shown)
			s.logPrompt(ctx, "approval", label, reason, verdict(ok), start)
			return ok
		},
		OnScreen: func(id string, v guardian.Verdict) {
			if isLive() {
				s.holdGuardian(id, v)
			}
		},
		OnUnsandboxed: func(id string) {
			if isLive() {
				s.holdUnsandboxed(id)
			}
		},
		// Questions ignore auto mode: a turn that stops to ask is asking on purpose, and
		// answering for the user would defeat the tool.
		Ask: func(ctx context.Context, q ask.Question) *ask.Answer {
			if !isLive() {
				return nil
			}
			hold.Pause()
			defer hold.Resume()
			s.flush()
			ch := make(chan *ask.Answer, 1)
			question := q
			start := time.Now()
			var seq int
			s.update(func(snap *Snapshot) {
				s.promptSeq++
				seq = s.promptSeq
				s.answer, s.answerSeq = ch, seq
				snap.Question = &question
				s.refreshPromptSeq()
			})
			select {
			case a := <-ch:
				outcome := "answered"
				if a == nil {
					outcome = "dismissed"
				} else {
					s.countPrompt()
				}
				s.logPrompt(ctx, "question", q.Question, "", outcome, start)
				return a
			case <-ctx.Done():
				s.logPrompt(ctx, "question", q.Question, "", "", start)
				s.update(func(snap *Snapshot) {
					if s.answerSeq == seq {
						s.answer, s.answerSeq = nil, 0
						snap.Question = nil
						s.refreshPromptSeq()
					}
				})
				return nil
			}
		},
		// A tool gets its box when it starts rather than when it answers, so a command that takes
		// a minute is visible for that minute instead of reading as a pane that has stopped.
		OnToolStart: func(id, label string, kind agent.ToolKind) {
			// Here rather than with noteWrite: the edit tool writes the moment this returns.
			s.keepBaseline(label, kind)
			if !isLive() {
				return
			}
			s.flush()
			s.update(func(snap *Snapshot) {
				snap.Items = append(snap.Items, Item{
					ID: s.nextID, Kind: KindTool, Text: label, Tool: kind, Call: id, Running: true,
				})
				s.nextID++
			})
			s.classifyTool(id, label, kind)
			s.judgeTool(id)
			s.labelTool(id, label)
		},
		OnToolResult: func(id, label, output string, kind agent.ToolKind) {
			s.noteWrite(id, label, kind)
			if !isLive() {
				return
			}
			s.labelTool(id, label)
			if s.resolveTool(id, label, output, kind) {
				return
			}
			s.flush()
			s.push(KindTool, label, output, kind)
		},
		// Ungated by isLive, unlike the two above: a box only exists here because the start fired
		// while this transcript was live, and an abandoned turn's tool still answers eventually.
		// Gated, the box it left behind would spin for the rest of the session.
		OnToolEnd: func(id, name string, failed bool) {
			s.wrote(id, failed)
			s.dropRunningTool(id)
			s.toolEnded(id, name, failed, isLive())
		},
		OnRules: func(rules []agent.FiredRule) {
			if isLive() {
				s.flush()
				s.pushRules(rules)
			}
		},
		OnCall: func(call agent.Call, res anthropic.ContentBlockParamUnion) {
			if isLive() {
				s.called(call, res)
			}
		},
		// A dispatched subagent gets its box immediately, holding only the run's id. Everything
		// shown in it is read from the roster at render time, which is what lets a background
		// filing keep reporting into a box whose turn ended long ago.
		//
		// Ungated by isLive, unlike every other callback: a background run outlives the turn
		// that dispatched it by design, and a box that never appeared cannot report anything.
		OnRun: s.ShowRun,
	}
}

// pushRules adds a collapsed box headed by the fired rules' categories and count, listing one rule per item.
func (s *Session) pushRules(rules []agent.FiredRule) {
	var categories, items []string
	for _, r := range rules {
		if !slices.Contains(categories, r.Subject) {
			categories = append(categories, r.Subject)
		}
		items = append(items, "- "+r.Subject+": "+r.Title)
	}
	noun := "rules"
	if len(rules) == 1 {
		noun = "rule"
	}
	head := fmt.Sprintf("%d %s · %s", len(rules), noun, strings.Join(categories, ", "))
	s.update(func(snap *Snapshot) {
		snap.Items = append(snap.Items, Item{ID: s.nextID, Kind: KindTool, Tool: agent.KindVault,
			Text: head, Body: strings.Join(items, "\n"), Closed: true})
		s.nextID++
	})
}

// ShowRun gives a subagent run its box, including one started outside any turn.
func (s *Session) ShowRun(id int) {
	s.update(func(snap *Snapshot) {
		snap.Items = append(snap.Items, Item{ID: s.nextID, Kind: KindTool, Tool: agent.KindAgent, Run: id})
		s.nextID++
	})
	s.dispatched(id)
}

var _ = fmt.Sprintf

// PushToolForTest adds a tool item without running a turn. Exported for the UI tests, which need
// a populated transcript but no model.
func (s *Session) PushToolForTest(label, body string, tool agent.ToolKind) {
	s.push(KindTool, label, body, tool)
}

// CalledForTest reports an answered call the way a turn does, without running one.
func (s *Session) CalledForTest(call agent.Call, res anthropic.ContentBlockParamUnion) {
	s.called(call, res)
}

// ParkForTest parks an approval the way a turn does, returning the channel Approve answers on.
func (s *Session) ParkForTest(label string) <-chan bool {
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.promptSeq++
	s.approval, s.approvalSeq = ch, s.promptSeq
	s.snap.Pending, s.snap.PendingBody, s.snap.PendingSummary = label, "", ""
	s.refreshPromptSeq()
	s.mu.Unlock()
	s.touch()
	return ch
}

// ShowRunForTest puts up the box a dispatched subagent gets, without dispatching one.
func (s *Session) ShowRunForTest(run int) {
	s.update(func(snap *Snapshot) {
		snap.Items = append(snap.Items, Item{ID: s.nextID, Kind: KindTool, Tool: agent.KindAgent, Run: run})
		s.nextID++
	})
}

// StartToolForTest puts up the box a running tool gets, without running one.
func (s *Session) StartToolForTest(id, label string, tool agent.ToolKind) {
	s.update(func(snap *Snapshot) {
		snap.Items = append(snap.Items, Item{
			ID: s.nextID, Kind: KindTool, Text: label, Tool: tool, Call: id, Running: true,
		})
		s.nextID++
	})
}

// ResolveToolForTest fills in a started box the way an answered call does, so a test can set up
// the box a background command leaves behind: resolved, but still naming the call it came from.
func (s *Session) ResolveToolForTest(id, label, body string, tool agent.ToolKind) {
	s.resolveTool(id, label, body, tool)
}

// VerdictForTest lands a classifier verdict on a box without a sidecar answering one.
func (s *Session) VerdictForTest(id string, verdict classify.Verdict) { s.setVerdict(id, verdict) }

// GuardianForTest lands a guardian verdict on a box without Ollama answering one.
func (s *Session) GuardianForTest(id string, verdict guardian.Verdict) { s.setGuardian(id, verdict) }

// UnsandboxedForTest marks a started box the way a command leaving the sandbox does.
func (s *Session) UnsandboxedForTest(id string) {
	s.holdUnsandboxed(id)
	s.judgeTool(id)
}

// ThinkForTest grows an in-flight thought without running a turn.
func (s *Session) ThinkForTest(delta string) { s.think(delta) }

// PushUserForTest adds a user item without running a turn.
func (s *Session) PushUserForTest(text string) { s.push(KindUser, text, "", "") }

func (s *Session) PushAssistantForTest(text string) { s.push(KindAssistant, text, "", "") }

// PushErrorForTest adds an error item without a turn failing.
func (s *Session) PushErrorForTest(text string) { s.push(KindError, text, "", "") }

// PushRulesForTest adds the box a turn's fired rules leave in the transcript.
func (s *Session) PushRulesForTest(rules []agent.FiredRule) { s.pushRules(rules) }

// SetBusyForTest marks the session as working, so a front end can be driven through the
// mid-turn path without a turn.
func (s *Session) SetBusyForTest(busy bool) {
	s.update(func(snap *Snapshot) { snap.Busy = busy })
}

// MeasureBaseline gives the gauge a counted figure before any reply; it never replaces a real one.
func (s *Session) MeasureBaseline(ctx context.Context) {
	u, err := s.agent.Baseline(ctx)
	if err != nil {
		return
	}
	s.update(func(snap *Snapshot) {
		s.baseline = u
		if !snap.Usage.Known() || snap.Usage.Counted {
			snap.Usage = u
		}
	})
}

// SetUsageForTest reports context usage without a request having been made.
func (s *Session) SetUsageForTest(u agent.Usage) {
	s.update(func(snap *Snapshot) { snap.Usage = u })
}
