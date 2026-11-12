// Package ui is the terminal front end: a bubbletea program over a session.
//
// bubbletea v2 declares alt-screen, mouse mode, focus reporting and bracketed paste as fields on
// the View it returns, so there is no escape-sequence plumbing at all. viewport owns scrolling and
// the scroll anchor.
// The only arithmetic left is mapping a clicked row to a transcript box, which is a lookup into
// a slice built during rendering.
package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/commands"
	"github.com/alexbieg/alux/internal/desktop"
	"github.com/alexbieg/alux/internal/programstatus"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/resolve"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/vcs"
	"github.com/alexbieg/alux/internal/workers"
)

// sessionChanged wakes the UI when the session's snapshot moved. It carries nothing: the model
// re-reads the snapshot, so a burst of streaming deltas collapses into one render.
type sessionChanged struct{}

type noticeExpired struct{ id int }

// NoticeMsg has the alux say an ambient line. Sent from outside the program, which is
// how a background vault filing reports itself long after its turn ended.
type NoticeMsg struct{ Text string }

type spawnedMsg struct {
	pid int
	err error
}

type vcsLoaded struct{ status *vcs.Status }

// NamesMsg says the resolver learned a name; the transcript picks it up once no selection is open.
type NamesMsg struct{}

// SessionsMsg carries the other running alux sessions, polled by the registry.
type SessionsMsg struct{ Sessions []registry.Info }

type notice struct {
	id   int
	text string
}

const (
	// noticesKept is the one being said plus those waiting; past it the oldest waiting one goes.
	noticesKept   = 4
	noticeLife    = 6 * time.Second
	noticeMaxTall = 6
	noticeMaxWide = 52
	noticeMinWide = 12 // narrower than this and a bordered card is all border
	minWidth      = 24
	maxInputRows  = 99

	// Bounds the queue list, which would otherwise push the input box off a short screen.
	queueShown = 5

	// Floor between two status-line reads that a burst could ask for. Each one forks jj, which
	// is 40ms of it in a large repo.
	vcsInterval = 500 * time.Millisecond
)

