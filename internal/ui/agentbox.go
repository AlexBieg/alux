package ui

import (
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

var (
	driverStyle    = lipgloss.NewStyle().Foreground(toolColor[agent.KindAgent])
	navigatorStyle = lipgloss.NewStyle().Foreground(askColor)
	plainStyle     = lipgloss.NewStyle()
)

func renderEntry(e subagent.Entry, width int) []string {
	switch e.Kind {
	case subagent.EntryTool:
		marker, label := dimStyle, plainStyle
		if tone := toneFor(e.Tool); tone != nil {
			marker = lipgloss.NewStyle().Foreground(tone)
			label = marker
		}
		return hang(marker.Render("• "), 2, e.Text, label, width)
	case subagent.EntryNote:
		marker, text := lipgloss.NewStyle().Foreground(pendingColor), plainStyle
		if e.Refused {
			marker, text = errorStyle, errorStyle
		}
		return hang(marker.Render("• "), 2, e.Text, text, width)
	case subagent.EntryPlan:
		head := speaker(e.From).Render(e.From) + dimStyle.Render(" · plan:")
		lines := []string{ansi.Truncate(head, width, "")}
		return append(lines, hang("  ", 2, e.Text, plainStyle, width)...)
	case subagent.EntryExchange:
		reply := plainStyle
		if e.Failed {
			reply = errorStyle
		}
		asked, askedW := addressed(e)
		answer := speaker(e.To).Render(e.To) + ": "
		return append(hang(asked, askedW, e.Prompt, plainStyle, width),
			hang(answer, ansi.StringWidth(e.To)+2, e.Text, reply, width)...)
	case subagent.EntrySteer:
		said, saidW := addressed(e)
		return hang(said, saidW, e.Text, plainStyle, width)
	}
	return styledWrap(e.Text, plainStyle, width)
}

// addressed is "from → to: ", each speaker in their own colour.
func addressed(e subagent.Entry) (string, int) {
	text := speaker(e.From).Render(e.From) + dimStyle.Render(" → ") + speaker(e.To).Render(e.To) + ": "
	return text, ansi.StringWidth(e.From + " → " + e.To + ": ")
}

func speaker(name string) lipgloss.Style {
	switch name {
	case "driver":
		return driverStyle
	case "alux":
		return dimStyle
	}
	return navigatorStyle
}
