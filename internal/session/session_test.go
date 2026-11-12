package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/compact"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// These tests build the real built-in tool set, which asks vault.Root() whether a vault is
// configured — and that reads ~/.config/alux/settings.json and caches the answer in a
// sync.Once for the whole process. Left alone, the suite would behave one way on a machine with
// a vault and another on a machine without one, decided by whichever test ran first. HOME is
// redirected before any test runs so the answer is always "none", deterministically.
//
// t.Setenv cannot do this: the first Root() may happen inside the first test, and by then the
// once has already fired with the real HOME.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "alux-session-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("ALUX_RECORDS", "off")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func newTestSession(t *testing.T, turns ...mockapi.Turn) (*Session, *mockapi.Server) {
	t.Helper()
	server := mockapi.New(turns...)
	t.Cleanup(server.Close)
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	a := &agent.Agent{
		Client:   &client,
		Model:    "mock",
		System:   "test",
		Standing: agent.NewRuleInjector(""),
		Tools:    agent.BuiltinTools,
		Handlers: agent.BuiltinHandlers(),
		Fallback: agent.RunBash,
		Screen:   passAll{},
	}
	s := New(a, nil)
	s.Start()
	return s, server
}

func waitUntil(t *testing.T, what string, cond func(Snapshot) bool, s *Session) Snapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if snap := s.Snapshot(); cond(snap) {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; snapshot: %+v", what, s.Snapshot())
	return Snapshot{}
}

func idle(snap Snapshot) bool { return !snap.Busy }

// settled waits for a turn to finish having produced something. Waiting on idle alone is wrong
// twice over: Submit only queues, so Busy is still false for a moment afterwards, and against
// the local mock a whole turn can come and go before a poll ever observes Busy as true. Keying
// on transcript content avoids both, because the content cannot appear before the turn ran.
func settled(t *testing.T, s *Session, want string) Snapshot {
	t.Helper()
	return waitUntil(t, want, func(sn Snapshot) bool {
		return !sn.Busy && strings.Contains(transcript(sn), want)
	}, s)
}

func transcript(snap Snapshot) string {
	var b strings.Builder
	for _, item := range snap.Items {
		// Bodies included: a tool item keeps its output there, and leaving it out made an
		// assertion about a disowned turn's output silently unable to see it.
		b.WriteString(string(item.Kind) + ": " + item.Text + "\n")
		if item.Body != "" {
			b.WriteString("  | " + item.Body + "\n")
		}
	}
	return b.String()
}

func TestTheBaselineStandsInUntilARealRequestIsMeasured(t *testing.T) {
	s, _ := newTestSession(t, mockapi.Turn{Text: "first reply", Usage: mockapi.Usage{InputTokens: 50_000}})
	ctx := context.Background()

	s.MeasureBaseline(ctx)
	base := s.Snapshot().Usage
	if !base.Known() || !base.Counted {
		t.Fatalf("usage after the baseline = %+v, want a counted figure", base)
	}

	s.Submit("go")
	settled(t, s, "first reply")
	if u := s.Snapshot().Usage; u.Counted || u.Prompt != 50_000 {
		t.Fatalf("usage after a reply = %+v, want the reply's own 50,000", u)
	}

	s.MeasureBaseline(ctx)
	if u := s.Snapshot().Usage; u.Counted {
		t.Fatalf("a later count overwrote the reply's figures: %+v", u)
	}

	s.NewSession()
	if u := s.Snapshot().Usage; u != base {
		t.Fatalf("usage after /new = %+v, want the baseline %+v back", u, base)
	}
}

// Cancelling mid-tool has to leave the pane usable and the conversation sendable: the whole point
// of the first press is that the transcript and history keep what already happened.
func TestCancelMidToolLeavesTheSessionUsable(t *testing.T) {
	s, server := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 30"}}}},
		mockapi.Turn{Text: "second turn ran"},
	)

	s.Submit("go")
	waitUntil(t, "the turn to start", func(sn Snapshot) bool { return sn.Busy }, s)
	time.Sleep(200 * time.Millisecond)
	s.Cancel()

	snap := waitUntil(t, "the turn to unwind", idle, s)
	if !strings.Contains(transcript(snap), "⊘ cancelled") {
		t.Errorf("no cancellation was recorded:\n%s", transcript(snap))
	}

	s.Submit("again")
	snap = settled(t, s, "second turn ran")
	if strings.Contains(transcript(snap), "stopped:") {
		t.Errorf("the follow-up turn failed:\n%s", transcript(snap))
	}

	// That the follow-up succeeded proves nothing on its own: the local mock accepts any
	// request shape. The real server rejects a conversation containing a tool_use with no
	// matching tool_result, so the repair has to be checked on the wire.
	assertHistorySendable(t, server)
}