type Model struct {
	session     SessionClient
	snap        session.Snapshot
	draftID     int
	clipboardID int

	width, height int
	viewport      viewport.Model
	input         textarea.Model
	note          textinput.Model
	// spin is never drawn: its tick is the clock every animation rides, counted by poseTick.
	spin     spinner.Model
	poseTick int
	// ticking is whether a spin tick is in flight; wakeAt is the tick an idle timer is set for, and
	// wakeGen tells a stale one from the current.
	ticking         bool
	wakeAt, wakeGen int
	alux            aluxState
	// finished is when this pane saw each run end, which times its one-shot flourish.
	finished map[int]time.Time
	// turnDone is when the last turn finished without being cancelled, which times the frame's flash.
	turnDone time.Time
	// recapFor is the turnDone a recap was last asked for, so a pane never recaps twice in a row.
	recapFor time.Time
	// recap reaches the session; injectable so tests need no model.
	recap func(ctx context.Context) error

	content transcript

	// blocks remembers each item's rendered lines; see [Model.itemLines] for why. Dropped on
	// resize rather than left to age out, since every entry in it is the wrong width.
	blocks map[blockKey][]string

	// View state, deliberately not in the session snapshot: expanding a box in one pane must
	// not move another pane that is mirroring the same conversation.
	expanded map[int]bool
	follow   bool

	// Wheel coalescing; see [Model.onWheel].
	wheelArmed bool
	wheelHeld  int
	frame      tea.View
	reuseFrame bool

	// Ask panel state, likewise local to whoever is looking at the question.
	cursor int
	noting bool

	// Text selection, owned here because the terminal cannot own it and click-to-expand at the
	// same time. See selection.go for the geometry and the highlight.
	sel selection

	// now is injectable so timing can be tested without sleeping.
	now func() time.Time

	// queueTop is the screen row the queue list starts on, -1 when none is drawn. Recorded
	// during layout, since the bottom section's height is not fixed.
	queueTop int

	notices    []notice
	nextNotice int

	vcs *vcs.Status
	// readVCS is injectable so a test can drive the refresh triggers without a jj repo. live is
	// true when the agent is not working and the snapshotting read is safe.
	readVCS func(live bool, scope vcs.Scope) *vcs.Status
	// VCSScope narrows the status line to a pool slot's work, zero outside a pool workspace.
	VCSScope vcs.Scope
	// VCSSlug is the GitHub repo whose PRs the slot's bookmark is looked up in.
	VCSSlug string
	// readTranscript feeds a subagent run's view; injectable for the same reason.
	readTranscript func(dispatch string) ([]subagent.TranscriptEvent, error)
	// vcsAt is when the last throttled refresh went out. Turn-end refreshes ignore it.
	vcsAt time.Time
	// vcsSig is the transcript shape the status line was last refreshed for: how many items, and
	// how many of them are still running. It moves when a tool box appears and again when it
	// resolves, which is the cadence the status line follows mid-turn.
	vcsSig  [2]int
	focused bool
	quit    bool
	// quitArmed is when a quit was refused over running subagents, zero when none was.
	quitArmed time.Time

	// disambiguated is the terminal confirming ctrl+[ arrives apart from esc.
	disambiguated bool

	// completion caches commands.Complete for compFor, since argument candidates read the disk.
	compFor   string
	compStart int
	comp      []commands.Suggestion

	// Cross-session awareness. Registry is nil when sessions are not being tracked, which is
	// how the tests run without touching the user's real registry directory.
	Registry *registry.Registry
	// Origin is this repo's origin; only workers published with the same one are shown.
	Origin string
	// Viewing reports whether another TUI drives the session, which then raises its notifications; nil drives.
	Viewing func() bool
	// CanAttach reports whether the session in pid serves TUIs; nil leaves session rows unswitchable.
	CanAttach func(pid int) bool
	// SwitchTo is the session the user picked to attach to next when the program quits, 0 for none.
	SwitchTo int
	// Spawn starts a served session and returns its pid; nil when this pane cannot attach elsewhere.
	Spawn func() (int, error)
	// CanKill lets /kill, /quit and ctrl+c close the pane with Kill set, for the caller to stop the session it detached from,
	// and /detach close it without; /quit and ctrl+c also set KillOnly, so the caller exits rather than moving to another session.
	CanKill  bool
	Kill     bool
	KillOnly bool
	// lastStatus is the OSC 7501 report last sent, so an unchanged state is not resent.
	lastStatus string
	others     []registry.Info
	ownState   registry.State
	// ParkedPrompt and AnswerPrompt reach a worker's session by pid; nil leaves worker prompts unshown.
	ParkedPrompt func(pid int) (workers.Prompt, bool)
	AnswerPrompt func(pid int, id string, approve bool, a *ask.Answer) (bool, error)
	// StopSession ends the session in pid unless a UI is attached to it; nil leaves x on a session row doing nothing.
	StopSession func(pid int) error
	// stopArmed is the session an x is waiting on a second x to stop, and when it was pressed.
	stopArmed   int
	stopArmedAt time.Time
	// Headless workers' prompts: the queue, the one pinned on screen, and those keyed to an id
	// already answered or set aside with esc.
	queue          []workerPrompt
	shown          *workerPrompt
	answered       map[string]string
	setAside       map[string]string
	workerRetrying bool
	// heldSince is when a wake turn's desktop notification was first held back, zero when none is.
	heldSince time.Time
	// announced is the runs a desktop notification has already covered.
	announced map[int]bool

	// Renders read names, a copy of Names taken outside a selection, so a drag holds the lines still.
	Names      *resolve.Resolver
	names      map[string]resolve.Entry
	namesStale bool

	prStates []prs.RepoState
	// PRs backs the picker's detail and refresh, nil when PR tracking is off.
	PRs    PRSource
	picker *prPicker
	// Land backs /land and the workspaces pane, nil outside a pool workspace.
	Land LandSource
	// Setup feeds the welcome screen, which is not drawn while it is nil.
	Setup    *Setup
	guardian guardianCheck
	welcome  welcomeState
	// account is the user's full name for the greeting; injectable so a test needs no account.
	account func() string

	runView   *runView
	skillPane *skillPane
	wsPane    *workspacePane
	fileGraph *fileGraphPane
	fileDiff  *fileDiffPane
	// pane scrolls whatever the expanded running box shows, leaving the transcript's scroll alone.
	pane viewport.Model
	// sidebarOpen is the running box's mode, rail when false; starts open and is never saved.
	sidebarOpen bool
	sideFocus   sideFocus

	newFindingChat func(dir string, brief subagent.FindingBrief) (session.Chat, error)
}

func New(s SessionClient) *Model {
	// textarea rather than textinput: a pasted block has to stay in the box. Enter is
	// intercepted below so it still submits.
	in := textarea.New()
	in.Prompt = ""
	in.ShowLineNumbers = false

	// A terminal without kitty disambiguation sends a bare carriage return for this, where the
	// binding will not match and enter submits instead — a silent degradation.
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"))

	// DynamicHeight sizes the box in soft-wrapped rows, which LineCount cannot see. MaxHeight
	// then caps only what is visible, so the guard against an unbounded paste moves here.
	in.DynamicHeight = true
	in.MaxContentHeight = maxInputRows

	// The marker is the textarea's prompt on its first visual row only, so wrapped rows line up
	// under the text and the widget reports a cursor that already accounts for it.
	in.SetPromptFunc(lipgloss.Width(inputMarker), func(info textarea.PromptInfo) string {
		if info.LineNumber > 0 {
			return ""
		}
		return promptStyle.Render(inputMarker)
	})

	// textarea highlights the line the cursor is on with a background, which is right for an
	// editor and wrong for a prompt: it paints whatever you are typing.
	st := in.Styles()
	st.Focused.CursorLine = st.Focused.CursorLine.UnsetBackground()
	st.Blurred.CursorLine = st.Blurred.CursorLine.UnsetBackground()
	in.SetStyles(st)

	// Use the terminal's own cursor rather than textarea's drawn one, so it blinks and is
	// shaped the way the user configured their terminal. Cursor() returns nil while the virtual
	// cursor is on, so without this the position reported to bubbletea is never used.
	in.SetVirtualCursor(false)
	in.Focus()

	note := textinput.New()
	note.Prompt = ""

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot

	return &Model{
		session:     s,
		input:       in,
		note:        note,
		spin:        sp,
		viewport:    viewport.New(),
		pane:        viewport.New(),
		expanded:    map[int]bool{},
		finished:    map[int]time.Time{},
		follow:      true,
		focused:     true,
		sidebarOpen: true,
		now:         time.Now,
		account:     accountName,
		queueTop:    -1,
		readVCS:     readVCS,
		readTranscript: func(dispatch string) ([]subagent.TranscriptEvent, error) {
			return subagent.ReadTranscript(subagent.DefaultTranscriptDir(), dispatch)
		},
		newFindingChat: s.FindingChat,
		recap:          s.Recap,
	}
}

