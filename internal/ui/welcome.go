package ui

import (
	"image/color"
	"math/rand/v2"
	"os/user"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/session"
)

// Setup is what the welcome screen's checklist reports.
type Setup struct {
	Project string
	// Missing is the search tools not found on PATH.
	Missing     []string
	AuthWarning string
	// Records is the record store's path, "" when it is off.
	Records              string
	Vault, PRs, Calendar bool
}

// GuardianMsg carries guardian.Check's answer; a nil Err means guardian is ready.
type GuardianMsg struct{ Err error }

type guardianCheck struct {
	done bool
	err  error
}

type welcomeState struct {
	shown   bool
	at, tip int
	// items is the transcript length last seen; only a clear lowers it.
	items int
}

const (
	welcomeHints = "/ commands · shift+tab mode · ⌘e running · ctrl+c kill"
	welcomeLabel = 12
	settingsPath = "~/.config/alux/settings.json"

	welcomeWaves        = 3
	welcomeWaveTicks    = 2 * welcomeWaves * aluxStepEvery
	welcomeSideGap      = 5
	welcomeSideMin      = 20
	welcomeWordmarkWide = 19
)

var welcomeWordmark = []string{"▄▀▀▄ █    █  █ ▀▄▄▀", "█▀▀█ █▄▄▄ ▀▄▄▀ ▄▀▀▄"}

var modeCallout = map[session.Mode]string{
	session.ModeReadOnly: "read-only mode: only reading runs without asking; edits, subagents and every other command ask first. shift+tab to change.",
	session.ModeLocal:    "local mode: reading, building and edits the guardian clears run without asking; network calls ask first. shift+tab to change.",
	session.ModeAuto:     "auto mode: commands, edits and network calls the guardian clears run without asking. shift+tab to change.",
}

var welcomeTips = []string{
	"shift+tab to read-only before poking at anything you care about",
	"/new or /clear starts a fresh session",
	"⌘e moves into the running box: up and down pick an item, enter opens it",
	"esc stops a running turn and drops anything you queued behind it",
	"backspace on an empty prompt pulls your last queued message back",
	"/skills lists your skills; /skills run <name> starts one",
	"ALUX_RECORDS=0 turns the record store off",
	"/context shows where the prompt's tokens are going",
}

var (
	okStyle  = lipgloss.NewStyle().Foreground(lipgloss.Green)
	boldText = lipgloss.NewStyle().Bold(true)
	mayaBlue = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2fa4b9"))
)

// welcoming holds until the first prompt, whatever notes arrive before it.
func (m *Model) welcoming() bool {
	if m.Setup == nil {
		return false
	}
	for _, item := range m.snap.Items {
		if item.Kind == session.KindUser {
			return false
		}
	}
	return true
}

func (m *Model) welcomeLines(width int) []string {
	lines := m.welcomeHead(width)
	lines = append(lines, "")
	if mode := session.Effective(m.snap.Mode); modeCallout[mode] != "" {
		lines = append(lines, callout(modeCallout[mode], modeColor[mode], width)...)
		lines = append(lines, "")
	}
	lines = append(lines, styledWrap(welcomeHints, dimStyle, width)...)
	lines = append(lines, "")
	lines = append(lines, styledWrap("tip: "+welcomeTips[m.welcome.tip], dimStyle, width)...)
	lines = append(lines, "")
	return append(lines, m.checklistLines(width)...)
}

// callout is a markdown blockquote: a bar in bar's colour down the left of every wrapped row.
func callout(text string, bar color.Color, width int) []string {
	mark := lipgloss.NewStyle().Foreground(bar).Render("▌") + " "
	lines := wrapPlain(text, width-2)
	for i, line := range lines {
		lines[i] = mark + line
	}
	return lines
}

// noteWelcome gives each new welcome its own wave and tip.
func (m *Model) noteWelcome() {
	w := &m.welcome
	if !m.welcoming() {
		*w = welcomeState{}
		return
	}
	if !w.shown || len(m.snap.Items) < w.items {
		*w = welcomeState{shown: true, at: m.poseTick, tip: rand.IntN(len(welcomeTips))}
	}
	w.items = len(m.snap.Items)
}

// greetingEyes is his waving face for the welcome's first few beats, false once he has rested.
func (m *Model) greetingEyes() (string, bool) {
	t := m.poseTick - m.welcome.at
	if !m.welcoming() || t >= welcomeWaveTicks {
		return "", false
	}
	if (t/aluxStepEvery)%2 == 1 {
		return aluxGreetDown, true
	}
	return aluxGreetUp, true
}

