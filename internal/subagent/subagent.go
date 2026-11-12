// Package subagent runs agents on behalf of the main one.
//
// A subagent exists to keep work out of the coding conversation: the main agent knows what it
// wants done and the subagent knows how, at the cost of a second context that the main one never
// has to read. What comes back is a summary, not a transcript — specifically the subagent's last
// message and nothing else, a contract its prompt states so that it writes the report there.
//
// Everything a kind of subagent is — its prompt, its tools, its budget, whether it blocks — is a
// Definition, which is data. Adding one is a value in a slice, not a new code path, and that is
// what will make an editing subagent an addition rather than a rewrite.
package subagent

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
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/skillrun"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

const (
	// backstopSteps is turns, not tool calls, and is only there to stop a run that loops.
	backstopSteps = 250

	// maxConcurrent caps how many execute at once, which is a bound on spend and nothing else.
	// It is deliberately not a bound on the dot row: a run joins the roster before it takes a
	// slot, a pair's navigator never takes one, and a failure holds its dot until it is
	// acknowledged, so the row sees more than this and collapses to a census when it does.
	//
	// Twelve, so a big piece of work can be split across as many agents as it is worth splitting
	// across. Nothing mechanical argues for a smaller number: the SDK already retries a 429 twice
	// with backoff and honours Retry-After, and a run costs a conversation's memory and no more.
	// What it costs is money, concurrently.
	maxConcurrent = 12
)

// outOfBudget is the closing instruction for a run that hit its step limit. Stopping half-done
// is survivable; stopping without saying what is left is not.
const outOfBudget = `You are out of turns and the task is unfinished. Do not call tools. Reply with one short paragraph: what you completed, then "Still unfinished:" and what remains, named precisely enough that another run could be handed it as its whole task.`

// stoppedEarly asks for the state left behind too, since a stop can land mid-change.
const stoppedEarly = `The agent that dispatched you has stopped this run before it finished. Do not call tools. Reply with one short paragraph: what you completed, what state you left things in (any half-made change, and where), then "Still unfinished:" and what remains, named precisely enough that another run could be handed it as its whole task.`

// contextFull is outOfBudget for a run whose window filled before its budget did.
const contextFull = `Your context window is full and the task is unfinished. Do not call tools. Reply with one short paragraph: what you completed, what state you left things in (any half-made change, and where), then "Still unfinished:" and what remains, named precisely enough that another run could be handed it as its whole task.`

// closingWithin bounds a stopped run's closing report, so a run whose report never comes still ends.
var closingWithin = 2 * time.Minute

// reportContract is appended to every subagent's system prompt, because it describes the loop
// rather than the job: only the last message survives, and a subagent that does not know that
// writes its findings somewhere they will be thrown away. Every definition gets it, so a new
// kind of subagent cannot be added having forgotten it.
const reportContract = `How your work is returned: only your FINAL message reaches the agent that dispatched you. Everything else is discarded — earlier messages, your tool calls and their output, your reasoning. The dispatching agent sees one string, and that string is the last thing you write.

So do not narrate. Say nothing at all while you work: no "I'll start by...", no commentary between tool calls, no progress notes. Then make your last message the entire report, written to stand on its own for a reader who has seen none of what you saw. Anything you leave out of it is lost.`

// schemaContract replaces it for an agent answering in fields. The silence instruction is the
// same and matters more: under a schema every word the model writes has to be the JSON object,
// so narration is not merely wasteful but malformed.
const schemaContract = `How your work is returned: only your FINAL message reaches the agent that dispatched you, and it must be a single JSON object matching the schema you have been given. Everything else is discarded — earlier messages, your tool calls and their output, your reasoning.

So do not narrate. Say nothing at all while you work: no commentary between tool calls, no progress notes. Write nothing but the closing object, and put the whole account of what you did inside its fields, written to stand on its own for a reader who has seen none of what you saw. Anything you leave out of it is lost.`

func (d *Definition) contract() string {
	if d.Report != nil {
		return schemaContract
	}
	return reportContract
}

// Definition is a kind of subagent.
type Definition struct {
	// Name is what the main agent passes as the agent argument, and what the UI labels runs with.
	Name string
	// Description is what the main agent reads when choosing. It decides whether delegation
	// happens at all, so it says what the agent is for and what it cannot do.
	Description string

	// System is rebuilt per run rather than captured once, which costs nothing for the agents that
	// return a constant.
	System func() string
	// Tools and Handlers are the subagent's own. The dispatch tool is deliberately not among
	// them: a subagent cannot dispatch a subagent.
	Tools    func() []anthropic.ToolUnionParam
	Handlers map[string]agent.Handler
	// Fallback runs when no handler matches, which is the only way to reach a tool whose name is
	// not known at construction — every MCP tool, since servers connect late.
	//
	// Nil means an unmatched name errors, and that is the right default: the main agent's fallback
	// sends anything it does not recognise to bash, so inheriting it would append a shell to every
	// subagent. An agent that wants MCP sets agent.McpHandler here, which refuses non-MCP names
	// rather than running them.
	Fallback agent.Handler

	// Model defaults to the coding model. Nil-safe, and read per run so an env override lands
	// without a restart.
	Model func() string
	// MaxSteps is model turns, not tool calls. Zero takes backstopSteps.
	MaxSteps int

	// Explore lets the manager's routing send a random share of this agent's runs to the cheap tier.
	Explore bool

	// Upkeep marks an agent alux usually starts itself, listed apart so it does not crowd the main agent's choice.
	Upkeep bool

	// Background runs return to the main agent immediately and report into the transcript when
	// they land. It is a property of the kind of work, not an argument the model may pass:
	// filing is always worth detaching, research never is, and leaving the choice to the model
	// makes it a knob to get wrong.
	Background bool

	// Lock serialises runs of this agent across every alux on the machine. The vault has one
	// because two sessions filing at once produced duplicate notes; nil means no serialisation.
	Lock func(ctx context.Context, job func() error) error

	// Trail turns one of the subagent's tool calls into the line the transcript box shows for
	// it, or "" to leave it out. Nil records every call's label, which is what a read-only agent
	// wants; the vault overrides it to report its writes and stay quiet about its looking around.
	// The kind is the one the handler reported, which is how a read is told from a write without
	// the callback having to carry a tool name it has no other use for.
	Trail func(kind agent.ToolKind, label, output string) string

	// Alias gives this agent a named tool of its own beside the generic dispatch.
	Alias *Alias

	// Report makes this agent answer in fields rather than prose. Nil is prose, which is what
	// research wants: its output is read, not branched on.
	Report *ReportSchema

	// Peer gives this agent a counterpart to talk to, as one extra tool whose handler holds a
	// conversation with a second agent. Nil is an agent that works alone, which is all of them
	// but the pair. See pair.go for why the conversation is a tool call rather than a channel.
	Peer *PeerSpec

	// Policy bounds what this agent may do with the tools it has, for the distinctions its tool
	// list cannot draw: which commands bash may run, which paths may be written, which of a
	// server's MCP tools are reachable. Nil is unbounded within its handlers, which is right for
	// an agent whose handlers are already the whole limit — research can only view, and the vault
	// filer's five tools resolve every path under one root.
	//
	// A policy is not a substitute for a narrow tool list. Withholding a tool is still the
	// strongest thing available; this is for the three tools whose name does not say what the call
	// will do.
	Policy *agent.Rules

	// Commands is what this agent's work involves; required of every agent, shell or not.
	Commands *guardian.Criteria

	// At builds this agent scoped to dir instead of the repository; nil means it cannot be pointed elsewhere.
	At func(dir string) *Definition
	// Layered runs each dispatch in its own workspace from the manager's Layers, when it has any.
	Layered bool

	// Skill is a skill every run opens before its first turn and must finish or fail; its final check joins the report.
	Skill string
	// SkillDir is where Skill is read from; nil means skills.Dir.
	SkillDir func() (string, error)
	// skillInputs is bound into Skill when it opens; set only on DispatchSkill's copy.
	skillInputs map[string]any
	// Seedable lets a dispatch hand this agent files to start from; Seeded is its system prompt when it was handed some, nil keeping System.
	Seedable bool
	Seeded   func() string
	// seeds are re-read into the first request; asked is the dispatch's seed input as given. Both set only on dispatchDef's copy.
	seeds []seedFile
	asked []Seed
	// Backlog counts the work an automatic run dispatched from session would find; nil means always run.
	Backlog func(session string) (int, error)
}

