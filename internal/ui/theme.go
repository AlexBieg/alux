package ui

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/session"
)

// The agent says what happened and the UI decides the colour, so a new tone is one entry here
// plus one ToolKind. Cyan, yellow and grey are reserved for session state and the approval
// border, so the later tones are hexes: the named palette ran out at four tools.
var toolColor = map[agent.ToolKind]color.Color{
	agent.KindBash:   lipgloss.Green,
	agent.KindMCP:    lipgloss.Blue,
	agent.KindEdit:   lipgloss.Color("#d78700"),
	agent.KindVault:  lipgloss.Color("#af87ff"),
	agent.KindSearch: lipgloss.Color("#00afaf"),
	// Not the search tone: searching the local disk and sending a query to a third party is the
	// distinction most worth seeing at a glance.
	agent.KindWeb:   lipgloss.Color("#afd75f"),
	agent.KindAsk:   lipgloss.Color("#ff87af"),
	agent.KindAgent: lipgloss.Color("#5fafff"),
	// A calendar write is the only builtin whose effect other people see, so it needs a tone of its
	// own — but the obvious warm pick, #ff8700, sits ΔE 18.3 from the edit orange, nearer than any
	// two tones here have ever been. Sand is the warm family nothing else occupies: ΔE 46.4 at its
	// closest, which is wider than the vault/agent pair that already coexists.
	agent.KindCalendar: lipgloss.Color("#d7af87"),
	// Sage's nearest tone is search, CIE76 ΔE 30.8 against the floor of 25.
	agent.KindSkill: lipgloss.Color("#87af87"),
	// Thistle's nearest tone is ask, ΔE 32.3; a green would have read as the bash box it must not look like.
	agent.KindPoll: lipgloss.Color("#d7afd7"),
}

// Traffic-light order, most cautious first, so the riskier the mode the warmer it reads.
var modeColor = map[session.Mode]color.Color{
	session.ModeReadOnly: lipgloss.Green,
	session.ModeLocal:    lipgloss.Yellow,
	session.ModeAuto:     lipgloss.Red,
}

var (
	thinkingColor = lipgloss.Magenta
	askColor      = toolColor[agent.KindAsk]
	userColor     = lipgloss.Cyan
	pendingColor  = lipgloss.Yellow
	errorColor    = lipgloss.Red
	mutedColor    = lipgloss.BrightBlack
	// The input frame while a turn runs, matching the session dot for the same state.
	workingColor = lipgloss.Cyan
	// A background command's dot, matching the box it points at.
	procColor = toolColor[agent.KindBash]
	pollColor = toolColor[agent.KindPoll]

	// The selection highlight is a background laid under text that keeps its own foreground, so
	// it has to be dark enough for syntax colours and diff tints to stay readable on top. A grey
	// rather than a tinted blue for the same reason: every ink in the transcript has to survive
	// it, and a neutral is the only thing none of them clash with.
	selectionColor = lipgloss.Color("#3a3f4b")
)

var (
	dimStyle         = lipgloss.NewStyle().Foreground(mutedColor)
	errorStyle       = lipgloss.NewStyle().Foreground(errorColor)
	userStyle        = lipgloss.NewStyle().Foreground(userColor)
	promptStyle      = lipgloss.NewStyle().Foreground(userColor)
	thisSessionStyle = lipgloss.NewStyle().Foreground(userColor).Bold(true)
	// The approval border's colour: a command only leaves the sandbox once guardian or the person let it.
	unsandboxedStyle = lipgloss.NewStyle().Foreground(pendingColor)
	unsandboxedMark  = "\U000F099E"
	// nf-md-coffee, one cell in Monaspace Neon NF: alux is holding the Mac awake.
	awakeMark = "\U000F0176"

	// The user's colour, dimmed: their own words, not said yet.
	queuedStyle = lipgloss.NewStyle().Foreground(userColor).Faint(true)

	// The input frame doubles as the status indicator: what colour it is says what the pane is
	// waiting for, which costs no rows.
	frameStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(mutedColor).
			Padding(0, 1)

	noticeStyle = lipgloss.NewStyle().Foreground(mutedColor)

	// Solid for the same reason as scrolledStyle, in the user's colour since it is their prompt.
	pinnedStyle = lipgloss.NewStyle().
			Background(selectionColor).
			Foreground(userColor).
			Padding(0, 1)

	// A solid background, since it sits on top of transcript text that would otherwise show through.
	scrolledStyle = lipgloss.NewStyle().
			Background(selectionColor).
			Foreground(lipgloss.Color("#e4e4e4")).
			Padding(0, 1)
)

func toneFor(kind agent.ToolKind) color.Color {
	// A record box is a vault box drawn as markdown, so it keeps the vault tone.
	if kind == agent.KindRecord {
		kind = agent.KindVault
	}
	if c, ok := toolColor[kind]; ok {
		return c
	}
	return nil
}
