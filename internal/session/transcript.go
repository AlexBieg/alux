package session

import (
	"strings"

	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/subagent"
)

// A typed prompt only collapses once it is long enough to bury the reply that follows it.
const UserCollapseLines = 12

// A box starts open until its body is longer than this, so short output is read without a click.
const BoxCollapseLines = 5

// Box is the head/body split behind every collapsible transcript item.
type Box struct {
	Head string
	Body string
	// Closed starts the box collapsed whatever its length.
	Closed bool
}

// Lines is the body's line count, the number the collapsed head shows.
func (b Box) Lines() int {
	return strings.Count(b.Body, "\n") + 1
}

// OpenByDefault reports whether the box starts expanded before the user has toggled it.
func (b Box) OpenByDefault() bool {
	return !b.Closed && b.Lines() <= BoxCollapseLines
}

// Collapsible returns the box for an item, or false when it renders as plain prose. runs is the
// snapshot's roster, which a subagent box reads instead of its own stored text, and live is its
// background processes, which a bash box reads the same way.
func Collapsible(item Item, runs []subagent.Run, live []procs.Info) (Box, bool) {
	if item.Run != 0 {
		return agentBox(item.Run, runs)
	}
	switch item.Kind {
	case KindThinking:
		// A static head beats the thought's first line: a coloured first line reads like a
		// command at a glance, and the line count already says how much thought there was.
		return Box{Head: "thinking", Body: strings.TrimSpace(item.Text), Closed: true}, true
	case KindTool:
		if p, ok := LiveProc(item, live); ok {
			return procBox(item, p), true
		}
		if item.Running {
			// A head and nothing under it: the call has no output yet, and the box has to be a box
			// from the start so it is clickable and keeps any expansion for when the output lands.
			return Box{Head: item.Text}, true
		}
		if item.Body == "" {
			return Box{}, false
		}
		return Box{Head: item.Text, Body: item.Body, Closed: item.Closed}, true
	case KindUser:
		lines := strings.Split(item.Text, "\n")
		if len(lines) <= UserCollapseLines {
			return Box{}, false
		}
		return Box{Head: lines[0], Body: strings.Join(lines[1:], "\n")}, true
	default:
		return Box{}, false
	}
}

// agentBox is a dispatch's head, drawn from the run itself so it tracks the run as it works.
func agentBox(id int, runs []subagent.Run) (Box, bool) {
	for _, run := range runs {
		if run.ID == id {
			return Box{Head: run.Headline(), Closed: true}, true
		}
	}
	return Box{Head: "agent (finished)", Closed: true}, true
}

// LiveProc is the background process a tool box belongs to, if one is still running under the
// call that put the box up. It is what makes such a box worth redrawing while nothing else moves.
func LiveProc(item Item, live []procs.Info) (procs.Info, bool) {
	if item.Call == "" {
		return procs.Info{}, false
	}
	for _, p := range live {
		if p.Call == item.Call {
			return p, true
		}
	}
	return procs.Info{}, false
}

// procBox draws a running command from the process itself, the way agentBox draws a dispatch from
// the run. Once it exits, its box draws the final output the session stored on the item.
func procBox(item Item, p procs.Info) Box {
	body := strings.TrimRight(p.Tail, "\n")
	if body == "" {
		body = item.Body
	}
	return Box{Head: item.Text, Body: body}
}
