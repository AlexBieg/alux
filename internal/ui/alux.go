package ui

import (
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/session"
)

// The alux stands at the empty foot of the open sidebar and poses for whatever the turn is doing.

const (
	aluxWide      = 7
	aluxStepEvery = 3  // ticks per step
	aluxBlinkGap  = 60 // ticks between blinks
	aluxTapEvery  = 4
	aluxBeat      = 6
	// aluxHold is the least a pose stays up, 1.5s, so a burst of calls does not flicker.
	aluxHold = 18
	// aluxGrace keeps a finished call's pose through the gap before the next call.
	aluxGrace     = 24
	aluxDozeAfter = 5 * time.Minute
)

type aluxMood int

const (
	moodStand aluxMood = iota
	moodTalk
	moodThink
	moodLook
	moodWork
	moodWait
	moodWeb
	moodDelegate
	moodNeed
	moodOops
	moodDone
	moodDoze
)

type aluxState struct {
	x, dir int
	mood   aluxMood
	at     int // tick the mood began
	// cue is the pose the latest tool call asked for, and cueAt its tick; moodStand is none.
	cue    aluxMood
	cueAt  int
	unseen int // lowest item id not yet reacted to
	primed bool
	idle   time.Time
}

var (
	aluxHat  = lipgloss.NewStyle().Foreground(lipgloss.Color("#C9B27C"))
	aluxFace = lipgloss.NewStyle().Foreground(mutedColor)
	aluxFeet = lipgloss.NewStyle().Foreground(mutedColor)
)

const (
	// The hats stop short of the cell top, so runs stacked in the sidebar do not touch.
	aluxHatRow    = " ▁▃▅▃▁ "
	aluxHatLifted = " ▔▀▀▀▔ "
	// Two question marks take turns hopping over his head while he waits on the user.
	aluxAskLeft     = "  ?    "
	aluxAskRight    = "    ?  "
	aluxHatAskRight = " ▁▃▅▃▁?"
	aluxHatAskLeft  = "?▁▃▅▃▁ "
	// Eye holes sit in the upper half only: the head narrows below, so a lower hole opens onto its edge.
	aluxEyesSad     = " ▝▄█▄▘ "
	aluxEyesDoze    = " ▝███▘z"
	aluxEyesUp      = " ▝▙█▟▘ "
	aluxEyesShut    = " ▝███▘ "
	aluxEyesUpLeft  = " ▝▟█▟▘ "
	aluxEyesUpRight = " ▝▙█▙▘ "
	aluxGreetUp     = " ▝▙█▟▘▀"
	aluxGreetDown   = " ▝▙█▟▘▄"
	aluxHammerUp    = aluxGreetUp
	aluxHammerDown  = aluxGreetDown
	aluxWaveUp      = aluxGreetUp
	aluxWaveDown    = aluxGreetDown
	aluxFeetApart   = "  ▔ ▔  "
	aluxFeetShut    = "   ▔   "
	aluxFeetLeft    = "  ▔    "
	aluxFeetRight   = "    ▔  "
	aluxFeetSit     = "  ▔▔▔  "
)

var aluxBlank = strings.Repeat(" ", aluxWide)

// runHats and hatColors dress each subagent run, picked by a hash of its id so a run keeps its hat;
// the main alux keeps aluxHatRow. The colours stay clear of the state dot's green, yellow and red.
var (
	runHats   = []string{" ▄▄▄▄▄ ", " ▃▅▅▅▃ ", " ▁▅▁▅▁ ", " ▅▂▅▂▅ ", " ▁▅▅▅▁ ", " ▅▅▃▁▁ ", " ▂▄▄▃▁ "}
	hatColors = []string{"#af87ff", "#d78700", "#ff87af", "#00afaf", "#87afd7", "#d7af87", "#d7afd7"}
)

func runHat(run int) string {
	return runHats[mix(uint32(run))%uint32(len(runHats))]
}

func runHatStyle(run int) lipgloss.Style {
	hex := hatColors[mix(mix(uint32(run)))%uint32(len(hatColors))]
	return lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
}

// tipHat slides a hat by n cells, right for n > 0, keeping it aluxWide cells.
func tipHat(hat string, n int) string {
	cells := []rune(hat)
	if n > 0 {
		return strings.Repeat(" ", n) + string(cells[:len(cells)-n])
	}
	return string(cells[-n:]) + strings.Repeat(" ", -n)
}

// holdFor is the least a pose stays up; resting poses give way at once.
func holdFor(mood aluxMood) int {
	switch mood {
	case moodStand, moodTalk, moodDoze:
		return 0
	case moodOops:
		return 2 * aluxBeat
	case moodDone, moodDelegate:
		return 3 * aluxBeat
	}
	return aluxHold
}