// assertHistorySendable walks the messages of the most recent request the way the API does, and
// fails if any tool_use went unanswered.
func assertHistorySendable(t *testing.T, server *mockapi.Server) {
	t.Helper()
	raw, err := json.Marshal(server.LastRequest()["messages"])
	if err != nil {
		t.Fatal(err)
	}
	var messages []struct {
		Content []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatalf("could not read the request messages: %v", err)
	}

	var opened []string
	answered := map[string]bool{}
	for _, m := range messages {
		for _, block := range m.Content {
			switch block.Type {
			case "tool_use":
				opened = append(opened, block.ID)
			case "tool_result":
				answered[block.ToolUseID] = true
			}
		}
	}
	if len(opened) == 0 {
		t.Fatal("no tool_use in the sent history, so this assertion is vacuous")
	}
	for _, id := range opened {
		if !answered[id] {
			t.Errorf("tool_use %q was sent with no tool_result; the real API would reject this", id)
		}
	}
}

// The second press exists because nothing guarantees a hung tool honours cancellation. This
// installs one that ignores its context, which is exactly the case the feature is for.
func TestSecondCancelDisownsAHungTurn(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "hang", Input: map[string]any{}}}},
		mockapi.Turn{Text: "follow-up ran"},
	)

	release := make(chan struct{})
	s.agent.Handlers["hang"] = func(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		<-release // deliberately ignores cancellation
		cb.OnToolResult(call.ID, "hung tool", "output from the zombie", agent.KindBash)
		return anthropic.NewToolResultBlock(call.ID, "late", false)
	}

	s.Submit("go")
	waitUntil(t, "the hung tool to start", func(sn Snapshot) bool { return sn.Busy }, s)

	s.Cancel() // ignored by the tool
	waitUntil(t, "the cancelling state", func(sn Snapshot) bool { return sn.Cancelling }, s)
	s.Cancel() // give up on it

	snap := waitUntil(t, "the pane to come back", idle, s)
	if !strings.Contains(transcript(snap), "abandoned") {
		t.Errorf("the turn was not reported as abandoned:\n%s", transcript(snap))
	}

	// The pane must accept work immediately, while the zombie is still running.
	s.Submit("again")
	waitUntil(t, "the follow-up turn", func(sn Snapshot) bool {
		return !sn.Busy && strings.Contains(transcript(sn), "follow-up ran")
	}, s)

	// Now let the zombie finish. Nothing it says may reach this transcript.
	close(release)
	time.Sleep(300 * time.Millisecond)
	if out := transcript(s.Snapshot()); strings.Contains(out, "zombie") {
		t.Errorf("output from the disowned turn reached the transcript:\n%s", out)
	}
}

// A command that takes a while must not leave the transcript empty until it is over, which reads
// as a pane that has stopped. The box goes up when the call starts and the output fills in the
// same box, rather than a second one appearing beside it.
func TestASlowCommandIsInTheTranscriptWhileItRuns(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 0.6; echo awake"}}}},
		mockapi.Turn{Text: "all done"},
	)
	s.Submit("go")

	snap := waitUntil(t, "the running command's box", func(sn Snapshot) bool {
		return running(sn) != nil
	}, s)
	started := *running(snap)
	if want := "$ sleep 0.6; echo awake"; started.Text != want {
		t.Errorf("the box went up without naming the command: got %q, want %q", started.Text, want)
	}
	if started.Body != "" {
		t.Errorf("a box for a call that has not answered carries output: %q", started.Body)
	}

	settled(t, s, "all done")
	tools := toolItems(s.Snapshot())
	if len(tools) != 1 {
		t.Fatalf("want the one box the start put up, got %d:\n%s", len(tools), transcript(s.Snapshot()))
	}
	if tools[0].ID != started.ID {
		t.Errorf("the output landed in a new box (id %d) instead of the one on screen (id %d)", tools[0].ID, started.ID)
	}
	if tools[0].Running || !strings.Contains(tools[0].Body, "awake") {
		t.Errorf("the box was not filled in by the result: %+v", tools[0])
	}
}

// A call that announces itself and then answers with nothing to show must leave the transcript as
// it found it, the way a declined command always has — otherwise its box spins forever.
func TestABoxGoesAwayWhenItsCallReportsNothing(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "quiet", Input: map[string]any{}}}},
		mockapi.Turn{Text: "turn over"},
	)
	s.agent.Handlers["quiet"] = func(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		cb.OnToolStart(call.ID, "$ quiet", agent.KindBash)
		return anthropic.NewToolResultBlock(call.ID, "nothing to report", false)
	}

	s.Submit("go")
	settled(t, s, "turn over")
	if tools := toolItems(s.Snapshot()); len(tools) != 0 {
		t.Errorf("a call that reported nothing left a box behind: %+v", tools)
	}
}

func running(snap Snapshot) *Item {
	for i, item := range snap.Items {
		if item.Running {
			return &snap.Items[i]
		}
	}
	return nil
}