// validate rejects a definition that cannot mean what it says. Called by NewManager, so a
// misconfigured agent stops alux at boot rather than at its first dispatch — the failure
// is in the definition, which is code, and code that cannot work should not start.
func (d *Definition) validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("a definition has no name")
	}
	if err := d.validateCommands(); err != nil {
		return err
	}
	if err := d.validatePeer(); err != nil {
		return err
	}
	if d.Skill != "" && (d.Peer != nil || d.Report != nil) {
		return fmt.Errorf("agent %s runs a skill, which reports in prose and works alone", d.Name)
	}
	if d.Policy != nil {
		if err := d.Policy.Validate(); err != nil {
			return fmt.Errorf("agent %s: %w", d.Name, err)
		}
	}
	// The hole this closes, which is a hang rather than a wrong answer. A background run outlives
	// the turn that dispatched it: the session's per-turn liveness only goes false when the user
	// abandons a turn, not when one ends normally, so a background subagent's callbacks still
	// reach a session whose turn is over. An approval raised there parks on the session's single
	// approval field from a dead turn, racing whatever the current turn puts in it — last writer
	// wins and the other caller waits on a channel nobody will answer.
	//
	// Manager.approving does not help: it serialises the approval taken at dispatch, not the ones
	// a subagent's own tool calls raise. So a background agent must be fully decided in advance,
	// and that is checkable here rather than hoped for.
	if d.Background {
		if err := d.detachable(); err != nil {
			return fmt.Errorf("agent %s runs in the background by default, but %w", d.Name, err)
		}
	}
	return nil
}

func (d *Definition) validateCommands() error {
	if d.Commands == nil {
		return fmt.Errorf("agent %s declares no command criteria; every agent must say what kinds of shell command its work involves", d.Name)
	}
	if d.Commands.Agent != d.Name {
		return fmt.Errorf("agent %s carries command criteria written for %q", d.Name, d.Commands.Agent)
	}
	return d.Commands.Validate()
}

// detachable reports why this agent must not run detached, or nil when it may.
//
// Asked at dispatch as well as at construction, because the model may ask for a background run
// the definition did not default to. A check that lived only in validate would be a check the
// override walked straight past.
//
// A peer is allowed. A conversation nobody is watching bills for both halves of itself, but it
// reports like any other run — the pair's report reaches the agent, and both halves appear in
// the roster while they run — so what is left of the objection is a cost, not a contradiction.
// Spending is the user's call to watch, which is what the roster is for.
func (d *Definition) detachable() error {
	if d.Policy != nil && d.Policy.CanAsk() {
		return errors.New("it has a rule that stops to ask, and there is nobody to ask once the " +
			"turn that dispatched it has ended. Make the rule allow or deny")
	}
	return nil
}

// validatePeer rejects a pairing that cannot work. Every check here is something that would
// otherwise fail in the middle of a run: a tool the driver is offered and then refused, an
// approval raised where nothing can answer it, a chain of peers with no end.
func (d *Definition) validatePeer() error {
	if d.Peer == nil {
		return nil
	}
	spec := d.Peer
	if spec.Def == nil {
		return fmt.Errorf("agent %s has a peer with no definition", d.Name)
	}
	if strings.TrimSpace(spec.Tool) == "" {
		return fmt.Errorf("agent %s's peer has no tool name, so there is no way to reach it", d.Name)
	}
	// A pair is two agents. A chain of them is a budget nobody is counting, in the one direction
	// the roster cannot show — a peer's rounds are not a run's steps.
	if spec.Def.Peer != nil {
		return fmt.Errorf("agent %s's peer %s has a peer of its own", d.Name, spec.Def.Name)
	}
	// The driver is inside its own loop for every exchange, and the session holds a single parked
	// approval. A peer that stops to ask would park on a slot the driver is already standing in
	// front of, and both wait forever. Same hole as a background run's, reached from the other side.
	if err := peerPolicy(d.Name, spec.Def); err != nil {
		return err
	}
	if p := spec.Planner; p != nil {
		if err := peerPolicy(d.Name, p); err != nil {
			return err
		}
		if p.Peer != nil {
			return fmt.Errorf("agent %s's planner %s has a peer of its own", d.Name, p.Name)
		}
		// Read-only is its handlers; a fallback reaches whatever name no handler matches.
		if p.Fallback != nil {
			return fmt.Errorf("agent %s's planner %s has a fallback, so it is not limited to its read-only handlers",
				d.Name, p.Name)
		}
	}
	// Offered and then refused is the worst of both: the driver spends a turn learning what this
	// check could have said at boot.
	if d.Policy != nil && !d.Policy.Permits(spec.Tool) {
		return fmt.Errorf("agent %s is offered the %s tool but its own policy denies it", d.Name, spec.Tool)
	}
	return nil
}