// toolMood is the pose a tool box asks for, false for one that asks for none.
func toolMood(item session.Item) (aluxMood, bool) {
	switch item.Tool {
	case agent.KindEdit:
		if strings.HasPrefix(item.Text, "view ") {
			return moodLook, true
		}
		return moodWork, true
	case agent.KindBash, agent.KindPoll:
		return moodWait, true
	case agent.KindWeb:
		return moodWeb, true
	case agent.KindAgent:
		return moodDelegate, true
	case agent.KindSearch, agent.KindVault, agent.KindRecord, agent.KindMCP, agent.KindCalendar, agent.KindSkill:
		return moodLook, true
	}
	return moodStand, false
}

// noteAlux reacts to new items: a tool call cues a pose; an error or a turn's end cuts in.
func (m *Model) noteAlux(wasBusy, wasCancelling bool) {
	a := &m.alux
	next := a.unseen
	cued := false
	for i := len(m.snap.Items) - 1; i >= 0; i-- {
		it := m.snap.Items[i]
		if it.ID < a.unseen {
			break
		}
		next = max(next, it.ID+1)
		if !a.primed {
			continue
		}
		if it.Kind == session.KindError {
			a.mood, a.at = moodOops, m.poseTick
		}
		if mood, ok := toolMood(it); ok && it.Kind == session.KindTool && !cued {
			a.cue, a.cueAt, cued = mood, m.poseTick, true
		}
	}
	a.unseen, a.primed = next, true
	if wasBusy && !m.snap.Busy {
		a.idle, a.cue = m.now(), moodStand
		if !wasCancelling && a.mood != moodOops {
			a.mood, a.at = moodDone, m.poseTick
		}
	}
}

// aluxWant is the pose the moment asks for, before the hold has its say.
func (m *Model) aluxWant() aluxMood {
	a := &m.alux
	switch {
	case m.onScreen().waiting():
		return moodNeed
	case !m.snap.Busy:
		if a.idle.IsZero() {
			a.idle = m.now()
		}
		if m.now().Sub(a.idle) >= aluxDozeAfter {
			return moodDoze
		}
		return moodStand
	}
	if mood, ok := m.runningMood(); ok {
		a.cue, a.cueAt = mood, m.poseTick
		return mood
	}
	if a.cue != moodStand && m.poseTick-a.cueAt < aluxGrace {
		return a.cue
	}
	if m.snap.LiveThinking != nil {
		return moodThink
	}
	if m.speaking() {
		return moodStand
	}
	return moodTalk
}

// runningMood is the pose for the calls still running, the busiest kind winning a parallel batch.
func (m *Model) runningMood() (aluxMood, bool) {
	best, found := moodStand, false
	for _, it := range m.snap.Items {
		if !it.Running || it.Kind != session.KindTool {
			continue
		}
		if mood, ok := toolMood(it); ok && (!found || aluxRank(mood) > aluxRank(best)) {
			best, found = mood, true
		}
	}
	return best, found
}

func aluxRank(mood aluxMood) int {
	switch mood {
	case moodWork:
		return 4
	case moodWait:
		return 3
	case moodWeb:
		return 2
	case moodDelegate:
		return 1
	}
	return 0
}

// poseAlux settles his pose for this tick and walks him if the pose walks.
func (m *Model) poseAlux() {
	a := &m.alux
	want := m.aluxWant()
	if want != a.mood && (want == moodNeed || m.poseTick-a.at >= holdFor(a.mood)) {
		a.mood, a.at = want, m.poseTick
	}
	m.stepAlux()
}

// stepAlux moves him one cell along the sidebar's foot, turning at either end.
func (m *Model) stepAlux() {
	a := &m.alux
	if a.mood != moodTalk || m.poseTick%aluxStepEvery != 0 {
		return
	}
	span := sidebarWide - aluxWide
	if a.dir == 0 {
		a.dir = 1
	}
	if next := a.x + a.dir; next < 0 || next > span {
		a.dir = -a.dir
	}
	a.x = min(max(a.x+a.dir, 0), span)
}

// aluxTop is the screen row of the clear row above his hat, false when he is not drawn. He stands
// just above the input, beside the command suggestions rather than above them.
func (m *Model) aluxTop() (int, bool) {
	if !m.sidebarDrawnOpen() || m.picker != nil {
		return 0, false
	}
	box, _ := m.sidebar()
	if box == "" {
		return 0, false
	}
	bottom := m.height - lipgloss.Height(m.renderBottom())
	feet := bottom
	if commands := m.renderCommands(); commands != "" {
		feet += lipgloss.Height(commands)
	}
	// He needs one clear row between his hat and the box, which hops and the lifted hat use.
	if feet-2 <= m.sidebarTop()+lipgloss.Height(box) {
		return 0, false
	}
	return feet - 3, true
}