func toolItems(snap Snapshot) []Item {
	var tools []Item
	for _, item := range snap.Items {
		if item.Kind == KindTool {
			tools = append(tools, item)
		}
	}
	return tools
}

// One queue for everything, so a prompt sent while a turn is running waits rather than
// interleaving with it.
func TestPromptsAreServedOneAtATime(t *testing.T) {
	s, server := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 0.4"}}}},
		mockapi.Turn{Text: "first done"},
		mockapi.Turn{Text: "second done"},
	)

	s.Submit("one")
	waitUntil(t, "the first turn to start", func(sn Snapshot) bool { return sn.Busy }, s)
	s.Submit("two")

	snap := waitUntil(t, "both turns", func(sn Snapshot) bool {
		return !sn.Busy && strings.Contains(transcript(sn), "second done")
	}, s)

	// Order is the assertion, and it has to include where the second prompt lands: if the
	// queue drained concurrently the second user item would appear before the first turn had
	// said anything, while its replies could still happen to arrive in order.
	out := transcript(snap)
	var at []int
	for _, marker := range []string{"user: one", "first done", "user: two", "second done"} {
		i := strings.Index(out, marker)
		if i < 0 {
			t.Fatalf("%q missing from the transcript:\n%s", marker, out)
		}
		at = append(at, i)
	}
	for i := 1; i < len(at); i++ {
		if at[i] < at[i-1] {
			t.Errorf("turns interleaved; the transcript is out of order:\n%s", out)
			break
		}
	}
	if server.RequestCount() != 3 {
		t.Errorf("model was called %d times, want 3", server.RequestCount())
	}
}

// steerable is newTestSession with the agent's Pending source wired up, as main does it. Without
// it a steer has no route into a running turn.
func steerable(t *testing.T, turns ...mockapi.Turn) (*Session, *mockapi.Server) {
	t.Helper()
	s, server := newTestSession(t, turns...)
	s.agent.Pending = s.PendingBlock
	return s, server
}

// waitForQueue waits until n prompts are lined up and returns the last one's id.
func waitForQueue(t *testing.T, s *Session, n int) int {
	t.Helper()
	snap := waitUntil(t, fmt.Sprintf("%d queued prompts", n), func(sn Snapshot) bool {
		return len(sn.Queued) == n
	}, s)
	return snap.Queued[n-1].ID
}

// A prompt sent while the agent works reaches that same turn. The tool call is what makes it
// observable: the steer has to ride the request carrying the tool's result.
func TestSteeringReachesTheRunningTurn(t *testing.T) {
	s, server := steerable(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 0.6"}}}},
		mockapi.Turn{Text: "folded it in"},
	)

	s.Submit("do the first thing")
	waitUntil(t, "the turn to start", func(sn Snapshot) bool { return sn.Busy && sn.Steerable }, s)
	s.Submit("also check the config")
	s.Steer(waitForQueue(t, s, 1))

	settled(t, s, "folded it in")

	if server.RequestCount() != 2 {
		t.Errorf("model was called %d times, want 2 — a steer must not cost a turn", server.RequestCount())
	}
	sent, err := json.Marshal(server.LastRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), "also check the config") {
		t.Errorf("the steer never reached the model:\n%s", sent)
	}
	if !strings.Contains(string(sent), "takes precedence") {
		t.Errorf("the steer arrived without the preamble that says who is speaking:\n%s", sent)
	}

	snap := s.Snapshot()
	if len(snap.Queued) != 0 || len(snap.Steering) != 0 {
		t.Errorf("queue not empty after delivery: queued=%v steering=%v", snap.Queued, snap.Steering)
	}
	if out := transcript(snap); !strings.Contains(out, "user: also check the config") {
		t.Errorf("the steer is nowhere in the transcript:\n%s", out)
	}
}

// Steering a turn ended without being told must not evaporate. Cancelling produces one on
// purpose: the agent skips its drain once the context is gone.
func TestSteeringTheTurnNeverHeardIsNotLost(t *testing.T) {
	s, _ := steerable(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 30"}}}},
		mockapi.Turn{Text: "ran it on its own"},
	)

	s.Submit("the long thing")
	waitUntil(t, "the turn to start", func(sn Snapshot) bool { return sn.Busy && sn.Steerable }, s)
	s.Submit("steer me")
	s.Steer(waitForQueue(t, s, 1))
	waitUntil(t, "the steer to be taken off the queue", func(sn Snapshot) bool {
		return len(sn.Steering) == 1
	}, s)

	s.Cancel()

	// Cancel preserves it; a pane that would rather drop it calls DropQueued first, as esc does.
	snap := settled(t, s, "ran it on its own")
	if out := transcript(snap); !strings.Contains(out, "user: steer me") {
		t.Errorf("a steer the turn never heard was lost:\n%s", out)
	}
	if len(snap.Steering) != 0 {
		t.Errorf("steering still held after the turn ended: %v", snap.Steering)
	}
}