func peerPolicy(driver string, peer *Definition) error {
	if err := peer.validateCommands(); err != nil {
		return fmt.Errorf("agent %s's peer: %w", driver, err)
	}
	if peer.Policy == nil {
		return nil
	}
	if err := peer.Policy.Validate(); err != nil {
		return fmt.Errorf("agent %s's peer %s: %w", driver, peer.Name, err)
	}
	if peer.Policy.CanAsk() {
		return fmt.Errorf("agent %s's peer %s cannot have a rule that asks: it runs inside its driver's loop, "+
			"which may already be holding the session's one approval, so the question would never be put. "+
			"Make the rule allow or deny", driver, peer.Name)
	}
	return nil
}

// ReportSchema is a schema the subagent's replies must satisfy, plus the way its fields are
// turned back into the line the dispatching agent reads.
//
// Rendering rather than forwarding the JSON is the point. The schema exists so alux can
// rely on a field being there — a filing that changed nothing says so in a boolean instead of by
// the prose convention of answering "NONE" — while what reaches the main agent stays the kind of
// sentence it reads everywhere else.
type ReportSchema struct {
	Schema map[string]any
	// Render turns one conforming reply into that line. It is handed the raw JSON so a
	// definition can unmarshal into its own struct rather than reading a map of any.
	Render func(raw json.RawMessage) string
}

// render applies the schema's renderer, reporting whether the reply conformed. A reply that does
// not parse is not an error: truncation at max_tokens produces half an object, and the half-
// written prose inside it is worth more to the main agent than a failure would be.
func (r *ReportSchema) render(reply string) (string, bool) {
	trimmed := strings.TrimSpace(reply)
	if trimmed == "" || r.Render == nil {
		return "", false
	}
	if !json.Valid([]byte(trimmed)) {
		return "", false
	}
	return r.Render(json.RawMessage(trimmed)), true
}

// Alias is a tool that dispatches one specific agent. vault_note is the only one, kept because
// it has been working: a well-tuned description the main agent reaches for reflexively beats a
// generic call it has to remember to make.
type Alias struct {
	Name        string
	Description string
	// TaskArg describes the single task property, since a wrapper's whole job is to ask for the
	// one argument in the words that suit it.
	TaskArg string
}

func (d *Definition) steps() int {
	if d.MaxSteps > 0 {
		return d.MaxSteps
	}
	return backstopSteps
}

func (d *Definition) model() string {
	if d.Model != nil {
		if m := d.Model(); m != "" {
			return m
		}
	}
	return llm.Model()
}

func (d *Definition) trail(kind agent.ToolKind, label, output string) string {
	if d.Trail != nil {
		return d.Trail(kind, label, output)
	}
	return label
}

// Manager owns the definitions and the roster. One per session.
type Manager struct {
	Roster *Roster
	// Layers, when set, gives every run of a Layered agent its own workspace.
	Layers Layers

	defs  map[string]*Definition
	order []string

	// sem caps concurrent runs. Background runs take it inside their own goroutine, so a queued
	// filing never blocks the turn that dispatched it.
	sem chan struct{}

	// approving serialises the approval prompt across concurrent dispatches. See dispatch.
	approving sync.Mutex

	// Client is explicit for the same reason the agent's is: a package singleton binds to the
	// first base URL it ever sees, which makes the loop untestable.
	Client *anthropic.Client

	// Notify puts a line in the ambient notice strip, for a background run reporting in long
	// after its turn ended. Nil is fine.
	Notify func(text string)

	// Landed hears every run a dispatch tool call started once it ends, detached or not. Nil hears none.
	Landed func(call agent.Call, failed bool, check *skills.SkillCheck)

	// Log receives one record per finished run. Nil logs nothing.
	Log func(Dispatch)
	// PeerLog receives every message a pair's navigator is sent and its reply. Nil logs nothing.
	PeerLog func(PeerExchange)
	// Transcripts is the directory each run's transcript is written to, by dispatch id. Empty writes none.
	Transcripts string
	// Session is copied into every record, naming the process that made it.
	Session string

	// Routing is read per run; nil or false keeps every agent on its own model.
	Routing func() (Routing, bool)
	// Tier scores an exploring run's task for Routing; nil keeps every agent on its own model.
	Tier Tierer

	// Guardian judges every subagent's shell commands. Nil judges none, so every command is stopped.
	Guardian *guardian.Client
	// screener replaces the guardian gate in tests.
	screener func(c guardian.Criteria, unattended func() bool) agent.Screener

	// Sandbox runs every subagent's shell commands under Seatbelt instead of guardian.
	Sandbox bool
}

func (m *Manager) screen(def *Definition, unattended func() bool) agent.Screener {
	if m.screener != nil {
		return m.screener(*def.Commands, unattended)
	}
	return guardian.Gate{Client: m.Guardian, Criteria: *def.Commands, Unattended: unattended}
}

// sandbox is def's sandbox, or nil when it is off: read-only for agents whose criteria allow only Read, else writes to its root and no network.
func (m *Manager) sandbox(def *Definition) func(context.Context, [][]string) (sandbox.Wrap, error) {
	if !m.Sandbox {
		return nil
	}
	write := def.Commands != nil && !slices.Equal(def.Commands.Allowed, []guardian.Category{guardian.Read})
	return func(ctx context.Context, _ [][]string) (sandbox.Wrap, error) {
		root, ok := tools.Scoped(ctx)
		if !ok {
			root = project.Root()
		}
		return sandbox.Build(sandbox.Profile{Root: root, Write: write, ReadOnly: "this agent only reads"})
	}
}

