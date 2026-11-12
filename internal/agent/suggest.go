package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

// pendingSuggestion is a blocked command held until the model takes the suggested tool, declines it, or the turn ends.
type pendingSuggestion struct {
	id, command, label, body, reason string
	call                             Call
	verdict                          guardian.Verdict
	suggestion                       guardian.Suggestion
	// local is a suggestion alux made by rule rather than guardian, so guardian's log is not told of it.
	local bool
	// targets, when set, are the paths a call must name to count as taking the suggestion.
	targets []string
}

func (a *Agent) recordSuggested(p pendingSuggestion, decision, outcome string) {
	if !p.local && a.Screen != nil {
		a.Screen.RecordSuggested(p.command, p.verdict, p.suggestion, decision, outcome)
	}
}

// suggestions is one turn's pending suggestions; tools may run concurrently, so every access locks.
type suggestions struct {
	mu      sync.Mutex
	next    int
	pending []pendingSuggestion
}

func (s *suggestions) add(p pendingSuggestion) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	p.id = "s" + strconv.Itoa(s.next)
	p.call.Input = slices.Clone(p.call.Input)
	s.pending = append(s.pending, p)
	return p.id
}

func (s *suggestions) take(id string) (pendingSuggestion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.pending {
		if p.id == id {
			s.pending = slices.Delete(s.pending, i, i+1)
			return p, true
		}
	}
	return pendingSuggestion{}, false
}

// accept takes the most recent suggestion naming tool, and naming target too when it lists targets.
func (s *suggestions) accept(tool, target string) (pendingSuggestion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.pending) - 1; i >= 0; i-- {
		p := s.pending[i]
		if slices.Contains(p.suggestion.Tools, tool) && (len(p.targets) == 0 || slices.Contains(p.targets, target)) {
			s.pending = slices.Delete(s.pending, i, i+1)
			return p, true
		}
	}
	return pendingSuggestion{}, false
}

func (s *suggestions) drain() []pendingSuggestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	s.pending = nil
	return pending
}

// lapse logs every suggestion the turn left unanswered as ignored.
func (a *Agent) lapse(s *suggestions) {
	for _, p := range s.drain() {
		a.recordSuggested(p, "suggested", "ignored")
	}
}

// suggestionMenu is the file tools this agent has that guardian may suggest in place of a command.
func (a *Agent) suggestionMenu() []string {
	if a.Tools == nil {
		return nil
	}
	var menu []string
	for _, t := range a.Tools() {
		switch name := t.GetName(); {
		case name == nil:
		case *name == editorTool:
			menu = append(menu, "view")
		case *name == "grep", *name == "glob":
			menu = append(menu, *name)
		}
	}
	return menu
}

// suggestedTool is the menu key a call answers to and the path it names, or "" when it answers to none.
func suggestedTool(ctx context.Context, call Call) (tool, target string) {
	switch call.Name {
	case "grep", "glob":
		var in tools.SearchInput
		if json.Unmarshal(call.Input, &in) == nil {
			target = in.Path
		}
		if target == "" {
			target = "."
		}
		return call.Name, filepath.Clean(inDir(ctx, target))
	case pollTool, backgroundTool:
		return call.Name, ""
	case editorTool:
		var in tools.EditInput
		if json.Unmarshal(call.Input, &in) == nil && in.Command == "view" {
			return "view", filepath.Clean(inDir(ctx, in.Path))
		}
	}
	return "", ""
}

// suggest holds a flagged command back when guardian thinks one of the agent's file tools can do its work.
func (a *Agent) suggest(ctx context.Context, call Call, cb Callbacks, in Intent, command, reason string, v guardian.Verdict) (*anthropic.ContentBlockParamUnion, guardian.Suggestion) {
	menu := a.suggestionMenu()
	if cb.suggestions == nil || len(menu) == 0 {
		return nil, guardian.Suggestion{}
	}
	sctx, cancel := context.WithTimeout(ctx, screenWait)
	s := a.Screen.Suggest(sctx, command, menu)
	cancel()
	if len(s.Tools) == 0 {
		return nil, s
	}
	id := cb.suggestions.add(pendingSuggestion{
		command: command, label: in.Label, body: in.Body, reason: reason, call: call, verdict: v, suggestion: s,
	})
	var callable []string
	for _, t := range s.Tools {
		if t == "view" {
			t = editorTool + ` with "command":"view"`
		}
		callable = append(callable, t)
	}
	named := strings.Join(callable, " or ")
	return block(anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("Not run: %s. guardian thinks %s can do this. "+
		"You have three options: call %s yourself; call decline_suggestion with id %q and a reason, which puts this exact "+
		"command to the user to approve (or refuses it when nobody can be asked); or do neither, if the command is not "+
		"needed or no tool fits, and carry on another way.", reason, named, named, id), true)), s
}

// decline puts a held command to the person exactly as it was first written; the model's reason only explains it.
func (a *Agent) decline(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(call.Input, &in) != nil || cb.suggestions == nil {
		return lapsed(call.ID)
	}
	p, found := cb.suggestions.take(in.ID)
	if !found {
		return lapsed(call.ID)
	}
	if cb.Require == nil {
		a.recordSuggested(p, "refused", "declined→refused")
		return refusedResult(call.ID, p.reason)
	}
	ok := cb.Require(ctx, p.label, p.body, p.reason+". The agent declined the suggested tool: "+in.Reason)
	switch {
	case ctx.Err() != nil:
		a.recordSuggested(p, "interrupted", "declined→interrupted")
		return interruptedResult(call.ID)
	case !ok:
		a.recordSuggested(p, "declined", "declined→declined")
		return notRun(call.ID, p.reason)
	}
	a.recordSuggested(p, "approved", "declined→approved")
	stored := Call{ID: call.ID, Name: p.call.Name, Input: p.call.Input}
	handler := a.Handlers[stored.Name]
	if handler == nil {
		handler = a.Fallback
	}
	if handler == nil {
		return failed(call.ID, fmt.Errorf("no handler for tool %s", stored.Name))
	}
	sctx, err := a.shellContext(ctx, stored)
	if err != nil {
		return refusedResult(call.ID, err.Error())
	}
	cb.Approve = approveAll
	a.markUnsandboxed(sctx, stored, cb)
	res := handler(sctx, stored, cb)
	cb.toolEnd(stored.ID, stored.Name, res)
	return res
}

func lapsed(id string) anthropic.ContentBlockParamUnion {
	return anthropic.NewToolResultBlock(id,
		"No such suggestion. Suggestions lapse when the turn ends, and each id can be declined once.", true)
}