// What esc does: the turn stops and what was lined up behind it goes too.
func TestDroppingTheQueueStopsItRunning(t *testing.T) {
	s, server := steerable(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 30"}}}},
		mockapi.Turn{Text: "should never run"},
	)

	s.Submit("the long thing")
	waitUntil(t, "the turn to start", func(sn Snapshot) bool { return sn.Busy }, s)
	s.Submit("queued behind it")
	waitForQueue(t, s, 1)

	if dropped := s.DropQueued(); dropped != 1 {
		t.Errorf("DropQueued reported %d, want 1", dropped)
	}
	s.Cancel()

	snap := waitUntil(t, "the cancelled turn to unwind", idle, s)
	if out := transcript(snap); strings.Contains(out, "queued behind it") {
		t.Errorf("a dropped prompt ran anyway:\n%s", out)
	}
	if server.RequestCount() != 1 {
		t.Errorf("model was called %d times, want 1 — the dropped prompt became a second", server.RequestCount())
	}
}

func TestUnqueueTakesAPromptBackWithoutRunningIt(t *testing.T) {
	s, server := steerable(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "sleep 30"}}}},
	)

	s.Submit("the long thing")
	waitUntil(t, "the turn to start", func(sn Snapshot) bool { return sn.Busy }, s)
	s.Submit("second thoughts")
	id := waitForQueue(t, s, 1)

	if got := s.Unqueue(id); got != "second thoughts" {
		t.Errorf("Unqueue returned %q", got)
	}
	if got := s.Unqueue(id); got != "" {
		t.Errorf("the same prompt came back twice: %q", got)
	}
	if queued := s.Snapshot().Queued; len(queued) != 0 {
		t.Errorf("still queued after being taken back: %v", queued)
	}

	s.DropQueued()
	s.Cancel()
	waitUntil(t, "the cancelled turn to unwind", idle, s)
	if server.RequestCount() != 1 {
		t.Errorf("model was called %d times, want 1", server.RequestCount())
	}
}

// Nothing to fold a steer into means it stays queued. /context and /vault mark the pane busy
// without starting a turn, and a steer taken off the queue then would be held by nobody.
func TestSteeringWithNoTurnRunningLeavesThePromptQueued(t *testing.T) {
	// Never started, so the prompt stays queued instead of being picked up as it lands.
	s := New(&agent.Agent{}, nil)

	s.Submit("not going anywhere")
	snap := s.Snapshot()
	if snap.Steerable {
		t.Fatal("an idle session claims to be steerable")
	}
	if len(snap.Queued) != 1 {
		t.Fatalf("prompt was not queued: %v", snap.Queued)
	}
	s.Steer(snap.Queued[0].ID)

	snap = s.Snapshot()
	if len(snap.Queued) != 1 || len(snap.Steering) != 0 {
		t.Errorf("a steer with no turn to take it moved the prompt: queued=%v steering=%v",
			snap.Queued, snap.Steering)
	}
}

// A slash command must never reach the model, and must not appear as a user prompt.
func TestSlashCommandIsHandledLocally(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "should not run"})

	s.Submit("hello")
	settled(t, s, "should not run")

	s.Submit("/new")
	waitUntil(t, "the transcript to clear", func(sn Snapshot) bool { return len(sn.Items) == 0 }, s)

	if server.RequestCount() != 1 {
		t.Errorf("model was called %d times; /new should not reach it", server.RequestCount())
	}
}

func TestNewRestartsTheWorkspaceAndSaysWhenThatFails(t *testing.T) {
	s, _ := newTestSession(t)
	restarts := make(chan struct{}, 2)
	s.Restart = func() error {
		restarts <- struct{}{}
		return errors.New("jj exploded")
	}

	s.Submit("/new")
	snap := waitUntil(t, "the error", func(sn Snapshot) bool { return len(sn.Items) > 0 }, s)

	if len(restarts) != 1 {
		t.Errorf("/new restarted the workspace %d times, want once", len(restarts))
	}
	if out := transcript(snap); !strings.Contains(out, "jj exploded") {
		t.Errorf("a failed restart was not reported:\n%s", out)
	}
}

func TestHandoffStartsAFreshConversationWithTheNoteAsTheDraft(t *testing.T) {
	s, server := newTestSession(t,
		mockapi.Turn{Text: "earlier reply"},
		mockapi.Turn{Text: "## Goal\nship the thing"},
		mockapi.Turn{Text: "picked up"},
	)
	s.Submit("earlier prompt")
	settled(t, s, "earlier reply")

	s.Submit("/handoff")
	snap := waitUntil(t, "the draft", func(sn Snapshot) bool { return sn.Draft.ID == 1 }, s)
	if !strings.Contains(snap.Draft.Text, "ship the thing") {
		t.Fatalf("draft = %q, want the agent's note", snap.Draft.Text)
	}
	if len(snap.Items) != 0 {
		t.Errorf("the old transcript survived the handoff:\n%s", transcript(snap))
	}

	s.Submit(snap.Draft.Text)
	settled(t, s, "picked up")
	if msgs, _ := server.LastRequest()["messages"].([]any); len(msgs) != 1 {
		t.Errorf("the new session sent %d messages, want only the note", len(msgs))
	}
}

