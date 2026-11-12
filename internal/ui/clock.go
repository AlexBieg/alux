package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/subagent"
)

// Every message costs a full render, so the clock ticks at MiniDot's twelve a second only while
// something animates; otherwise it sets one timer for the next tick whose frame differs.

type idleWake struct{ gen, at int }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if wake, ok := msg.(idleWake); ok {
		m.onIdleWake(wake)
		return m, m.clock()
	}
	model, cmd := m.update(msg)
	return model, tea.Batch(cmd, m.clock())
}

func (m *Model) clock() tea.Cmd {
	if m.animating() {
		m.wakeGen, m.wakeAt = m.wakeGen+1, 0
		if m.ticking {
			return nil
		}
		m.ticking = true
		return m.spin.Tick
	}
	at := m.nextIdleFrame()
	if at == m.wakeAt {
		return nil
	}
	m.wakeGen, m.wakeAt = m.wakeGen+1, at
	if at == 0 {
		return nil
	}
	gen, wait := m.wakeGen, time.Duration(at-m.poseTick)*m.spin.Spinner.FPS
	return tea.Tick(wait, func(time.Time) tea.Msg { return idleWake{gen: gen, at: at} })
}

func (m *Model) onIdleWake(wake idleWake) {
	if wake.gen != m.wakeGen {
		return
	}
	m.reuseFrame = false
	m.wakeAt = 0
	m.poseTick = wake.at
	m.poseAlux()
}

// animating is whether the next tick's frame can differ from this one's.
func (m *Model) animating() bool {
	if m.snap.Busy || m.snap.Question != nil || m.snap.Pending != "" || len(m.finished) > 0 || m.hasLiveBox() {
		return true
	}
	if m.welcoming() && m.poseTick-m.welcome.at < welcomeWaveTicks {
		return true
	}
	if a := &m.alux; a.mood != moodStand && a.mood != moodDoze || m.aluxWant() != a.mood {
		return true
	}
	if m.fileGraph != nil && !m.fileGraph.diff && m.fileGraph.anyAsking() {
		return true
	}
	for _, run := range m.snap.Runs {
		if run.State == subagent.Running || run.State == subagent.Waiting {
			return true
		}
	}
	if m.Registry != nil {
		for _, s := range m.otherSessions(m.others) {
			if s.State == registry.Waiting {
				return true
			}
		}
	}
	return false
}

// nextIdleFrame is the tick of the next change at rest: his blink or his doze's z, and an idle
// run's dozing eyes. It depends on the tick alone, so a message at rest re-arms no timer; dozing
// off waits for the blink after aluxDozeAfter.
func (m *Model) nextIdleFrame() int {
	t, next := m.poseTick, 0
	soonest := func(at int) {
		if next == 0 || at < next {
			next = at
		}
	}
	switch m.alux.mood {
	case moodStand:
		soonest(nextTick(t, aluxBlinkGap, 0))
		soonest(nextTick(t, aluxBlinkGap, 2))
	case moodDoze:
		soonest(nextTick(t, 2*aluxHold, 0))
	}
	for _, run := range m.tileRuns() {
		if run.State == subagent.Idle {
			soonest(nextTick(t, 2*aluxHold, 0))
		}
	}
	return next
}

// nextTick is the first tick after t that is off modulo every.
func nextTick(t, every, off int) int {
	return t + 1 + ((off-t-1)%every+every)%every
}