func (m *Model) Init() tea.Cmd {
	m.snap = m.session.Snapshot()
	m.ticking = true
	return tea.Batch(m.spin.Tick, m.refreshVCS())
}

// Changed is handed to the session as its notifier. It is safe from any goroutine.
func Changed() tea.Msg { return sessionChanged{} }

func readVCS(live bool, scope vcs.Scope) *vcs.Status {
	if live {
		return vcs.Read(scope)
	}
	return vcs.Peek(scope)
}

// refreshVCS reads the working copy the way the current moment allows: the full read between
// turns, the non-snapshotting one while the agent works, since snapshotting there breaks the
// agent's own jj commands.
func (m *Model) refreshVCS() tea.Cmd {
	m.vcsAt = m.now()
	m.vcsSig = m.vcsSignature()
	live := !m.snap.Busy
	read, scope := m.readVCS, m.VCSScope
	return func() tea.Msg { return vcsLoaded{status: read(live, scope)} }
}

// refreshVCSSoon is refreshVCS held to one read per vcsInterval, for triggers that can arrive in
// bursts: transcript items during a turn, focus as the user clicks between panes.
func (m *Model) refreshVCSSoon() tea.Cmd {
	if m.now().Sub(m.vcsAt) < vcsInterval {
		return nil
	}
	return m.refreshVCS()
}

func (m *Model) vcsSignature() [2]int {
	running := 0
	for _, it := range m.snap.Items {
		if it.Running {
			running++
		}
	}
	return [2]int{len(m.snap.Items), running}
}

// Notify queues an ambient message for the alux to say. Only the first is said, and its life
// starts when it reaches the front, so one waiting behind it still gets its full time.
func (m *Model) Notify(text string) tea.Cmd {
	m.notices = append(m.notices, notice{id: m.nextNotice, text: text})
	m.nextNotice++
	if len(m.notices) > noticesKept {
		m.notices = append(m.notices[:1], m.notices[2:]...)
	}
	if len(m.notices) == 1 {
		return m.noticeTimer()
	}
	return nil
}