func TestCompactSendsTheViewAsTheOnlyMessageAndKeepsTheWorkspace(t *testing.T) {
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	s, server := newTestSession(t,
		mockapi.Turn{Text: "earlier reply"},
		mockapi.Turn{Text: "picked up"},
	)
	s.Transcript = subagent.NewTranscript(filepath.Join(t.TempDir(), "sessions"), "sess1")
	var restarts atomic.Int32
	s.Restart = func() error { restarts.Add(1); return nil }
	s.Submit("earlier prompt")
	settled(t, s, "earlier reply")

	s.Submit("/compact")
	settled(t, s, "picked up")
	msgs, _ := server.LastRequest()["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("the compacted session sent %d messages, want only the view", len(msgs))
	}
	if body := fmt.Sprint(msgs[0]); !strings.Contains(body, compact.Marker) || !strings.Contains(body, "earlier prompt") {
		t.Errorf("first message is not the view of the earlier conversation: %s", body)
	}
	if n := restarts.Load(); n != 0 {
		t.Errorf("/compact restarted the workspace %d times; the work carries on in it", n)
	}
}

func TestEachCompactionLogsItsNumberAndANewSessionStartsTheCountOver(t *testing.T) {
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	s, _ := newTestSession(t,
		mockapi.Turn{Text: "reply-1"}, mockapi.Turn{Text: "reply-2"}, mockapi.Turn{Text: "reply-3"},
		mockapi.Turn{Text: "reply-4"}, mockapi.Turn{Text: "reply-5"}, mockapi.Turn{Text: "reply-6"},
	)
	dir := t.TempDir()
	s.Transcript = subagent.NewTranscript(filepath.Join(dir, "sessions"), "sess1")
	s.Restart = func() error { return nil }
	for _, step := range []struct{ in, out string }{
		{"one", "reply-1"}, {"/compact", "reply-2"}, {"two", "reply-3"}, {"/compact", "reply-4"},
		{"", ""}, {"three", "reply-5"}, {"/compact", "reply-6"},
	} {
		if step.in == "" {
			s.NewSession()
			continue
		}
		s.Submit(step.in)
		settled(t, s, step.out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "compact", "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e struct{ N int }
		_ = json.Unmarshal([]byte(line), &e)
		got = append(got, e.N)
	}
	if fmt.Sprint(got) != "[1 2 1]" {
		t.Errorf("logged compaction numbers %v, want [1 2 1]", got)
	}
}

func TestRecallAfterASecondCompactionReturnsTheStepTheViewNumbers(t *testing.T) {
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	s, server := newTestSession(t,
		mockapi.Turn{Text: "first reply"},
		mockapi.Turn{Text: "ok"},
		mockapi.Turn{Text: "second reply"},
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "r1", Name: compact.RecallTool, Input: map[string]any{"steps": "3"}}}},
		mockapi.Turn{Text: "recalled"},
	)
	s.Transcript = subagent.NewTranscript(filepath.Join(t.TempDir(), "sessions"), "sess1")
	s.Submit("first prompt")
	settled(t, s, "first reply")
	if tools := fmt.Sprint(server.LastRequest()["tools"]); strings.Contains(tools, compact.RecallTool) {
		t.Errorf("recall was offered before any compaction")
	}
	s.Submit("/compact")
	settled(t, s, "ok")
	s.Submit("second prompt")
	settled(t, s, "second reply")

	s.Submit("/compact")
	settled(t, s, "recalled")
	req := server.LastRequest()
	if tools := fmt.Sprint(req["tools"]); !strings.Contains(tools, compact.RecallTool) {
		t.Errorf("recall was not offered in the compacted session")
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want the view, the recall call and its result", len(msgs))
	}
	if view := fmt.Sprint(msgs[0]); !strings.Contains(view, "@3 USER: second prompt") {
		t.Errorf("the view does not number second prompt as step 3:\n%s", view)
	}
	raw, _ := json.Marshal(msgs[2])
	var last struct {
		Content []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &last)
	if len(last.Content) == 0 || last.Content[0].Type != "tool_result" || len(last.Content[0].Content) == 0 ||
		!strings.HasPrefix(last.Content[0].Content[0].Text, "@3 USER: second prompt") {
		t.Errorf("recall 3 did not return the step the view numbers 3:\n%s", raw)
	}
}

