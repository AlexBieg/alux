package ui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/session"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/charmbracelet/x/ansi"
)

// suggestView flags every command and suggests the editor's view in its place.
type suggestView struct{}

func (suggestView) Screen(context.Context, string) guardian.Verdict {
	return guardian.Verdict{Agent: "test", Top: guardian.Network, Danger: 0.95, Threshold: 0.3, Flagged: true}
}
func (suggestView) Record(string, guardian.Verdict, string) {}
func (suggestView) Suggest(context.Context, string, []string) guardian.Suggestion {
	return guardian.Suggestion{Tools: []string{"view"}}
}
func (suggestView) RecordSuggested(string, guardian.Verdict, guardian.Suggestion, string, string) {}

// declined runs a turn whose bash call is held for a suggestion and then declined, answering the approval with approve.
func declined(t *testing.T, width int, approve bool) (approval, after string) {
	t.Helper()
	reason := "view cannot run the command, and its output is what I need to see ENDREASON"
	server := mockapi.New(
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo original-ran"}}}},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t2", Name: agent.DeclineSuggestion, Input: map[string]any{"id": "s1", "reason": reason}}}},
		mockapi.Turn{Text: "turn over"},
	)
	t.Cleanup(server.Close)
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	s := session.New(&agent.Agent{
		Client: &client, Model: "mock", System: "test", Standing: agent.NewRuleInjector(""),
		Tools: agent.BuiltinTools, Handlers: agent.BuiltinHandlers(), Fallback: agent.RunBash, Screen: suggestView{},
	}, nil)
	m := New(s)
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	s.Start()
	s.Submit("go")

	waitForUI(t, "the approval", func() bool { return s.Snapshot().Pending != "" })
	settle(m)
	approval = screen(m)
	s.Approve(approve)
	waitForUI(t, "the turn to end", func() bool {
		snap := s.Snapshot()
		return !snap.Busy && len(snap.Items) > 0 && snap.Items[len(snap.Items)-1].Text == "turn over"
	})
	settle(m)
	return approval, screen(m)
}

// unbox is the screen's words in order, without the frame's bars between wrapped lines.
func unbox(out string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " ")
}

func assertFits(t *testing.T, out string, width int) {
	t.Helper()
	for i, line := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("row %d is %d columns, past %d: %q", i, w, width, line)
		}
	}
}

func TestADeclinedSuggestionTheyApproveShowsAsTheCommandItRan(t *testing.T) {
	const width = 50
	approval, out := declined(t, width, true)
	flat := unbox(approval)
	for _, want := range []string{"$ echo original-ran", "needs your say-so: guardian thinks it uses the network · high risk",
		"The agent declined the suggested tool: view cannot run", "ENDREASON"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the approval is missing %q:\n%s", want, approval)
		}
	}
	assertFits(t, approval, width)

	if !strings.Contains(out, "$ echo original-ran") || !strings.Contains(out, "[exit 0]") ||
		strings.Count(out, "original-ran") < 2 {
		t.Errorf("the command's box does not show the command and its output:\n%s", out)
	}
	if strings.Contains(out, "decline suggestion") || strings.Contains(out, `"id"`) {
		t.Errorf("a decline that ran its command drew a box of its own:\n%s", out)
	}
	assertFits(t, out, width)
}

func TestADeclinedSuggestionTheyRefuseShowsTheReasonAndTheAnswer(t *testing.T) {
	const width = 50
	_, out := declined(t, width, false)
	flat := unbox(out)
	for _, want := range []string{"decline suggestion s1", "view cannot run the command", "ENDREASON",
		"Not run: guardian thinks it uses the network", "did not allow it"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the decline's box is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"id"`) || strings.Contains(out, "$ echo original-ran") || strings.Contains(out, "[exit") {
		t.Errorf("a refused decline showed raw input or a command that never ran:\n%s", out)
	}
	assertFits(t, out, width)
}