func (m *Model) noticeTimer() tea.Cmd {
	id := m.notices[0].id
	return tea.Tick(noticeLife, func(time.Time) tea.Msg { return noticeExpired{id: id} })
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.reuseFrame = false
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width != m.width {
			m.blocks = nil
		}
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, m.loadSkill()

	case tea.KeyboardEnhancementsMsg:
		m.disambiguated = msg.SupportsKeyDisambiguation()
		return m, nil

	case tea.FocusMsg:
		m.focused = true
		if m.Registry != nil {
			seen := false
			m.Registry.Publish(registry.Patch{Unseen: &seen})
		}
		// A pane left unfocused for an hour is showing an hour-old working copy.
		return m, tea.Batch(m.refreshVCSSoon(), m.recapOnReturn())

	case tea.BlurMsg:
		m.focused = false
		return m, nil

	case sessionChanged:
		wasBusy, wasCancelling := m.snap.Busy, m.snap.Cancelling
		snap := m.session.Snapshot()
		snap.Procs = m.heldProcs(snap.Procs)
		m.noteFinishes(m.snap.Runs, snap.Runs)
		m.snap = snap
		if snap.Draft.ID != m.draftID {
			m.draftID = snap.Draft.ID
			m.input.SetValue(snap.Draft.Text)
		}
		m.pickPrompt()
		m.layout()
		var cmds []tea.Cmd
		if snap.Clipboard.ID != m.clipboardID {
			m.clipboardID = snap.Clipboard.ID
			note := fmt.Sprintf("copied the handoff note (%d lines)", strings.Count(snap.Clipboard.Text, "\n")+1)
			cmds = append(cmds, tea.SetClipboard(snap.Clipboard.Text), m.Notify(note))
		}
		cmds = append(cmds, m.publishState(), m.reportStatus(), m.loadRun(), m.loadSkill())
		// A question or an approval arriving while scrolled up is worth jumping to.
		if m.snap.Question != nil || m.snap.Pending != "" {
			m.follow = true
			m.viewport.GotoBottom()
			m.sideFocus.on = false
		}
		switch {
		case wasBusy && !m.snap.Busy:
			if !wasCancelling {
				m.turnDone = m.now()
			}
			cmds = append(cmds, m.refreshVCS())
		case m.vcsSignature() != m.vcsSig:
			cmds = append(cmds, m.refreshVCSSoon())
		}
		m.noteAlux(wasBusy, wasCancelling)
		return m, tea.Batch(cmds...)

	case vcsLoaded:
		m.vcs = msg.status
		return m, nil

	case SessionsMsg:
		cmd := m.noticeOthers(msg.Sessions)
		m.others = msg.Sessions
		workerCmd := m.refreshWorkers(msg.Sessions)
		m.setAside = nil
		m.pickPrompt()
		m.layout()
		return m, tea.Batch(cmd, workerCmd)

	case workerRetry:
		m.workerRetrying = false
		cmd := m.refreshWorkers(m.others)
		m.pickPrompt()
		m.layout()
		return m, cmd

	case NamesMsg:
		m.namesStale = true
		if !m.selecting() {
			m.layout()
		}
		return m, nil

	case PRsMsg:
		m.prStates = msg.States
		m.refreshPicker()
		m.layout()
		return m, nil

	case prSyncedMsg:
		if m.picker != nil {
			m.picker.syncing = false
			m.layout()
		}
		return m, nil

	case workspacesMsg:
		m.onWorkspaces(msg)
		m.layout()
		if m.wsPane != nil {
			m.keepCursorInView()
		}
		if msg.sweep && msg.err == nil && m.wsPane != nil {
			return m, m.sweepWorkspaces()
		}
		return m, nil

	case wsSweepMsg:
		return m, m.onWsSweep(msg)

	case wsDiffMsg:
		m.onWsDiff(msg)
		return m, nil

	case wsActionMsg:
		return m, m.onWsAction(msg)

	case prDetailMsg:
		cmd := m.onPRDetail(msg)
		m.layout()
		return m, cmd

	case fileGraphMsg:
		cmd := m.onFileGraph(msg)
		m.layout()
		return m, cmd
	case reviewMsg:
		cmd := m.onReview(msg)
		m.layout()
		return m, cmd

	case e2eMsg:
		cmd := m.onE2E(msg)
		m.layout()
		return m, cmd

	case labelledMsg:
		return m, m.onLabelled(msg)

	case mergeCheckMsg:
		cmd := m.onMergeCheck(msg)
		m.layout()
		return m, cmd

	case openedMsg:
		if msg.err != nil {
			return m, m.Notify("could not open the browser: " + msg.err.Error())
		}
		return m, nil

	case NoticeMsg:
		return m, m.Notify(msg.Text)

	case spawnedMsg:
		if msg.err != nil {
			return m, m.Notify("spawn: " + msg.err.Error())
		}
		m.SwitchTo, m.quit = msg.pid, true
		return m, tea.Quit

	case GuardianMsg:
		m.guardian = guardianCheck{done: true, err: msg.Err}
		m.layout()
		return m, nil

	case ReviewMsg:
		return m, m.onReviewLoaded(msg.Review)

	case noticeExpired:
		if len(m.notices) == 0 || m.notices[0].id != msg.id {
			return m, nil
		}
		m.notices = m.notices[1:]
		if len(m.notices) > 0 {
			return m, m.noticeTimer()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if cmd == nil {
			return m, nil
		}
		m.poseTick++
		m.poseAlux()
		// The status line is rebuilt by every View, but a running tool's spinner is baked into the
		// transcript, and only layout rebuilds that. Skipped when nothing is live, so an idle pane
		// still re-renders the conversation only when it changes.
		// An expired flourish takes its row off the running box.
		expired := m.pruneFinished()
		if expired || m.hasLiveBox() {
			// Only the process list, not a fresh snapshot: sessionChanged diffs the snapshot it
			// is handed against the one before it, and a tick taking its own would swallow the
			// transitions it watches for.
			m.snap.Procs = m.heldProcs(m.session.LiveProcs())
			m.layout()
		} else if m.fileGraph != nil && !m.fileGraph.diff && m.fileGraph.anyAsking() {
			m.layout()
		}
		if !m.animating() {
			m.ticking = false
			return m, nil
		}
		return m, cmd

	case tea.PasteMsg:
		// bubbletea reports a bracketed paste as one message and textarea inserts it whole, so
		// a multi-line paste never has to be inferred from a chunk that happened to contain a
		// newline.
		var cmd tea.Cmd
		switch {
		case m.fileGraph != nil && m.fileGraph.chatting:
			*m.fileGraph.input, cmd = m.fileGraph.input.Update(msg)
			m.fileGraph.follow = true
		case m.panelOpen():
		case m.typingAnswer():
			focus := m.note.Focus()
			m.note, cmd = m.note.Update(msg)
			cmd = tea.Batch(focus, cmd)
		case !m.onScreen().waiting():
			m.input, cmd = m.input.Update(msg)
		}
		m.layout()
		return m, cmd

	case tea.MouseWheelMsg:
		return m, m.onWheel(msg)

	case wheelFrameMsg:
		return m, m.onWheelFrame()

	case tea.MouseClickMsg:
		return m, m.onMouseDown(msg)

	case tea.MouseMotionMsg:
		return m, m.onMouseDrag(msg)

	case tea.MouseReleaseMsg:
		return m, m.onMouseUp(msg)

	case runTranscriptMsg:
		return m, m.onRunTranscript(msg)

	case skillDrawnMsg:
		return m, m.onSkillDrawn(msg)

	case fileDiffMsg:
		m.onFileDiff(msg)
		return m, nil

	case findingChatMsg:
		return m, m.onFindingChatReply(msg)

	case tea.KeyPressMsg:
		return m, m.onKey(msg)
	}
	return m, nil
}

const wheelStep = 3

// wheelFrame matches the renderer's 60fps: scrolling more often than it draws is never seen.
const wheelFrame = time.Second / 60

type wheelFrameMsg struct{}