func TestHandoffCopyPutsTheNoteOnTheClipboardAndKeepsTheConversation(t *testing.T) {
	s, server := newTestSession(t,
		mockapi.Turn{Text: "earlier reply"},
		mockapi.Turn{Text: "## Goal\nship the thing"},
		mockapi.Turn{Text: "still here"},
	)
	s.Submit("earlier prompt")
	settled(t, s, "earlier reply")

	s.Submit("/handoff copy")
	snap := waitUntil(t, "the clipboard", func(sn Snapshot) bool { return sn.Clipboard.ID == 1 && !sn.Busy }, s)
	if !strings.Contains(snap.Clipboard.Text, "ship the thing") {
		t.Fatalf("clipboard = %q, want the agent's note", snap.Clipboard.Text)
	}
	if snap.Draft.ID != 0 {
		t.Errorf("copy also filled the input box with %q", snap.Draft.Text)
	}

	s.Submit("next prompt")
	settled(t, s, "still here")
	if msgs, _ := server.LastRequest()["messages"].([]any); len(msgs) < 3 {
		t.Errorf("the session sent %d messages after a copy, want the earlier conversation kept", len(msgs))
	}
}

func TestUnknownCommandReportsWhatExists(t *testing.T) {
	s, server := newTestSession(t)
	s.Submit("/nope")
	snap := waitUntil(t, "the error", func(sn Snapshot) bool { return len(sn.Items) > 0 }, s)

	if out := transcript(snap); !strings.Contains(out, "unknown command /nope") || !strings.Contains(out, "/clear") {
		t.Errorf("unhelpful error:\n%s", out)
	}
	if server.RequestCount() != 0 {
		t.Error("an unknown command was sent to the model")
	}
}

// A path typed at the prompt starts with a slash but is not a command.
func TestAbsolutePathIsNotACommand(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "read it"})
	s.Submit("/Users/someone/notes.md please read this")
	waitUntil(t, "the turn", func(sn Snapshot) bool {
		return !sn.Busy && strings.Contains(transcript(sn), "read it")
	}, s)
	if server.RequestCount() != 1 {
		t.Errorf("a path was swallowed as a command; model calls = %d", server.RequestCount())
	}
}

func TestAParkedCommandShowsItsSummaryUntilAnswered(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo hi"}}}},
		mockapi.Turn{Text: "understood"},
	)
	var asked atomic.Value
	s.Explain = func(_ context.Context, command string) (string, error) {
		asked.Store(command)
		return "Prints hi.", nil
	}
	s.SetMode(ModeReadOnly)

	s.Submit("go")
	waitUntil(t, "the summary", func(sn Snapshot) bool { return sn.PendingSummary == "Prints hi." }, s)
	if got := asked.Load(); got != "echo hi" {
		t.Errorf("explained %q, want the bare command", got)
	}
	s.Approve(false)
	if sn := s.Snapshot(); sn.PendingSummary != "" {
		t.Errorf("summary outlived its approval: %q", sn.PendingSummary)
	}
	settled(t, s, "understood")
}

func TestTheSessionIsNamedOnItsFirstPromptAndRenamedFromItsHistoryAtTheSixth(t *testing.T) {
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	var turns []mockapi.Turn
	for i := 1; i <= 6; i++ {
		turns = append(turns, mockapi.Turn{Text: fmt.Sprintf("reply %02d", i)})
	}
	s, _ := newTestSession(t, turns...)
	s.Transcript = subagent.NewTranscript(filepath.Join(t.TempDir(), "sessions"), "sess1")
	var calls atomic.Int32
	var history atomic.Value
	s.Namer = func(_ context.Context, text string) (string, error) {
		history.Store(text)
		return fmt.Sprintf("name %d", calls.Add(1)), nil
	}

	s.Submit("prompt 01")
	settled(t, s, "reply 01")
	waitUntil(t, "the first name", func(sn Snapshot) bool { return sn.Topic == "name 1" }, s)
	s.Submit("/nosuchcommand")
	for i := 2; i <= 5; i++ {
		s.Submit(fmt.Sprintf("prompt %02d", i))
		settled(t, s, fmt.Sprintf("reply %02d", i))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("named %d times by the fifth prompt, want once", n)
	}
	s.Submit("prompt 06")
	settled(t, s, "reply 06")
	waitUntil(t, "the rename", func(sn Snapshot) bool { return sn.Topic == "name 2" }, s)
	if got := history.Load().(string); !strings.Contains(got, "reply 03") {
		t.Errorf("renamed from %q, want the compacted view with the replies in it", got)
	}
}