// NewManager registers the definitions, rejecting any that cannot work. The error is a
// programming error rather than a user one — definitions are code — so the caller's only
// reasonable response is to refuse to start, which is the point: a capability gate that was
// configured wrongly must not run in a weakened form.
//
// A nil definition is skipped rather than refused, because that is how an agent that needs
// something absent declines to exist.
func NewManager(roster *Roster, defs ...*Definition) (*Manager, error) {
	m := &Manager{
		Roster: roster,
		defs:   map[string]*Definition{},
		sem:    make(chan struct{}, maxConcurrent),
	}
	for _, def := range defs {
		if def == nil {
			continue
		}
		if err := def.validate(); err != nil {
			return nil, err
		}
		if _, clash := m.defs[def.Name]; clash {
			return nil, fmt.Errorf("two agents are both named %s", def.Name)
		}
		m.defs[def.Name] = def
		m.order = append(m.order, def.Name)
	}
	return m, nil
}

// Names lists the registered agents in registration order, which is the order the dispatch
// tool's enum presents them in.
func (m *Manager) Names() []string { return m.order }

func (m *Manager) notify(text string) {
	if m.Notify != nil {
		m.Notify(text)
	}
}

// Dispatch runs one subagent. Foreground runs block until the subagent is done and return its
// report; background runs return a line telling the main agent not to wait, and report
// themselves into the transcript and the next turn when they land.
func (m *Manager) Dispatch(ctx context.Context, name, task, extra string, cb agent.Callbacks) (string, error) {
	return m.DispatchWith(ctx, name, task, extra, nil, cb)
}

// DispatchWith is Dispatch with the model's answer to whether this one should detach. Nil takes
// the definition's default, which is what every caller that has no opinion passes.
//
// Refused rather than quietly downgraded when the agent cannot detach: a model that asked for
// background and got foreground would carry on as though its turn were free, and the first thing
// it noticed would be the wait.
func (m *Manager) DispatchWith(ctx context.Context, name, task, extra string, detach *bool, cb agent.Callbacks) (string, error) {
	return m.dispatchWith(ctx, agent.Call{}, name, "", task, extra, "", detach, nil, cb)
}

// land tells Landed that the run a dispatch tool call started has ended, with its final report on the check.
func (m *Manager) land(call agent.Call, h *handle, report string, err error) {
	if call.ID == "" || m.Landed == nil {
		return
	}
	h.roster.mu.Lock()
	check := h.skill
	h.roster.mu.Unlock()
	if check != nil {
		withReport := *check
		withReport.Report = report
		check = &withReport
	}
	m.Landed(call, err != nil, check)
}

func (m *Manager) dispatchWith(ctx context.Context, call agent.Call, name, label, task, extra, base string, detach *bool, seeds []Seed, cb agent.Callbacks) (string, error) {
	def, ok := m.defs[name]
	if !ok {
		return "", fmt.Errorf("no such agent %q; available: %s", name, strings.Join(m.order, ", "))
	}
	report, _, err := m.dispatchDef(ctx, call, def, "", nil, "", base, seeds, label, task, extra, detach, cb)
	return report, err
}

// DispatchSkill starts a detached run of agent name with skill pinned and inputs bound; Landed hears call when it ends.
// A non-empty change is the jj change its workspace edits in place.
func (m *Manager) DispatchSkill(call agent.Call, name, skill string, inputs map[string]any, change, task string, cb agent.Callbacks) (runID int, err error) {
	if err := m.CheckSkillAgent(name, skill); err != nil {
		return 0, err
	}
	detach := true
	_, runID, err = m.dispatchDef(m.Roster.Context(), call, m.defs[name], skill, inputs, change, "", nil, skill, task, "", &detach, cb)
	return runID, err
}

