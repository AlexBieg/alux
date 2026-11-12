package session

import (
	"context"
	"encoding/json"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// following builds a session with a real subagent manager on a server of its own, so a subagent's
// request never takes a turn scripted for the main agent, and records every followup it logs.
func following(t *testing.T, mainTurns []mockapi.Turn, subTurns ...mockapi.Turn) (*Session, *subagent.Roster, func() []subagent.Followup) {
	t.Helper()
	s, _ := newTestSession(t, mainTurns...)
	roster := withRoster(s)
	t.Cleanup(func() { roster.Drain(5 * time.Second) })

	sub := mockapi.New(subTurns...)
	t.Cleanup(sub.Close)
	client := anthropic.NewClient(option.WithBaseURL(sub.URL), option.WithAPIKey("test"))
	m, err := subagent.NewManager(roster, &subagent.Definition{
		Name:        "research",
		Description: "test agent",
		System:      func() string { return "be a test" },
		Tools:       func() []anthropic.ToolUnionParam { return nil },
		Model:       func() string { return "mock" },
		MaxSteps:    5,
		Commands:    &guardian.Criteria{Agent: "research", Allowed: []guardian.Category{guardian.Read}, Threshold: 0.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Client = &client
	maps.Copy(s.agent.Handlers, m.Handlers())
	s.agent.Handlers["probe"] = func(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Fail bool `json:"fail"`
		}
		_ = json.Unmarshal(call.Input, &in)
		cb.OnToolResult(call.ID, "probe the thing", "probed", agent.KindBash)
		return anthropic.NewToolResultBlock(call.ID, "probed", in.Fail)
	}

	var mu sync.Mutex
	var got []subagent.Followup
	s.ID = "the-session"
	s.Followups = func(f subagent.Followup) {
		mu.Lock()
		got = append(got, f)
		mu.Unlock()
	}
	return s, roster, func() []subagent.Followup {
		mu.Lock()
		defer mu.Unlock()
		return append([]subagent.Followup(nil), got...)
	}
}

func dispatch(id string, background bool) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: subagent.DispatchTool, Input: map[string]any{
		"agent": "research", "objective": "look into it", "done": "d", "report": "r", "background": background,
	}}}}
}

func probe(id string, fail bool) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: "probe", Input: map[string]any{"fail": fail}}}}
}

func dispatchThenProbe(t *testing.T, fail bool) (string, []subagent.Followup) {
	t.Helper()
	s, roster, followed := following(t,
		[]mockapi.Turn{dispatch("t1", false), probe("t2", fail), {Text: "first done"}, {Text: "second done"}, {Text: "third done"}},
		mockapi.Turn{Text: "SUB-REPORT"},
	)
	s.Submit("go")
	settled(t, s, "first done")
	s.Submit("thanks")
	settled(t, s, "second done")
	s.Submit("and another thing")
	settled(t, s, "third done")
	return roster.Runs()[0].Dispatch, followed()
}

func TestAForegroundReportIsFollowedByTheNextToolCallAndMessage(t *testing.T) {
	run, got := dispatchThenProbe(t, false)

	want := []subagent.Followup{
		{Session: "the-session", Event: subagent.EventReport, Run: run},
		{Session: "the-session", Event: subagent.EventTool, Tool: "probe", Label: "probe the thing"},
		{Session: "the-session", Event: subagent.EventMessage, Text: "thanks"},
	}
	if len(got) != len(want) {
		t.Fatalf("logged %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Time.IsZero() {
			t.Errorf("event %d has no time", i)
		}
		got[i].Time = time.Time{}
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAFailedToolCallIsLoggedAsOne(t *testing.T) {
	_, got := dispatchThenProbe(t, true)
	if len(got) < 2 || got[1].Event != subagent.EventTool || !got[1].Error {
		t.Fatalf("logged %+v, want the probe as a failed tool call", got)
	}
	if got[0].Error {
		t.Errorf("the report was marked failed: %+v", got[0])
	}
}

func TestABackgroundReportIsLoggedWhenItIsHandedToTheAgent(t *testing.T) {
	s, roster, followed := following(t,
		[]mockapi.Turn{dispatch("t1", true), {Text: "carrying on"}, {Text: "read it"}},
		mockapi.Turn{Text: "BG-REPORT"},
	)
	s.Submit("go")
	waitUntil(t, "the report to be logged", func(Snapshot) bool { return len(followed()) > 0 }, s)
	if !roster.Drain(5 * time.Second) {
		t.Fatal("background run never finished")
	}

	got := followed()
	run := roster.Runs()[0].Dispatch
	if len(got) != 1 || got[0].Event != subagent.EventReport || got[0].Run != run || run == "" {
		t.Errorf("logged %+v, want one report for run %q", got, run)
	}
}

func TestNothingIsFollowedBeforeAnyReport(t *testing.T) {
	s, _, followed := following(t, []mockapi.Turn{probe("t1", true), {Text: "first done"}, {Text: "second done"}})
	s.Submit("go")
	settled(t, s, "first done")
	s.Submit("thanks")
	settled(t, s, "second done")
	if got := followed(); len(got) != 0 {
		t.Errorf("logged %+v with no report to follow", got)
	}
}
