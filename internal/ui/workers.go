package ui

import (
	"fmt"
	"slices"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/workers"
)

// workerPrompt is a headless worker's parked prompt, as its session reported it; seen is when this pane first saw it.
type workerPrompt struct {
	key  string
	pid  int
	p    workers.Prompt
	seen time.Time
}

// workerRetry asks the workers again after one published a prompt id it then could not be asked for.
type workerRetry struct{}

// onScreenPrompt is the prompt the input box is showing; worker is nil when it is this session's own.
type onScreenPrompt struct {
	worker                 *workerPrompt
	pending, body, summary string
	question               *ask.Question
}

func (p onScreenPrompt) waiting() bool { return p.question != nil || p.pending != "" }

func (m *Model) onScreen() onScreenPrompt {
	if t := m.shown; t != nil {
		if t.p.Kind == "question" {
			return onScreenPrompt{worker: t, question: t.p.Question}
		}
		return onScreenPrompt{worker: t, pending: t.p.Label, body: t.p.Body}
	}
	return onScreenPrompt{pending: m.snap.Pending, body: m.snap.PendingBody, summary: m.snap.PendingSummary, question: m.snap.Question}
}

// refreshWorkers rebuilds the worker prompt queue from the registry, the prompt this pane saw first at its head.
func (m *Model) refreshWorkers(others []registry.Info) tea.Cmd {
	previous := map[string]workerPrompt{}
	for _, t := range m.queue {
		previous[t.key] = t
	}
	var queue []workerPrompt
	missing := false
	for _, o := range others {
		if o.Worker == "" || o.PromptID == "" || o.Origin != m.Origin || m.ParkedPrompt == nil {
			continue
		}
		if id, ok := m.answered[o.Worker]; ok && id == o.PromptID {
			continue
		}
		if t, ok := previous[o.Worker]; ok && t.p.ID == o.PromptID {
			queue = append(queue, t)
			continue
		}
		p, ok := m.ParkedPrompt(o.PID)
		if !ok || p.ID != o.PromptID || !p.Complete() {
			missing = true
			continue
		}
		queue = append(queue, workerPrompt{key: o.Worker, pid: o.PID, p: p, seen: m.now()})
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].seen.Before(queue[j].seen) })
	m.queue = queue
	for key, id := range m.answered {
		if !slices.ContainsFunc(others, func(o registry.Info) bool { return o.Worker == key && o.PromptID == id }) {
			delete(m.answered, key)
		}
	}
	if s := m.shown; s != nil && !slices.ContainsFunc(queue, func(t workerPrompt) bool { return t.key == s.key && t.p.ID == s.p.ID }) {
		m.setShown(nil)
	}

	var cmds []tea.Cmd
	for _, t := range queue {
		if previous[t.key].p.ID != t.p.ID {
			cmds = append(cmds, m.desktopNotify(fmt.Sprintf("[%s] is waiting: %s", t.key, t.p.Label)))
		}
	}
	if missing && !m.workerRetrying {
		m.workerRetrying = true
		cmds = append(cmds, tea.Tick(time.Second, func(time.Time) tea.Msg { return workerRetry{} }))
	}
	return tea.Batch(cmds...)
}

// pickPrompt pins the oldest worker prompt once nothing else is on screen; this session's own prompt goes first.
func (m *Model) pickPrompt() {
	if m.shown != nil || m.snap.Question != nil || m.snap.Pending != "" {
		return
	}
	for i := range m.queue {
		t := m.queue[i]
		if id, ok := m.setAside[t.key]; ok && id == t.p.ID {
			continue
		}
		m.setShown(&t)
		return
	}
}

func (m *Model) setShown(t *workerPrompt) {
	m.shown = t
	m.cursor, m.noting = 0, false
	m.note.SetValue("")
	if t != nil {
		m.sideFocus.on = false
	}
}

// respondWorker sends this pane's answer to the worker and takes the prompt off screen; one the
// worker had already moved past is dropped there, and the registry brings up whatever replaced it.
func (m *Model) respondWorker(t *workerPrompt, approve bool, a *ask.Answer) tea.Cmd {
	if m.AnswerPrompt == nil {
		return m.Notify(fmt.Sprintf("could not answer [%s]: this pane cannot reach workers", t.key))
	}
	if _, err := m.AnswerPrompt(t.pid, t.p.ID, approve, a); err != nil {
		return m.Notify(fmt.Sprintf("could not answer [%s]: %v", t.key, err))
	}
	if m.answered == nil {
		m.answered = map[string]string{}
	}
	m.answered[t.key] = t.p.ID
	m.queue = slices.DeleteFunc(m.queue, func(q workerPrompt) bool { return q.key == t.key })
	m.setShown(nil)
	m.pickPrompt()
	m.layout()
	return nil
}

// setAsideWorker takes the prompt off screen unanswered, until the registry next changes.
func (m *Model) setAsideWorker(t *workerPrompt) tea.Cmd {
	if m.setAside == nil {
		m.setAside = map[string]string{}
	}
	m.setAside[t.key] = t.p.ID
	m.setShown(nil)
	m.pickPrompt()
	m.layout()
	return nil
}