func TestAnAnsweredQuestionCountsTowardsTheRename(t *testing.T) {
	var turns []mockapi.Turn
	for i := 1; i <= 4; i++ {
		turns = append(turns, mockapi.Turn{Text: fmt.Sprintf("reply %02d", i)})
	}
	turns = append(turns,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "asks", Input: map[string]any{}}}},
		mockapi.Turn{Text: "turn over"},
	)
	s, _ := newTestSession(t, turns...)
	s.agent.Handlers["asks"] = func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		cb.Ask(ctx, ask.Question{Question: "which?"})
		return anthropic.NewToolResultBlock(call.ID, "done", false)
	}
	var calls atomic.Int32
	s.Namer = func(context.Context, string) (string, error) {
		return fmt.Sprintf("name %d", calls.Add(1)), nil
	}

	for i := 1; i <= 4; i++ {
		s.Submit(fmt.Sprintf("prompt %02d", i))
		settled(t, s, fmt.Sprintf("reply %02d", i))
	}
	s.Submit("prompt 05")
	waitUntil(t, "the question", func(sn Snapshot) bool { return sn.Question != nil }, s)
	s.Answer(&ask.Answer{})
	settled(t, s, "turn over")
	waitUntil(t, "the rename", func(sn Snapshot) bool { return sn.Topic == "name 2" }, s)
}

// Declining in read-only mode has to stop the tool actually running.
func TestDecliningInReadOnlyModeStopsTheTool(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo should-not-appear"}}}},
		mockapi.Turn{Text: "understood"},
	)
	s.SetMode(ModeReadOnly)

	s.Submit("go")
	waitUntil(t, "the approval prompt", func(sn Snapshot) bool { return sn.Pending != "" }, s)
	s.Approve(false)

	snap := waitUntil(t, "the turn to finish", idle, s)
	if out := transcript(snap); strings.Contains(out, "should-not-appear") {
		t.Errorf("a declined command ran anyway:\n%s", out)
	}
}

func TestANewSessionStartsInAutoMode(t *testing.T) {
	s, _ := newTestSession(t)
	if got := s.Snapshot().Mode; got != ModeAuto {
		t.Errorf("a new session starts in %s mode, want auto", got)
	}
}

// Auto mode must not park the turn on an approval.
func TestAutoModeRunsToolsWithoutAsking(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo ran-it"}}}},
		mockapi.Turn{Text: "done"},
	)
	s.SetMode(ModeAuto)

	s.Submit("go")
	snap := settled(t, s, "done")

	if snap.Pending != "" {
		t.Error("auto mode parked on an approval")
	}
	found := false
	for _, item := range snap.Items {
		if item.Kind == KindTool && strings.Contains(item.Body, "ran-it") {
			found = true
		}
	}
	if !found {
		t.Errorf("the command did not run:\n%s", transcript(snap))
	}
}

// withRoster gives a session a roster and the waker that hangs off it, which is the wiring main
// does. A test that set the roster and forgot the waker would be testing an alux nobody runs.
func withRoster(s *Session) *subagent.Roster {
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	roster.OnOutcome = s.Wake
	return roster
}

// The point of the whole feature: a run that lands while nobody is typing gets picked up on its
// own. Without the wake it would sit on the roster until the user next said something, which is
// exactly the hang this replaces.
func TestAFinishedRunStartsATurnWhileTheSessionIsIdle(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "I have read the finding."})
	roster := withRoster(s)

	roster.QueueOutcomeForTest(subagent.Outcome{Agent: "research", Report: "THE-FINDING"})

	settled(t, s, "I have read the finding.")
	if !sentToModel(server, "THE-FINDING") {
		t.Error("the finished run never reached the model")
	}
}

// A background command finishing while the session is idle is the same case as a finished run.
func TestABackgroundCommandExitStartsATurnWhileTheSessionIsIdle(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "I saw it exit."})
	shells := procs.NewRegistry()
	s.Procs = shells
	shells.OnExit = s.Wake

	p, err := procs.Start(context.Background(), "echo THE-EXIT-OUTPUT", nil)
	if err != nil {
		t.Fatal(err)
	}
	shells.Adopt(p, "call_1")

	settled(t, s, "I saw it exit.")
	if !sentToModel(server, "THE-EXIT-OUTPUT") {
		t.Error("the background exit never reached the model")
	}
}