// bubbletea renders a view per message, so a burst scrolls at once, then banks and applies once a frame.
func (m *Model) onWheel(msg tea.MouseWheelMsg) tea.Cmd {
	var delta int
	switch msg.Button {
	case tea.MouseWheelUp:
		delta = -wheelStep
	case tea.MouseWheelDown:
		delta = wheelStep
	default:
		return nil
	}
	if m.wheelArmed {
		m.wheelHeld += delta
		m.reuseFrame = true
		return nil
	}
	m.scrollBy(delta)
	m.wheelArmed = true
	return wheelTick()
}

func (m *Model) onWheelFrame() tea.Cmd {
	if m.wheelHeld == 0 {
		m.wheelArmed = false
		return nil
	}
	m.scrollBy(m.wheelHeld)
	m.wheelHeld = 0
	return wheelTick()
}

func wheelTick() tea.Cmd {
	return tea.Tick(wheelFrame, func(time.Time) tea.Msg { return wheelFrameMsg{} })
}

func (m *Model) scrollBy(delta int) {
	vp := &m.viewport
	if m.panelOpen() {
		vp = &m.pane
	}
	switch {
	case delta < 0:
		vp.ScrollUp(-delta)
	case delta > 0:
		vp.ScrollDown(delta)
	default:
		return
	}
	switch {
	case m.runView != nil:
		m.runView.follow = m.pane.AtBottom()
	case !m.panelOpen():
		m.follow = delta > 0 && m.viewport.AtBottom()
	}
}

// The gestures: a click toggles the box under it and a drag selects a range. The press starts a
// selection and the release decides which it was, so a click acts the moment the button comes up.
func (m *Model) onMouseDown(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	if id, ok := m.queuedAt(msg.Y); ok {
		m.session.Steer(id)
		m.layout()
		return m.clearSelection()
	}
	if m.panelOpen() {
		return m.onPanelClick(msg.X, msg.Y)
	}
	// The running box's whole column keeps the selection, rows or not.
	if m.inSidebarColumn(msg.X, msg.Y) {
		return m.onSidebarClick(msg.X, msg.Y)
	}
	if m.onPRSummary(msg.X, msg.Y) {
		return m.openPicker(false)
	}
	if m.onScrolledPill(msg.X, msg.Y) {
		m.follow = true
		m.viewport.GotoBottom()
		return m.clearSelection()
	}
	if m.onPinnedRow(msg.X, msg.Y) {
		m.scrollToPinned()
		return m.clearSelection()
	}
	pt, ok := m.pointAt(msg.X, msg.Y)
	if !ok {
		// A press on the input box or the chrome drops whatever was selected, the same as
		// clicking away from a selection anywhere else.
		return m.clearSelection()
	}

	m.sel = selection{anchor: pt, head: pt, dragging: true}
	m.layout()
	return nil
}

// onMouseDrag extends the selection. The point is clamped rather than rejected: dragging past
// the bottom of the transcript should reach the last line, not stop updating.
func (m *Model) onMouseDrag(msg tea.MouseMotionMsg) tea.Cmd {
	if !m.sel.dragging || msg.Button != tea.MouseLeft {
		return nil
	}
	m.sel.head = m.clampedPointAt(msg.X, msg.Y)
	m.layout()
	return nil
}

// onMouseUp ends the gesture: a press that never moved toggles the box it landed on, and a drag
// copies what it selected. Copying on release rather than on a key is what makes this feel like
// the terminal's own selection, which is the behaviour it replaced.
func (m *Model) onMouseUp(msg tea.MouseReleaseMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft || !m.sel.dragging {
		return nil
	}
	m.sel.dragging = false
	if !m.sel.empty() {
		return m.copySelection()
	}
	if s, ok := m.nameAt(m.sel.anchor); ok {
		m.session.Show(s.id + " — " + s.name)
		return nil
	}
	if box := m.content.ownerAt(m.sel.anchor.line); box != noOwner {
		cmd := m.toggle(box)
		m.layout()
		return cmd
	}
	return nil
}

// copySelection puts the selected text on the system clipboard.
//
// OSC 52 (via tea.SetClipboard) rather than a native clipboard binding: it needs no cgo and no
// new dependency, and it is the only mechanism that also works when alux is run over SSH. The
// terminal may refuse it — Ghostty has a clipboard-write permission — and the app cannot tell,
// which is exactly why this reports what it copied. A notice that says "copied" while the
// clipboard stayed empty is a bug the user can at least see and go and fix.
func (m *Model) copySelection() tea.Cmd {
	if m.sel.empty() {
		return nil
	}
	from, to := m.sel.span()
	text := m.content.selectedText(from, to)
	if strings.TrimSpace(text) == "" {
		return nil
	}

	note := fmt.Sprintf("copied %d lines", strings.Count(text, "\n")+1)
	if !strings.Contains(text, "\n") {
		note = fmt.Sprintf("copied %q", oneRow(text, 32))
	}
	return tea.Batch(tea.SetClipboard(text), m.Notify(note))
}

// clearSelection drops the selection and redraws without the highlight. It returns a Cmd so it
// can be used directly as a handler's result.
func (m *Model) clearSelection() tea.Cmd {
	if m.sel == (selection{}) {
		return nil
	}
	m.sel = selection{}
	m.layout()
	return nil
}

