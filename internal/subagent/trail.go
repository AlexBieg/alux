package subagent

import (
	"strings"

	"github.com/alexbieg/alux/internal/agent"
)

type EntryKind int

const (
	// EntryTool is one of the run's own tool calls, by its label.
	EntryTool EntryKind = iota
	// EntryNote is something alux did on the run's behalf.
	EntryNote
	// EntryPlan is the navigator's plan, From the navigator.
	EntryPlan
	// EntryExchange is one message From a speaker To the run and the run's reply, in Text.
	EntryExchange
	// EntrySteer is one message From a speaker To the run that needs no reply.
	EntrySteer
)

// Entry is one line of a run's trail, kept in parts so the box can say who is speaking.
type Entry struct {
	Kind     EntryKind
	Tool     agent.ToolKind
	From, To string
	Prompt   string
	Text     string
	// Failed marks an exchange whose reply never came; Refused, a note the run was denied.
	Failed  bool
	Refused bool
}

// String is the entry as plain text, continuation lines indented under their speaker.
func (e Entry) String() string {
	switch e.Kind {
	case EntryPlan:
		return e.From + " · plan:\n" + indent(e.Text)
	case EntryExchange:
		return e.From + " → " + e.To + ": " + hanging(e.Prompt) + "\n" + e.To + ": " + hanging(e.Text)
	case EntrySteer:
		return e.From + " → " + e.To + ": " + hanging(e.Text)
	}
	return e.Text
}

func hanging(text string) string {
	first, rest, ok := strings.Cut(strings.TrimSpace(text), "\n")
	if !ok {
		return first
	}
	return first + "\n" + indent(rest)
}

func indent(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "  " + line
		}
	}
	return strings.Join(lines, "\n")
}

// TrailText is the whole trail as plain text.
func (r Run) TrailText() string {
	lines := make([]string, len(r.Trail))
	for i, e := range r.Trail {
		lines[i] = e.String()
	}
	return strings.Join(lines, "\n")
}
