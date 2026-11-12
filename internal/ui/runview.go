package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

// runView is one subagent's detail in the expanded running box.
type runView struct {
	id int
	// follow keeps a live run's newest step in sight until the user scrolls up.
	follow bool

	// events is the run's transcript, read off the update goroutine; nil until the first read lands.
	events  []subagent.TranscriptEvent
	loading bool
	// readStep, readState and readTrail are what the run was at when events was read, so a read is redone only once it
	// moves; the trail is there for a navigator, whose step moves once a consultation however much it reads.
	readStep  int
	readState subagent.State
	readTrail int
	read      bool
	// owner maps each view row to the box it belongs to; open is each box's default and expanded the user's clicks.
	owner    []int
	open     map[int]bool
	expanded map[int]bool
}

// runTranscriptMsg is a run's transcript as read for the open view.
type runTranscriptMsg struct {
	run    int
	step   int
	state  subagent.State
	trail  int
	events []subagent.TranscriptEvent
}

// loadRun reads the open run's transcript when it has never been read or the run has moved since.
func (m *Model) loadRun() tea.Cmd {
	v := m.runView
	if v == nil || v.loading || m.readTranscript == nil {
		return nil
	}
	run, ok := m.findRun(v.id)
	if !ok || run.Dispatch == "" || v.read && v.readStep == run.Step && v.readState == run.State && v.readTrail == len(run.Trail) {
		return nil
	}
	v.loading = true
	read, id, step, state, trail, dispatch := m.readTranscript, run.ID, run.Step, run.State, len(run.Trail), run.Dispatch
	return func() tea.Msg {
		events, _ := read(dispatch)
		return runTranscriptMsg{run: id, step: step, state: state, trail: trail, events: events}
	}
}

func (m *Model) onRunTranscript(msg runTranscriptMsg) tea.Cmd {
	v := m.runView
	if v == nil || v.id != msg.run {
		return nil
	}
	v.loading, v.read, v.readStep, v.readState, v.readTrail = false, true, msg.step, msg.state, msg.trail
	if len(msg.events) > 0 {
		v.events = msg.events
	}
	m.layout()
	return m.loadRun()
}

// runBoxOpen is isExpanded for a box in the run view: the user's click, or the default the view gave it.
func (m *Model) runBoxOpen(id int) (open, ok bool) {
	v := m.runView
	if v == nil || v.open == nil {
		return false, false
	}
	if _, mine := v.open[id]; !mine {
		return false, false
	}
	if open, clicked := v.expanded[id]; clicked {
		return open, true
	}
	return v.open[id], true
}

// onRunClick opens or closes the box under the panel's row.
func (m *Model) onRunClick(row int) tea.Cmd {
	v := m.runView
	line := m.pane.YOffset() + row
	if line < 0 || line >= len(v.owner) || v.owner[line] == noOwner {
		return nil
	}
	id := v.owner[line]
	open, _ := m.runBoxOpen(id)
	if v.expanded == nil {
		v.expanded = map[int]bool{}
	}
	v.expanded[id] = !open
	v.follow = false
	offset := m.pane.YOffset()
	m.layout()
	m.pane.SetYOffset(offset)
	return nil
}

const runViewHint = "esc closes the run"

// openRun expands the running box to a run's view and starts reading its transcript file.
func (m *Model) openRun(id int) tea.Cmd {
	if v := m.runView; v != nil {
		if v.id != id {
			*v = runView{id: id}
		}
		v.follow = true
	} else {
		m.closePanel()
		m.runView = &runView{id: id, follow: true}
		m.sel = selection{}
	}
	m.layout()
	return m.loadRun()
}

func (m *Model) closeRun() {
	m.runView = nil
	m.layout()
}

func (m *Model) onRunKey(msg tea.KeyPressMsg) tea.Cmd {
	if msg.String() == "enter" {
		m.closeRun()
		return nil
	}
	if !m.onPaneKey(msg.String()) {
		return nil
	}
	if m.runView != nil {
		m.runView.follow = m.pane.AtBottom()
	}
	return nil
}