// pointAt maps screen coordinates to a position in the transcript, reporting whether they landed
// in it at all. The one column of indent the transcript is drawn with is taken off here, so
// everything downstream works in content coordinates.
func (m *Model) pointAt(x, y int) (point, bool) {
	top := m.transcriptTop()
	if y < top || y >= top+m.viewport.Height() || x >= m.mainWidth() {
		return point{}, false
	}
	line := m.viewport.YOffset() + (y - top)
	if line < 0 || line >= len(m.content.lines) {
		return point{}, false
	}
	return point{line: line, col: max(0, x-1)}, true
}

// clampedPointAt is pointAt for a drag in progress, where coordinates off the end of the
// transcript mean "as far as it goes" rather than "nowhere".
func (m *Model) clampedPointAt(x, y int) point {
	top := m.transcriptTop()
	line := m.viewport.YOffset() + (y - top)
	line = min(max(line, 0), max(0, len(m.content.lines)-1))
	return point{line: line, col: min(max(0, x-1), max(0, m.mainWidth()-2))}
}

// toggle opens or closes a box, or opens a subagent's run view, whose transcript read it returns.
//
// It does not need to drop the selection, even though expanding reflows every row below the box:
// the only route here is a click, and its press has already replaced the selection with an empty one.
func (m *Model) toggle(id int) tea.Cmd {
	for _, item := range m.items() {
		if item.ID != id {
			continue
		}
		if item.Run != 0 {
			return m.openRun(item.Run)
		}
		if box, ok := session.Collapsible(item, m.snap.Runs, m.snap.Procs); ok {
			m.expanded[id] = !m.isExpanded(item, box)
		}
		return nil
	}
	return nil
}

// isExpanded is the user's last click on a box, or the box's own default before one.
func (m *Model) isExpanded(item session.Item, box session.Box) bool {
	if open, ok := m.runBoxOpen(item.ID); ok {
		return open
	}
	if open, ok := m.wsBoxOpen(item.ID); ok {
		return open
	}
	if open, ok := m.expanded[item.ID]; ok {
		return open
	}
	return box.OpenByDefault()
}

// requestKill warns instead of killing while subagents run, unless the warning is still showing.
func (m *Model) requestKill() tea.Cmd {
	live := 0
	for _, run := range m.snap.Runs {
		if run.Live() && run.Parent == 0 {
			live++
		}
	}
	if live > 0 && m.now().Sub(m.quitArmed) > noticeLife {
		m.quitArmed = m.now()
		return m.Notify(fmt.Sprintf("%s still running — ctrl+c again to kill the session", plural(live, "subagent")))
	}
	m.Kill, m.KillOnly, m.quit = m.CanKill, m.CanKill, true
	return tea.Quit
}

func (m *Model) onKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	switch key {
	case "ctrl+c", "ctrl+d":
		return m.requestKill()
	case "super+s":
		return m.toggleSidebar()
	}

	// An open panel owns the keyboard even over a question or approval, which waits until it closes.
	if m.panelOpen() && key != "shift+tab" && key != "super+e" {
		switch {
		case m.runView != nil:
			return m.onRunKey(msg)
		case m.skillPane != nil:
			return m.onSkillKey(key)
		case m.wsPane != nil:
			return m.onWorkspaceKey(key)
		case m.fileDiff != nil:
			m.onPaneKey(key)
			return nil
		case m.fileGraph.chatting:
			return m.onFindingChatKey(msg)
		default:
			return m.onFileGraphKey(key)
		}
	}

	prompt := m.onScreen()
	if prompt.question != nil {
		return m.onQuestionKey(msg, key, prompt)
	}
	pending := prompt.pending != ""
	if m.sideFocused() && !pending {
		return m.onSideKey(key)
	}
	if key == "super+e" && !pending {
		return m.focusSidebar()
	}
	if m.picker != nil && !pending {
		return m.onPickerKey(key)
	}

	switch key {
	case "esc":
		// Dropping a selection comes first: while one is on screen esc is the obvious way to
		// get rid of it, and cancelling the turn as well would be a surprise.
		if !m.sel.empty() {
			return m.clearSelection()
		}
		if prompt.worker != nil {
			return m.setAsideWorker(prompt.worker)
		}
		if m.snap.Busy {
			// The queue goes too: it was lined up on the strength of this turn finishing.
			dropped := m.session.DropQueued()
			m.session.Cancel()
			if dropped > 0 {
				return m.Notify(fmt.Sprintf("dropped %s", plural(dropped, "queued message")))
			}
		}
		return nil

	case "backspace":
		if m.unqueue() {
			return nil
		}

	case "y", "n":
		if prompt.worker != nil {
			return m.respondWorker(prompt.worker, key == "y", nil)
		}
		if pending {
			m.session.ApproveID(m.snap.ApprovalSeq, key == "y")
			return nil
		}

	case "shift+tab":
		i := slices.Index(session.Modes, m.snap.Mode)
		m.session.SetMode(session.Modes[(i+1)%len(session.Modes)])
		return nil

	case "enter":
		return m.submit()

	case "tab":
		if !pending && m.complete() {
			return nil
		}

	case "pgup":
		m.viewport.ScrollUp(max(1, m.viewport.Height()-2))
		m.follow = false
		return nil

	case "pgdown":
		m.viewport.ScrollDown(max(1, m.viewport.Height()-2))
		m.follow = m.viewport.AtBottom()
		return nil

	case "up", "down":
		// A multi-line box has its own use for these; otherwise they scroll the transcript.
		if m.input.LineCount() > 1 {
			break
		}
		if key == "up" {
			m.viewport.ScrollUp(1)
			m.follow = false
		} else {
			m.viewport.ScrollDown(1)
			m.follow = m.viewport.AtBottom()
		}
		return nil

	// alt+arrows stay for terminals that send ctrl+[ as esc.
	case "ctrl+[", "alt+up":
		m.jumpPrompt(-1)
		return nil

	case "ctrl+]", "alt+down":
		m.jumpPrompt(1)
		return nil

	case "end":
		// Back to the live tail.
		m.follow = true
		m.viewport.GotoBottom()
		return nil
	}

	if pending {
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.layout()
	return cmd
}