// CheckSkillAgent says why agent name cannot run skill, or nil.
func (m *Manager) CheckSkillAgent(name, skill string) error {
	def, ok := m.defs[name]
	switch {
	case !ok:
		return fmt.Errorf("no such agent %q; available: %s", name, strings.Join(m.order, ", "))
	case def.Peer != nil:
		return fmt.Errorf("%s cannot run %s: it works with a peer", name, skill)
	case def.Report != nil:
		return fmt.Errorf("%s cannot run %s: it reports to a schema, not in prose", name, skill)
	case skill == "" || strings.ContainsAny(skill, `/\`) || strings.HasPrefix(skill, "."):
		return fmt.Errorf("%s cannot run %q: not a skill name", name, skill)
	}
	dir := skills.Dir
	if def.SkillDir != nil {
		dir = def.SkillDir
	}
	root, err := dir()
	if err != nil {
		return fmt.Errorf("%s cannot run %s: %w", name, skill, err)
	}
	s, err := skills.Load(filepath.Join(root, skill+".yaml"))
	if err != nil {
		return fmt.Errorf("%s cannot run %s: %w", name, skill, err)
	}
	if !slices.Contains(s.Starts, string(skillrun.ByModel)) {
		return fmt.Errorf("%s cannot run %s: it starts only when the user asks for it", name, skill)
	}
	var offered []string
	if def.Tools != nil {
		for _, t := range def.Tools() {
			if n := t.GetName(); n != nil {
				offered = append(offered, *n)
			}
		}
	}
	var missing []string
	for _, t := range s.Requires.Tools {
		if !slices.Contains(offered, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s cannot run %s: it has no %s", name, skill, strings.Join(missing, ", "))
	}
	return nil
}

// dispatchDef runs def, with skill and inputs pinned in place of its own when skill is set, and returns its report and run id.
func (m *Manager) dispatchDef(ctx context.Context, call agent.Call, def *Definition, skill string, inputs map[string]any, change, base string,
	seeds []Seed, label, task, extra string, detach *bool, cb agent.Callbacks) (string, int, error) {
	report, id, err := m.dispatchSeeded(ctx, call, def, skill, inputs, change, base, seeds, label, task, extra, detach, cb)
	if err == nil && len(seeds) > 0 && !def.Seedable {
		report = strings.TrimSpace(report) + "\n\nThe seeds were ignored: only coder, pair and research take seeds."
	}
	return report, id, err
}

func (m *Manager) dispatchSeeded(ctx context.Context, call agent.Call, def *Definition, skill string, inputs map[string]any, change, base string,
	seeds []Seed, label, task, extra string, detach *bool, cb agent.Callbacks) (string, int, error) {
	name := def.Name
	background := def.Background
	if detach != nil {
		background = *detach
	}
	if background && !def.Background {
		if err := def.detachable(); err != nil {
			return "", 0, fmt.Errorf("agent %s cannot run in the background: %w", def.Name, err)
		}
	}

	var layer Layer
	layered := def.Layered && m.Layers != nil && def.At != nil
	switch {
	case change != "" && !layered:
		return "", 0, fmt.Errorf("agent %s cannot edit change %s: it has no workspace of its own", def.Name, change)
	case base != "" && base != BaseParent && base != BaseTrunk:
		return "", 0, fmt.Errorf("agent %s cannot start from %q: base is %q or %q", def.Name, base, BaseParent, BaseTrunk)
	case base != "" && !layered:
		return "", 0, fmt.Errorf("agent %s cannot start from %s: it has no workspace of its own", def.Name, base)
	case base != "" && change != "":
		return "", 0, fmt.Errorf("agent %s cannot start from %s: it edits change %s in place, which has a base already", def.Name, base, change)
	}
	if base == "" {
		base = BaseParent
	}
	if layered {
		var err error
		if layer, err = m.Layers.Open(ctx, firstLine(task), change, base); err != nil {
			return "", 0, fmt.Errorf("agent %s was not started: making its workspace failed: %w", def.Name, err)
		}
		if def = def.At(layer.Dir()); def == nil {
			_, err := layer.Close(ctx)
			return "", 0, errors.Join(fmt.Errorf("agent %s cannot be scoped to %s", name, layer.Dir()), err)
		}
		extra = layerNote(layer, extra)
	}
	if skill != "" {
		pinned := *def
		pinned.Skill, pinned.skillInputs = skill, inputs
		def = &pinned
	}
	if len(seeds) > 0 {
		pinned := *def
		pinned.asked = seeds
		if def.Seedable {
			dir, roots := tools.Dir(ctx), []string(nil)
			if layer != nil {
				dir, roots = layer.Dir(), []string{layer.Origin(), project.Root()}
			}
			pinned.seeds = resolveSeeds(seeds, dir, roots...)
		}
		def = &pinned
	}

	h := m.Roster.start(def.Name, label, task, def.steps(), background)
	if call.ID != "" {
		h.set(func(run *Run) { run.Call = call.ID })
	}
	// Announced before it runs, so the box is in the transcript tracking the run rather than
	// appearing when it is already over. It is the same box either way: the difference between
	// foreground and background is who waits, not what is shown.
	if cb.OnRun != nil {
		cb.OnRun(h.run.ID)
	}

	if !background {
		runCtx := ctx
		if layer != nil {
			runCtx = tools.WithDir(ctx, layer.Dir())
		}
		raw, runErr := m.execute(runCtx, def, task, extra, h, cb)
		report, err := m.closeInto(ctx, h, layer, raw, runErr)
		h.end(raw, runErr)
		if def.Layered && layer == nil && err == nil {
			report = strings.TrimSpace(report) + "\n\n" + inCheckout
		}
		m.land(call, h, report, err)
		return report, h.run.ID, err
	}

	// Deliberately detached from the turn's context: a background run must survive the turn that
	// started it, and cancelling the turn should not abandon a half-written note.
	//
	// Not context.Background() though, which is what this was: a run that can never be cancelled
	// is a run that can never be given up on, and a detached run can wait — on the global slot,
	// or on a cross-process lock another pane is holding. The roster's context is cancelled when
	// the session quits and at no other time, so the run still outlives the turn that made it.
	runCtx := m.Roster.Context()
	if layer != nil {
		runCtx = tools.WithDir(runCtx, layer.Dir())
	}
	m.detach(runCtx, def, task, extra, h, cb, layer, func(report string, err error) { m.land(call, h, report, err) })

	if c := editedChange(layer); c != "" {
		return fmt.Sprintf("Started in the background as run %d, in its own workspace on change %s; it edits that change in place, "+
			"and %s reports what it touched and whether the change is conflicted. Carry on — do not wait for it.",
			h.run.ID, c, LandTool), h.run.ID, nil
	}
	if onTrunk(layer) {
		return fmt.Sprintf("Started in the background as run %d, in its own workspace from %s; its edits wait as their own change "+
			"on trunk, to push rather than land with %s. Carry on — do not wait for it.", h.run.ID, trunkOf(layer), LandTool), h.run.ID, nil
	}
	if layer != nil {
		return fmt.Sprintf("Started in the background as run %d, in its own workspace from @-; its edits wait as their own change "+
			"until you call %s.%s Carry on — do not wait for it.", h.run.ID, LandTool, unseenNote(layer)), h.run.ID, nil
	}
	if def.Layered {
		return fmt.Sprintf("Started in the background as run %d; it will report in the transcript when it lands. %s "+
			"Carry on — do not wait for it.", h.run.ID, inCheckout), h.run.ID, nil
	}
	return fmt.Sprintf("Started in the background as run %d; it will report in the transcript when it lands. "+
		"Carry on — do not wait for it.", h.run.ID), h.run.ID, nil
}

// detach runs h in the background, closes its layer (if any), queues its outcome, then calls onEnd (if any) with the run's report and error.
func (m *Manager) detach(ctx context.Context, def *Definition, task, extra string, h *handle, cb agent.Callbacks, layer Layer, onEnd func(report string, err error)) {
	m.Roster.wg.Add(1)
	go func() {
		defer m.Roster.wg.Done()
		raw, runErr := m.execute(ctx, def, task, extra, h, cb)
		report, err := m.closeInto(ctx, h, layer, raw, runErr)
		h.end(raw, runErr)
		summary := describe(def, report, err)
		m.notify(summary)
		// Queued for the next turn as well as shown. A background run that announces itself only
		// into the transcript reaches an agent that has stopped reading it; this reaches one that
		// can still act on it. The strip gets the summary and the agent gets the whole report:
		// one line is a headline, and a detached research agent's findings are the product.
		outcome := Outcome{Dispatch: h.run.Dispatch, Agent: def.Name, Summary: summary, Report: report}
		if err != nil {
			outcome.Err = err.Error()
		}
		h.queue(outcome)
		if onEnd != nil {
			onEnd(report, err)
		}
	}()
}

// none is the reply an agent gives when it found nothing worth reporting. A constant because the
// notice strip and the message the agent reads both test for it, and a sentinel spelled twice is
// one rename away from only one of them noticing.
const none = "NONE"

// describe is the one line a finished background run is announced by, in the notice strip and
// in the next turn's context.
func describe(def *Definition, report string, err error) string {
	prefix := def.Name + ": "
	switch {
	case err != nil:
		return prefix + "failed — " + err.Error()
	case report == "" || report == none:
		return prefix + "nothing to add — already recorded"
	default:
		return prefix + firstLine(report)
	}
}

// execute is the run itself, under whatever lock the definition carries.
func (m *Manager) execute(ctx context.Context, def *Definition, task, extra string, h *handle, outer agent.Callbacks) (string, error) {
	t := m.newTally(def, task, extra, h)
	t.set(func(rec *Dispatch) { rec.Cwd = tools.Dir(ctx) })
	t.transcript = newRecorder(m.Transcripts, h.run.Dispatch)
	t.transcript.add(TranscriptEvent{Kind: TranscriptTask, Name: def.Name, Text: task, Output: extra})
	report, err := m.run(ctx, def, task, extra, h, outer, t)
	t.transcript.finish(report, err)
	if m.Log != nil {
		t.set(func(rec *Dispatch) {
			rec.Report = report
			if err == nil {
				return
			}
			rec.Err = err.Error()
			switch {
			case rec.Stop != "":
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				rec.Stop = StopCancelled
			default:
				rec.Stop = StopError
			}
		})
		m.Log(t.record())
	}
	return report, err
}

func (m *Manager) newTally(def *Definition, task, extra string, h *handle) *tally {
	cwd, _ := os.Getwd()
	return &tally{from: time.Now(), rec: Dispatch{
		ID:         h.run.Dispatch,
		Session:    m.Session,
		Time:       time.Now(),
		PID:        os.Getpid(),
		Cwd:        cwd,
		Agent:      def.Name,
		Background: h.detached(),
		Task:       task,
		Context:    extra,
		SeedsAsked: def.asked,
		MaxSteps:   def.steps(),
	}}
}

func (m *Manager) run(ctx context.Context, def *Definition, task, extra string, h *handle, outer agent.Callbacks, t *tally) (string, error) {
	work, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	defer h.stoppable(stop)()

	var report string
	job := func() error {
		// The global slot is taken here, inside whatever lock the definition carries, rather than
		// before the lock is asked for. Taken the other way round, runs queued on one agent's own
		// lock would each hold a slot while waiting and starve every other agent: four queued
		// agents sharing one lock can hold every slot with a research run unable to start at all.
		// Only the vault carries a lock today, and it is cross-process: another pane can hold it
		// for as long as it likes.
		select {
		case m.sem <- struct{}{}:
			defer func() { <-m.sem }()
		case <-work.Done():
			return fmt.Errorf("abandoned before it started: %w", context.Cause(work))
		}
		var err error
		report, err = m.loop(ctx, work, def, task, extra, h, outer, t)
		return err
	}

	var err error
	if def.Lock != nil {
		err = def.Lock(work, job)
	} else {
		err = job()
	}

	if err != nil {
		return "", err
	}
	return report, nil
}

// loop runs under work, which Stop cancels; ctx outlives a stop so the closing report can be written.
func (m *Manager) loop(ctx, work context.Context, def *Definition, task, extra string, h *handle, outer agent.Callbacks, t *tally) (report string, err error) {
	browser := fmt.Sprintf("alux-%d-run%d", os.Getpid(), h.run.ID)
	work = agent.WithBrowserSession(work, browser)
	work = agent.WithRecordSession(work, m.Session)
	defer agent.CloseBrowserSession(browser)
	sh, err := shell.New(tools.Dir(work))
	if err != nil {
		return "", err
	}
	defer sh.Close()
	work = shell.With(work, sh)
	inner := &agent.Agent{
		Client:   m.Client,
		System:   def.System() + "\n\n" + def.contract(),
		Standing: agent.NewRuleInjector(def.Name),
		Tools:    def.Tools,
		Handlers: m.wrap(def, h),
		Fallback: announce(def.Fallback, h),
		MaxSteps: def.steps(),
		Policy:   def.Policy,
		Screen:   m.screen(def, h.detached),
		Sandbox:  m.sandbox(def),

		ClearToolResults: true,
	}
	if def.Report != nil {
		inner.OutputSchema = def.Report.Schema
	}
	var skill *skillrun.Host
	if def.Skill != "" {
		var closing string
		skill = &skillrun.Host{
			Dir:    def.SkillDir,
			Pinned: true,
			Closed: func(s *skills.Skill, res skills.Result, how string) {
				closing = skills.Summary(s.Name, how, res)
				check := &skills.SkillCheck{Name: s.Name, Finished: how == skillrun.Finished && res.Complete, Waivers: res.Waivers}
				h.roster.mu.Lock()
				h.skill = check
				h.roster.mu.Unlock()
			},
		}
		skill.Wire(inner)
		if inner.Policy != nil {
			rules := *inner.Policy
			rules.Tools = append(agent.AllowTools(skillrun.FinishTool, skillrun.FinishStageTool, skillrun.FailTool), rules.Tools...)
			inner.Policy = &rules
		}
		brief, err := skill.Start(def.Skill, def.skillInputs)
		if err != nil {
			return "", err
		}
		task += "\n\n" + brief
		defer func() {
			skill.End("ended when the agent stopped without finishing it")
			if err == nil && closing != "" {
				report = strings.TrimSpace(report + "\n\n" + closing)
			}
		}()
	}
	// Picked after the skill brief is bound, because a skill's dispatch task is only a pointer to it.
	choice := def.pick(work, m.Routing, m.Tier, tierText(def.Name, promptText(task, extra)))
	inner.Model = choice.Model
	h.routed(choice)
	t.transcript.add(TranscriptEvent{Kind: TranscriptModel, Name: choice.Model})
	t.set(func(rec *Dispatch) {
		rec.Model, rec.Tier, rec.TierScores, rec.Inputs = choice.Model, choice.Tier, choice.Scores, def.skillInputs
	})
	first := prompt(task, extra)
	// The peer is built per run, not per definition: its conversation belongs to this piece of
	// work and must not be carried into the next dispatch. Injected here rather than in the
	// definition because this is the first point at which there is a run to attach it to.
	var companion *peer
	if def.Peer != nil {
		ph := m.Roster.startPeer(h.run.ID, h.run.Dispatch, def.Peer.Def.Name, h.run.Name, def.Peer.exchanges(def.steps()))
		// Its own box, announced like any other run's and immediately after the driver's, so the
		// two sit together in the transcript.
		if outer.OnRun != nil {
			outer.OnRun(ph.run.ID)
		}
		companion = m.newPeer(def.Peer, ph, h, def.steps())
		if companion.shell, err = shell.New(tools.Dir(work)); err != nil {
			return "", err
		}
		defer companion.shell.Close()
		if plan := companion.plan(work, promptText(task, extra), def.seeds); plan != "" {
			first = []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(
				withPlan(promptText(task, extra), plan, def.Peer.Tool)))}
		}
		offered, ask := def.Tools, def.Peer.tool()
		inner.Tools = func() []anthropic.ToolUnionParam { return append(offered(), ask) }
		// Safe to write into: wrap builds a fresh map rather than handing back the definition's.
		inner.Handlers[def.Peer.Tool] = announce(companion.handler(), h)
		// Asking is not progress on the task, so it does not come out of the budget for making
		// it. Charging for it put the two budgets in competition and the driver resolved it the
		// wrong way: a run that is short on turns stops asking exactly when it should not.
		inner.Free = map[string]bool{def.Peer.Tool: true}
		inner.ExcludeTools = append(inner.ExcludeTools, def.Peer.Tool)
		inner.Pending = companion.pending(work)
		if seeded, read := companion.seedReads(work, inner); len(seeded) > 0 {
			first = append(first, seeded...)
			from := "the plan"
			if len(def.seeds) > 0 {
				from = "the dispatch and the plan"
			}
			h.appendTrail(Entry{Kind: EntryNote, Text: seededNote(len(read), from)})
			recordSeeds(t, read)
			if def.Peer.Seeded != nil {
				inner.System = def.Peer.Seeded() + "\n\n" + def.contract()
			}
		}
	} else if seeded, read := seedViews(work, inner, def.seeds); len(seeded) > 0 {
		first = append(first, seeded...)
		h.appendTrail(Entry{Kind: EntryNote, Text: seededNote(len(read), "the dispatch")})
		recordSeeds(t, read)
		if def.Seeded != nil {
			inner.System = def.Seeded() + "\n\n" + def.contract()
		}
	}
	if h.detached() {
		offerAsk(inner, m.askMain(h), h)
	}

	cb := m.callbacks(def, h, outer)
	offset := 0
	cb.OnStep = func(n int) {
		h.step(offset + n)
		t.step(offset + n)
		h.observeStep(offset + n)
	}
	cb.OnUsage = t.usage
	if skill != nil {
		observe := cb.OnCall
		cb.OnCall = func(call agent.Call, res anthropic.ContentBlockParamUnion) {
			if observe != nil {
				observe(call, res)
			}
			skill.Observe(call, res)
		}
	}
	if companion != nil {
		companion.watch(&cb)
	}
	t.transcript.watch(&cb)
	res := inner.RunTurn(work, first, cb)
	var refused string
	for res.StopReason == agent.StepLimit && h.detached() {
		granted, account := m.extend(work, inner, res.History, h)
		if !granted {
			refused = account
			if work.Err() != nil {
				res.StopReason = agent.Cancelled
			}
			break
		}
		offset += def.steps()
		h.set(func(run *Run) { run.MaxSteps += def.steps() })
		res = inner.RunTurn(work, resumed(res.History, def.steps()), cb)
	}
	t.set(func(rec *Dispatch) { rec.Stop = stopOf(res.StopReason) })
	halted := res.StopReason == agent.Cancelled && errors.Is(context.Cause(work), errStopped)
	closing := ctx
	if halted {
		var cancel context.CancelFunc
		closing, cancel = context.WithTimeout(ctx, closingWithin)
		defer cancel()
	}
	// Closed before anything returns, down every path: a peer row left live holds a dot in the
	// top bar for the rest of the session, and the closing view is wanted even when the driver
	// ran out of turns — especially then, since the driver's own account is a stub.
	var parting string
	if companion != nil {
		parting = companion.close(closing, stopOf(res.StopReason))
	}
	switch res.StopReason {
	case agent.Cancelled:
		if !halted {
			return "", context.Canceled
		}
		head := fmt.Sprintf("Stopped at your request after %d steps.", h.steps())
		if refused != "" {
			return withParting(head+" "+strings.ReplaceAll(refused, "\n", " "), def, parting), nil
		}
		if said := inner.Finalize(closing, res.History, stoppedEarly); said != "" {
			return withParting(head+" "+strings.ReplaceAll(said, "\n", " "), def, parting), nil
		}
		return withParting(head+" Its closing report did not arrive, so what it left unfinished is unknown.", def, parting), nil
	case agent.StepLimit:
		stopped := fmt.Sprintf("Stopped after %d steps without finishing.", h.steps())
		if refused != "" {
			return withParting(stopped+" More turns were not granted. "+strings.ReplaceAll(refused, "\n", " "), def, parting), nil
		}
		if said := inner.Finalize(ctx, res.History, outOfBudget); said != "" {
			return withParting(stopped+" "+strings.ReplaceAll(said, "\n", " "), def, parting), nil
		}
		return withParting(stopped, def, parting), nil
	case agent.ContextFull:
		stopped := fmt.Sprintf("Stopped after %d steps without finishing because its context window was full.", h.steps())
		if said := inner.Finalize(ctx, res.History, contextFull); said != "" {
			return withParting(stopped+" "+strings.ReplaceAll(said, "\n", " "), def, parting), nil
		}
		return withParting(stopped, def, parting), nil
	}
	if strings.HasPrefix(res.StopReason, "error: ") {
		return "", fmt.Errorf("%s", strings.TrimPrefix(res.StopReason, "error: "))
	}
	// Final, not Text: the report is the last thing the subagent wrote, not everything it wrote.
	// A run that narrates its way through fifteen tool calls would otherwise hand the main agent
	// the narration too, which is the context the delegation existed to save. reportContract is
	// the other half — a subagent that does not know this is the rule writes its findings into a
	// middle message and reports "Done."
	//
	// A schema'd agent's reply is rendered back into a sentence here. An unconforming one falls
	// through as prose rather than failing: truncation at max_tokens leaves half an object, and
	// the account of itself inside it is worth more than an error would be.
	if def.Report != nil {
		rendered, conformed := def.Report.render(res.Final)
		t.set(func(rec *Dispatch) { rec.Conformed = &conformed })
		if conformed {
			return withParting(rendered, def, parting), nil
		}
	}
	return withParting(res.Final, def, parting), nil
}

func stopOf(reason string) string {
	switch {
	case reason == agent.StepLimit:
		return StopStepLimit
	case reason == agent.ContextFull:
		return StopContextFull
	case reason == agent.Cancelled:
		return StopCancelled
	case strings.HasPrefix(reason, "error: "):
		return StopError
	default:
		return StopDone
	}
}

// withParting appends the peer's closing view to the driver's report, attributed and fenced.
//
// Appended rather than merged, and the attribution is the whole reason: these are two agents who
// may not agree, and a dispatching agent that cannot tell which of them said what has been handed
// a consensus that was never reached. Empty parting leaves the report exactly as it was, so an
// unpaired agent's return value is byte-identical to before.
func withParting(report string, def *Definition, parting string) string {
	if parting == "" {
		return report
	}
	return report + "\n\n<" + def.Peer.Def.Name + ">\n" + parting + "\n</" + def.Peer.Def.Name + ">"
}

// prompt is what the subagent is handed: the task, and whatever verbatim material came with it.
// Never the parent's history — keeping that out is the point of delegating.
func prompt(task, extra string) []anthropic.MessageParam {
	return []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(promptText(task, extra)))}
}

func promptText(task, extra string) string {
	text := "Today is " + time.Now().Format("2006-01-02") + ".\n\n" + task
	if strings.TrimSpace(extra) != "" {
		text += "\n\n<context>\n" + strings.TrimSpace(extra) + "\n</context>"
	}
	return text
}

// askUser puts one prompt to the user on a subagent's behalf: serialised against every other run
// doing the same, and with the run shown as waiting for as long as it is parked.
//
// The mutex is the same one the dispatch approval takes, and it has to be: the session holds a
// single parked approval, so the thing being serialised is access to the user, not access to one
// code path.
func (m *Manager) askUser(h *handle, prompt func() bool) bool {
	m.approving.Lock()
	defer m.approving.Unlock()
	h.waiting(true)
	defer h.waiting(false)
	return prompt()
}

// wrap puts the tool name on the run before the tool runs, so a box says what is happening
// rather than what just happened. The full label replaces it when the call returns.
func (m *Manager) wrap(def *Definition, h *handle) map[string]agent.Handler {
	wrapped := make(map[string]agent.Handler, len(def.Handlers))
	for name, inner := range def.Handlers {
		wrapped[name] = announce(inner, h)
	}
	return wrapped
}

// announce is the same pre-call label for one handler, applied to the fallback as well as the
// table: an MCP call reaches a subagent through the fallback, and without this its box would show
// nothing for the length of the call.
func announce(inner agent.Handler, h *handle) agent.Handler {
	if inner == nil {
		return nil
	}
	return func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		h.activity(call.Name)
		return inner(ctx, call, cb)
	}
}

// callbacks are the subagent's, not the main agent's. Its text, thinking and usage are its own
// business — only its tool calls surface, as the trail its transcript box shows.
//
// Approve and Require come from the parent for one reason: so that an editing subagent has
// somewhere to send them. A read-only agent calls neither.
func (m *Manager) callbacks(def *Definition, h *handle, outer agent.Callbacks) agent.Callbacks {
	// A call stopped before it ran reports no result, so its answer puts it on the trail unless a note already did.
	var mu sync.Mutex
	shown, noted := map[string]bool{}, map[string]bool{}
	return agent.Callbacks{
		OnStep: h.step,
		OnCall: func(call agent.Call, res anthropic.ContentBlockParamUnion) {
			h.observeCall(call)
			mu.Lock()
			label, _ := agent.Describe(call)
			unshown := !shown[call.ID] && !noted[label]
			delete(shown, call.ID)
			delete(noted, label)
			mu.Unlock()
			if tr := res.OfToolResult; unshown && tr != nil && tr.IsError.Or(false) {
				h.appendTrail(Entry{Kind: EntryNote, Refused: true, Text: "not run: " + label + " — " + firstLine(toolResultText(tr))})
			}
		},
		// Attributed and serialised. Attributed because an unlabelled prompt in a pane running
		// three agents asks the user to approve a write without saying who wants it. Serialised
		// through the same mutex the dispatch approval takes, because the session holds one parked
		// approval and two runs reaching it at once leave the first waiting forever.
		//
		// The run is marked Waiting across the prompt, which is what the dot row and /agents read
		// to show that a run is stopped on the user rather than working.
		Approve: func(ctx context.Context, label, body string) bool {
			// A detached run has nobody to ask: its turn is over, and the session's one approval
			// slot belongs to whatever is running now. What stands in for the user's answer is the
			// approval already given — the dispatch itself — bounded by the policy, which for an
			// editing agent confines every write to the repository and denies the rest.
			//
			// Recorded in the trail rather than taken silently. The run's box is the only place
			// this is visible, and an edit nobody approved and nobody can see is the version of
			// this that should never ship.
			if h.detached() {
				h.appendTrail(Entry{Kind: EntryNote, Text: "auto-approved (detached): " + label})
				return true
			}
			if outer.Approve == nil {
				return false
			}
			return m.askUser(h, func() bool {
				return outer.Approve(ctx, "agent "+def.Name+" — "+label, body)
			})
		},
		Require: func(ctx context.Context, label, body, reason string) bool {
			// Unreachable if detachable() did its job, since that refuses to detach an agent whose
			// policy can ask. Denied rather than trusted to be unreachable, and denied rather than
			// auto-allowed: Require is what the policy raises when nothing but the user should
			// answer, so the safe failure is no, and the unsafe one is a hang on a dead turn.
			if h.detached() {
				mu.Lock()
				noted[label] = true
				mu.Unlock()
				h.appendTrail(Entry{Kind: EntryNote, Refused: true, Text: "refused (detached, needs a person): " + label})
				return false
			}
			if outer.Require == nil {
				return false
			}
			return m.askUser(h, func() bool {
				return outer.Require(ctx, "agent "+def.Name+" — "+label, body, reason)
			})
		},
		Ask: outer.Ask,
		OnToolResult: func(id, label, output string, kind agent.ToolKind) {
			mu.Lock()
			shown[id] = true
			mu.Unlock()
			h.activity(label)
			if line := def.trail(kind, label, output); line != "" {
				h.appendTrail(Entry{Kind: EntryTool, Tool: kind, Text: line})
			}
		},
	}
}