func (m *Model) layoutRun() {
	d := m.runDetail(m.runView.id, m.paneWidth())
	m.runView.owner = d.owner
	m.pane.SetContentLines(d.lines)
	m.pane.SetXOffset(0)
	if m.runView.follow {
		m.pane.GotoBottom()
	}
}

// runDetail is the run's transcript as boxes a click opens once it has been read, else its task,
// trail and outcome.
func (m *Model) runDetail(id int, width int) owned {
	var o owned
	run, ok := m.findRun(id)
	if !ok {
		o.add(styledWrap("This run is no longer in the roster.", dimStyle, width), noOwner)
		return o
	}
	if v := m.runView; len(v.events) > 0 {
		items, open := transcriptItems(v.events, transcriptIDBase)
		v.open = open
		m.drawItems(&o, items, width)
		if run.Err != "" && !hasError(v.events) {
			o.add([]string{"", runRule("error", width)}, noOwner)
			o.add(styledWrap(run.Err, errorStyle, width), noOwner)
		}
	} else {
		o.add(runSections(run, width), noOwner)
	}
	return o
}

func hasError(events []subagent.TranscriptEvent) bool {
	for _, e := range events {
		if e.Kind == subagent.TranscriptError {
			return true
		}
	}
	return false
}

func runSections(run subagent.Run, width int) []string {
	out := []string{runRule("task", width)}
	out = append(out, styledWrap(run.Task, plainStyle, width)...)
	if trail := runTrail(run, width); len(trail) > 0 {
		out = append(out, "", runRule("trail", width))
		out = append(out, trail...)
	}
	if run.Result != "" {
		out = append(out, "", runRule("report", width))
		out = append(out, splitBlock(prose(run.Result, width))...)
	}
	if run.Err != "" {
		out = append(out, "", runRule("error", width))
		out = append(out, styledWrap(run.Err, errorStyle, width)...)
	}
	return out
}

func runTrail(run subagent.Run, width int) []string {
	var out []string
	for _, e := range run.Trail {
		out = append(out, renderEntry(e, width)...)
	}
	return out
}

// runRule opens a section of the run view, at most 40 columns so it reads as a heading and not a divider.
func runRule(name string, width int) string {
	rule := "── " + name + " "
	rule += strings.Repeat("─", max(0, min(width, 40)-ansi.StringWidth(rule)))
	return dimStyle.Render(ansi.Truncate(rule, max(1, width), ""))
}

func (m *Model) findRun(id int) (subagent.Run, bool) {
	for _, run := range m.snap.Runs {
		if run.ID == id {
			return run, true
		}
	}
	return subagent.Run{}, false
}

// runStatus is how far a run has got, in the words its finished header uses.
func runStatus(run subagent.Run) string {
	switch run.State {
	case subagent.Done:
		return fmt.Sprintf("done in %s · %s", runDuration(run.Ended.Sub(run.Started)), plural(run.Step, "step"))
	case subagent.Failed:
		return fmt.Sprintf("failed after %s · %s", runDuration(run.Ended.Sub(run.Started)), plural(run.Step, "step"))
	case subagent.Waiting:
		return "waiting on you"
	default:
		return fmt.Sprintf("running · %s", stepCount(run))
	}
}

func runDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// renderRunItem is one row: a dim marker while the run works, then its outcome; the output is in the run view.
func (m *Model) renderRunItem(item session.Item, width int) string {
	run, ok := m.findRun(item.Run)
	if !ok {
		return strings.Join(styledWrap("▸ agent (finished) · no longer in the roster", dimStyle, width), "\n")
	}
	tone := toneFor(item.Tool)
	if run.Live() {
		return oneRow(dimStyle.Render("▸ "+tileTitle(run)+" · dispatched"), width)
	}
	headStyle := plainStyle
	switch {
	case run.State == subagent.Failed:
		headStyle = errorStyle
	case tone != nil:
		headStyle = lipgloss.NewStyle().Foreground(tone)
	}
	return oneRow(headStyle.Render("▸ "+tileTitle(run)+" · "+runStatus(run)), width)
}

// visibleItem is false for a pair's navigator, whose steps show in its driver's run view.
func (m *Model) visibleItem(item session.Item) bool {
	if item.Run == 0 {
		return true
	}
	run, ok := m.findRun(item.Run)
	return !ok || run.Parent == 0
}