func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	m.input.SetValue("")
	// The picker is UI, so it opens now rather than queueing behind a running turn.
	if name, args, ok := commands.Parse(text); ok && name == "prs" && m.PRs != nil {
		if arg := strings.TrimSpace(args); arg == "" || arg == "refresh" {
			return m.openPicker(arg == "refresh")
		}
	}
	if name, args, ok := commands.Parse(text); ok && name == "land" && m.Land != nil && strings.TrimSpace(args) == "" {
		return m.openWorkspaces()
	}
	if name, _, ok := commands.Parse(text); ok && name == "spawn" && m.Spawn != nil {
		spawn := m.Spawn
		return tea.Batch(m.Notify("starting a new session…"), func() tea.Msg {
			pid, err := spawn()
			return spawnedMsg{pid: pid, err: err}
		})
	}
	if name, _, ok := commands.Parse(text); ok && (name == "kill" || name == "quit") && m.CanKill {
		m.Kill, m.KillOnly, m.quit = true, name == "quit", true
		return tea.Quit
	}
	if name, _, ok := commands.Parse(text); ok && name == "detach" && m.CanKill {
		m.quit = true
		return tea.Quit
	}
	m.follow = true
	m.session.Submit(text)
	m.layout()
	return nil
}

// completions is the list under the prompt for what is typed now, empty while the box is not taking commands.
func (m *Model) completions() (int, []commands.Suggestion) {
	if m.onScreen().waiting() {
		return 0, nil
	}
	if v := m.input.Value(); v != m.compFor {
		m.compFor = v
		m.compStart, m.comp = commands.Complete(v)
	}
	return m.compStart, m.comp
}

// complete swaps the last word for the top completion plus a space, which opens the next argument's list.
func (m *Model) complete() bool {
	start, list := m.completions()
	if len(list) == 0 {
		return false
	}
	v := m.input.Value()
	m.input.SetValue(v[:start] + list[0].Value + " ")
	m.input.MoveToEnd()
	m.layout()
	return true
}

// unqueue pulls the last queued prompt back into the box, reporting false when there is none and
// the keystroke belongs to the textarea.
func (m *Model) unqueue() bool {
	if m.input.Value() != "" || len(m.snap.Queued) == 0 {
		return false
	}
	last := m.snap.Queued[len(m.snap.Queued)-1]
	text := m.session.Unqueue(last.ID)
	if text == "" {
		return false
	}
	m.input.SetValue(text)
	m.layout()
	return true
}