// An unclaimed held command dies when its turn ends, and the transcript says so.
func TestATurnEndingKillsTheCommandsItHeld(t *testing.T) {
	s, _ := newTestSession(t, mockapi.Turn{Text: "done"})
	shells := procs.NewRegistry()
	s.Procs = shells
	shells.OnExit = s.Wake
	t.Cleanup(func() { shells.KillAll() })

	p, err := procs.Start(shells.Context(), "sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	shells.Hold(p, "call_1")

	s.Submit("go")
	snap := waitUntil(t, "the held command to be reported killed", func(snap Snapshot) bool {
		return strings.Contains(transcript(snap), "proc 1 killed")
	}, s)
	if !strings.Contains(transcript(snap), "done") {
		t.Errorf("the turn did not finish:\n%s", transcript(snap))
	}
	deadline := time.Now().Add(5 * time.Second)
	for !p.Exited() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.Exited() {
		t.Fatal("the held command was still running after its turn ended")
	}
}

// A woken turn is alux talking to itself, and the transcript has to say so. Showing it as
// a user message would put words in the user's mouth that they can see they never typed.
func TestASelfStartedTurnIsMarkedAsOneInTheTranscript(t *testing.T) {
	s, _ := newTestSession(t, mockapi.Turn{Text: "picked up"})
	roster := withRoster(s)

	roster.QueueOutcomeForTest(subagent.Outcome{Agent: "research", Report: "THE-FINDING"})

	snap := settled(t, s, "picked up")
	for _, item := range snap.Items {
		if item.Kind == KindUser {
			t.Errorf("the self-started turn was shown as something the user said: %q", item.Text)
		}
	}
	if !strings.Contains(transcript(snap), "background run landed") {
		t.Errorf("nothing in the transcript says alux started this turn:\n%s", transcript(snap))
	}
}

// A wake whose outcome some other path already delivered must cost nothing. The common case is a
// run landing mid-turn: that turn's own drain takes it, and the wake queued behind it arrives to
// find the queue empty. Starting a turn anyway would bill for a request with nothing to say.
func TestAWakeWithNothingLeftToSayStartsNoTurn(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "should not happen"})
	withRoster(s)

	s.Wake()

	// No transcript content to wait on, so settle by submitting afterwards: the queue is served
	// in order, so a reply to this proves the wake ahead of it is long done.
	s.Submit("are you there")
	settled(t, s, "should not happen")

	if got := server.RequestCount(); got != 1 {
		t.Errorf("requests = %d, want 1 — the empty wake started a turn of its own", got)
	}
}

// A run dispatched by a conversation that has since been cleared must not report into the one
// that replaced it. The new conversation never asked for the work and cannot judge what the
// answer is worth — and with a detached coder the edits have already landed, so the report is
// the first the new agent would hear of changes it cannot account for.
func TestARunFinishingAfterAClearDoesNotReportIntoTheNewConversation(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "answering you"})
	roster := withRoster(s)
	run := roster.StartForTest("research")

	s.NewSession()
	run.FinishForTest(subagent.Outcome{Agent: "research", Report: "STALE-FINDING"})

	// Settled by a prompt of its own: the wake, if one came, is queued ahead of this and would
	// have been served first.
	s.Submit("something unrelated")
	settled(t, s, "answering you")

	if sentToModel(server, "STALE-FINDING") {
		t.Error("a run from the cleared conversation reported into the new one")
	}
}

// The same for an outcome that had already landed and was waiting to be delivered: clearing has
// to drop what is queued as well as what is still coming.
func TestClearingDropsAnOutcomeThatWasAlreadyWaiting(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "answering you"})
	roster := withRoster(s)

	roster.QueueOutcomeForTest(subagent.Outcome{Agent: "research", Report: "STALE-FINDING"})
	s.NewSession()

	s.Submit("something unrelated")
	settled(t, s, "answering you")

	if sentToModel(server, "STALE-FINDING") {
		t.Error("an outcome queued before the clear was delivered after it")
	}
}

// Clearing must not start a turn in the conversation it just emptied. A wake that survived the
// clear would open the new session by talking to itself about work the user cannot see.
func TestAClearedRunDoesNotWakeTheSession(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "should not happen"})
	roster := withRoster(s)
	run := roster.StartForTest("research")

	s.NewSession()
	run.FinishForTest(subagent.Outcome{Agent: "research", Report: "STALE-FINDING"})

	s.Submit("are you there")
	settled(t, s, "should not happen")

	if got := server.RequestCount(); got != 1 {
		t.Errorf("requests = %d, want 1 — the cleared run woke a turn of its own", got)
	}
}

// The UI holds back a woken turn's desktop notification, so Woken must mark that turn and no
// turn the user starts.
func TestOnlyATurnALandedRunRaisesIsMarkedWoken(t *testing.T) {
	s, _ := newTestSession(t, mockapi.Turn{Text: "picked it up"}, mockapi.Turn{Text: "answering you"})
	roster := withRoster(s)
	run := roster.StartForTest("research")

	run.FinishForTest(subagent.Outcome{Agent: "research", Report: "FINDING"})
	if !settled(t, s, "picked it up").Woken {
		t.Error("the turn a landed run raised was not marked woken")
	}
	s.Submit("are you there")
	if settled(t, s, "answering you").Woken {
		t.Error("a turn the user started was marked woken")
	}
}

// The bump must retire the old conversation and nothing else. An off-by-one here would be
// invisible in the three tests above and would silently stop every future report.
func TestARunDispatchedAfterAClearStillReports(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "I have read it."})
	roster := withRoster(s)

	s.NewSession()
	roster.QueueOutcomeForTest(subagent.Outcome{Agent: "research", Report: "FRESH-FINDING"})

	settled(t, s, "I have read it.")
	if !sentToModel(server, "FRESH-FINDING") {
		t.Error("a run dispatched after the clear never reached the model")
	}
}

// sentToModel reports whether any request carried this text.
func sentToModel(server *mockapi.Server, text string) bool {
	for _, body := range server.Requests {
		if strings.Contains(string(body), text) {
			return true
		}
	}
	return false
}