func (m *Model) overlayAlux(body string) string {
	top, ok := m.aluxTop()
	if !ok {
		return body
	}
	x := m.mainWidth() + m.alux.x
	layers := []*lipgloss.Layer{
		lipgloss.NewLayer(body),
		lipgloss.NewLayer(strings.Join(m.aluxRows(), "\n")).X(x).Y(top).Z(1),
	}
	if bubble := m.noticeCard(sidebarWide, top-1-m.sidebarTop()); bubble != "" {
		// The tail leans from the bubble's middle down to his head, and moves with him.
		head := x + aluxWide/2
		tail, tailX := "╲", head-1
		if head < m.mainWidth()+sidebarWide/2 {
			tail, tailX = "╱", head+1
		}
		layers = append(layers,
			lipgloss.NewLayer(noticeStyle.Render(tail)).X(tailX).Y(top-1).Z(1),
			lipgloss.NewLayer(bubble).X(m.mainWidth()).Y(top-1-lipgloss.Height(bubble)).Z(1))
	}
	return lipgloss.NewCompositor(layers...).Render()
}

// noticeCard is the front notice framed width wide in at most room rows, opaque over whatever is
// behind it; empty when there is nothing to say or no room. Its title counts those waiting.
func (m *Model) noticeCard(width, room int) string {
	if len(m.notices) == 0 || room < 3 || width < noticeMinWide {
		return ""
	}
	rows := wrapPlain(m.notices[0].text, panelInner(width))
	for i, r := range rows {
		rows[i] = noticeStyle.Render(r)
	}
	title := ""
	if waiting := len(m.notices) - 1; waiting > 0 {
		title = "+" + strconv.Itoa(waiting)
	}
	return strings.Join(framed(title, clipRows(rows, min(noticeMaxTall, room-2), ""), width, 1, noticeStyle), "\n")
}

// overlayNotices is the fallback while he is not drawn: the card over the transcript's top-left.
func (m *Model) overlayNotices(body string) string {
	if _, drawn := m.aluxTop(); drawn {
		return body
	}
	card := m.noticeCard(min(noticeMaxWide, m.mainWidth()-2), m.height-m.transcriptTop())
	if card == "" {
		return body
	}
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(body),
		lipgloss.NewLayer(card).X(1).Y(m.transcriptTop()).Z(1),
	).Render()
}

// speaking is whether a notice is up, which stops him walking so the tail holds still to be read.
func (m *Model) speaking() bool {
	_, drawn := m.aluxTop()
	return drawn && len(m.notices) > 0
}

// aluxRows is the clear row above him and his three rows, posed for the current tick.
func (m *Model) aluxRows() []string {
	a := &m.alux
	t := m.poseTick - a.at
	top, topStyle := aluxBlank, aluxFace
	hat, eyes, feet := aluxHatRow, aluxEyesUp, aluxFeetApart
	blinks, airborne := true, false
	switch a.mood {
	case moodTalk:
		blinks = false
		eyes = aluxEyesUpRight
		if a.dir < 0 {
			eyes = aluxEyesUpLeft
		}
		if (m.poseTick/aluxStepEvery)%2 == 1 {
			feet = aluxFeetShut
		}
	case moodLook:
		eyes = aluxEyesUpLeft
		if (m.poseTick/(2*aluxStepEvery))%2 == 1 {
			eyes = aluxEyesUpRight
		}
	case moodThink:
		eyes = aluxEyesUpLeft
		blinks = false
		if m.poseTick%(2*aluxBlinkGap) < 3 {
			eyes = aluxEyesShut
		}
	case moodWork:
		blinks = false
		eyes = aluxHammerUp
		if (m.poseTick/aluxStepEvery)%2 == 1 {
			eyes = aluxHammerDown
		}
	case moodWait:
		eyes, feet = aluxEyesUpRight, aluxFeetLeft
		if (m.poseTick/aluxTapEvery)%2 == 1 {
			feet = aluxFeetRight
		}
	case moodWeb:
		hat, eyes = tipHat(aluxHatRow, 1), aluxEyesUpRight
	case moodDelegate:
		if t >= aluxBeat && t < 2*aluxBeat {
			top, topStyle, hat = aluxHatRow, aluxHat, aluxBlank
		}
	case moodNeed:
		eyes, blinks, topStyle = aluxEyesUp, false, aluxHat
		top, hat = aluxAskLeft, aluxHatAskRight
		if (t/aluxBeat)%2 == 1 {
			top, hat = aluxAskRight, aluxHatAskLeft
		}
	case moodOops:
		hat, eyes, feet, blinks = tipHat(aluxHatRow, -1), aluxEyesShut, aluxFeetShut, false
	case moodDone:
		blinks = false
		airborne = t >= aluxBeat && t < 2*aluxBeat
	case moodDoze:
		hat, eyes, feet, blinks = tipHat(aluxHatRow, 1), aluxEyesShut, aluxFeetSit, false
		top = "     z "
		if (m.poseTick/(2*aluxHold))%2 == 1 {
			top = "      z"
		}
	}
	if blinks && m.poseTick%aluxBlinkGap < 2 {
		eyes = aluxEyesShut
	}
	if wave, ok := m.greetingEyes(); ok && a.mood == moodStand {
		eyes = wave
	}
	if airborne {
		feet = aluxFeetShut
	}
	rows := []string{topStyle.Render(top), aluxHat.Render(hat), aluxFace.Render(eyes), aluxFeet.Render(feet)}
	if airborne {
		rows = append(rows[1:], aluxBlank)
	}
	return rows
}