// queuedAt maps a screen row to the queued message drawn on it. The steering rows come first in
// the list and are not offered, being already on their way.
func (m *Model) queuedAt(y int) (int, bool) {
	if m.queueTop < 0 || !m.snap.Steerable {
		return 0, false
	}
	_, spans := m.queueRows()
	row := y - m.queueTop
	for _, s := range spans {
		if row >= s.first && row <= s.last {
			return s.id, s.clickable
		}
	}
	return 0, false
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

var _ = fmt.Sprintf
var _ = lipgloss.Width

// sessionState is what the dot bar and the other panes see: what this session is doing, not
// what it is rendering.
func (m *Model) sessionState() registry.State { return SessionState(m.snap) }

// SessionState is sessionState for a snapshot read outside a pane.
func SessionState(snap session.Snapshot) registry.State {
	switch {
	case snap.Question != nil || snap.Pending != "":
		return registry.Waiting
	case snap.Busy && !snap.Quiet:
		return registry.Working
	default:
		return registry.Idle
	}
}

// SessionTasks is how many subagents and background commands the session has running.
func SessionTasks(snap session.Snapshot) int {
	n := 0
	for _, run := range snap.Runs {
		if run.State == subagent.Running || run.State == subagent.Waiting {
			n++
		}
	}
	for _, p := range snap.Procs {
		if !p.Foreground {
			n++
		}
	}
	return n
}

func (m *Model) lastMessage() string { return LastMessage(m.snap) }

// LastMessage is the one line other panes show for this session.
func LastMessage(snap session.Snapshot) string {
	text := ""
	switch {
	case snap.Question != nil:
		text = snap.Question.Question
	case snap.Pending != "":
		text = snap.Pending
	default:
		for i := len(snap.Items) - 1; i >= 0; i-- {
			if snap.Items[i].Kind != session.KindAssistant {
				continue
			}
			for _, line := range strings.Split(snap.Items[i].Text, "\n") {
				if strings.TrimSpace(line) != "" {
					text = line
					break
				}
			}
			break
		}
	}
	if len(text) > 120 {
		text = text[:120]
	}
	return text
}

// programStatus maps the session state onto OSC 7501. An idle session that has replied reads as
// done, which the terminal keeps showing until the user has looked.
func (m *Model) programStatus() string {
	message := m.lastMessage()
	switch {
	case m.snap.Question != nil:
		return programstatus.Report("blocked", "question", message)
	case m.snap.Pending != "":
		return programstatus.Report("blocked", "permission", message)
	case m.sessionState() == registry.Working:
		return programstatus.Report("working", "", "")
	case message != "":
		return programstatus.Report("done", "", message)
	default:
		return programstatus.Report("idle", "", "")
	}
}

// reportStatus sends the program status when it changed; tea.Raw keeps it out of a frame's bytes.
func (m *Model) reportStatus() tea.Cmd {
	status := m.programStatus()
	if status == m.lastStatus {
		return nil
	}
	m.lastStatus = status
	return tea.Raw(status)
}

var (
	notifyDesktop   = desktop.Notify
	terminalInFront = desktop.TerminalInFront
)

// publishState records this session's state and announces its own transitions.
//
// A session announces only its own, so one event produces one notification however many panes
// are running. The desktop notification is skipped while the user is looking at this pane: it
// has focus and the terminal is the frontmost app. Focus alone is not enough, since a
// multiplexer may never tell the pane that the terminal went to the background.
func (m *Model) publishState() tea.Cmd {
	if m.Registry == nil {
		return nil
	}
	state := m.sessionState()
	message := m.lastMessage()

	previous := m.ownState
	m.ownState = state

	bookmark, change := "", ""
	if m.vcs != nil {
		bookmark, change = m.vcs.Bookmark, m.vcs.Description
	}
	tasks := SessionTasks(m.snap)
	topic := m.snap.Topic
	patch := registry.Patch{State: &state, LastMessage: &message, Bookmark: &bookmark, Change: &change, Tasks: &tasks, Topic: &topic}
	if previous == registry.Working && state == registry.Idle && !m.focused {
		unseen := true
		patch.Unseen = &unseen
	}
	m.Registry.Publish(patch)

	transition := registry.DescribeTransition(previous, registry.Info{State: state, LastMessage: message})
	if transition == "" {
		return nil
	}
	if state == registry.Idle && m.snap.Woken {
		if m.holdForBatch() {
			return nil
		}
		if summary := m.batchSummary(); summary != "" {
			transition = summary
		}
	}
	m.heldSince = time.Time{}
	m.markAnnounced()
	if m.Viewing != nil && m.Viewing() {
		return nil
	}
	return m.desktopNotify(transition)
}

// desktopNotify raises a desktop notification under this session's name unless the user is looking at the pane.
func (m *Model) desktopNotify(text string) tea.Cmd {
	if m.Registry == nil {
		return nil
	}
	name := m.Registry.Name()
	if !m.focused {
		notifyDesktop(name, text)
		return nil
	}
	return func() tea.Msg {
		if !terminalInFront() {
			notifyDesktop(name, text)
		}
		return nil
	}
}

// batchHold is the longest a wake turn's desktop notification waits on runs still going.
const batchHold = 10 * time.Minute

// holdForBatch reports whether a finished wake turn should stay off the desktop because other
// runs are still going, so a batch of subagents notifies once when the last one lands.
func (m *Model) holdForBatch() bool {
	if !slices.ContainsFunc(m.snap.Runs, subagent.Run.Live) {
		return false
	}
	if m.heldSince.IsZero() {
		m.heldSince = m.now()
	}
	return m.now().Sub(m.heldSince) < batchHold
}

// batchSummary names every finished background run no notification has covered yet, or "" when
// there are fewer than two, since one run is better told by the turn's own last message.
func (m *Model) batchSummary() string {
	var parts []string
	for _, run := range m.snap.Runs {
		if !run.Background || run.Live() || m.announced[run.ID] {
			continue
		}
		label := run.Label()
		if label == "" {
			label = run.Agent
		}
		if run.State == subagent.Done {
			parts = append(parts, label+" ✓")
		} else {
			parts = append(parts, label+" ✗ "+string(run.State))
		}
	}
	if len(parts) < 2 {
		return ""
	}
	return fmt.Sprintf("%d subagents finished: %s", len(parts), strings.Join(parts, ", "))
}

func (m *Model) markAnnounced() {
	if m.announced == nil {
		m.announced = map[int]bool{}
	}
	for _, run := range m.snap.Runs {
		if !run.Live() {
			m.announced[run.ID] = true
		}
	}
}

// noticeOthers reports what changed about the other sessions since the last poll. The first
// read is a baseline: only changes after it are worth a notice.
func (m *Model) noticeOthers(sessions []registry.Info) tea.Cmd {
	previous := map[int]registry.State{}
	for _, s := range m.others {
		previous[s.PID] = s.State
	}
	if len(m.others) == 0 {
		return nil
	}
	var cmds []tea.Cmd
	for _, other := range sessions {
		if transition := registry.DescribeTransition(previous[other.PID], other); transition != "" {
			cmds = append(cmds, m.Notify(registry.Label(other, sessions)+" "+transition))
		}
	}
	return tea.Batch(cmds...)
}