// The mascot is not part of the head: he greets from his place at the sidebar's foot.
func (m *Model) welcomeHead(width int) []string {
	if width < welcomeWordmarkWide {
		return m.welcomeText(width)
	}
	var word []string
	for _, row := range welcomeWordmark {
		word = append(word, mayaBlue.Render(row))
	}
	wordmark := strings.Join(word, "\n")
	side := width - welcomeWordmarkWide - welcomeSideGap
	if side < welcomeSideMin {
		return append(strings.Split(wordmark, "\n"), m.welcomeText(width)...)
	}
	text := strings.Join(m.welcomeText(side), "\n")
	head := lipgloss.JoinHorizontal(lipgloss.Top, wordmark, strings.Repeat(" ", welcomeSideGap), text)
	return strings.Split(head, "\n")
}

// welcomeText is the greeting over the mode and project.
func (m *Model) welcomeText(width int) []string {
	project := "alux"
	if m.Setup.Project != "" {
		project = m.Setup.Project
	}
	mode := string(session.Effective(m.snap.Mode)) + " mode · " + project
	return append(styledWrap(greeting(m.now(), m.account()), boldText, width), styledWrap(mode, dimStyle, width)...)
}

// greeting is the time of day and the first word of name, when there is one.
func greeting(now time.Time, name string) string {
	hello := "good evening"
	switch h := now.Hour(); {
	case h < 12:
		hello = "good morning"
	case h < 18:
		hello = "good afternoon"
	}
	if words := strings.Fields(name); len(words) > 0 {
		hello += ", " + words[0]
	}
	return hello
}

func accountName() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Name
}

// checklistLines is one line until a row needs fixing, then a row per item.
func (m *Model) checklistLines(width int) []string {
	rows := m.checklist()
	var done, unset []string
	for _, row := range rows {
		switch row.mark {
		case markFail, markPending:
			var lines []string
			for _, row := range rows {
				lines = append(lines, row.render(width)...)
			}
			return lines
		case markOK:
			done = append(done, markOK+" "+row.label)
		default:
			unset = append(unset, row.label)
		}
	}
	text := strings.Join(done, "  ")
	if len(unset) > 0 {
		text += "  " + markUnset + " " + strings.Join(unset, ", ") + " not set up"
	}
	var lines []string
	faded := false
	for _, line := range wrapPlain(text, width) {
		line = strings.TrimLeft(line, " ")
		if !faded {
			head, tail, found := strings.Cut(line, markUnset)
			faded = found
			line = strings.ReplaceAll(head, markOK, okStyle.Render(markOK))
			if found {
				line += dimStyle.Render(markUnset + tail)
			}
		} else {
			line = dimStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return lines
}

type checkRow struct {
	mark, label, detail string
}

const (
	markOK      = "✓"
	markFail    = "✗"
	markUnset   = "·"
	markPending = "…"
)

func (r checkRow) render(width int) []string {
	markStyle, detailStyle := dimStyle, dimStyle
	switch r.mark {
	case markOK:
		markStyle = okStyle
	case markFail:
		markStyle, detailStyle = errorStyle, plainStyle
	}
	label := r.label + strings.Repeat(" ", max(1, welcomeLabel-len([]rune(r.label))))
	prefix := markStyle.Render(r.mark) + " " + label
	return hang(prefix, 2+lipgloss.Width(label), r.detail, detailStyle, width)
}

func (m *Model) checklist() []checkRow {
	s := m.Setup
	rows := []checkRow{m.guardianRow()}

	if len(s.Missing) > 0 {
		rows = append(rows, checkRow{markFail, "rg and fd", strings.Join(s.Missing, " and ") + " not found on PATH; run `brew install ripgrep fd`"})
	} else {
		rows = append(rows, checkRow{markOK, "rg and fd", "on PATH"})
	}

	if s.AuthWarning != "" {
		rows = append(rows, checkRow{markFail, "API key", s.AuthWarning})
	} else {
		rows = append(rows, checkRow{markOK, "API key", "found"})
	}

	if s.Records != "" {
		rows = append(rows, checkRow{markOK, "records", "on · " + s.Records})
	} else {
		rows = append(rows, checkRow{markUnset, "records", "off; unset ALUX_RECORDS to turn it on"})
	}

	rows = append(rows,
		configuredRow("vault", s.Vault, `add "vault": "/path/to/vault" to `+settingsPath),
		configuredRow("PRs", s.PRs, `add a "prs" block to `+settingsPath),
		configuredRow("calendar", s.Calendar, `add "calendar": true to `+settingsPath),
	)
	return rows
}

func (m *Model) guardianRow() checkRow {
	switch {
	case !m.guardian.done:
		return checkRow{markPending, "guardian", "checking…"}
	case m.guardian.err != nil:
		return checkRow{markFail, "guardian", strings.TrimPrefix(m.guardian.err.Error(), "guardian: ")}
	}
	return checkRow{markOK, "guardian", "Ollama is up and the model is pulled"}
}

func configuredRow(label string, on bool, fix string) checkRow {
	if on {
		return checkRow{markOK, label, "configured"}
	}
	return checkRow{markUnset, label, "not set up; " + fix}
}
