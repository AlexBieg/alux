package skillrun

import (
	"fmt"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/skills"
)

// progress is a line for each done node with a pending predecessor not yet told of, marking those pairs sent.
func (h *Host) progress() string {
	h.mu.Lock()
	r := h.open
	var log []skills.Event
	if r != nil && r.run != nil && r.closing == "" {
		log = slices.Clone(r.log)
	}
	h.mu.Unlock()
	if log == nil {
		return ""
	}
	behind := skills.NodesBehind(r.run, log, r.env)
	h.mu.Lock()
	defer h.mu.Unlock()
	var lines []string
	for _, b := range behind {
		var pending []string
		for _, p := range b.Pending {
			pair := [2]string{b.Node, p}
			if r.noticed[pair] {
				continue
			}
			if r.noticed == nil {
				r.noticed = map[[2]string]bool{}
			}
			r.noticed[pair] = true
			pending = append(pending, p)
		}
		if len(pending) > 0 {
			lines = append(lines, behindLine(r.run.Skill.Name, b.Node, pending))
		}
	}
	return strings.Join(lines, "\n")
}

// behindLine tells the model to do the pending nodes before a node it counted as done.
func behindLine(skill, node string, pending []string) string {
	them := plural(pending, "it", "them")
	return fmt.Sprintf("Skill %s: `%s` counts as done, but %s %s before it and %s still pending. Do %s now, then redo `%s` if it depended on %s.",
		skill, node, ticked(pending), plural(pending, "comes", "come"), plural(pending, "is", "are"), them, node, them)
}

func plural(names []string, one, many string) string {
	if len(names) == 1 {
		return one
	}
	return many
}

// ticked is names in backticks, joined with commas and "and" before the last.
func ticked(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "`" + n + "`"
	}
	return listed(q)
}

// listed joins names with commas and "and" before the last.
func listed(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
