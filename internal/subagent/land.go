package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

const LandTool = "land_run"

func (r *Roster) keep(run int, w Work) {
	r.mu.Lock()
	if r.kept == nil {
		r.kept = map[int]Work{}
	}
	r.kept[run] = w
	r.setKept(run, w.Files())
	r.mu.Unlock()
	r.changed()
}

// claim takes run's kept work out of the roster, so two calls can never land or drop it twice.
func (r *Roster) claim(run int) (Work, []int) {
	r.mu.Lock()
	if w, ok := r.kept[run]; ok {
		delete(r.kept, run)
		r.setKept(run, nil)
		r.mu.Unlock()
		r.changed()
		return w, nil
	}
	defer r.mu.Unlock()
	ids := make([]int, 0, len(r.kept))
	for id := range r.kept {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return nil, ids
}

// Shipped stops tracking the trunk-based run whose kept change is change, a full jj change id, now it has been pushed.
func (r *Roster) Shipped(change string) (int, bool) {
	r.mu.Lock()
	for id, w := range r.kept {
		if onTrunk(w) && w.Change() != "" && strings.HasPrefix(change, w.Change()) {
			delete(r.kept, id)
			r.setKept(id, nil)
			r.mu.Unlock()
			r.changed()
			return id, true
		}
	}
	r.mu.Unlock()
	return 0, false
}

func (r *Roster) keptWork(run int) Work {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.kept[run]
}

func (r *Roster) setKept(id int, files []string) {
	for _, run := range r.runs {
		if run.ID == id {
			run.Kept = files
		}
	}
}

func (m *Manager) landTool() anthropic.ToolUnionParam {
	return strict(LandTool,
		"Bring a finished coder run's change into the user's working copy, or drop it. A coder runs in its own workspace "+
			"and its edits wait as a separate change until you call this; the harness does the jj steps itself. Land at a "+
			"point where files changing under you is safe: not halfway through your own edit of the same files. A landing "+
			"that overlaps the user's edits is recorded as a jj conflict in @, and the result says so. A run dispatched onto "+
			"an existing change edited it in place: landing it moves nothing and reports the files and any conflict in that "+
			"change or its descendants, and drop restores the change to how it was before the run.",
		map[string]any{
			"run":  map[string]any{"type": "integer", "description": "The run whose change to land, as its report names it."},
			"drop": map[string]any{"type": "boolean", "description": "Abandon the change instead of landing it."},
		}, []string{"run"})
}

func (m *Manager) runLand(ctx context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Run  *int `json:"run"`
		Drop bool `json:"drop"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if in.Run == nil {
		return anthropic.NewToolResultBlock(call.ID, "run is required", true)
	}
	if w := m.Roster.keptWork(*in.Run); !in.Drop && onTrunk(w) {
		return anthropic.NewToolResultBlock(call.ID, trunkKept(*in.Run, w.Change()), true)
	}
	work, kept := m.Roster.claim(*in.Run)
	if work == nil {
		text := fmt.Sprintf("run %d has no change waiting", *in.Run)
		if len(kept) > 0 {
			text += fmt.Sprintf("; runs with one: %v", kept)
		}
		return anthropic.NewToolResultBlock(call.ID, text, true)
	}
	if in.Drop {
		if err := work.Drop(ctx); err != nil {
			m.Roster.keep(*in.Run, work)
			return anthropic.NewToolResultBlock(call.ID, "dropping it failed, so it is still kept: "+err.Error(), true)
		}
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("Dropped change %s (%s).", work.Change(), strings.Join(work.Files(), ", ")), false)
	}
	text, err := work.Land(ctx)
	if err != nil {
		m.Roster.keep(*in.Run, work)
		return anthropic.NewToolResultBlock(call.ID, "landing it failed, so it is still kept: "+err.Error(), true)
	}
	return anthropic.NewToolResultBlock(call.ID, text, false)
}
