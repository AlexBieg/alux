package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/prs"
)

// PRsMsg carries the latest PR poll, sent whenever any repo's state changes.
type PRsMsg struct{ States []prs.RepoState }

var readyColor = lipgloss.Green

// renderPRs is the top row's PR summary, most urgent first; empty until the first poll lands.
func (m *Model) renderPRs() string {
	if len(m.prStates) == 0 {
		return ""
	}
	var flags, review int
	mine := map[prs.Status]int{}
	stale := false
	for _, s := range m.prStates {
		flags += len(s.Queue.Flags)
		review += len(s.Queue.Review)
		for _, it := range s.Queue.Mine {
			mine[it.Status]++
		}
		stale = stale || s.Err != nil
	}

	var parts []string
	if flags > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(pendingColor).Render(fmt.Sprintf("⚑%d", flags)))
	}
	parts = append(parts, dimStyle.Render("review ")+countStyle(review).Render(fmt.Sprint(review)))
	parts = append(parts, dimStyle.Render("mine ")+mineSummary(mine))
	out := dimStyle.Render("PRs ") + strings.Join(parts, dimStyle.Render(" · "))
	if stale {
		out += errorStyle.Render(" stale")
	}
	return out
}

func countStyle(n int) lipgloss.Style {
	if n == 0 {
		return dimStyle
	}
	return lipgloss.NewStyle()
}

// mineSummary is the total, then a count per status that sums to it.
func mineSummary(mine map[prs.Status]int) string {
	total := 0
	for _, n := range mine {
		total += n
	}
	parts := []string{countStyle(total).Render(fmt.Sprint(total))}
	for _, s := range []prs.Status{prs.StatusReady, prs.StatusConflict, prs.StatusChangesRequested, prs.StatusFailing,
		prs.StatusWaitingCI, prs.StatusWaiting, prs.StatusDraft} {
		if n := mine[s]; n > 0 {
			glyph, style := statusIcon(s)
			parts = append(parts, style.Render(fmt.Sprintf("%s%d", glyph, n)))
		}
	}
	return strings.Join(parts, " ")
}

// Icons are single-cell glyphs: the picker's columns and the top row's hit test count cells.
const (
	iconReady    = "✓"
	iconConflict = "≠"
	iconChanges  = "✗"
	iconFailing  = "●"
	iconRunning  = "↻"
	iconWaiting  = "◷"
	iconDraft    = "○"
)

const iconLegend = "✓ ready · ≠ conflict · ✗ changes asked · ● CI failing · ↻ CI running · ◷ awaiting review · ○ draft · R reviewed (dim: PR moved since) · ┬├└ stack (dim rows: not in your lists) · time idle"

func statusIcon(s prs.Status) (string, lipgloss.Style) {
	switch s {
	case prs.StatusReady:
		return iconReady, lipgloss.NewStyle().Foreground(readyColor)
	case prs.StatusConflict:
		return iconConflict, errorStyle
	case prs.StatusChangesRequested:
		return iconChanges, errorStyle
	case prs.StatusFailing:
		return iconFailing, errorStyle
	case prs.StatusWaitingCI:
		return iconRunning, lipgloss.NewStyle().Foreground(workingColor)
	case prs.StatusDraft:
		return iconDraft, dimStyle
	}
	return iconWaiting, dimStyle
}

// checksIcon is a PR's CI state, for the review list where its approval status is not the question.
func checksIcon(p prs.PR) (string, lipgloss.Style) {
	switch {
	case p.Red():
		return iconFailing, errorStyle
	case p.Checks == "SUCCESS":
		return iconReady, lipgloss.NewStyle().Foreground(readyColor)
	case p.Checks == "PENDING" || p.Checks == "EXPECTED":
		return iconRunning, lipgloss.NewStyle().Foreground(workingColor)
	}
	return " ", dimStyle
}
