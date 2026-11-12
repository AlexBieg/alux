package subagent

import (
	"context"
	"fmt"
	"strings"
)

// Layer is a separate workspace one run edits instead of the user's working copy.
type Layer interface {
	// Dir is the workspace root the run is scoped to.
	Dir() string
	// Origin is the user's working copy, which the run's brief may still name.
	Origin() string
	// Unseen lists the files with edits in the user's working copy that the run's workspace does not have.
	Unseen() []string
	// Base is BaseParent or BaseTrunk, whichever the workspace was cut from.
	Base() string
	// Close records the run's edits and removes the workspace, keeping them as a change; nil when it changed nothing.
	Close(ctx context.Context) (Work, error)
}

// Work is a finished run's change, kept apart until the main agent lands or drops it.
type Work interface {
	Change() string
	Files() []string
	// Land squashes the change into the user's working copy and says what it did; a change edited in place stays where it is.
	Land(ctx context.Context) (string, error)
	// Drop abandons the change, or restores a change edited in place to how it was before the run.
	Drop(ctx context.Context) error
}

// Layers opens a Layer per run of a Layered agent, on a new change from base or, when change is set, in place on that existing one.
type Layers interface {
	Open(ctx context.Context, label, change, base string) (Layer, error)
}

const (
	// BaseParent cuts a run's workspace from the last commit of the user's working copy.
	BaseParent = "@-"
	// BaseTrunk cuts it from the repo's trunk, so its change never lands in the user's working copy.
	BaseTrunk = "main"
)

// onTrunk says whether a layer or its work was cut from trunk.
func onTrunk(v any) bool {
	b, ok := v.(interface{ Base() string })
	return ok && b.Base() == BaseTrunk
}

// trunked is a trunk-based layer, whose Trunk names the trunk revision and how fetching it went.
type trunked interface{ Trunk() string }

func trunkOf(l Layer) string {
	if t, ok := l.(trunked); ok {
		return t.Trunk()
	}
	return "trunk"
}

// trunkKept says why a trunk-based run's change cannot be landed, and what to do with it instead.
func trunkKept(run int, change string) string {
	return fmt.Sprintf("Run %d's change %s was cut from trunk, not from your working copy, so it cannot be landed into @. "+
		"It stays as its own change on trunk: run `jj bookmark create <name> -r %s` and pr_push it, or %s drop: true to abandon it.",
		run, change, change, LandTool)
}

// editing is a layer or its work on an existing change, which the run edits in place; Land then keeps it where it is.
type editing interface{ Editing() string }

func editedChange(v any) string {
	if e, ok := v.(editing); ok {
		return e.Editing()
	}
	return ""
}

// inCheckout tells the main agent an editing run has no workspace of its own.
const inCheckout = "It works in your working copy, with no workspace of its own: its edits appear in your files as it makes them, " +
	"because this repo's workspaces settings do not set coders."

// layerNote tells the run where it is and how paths in its brief map onto it.
func layerNote(l Layer, extra string) string {
	from := "made from the last commit (@-) of the user's working copy at " + l.Origin()
	kept := "Your edits are kept as their own change when you finish, for the main agent to land."
	if c := editedChange(l); c != "" {
		from = "with jj change " + c + " checked out, from the user's working copy at " + l.Origin()
		kept = "Your edits go into that change itself; the main agent confirms or drops them when you finish."
	}
	if onTrunk(l) {
		from = "cut from " + trunkOf(l) + ", not from the user's working copy at " + l.Origin()
		kept = "Your edits are kept as their own change on trunk when you finish, for the main agent to push."
	}
	note := "You are working in a separate workspace at " + l.Dir() + ", " + from +
		". A path under " + l.Origin() + " in your task means the same file under " + l.Dir() +
		": read and edit it there. " + kept
	if unseen := l.Unseen(); len(unseen) > 0 {
		note += " The user's uncommitted edits to " + strings.Join(unseen, ", ") + " are not in your workspace; if your task needs them, say so in your report."
	}
	if strings.TrimSpace(extra) == "" {
		return note
	}
	return note + "\n\n" + extra
}

// frozen is a layer that committed the session's work first so the run starts on it.
type frozen interface{ Frozen() string }

// unseenNote warns the main agent at dispatch about its edits the run cannot see, or that its work was frozen for the run.
func unseenNote(l Layer) string {
	if f, ok := l.(frozen); ok && f.Frozen() != "" {
		return " Your work so far was committed as change " + f.Frozen() + " so the run starts on it; your working copy is a new, " +
			"undescribed change on top, so run jj describe -m before your next edit."
	}
	unseen := l.Unseen()
	if len(unseen) == 0 {
		return ""
	}
	return " It cannot see the uncommitted edits in your working copy (" + strings.Join(unseen, ", ") +
		"); if it needs them, cancel it, run jj commit, and dispatch again."
}

// closeInto closes l, when there is one, and adds what became of its edits to the run's report or error.
func (m *Manager) closeInto(ctx context.Context, h *handle, l Layer, report string, err error) (string, error) {
	if l == nil {
		return report, err
	}
	kept := m.closeLayer(context.WithoutCancel(ctx), h.run.ID, l)
	if err != nil {
		return report, fmt.Errorf("%w\n%s", err, kept)
	}
	return strings.TrimSpace(report) + "\n\n" + kept, nil
}

// closeLayer closes l, keeps its work under the run, and says what the main agent can do with it.
func (m *Manager) closeLayer(ctx context.Context, run int, l Layer) string {
	work, err := l.Close(ctx)
	if err != nil {
		return "Recording its workspace failed: " + err.Error()
	}
	if work == nil {
		return "It changed no files."
	}
	m.Roster.keep(run, work)
	if onTrunk(work) {
		return fmt.Sprintf("Its edits are kept as change %s (%s). ", work.Change(), strings.Join(work.Files(), ", ")) + trunkKept(run, work.Change())
	}
	if editedChange(work) != "" {
		return fmt.Sprintf("Its edits (%s) are already in change %s. Check the result with %s run %d, or restore the change to how it was with drop: true.",
			strings.Join(work.Files(), ", "), work.Change(), LandTool, run)
	}
	return fmt.Sprintf("Its edits are kept as change %s (%s), not yet in your working copy. Land them with %s run %d, or drop them with drop: true.",
		work.Change(), strings.Join(work.Files(), ", "), LandTool, run)
}
