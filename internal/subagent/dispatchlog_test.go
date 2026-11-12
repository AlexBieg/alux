package subagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
)

func capture(m *Manager) func() []Dispatch {
	var mu sync.Mutex
	var got []Dispatch
	m.Log = func(rec Dispatch) {
		mu.Lock()
		got = append(got, rec)
		mu.Unlock()
	}
	return func() []Dispatch {
		mu.Lock()
		defer mu.Unlock()
		return append([]Dispatch(nil), got...)
	}
}

func note(id string) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: "note", Input: map[string]any{"text": "a"}}}}
}

func TestADispatchIsLoggedWithItsUsageSummedAcrossRequests(t *testing.T) {
	m, _ := manager(t, echo(false),
		mockapi.Turn{
			Tools: note("t1").Tools,
			Usage: mockapi.Usage{InputTokens: 100, CacheRead: 1000, OutputTokens: 20},
		},
		mockapi.Turn{
			Text:  "The answer is 42.",
			Usage: mockapi.Usage{InputTokens: 50, CacheRead: 1100, OutputTokens: 10},
		},
	)
	logged := capture(m)

	if _, err := m.Dispatch(context.Background(), "echo", "find the answer", "some context", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}

	recs := logged()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Agent != "echo" || rec.Model != "mock" || rec.Task != "find the answer" || rec.Context != "some context" {
		t.Errorf("identity fields = %+v", rec)
	}
	if rec.Stop != StopDone || rec.Err != "" || rec.Report != "The answer is 42." {
		t.Errorf("outcome = stop %q err %q report %q", rec.Stop, rec.Err, rec.Report)
	}
	if rec.Steps != 2 || rec.MaxSteps != 5 || rec.Requests != 2 {
		t.Errorf("steps %d/%d requests %d, want 2/5 and 2", rec.Steps, rec.MaxSteps, rec.Requests)
	}
	if rec.Input != 150 || rec.CacheRead != 2100 || rec.Output != 30 {
		t.Errorf("tokens in %d cache %d out %d, want 150, 2100, 30", rec.Input, rec.CacheRead, rec.Output)
	}
	if rec.Conformed != nil {
		t.Error("a prose agent recorded schema conformance")
	}
}

func TestARunThatExhaustsItsBudgetIsLoggedAsAStepLimit(t *testing.T) {
	turns := []mockapi.Turn{note("t1"), note("t2"), note("t3"), note("t4"), note("t5"), {Text: "Still unfinished: all of it"}}
	m, _ := manager(t, echo(false), turns...)
	logged := capture(m)

	if _, err := m.Dispatch(context.Background(), "echo", "loop forever", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	rec := logged()[0]
	if rec.Stop != StopStepLimit || rec.Steps != 5 {
		t.Errorf("stop %q after %d steps, want step_limit after 5", rec.Stop, rec.Steps)
	}
}

// Full is a stop, not a failure: the run still owes an account of what it left undone.
func TestARunWhoseWindowFillsReportsWhatIsUnfinished(t *testing.T) {
	full := note("t1")
	full.Usage = mockapi.Usage{InputTokens: 10_000_000}
	m, server := manager(t, echo(false), full, mockapi.Turn{Text: "Still unfinished: the second half"})
	logged := capture(m)

	report, err := m.Dispatch(context.Background(), "echo", "read everything", "", agent.Callbacks{})
	if err != nil {
		t.Fatalf("a full window failed the run: %v", err)
	}
	if !strings.Contains(report, "context window was full") || !strings.Contains(report, "Still unfinished: the second half") {
		t.Errorf("report = %q, want the full window and the closing account", report)
	}
	if n := server.RequestCount(); n != 2 {
		t.Errorf("requests = %d, want the one that filled the window and the closing one", n)
	}
	if rec := logged()[0]; rec.Stop != StopContextFull {
		t.Errorf("stop %q, want %q", rec.Stop, StopContextFull)
	}
	cm, _ := server.LastRequest()["context_management"].(map[string]any)
	edits, _ := cm["edits"].([]any)
	if len(edits) != 2 {
		t.Fatalf("closing request edits = %v, want clear_thinking and clear_tool_uses", edits)
	}
	if edit, _ := edits[1].(map[string]any); edit["type"] != "clear_tool_uses_20250919" {
		t.Errorf("second edit = %v, want clear_tool_uses", edit)
	}
}

// clearedSpares is the exclude_tools of the clear_tool_uses edit on one request body.
func clearedSpares(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		ContextManagement struct {
			Edits []map[string]any `json:"edits"`
		} `json:"context_management"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	for _, edit := range req.ContextManagement.Edits {
		if edit["type"] == "clear_tool_uses_20250919" {
			return fmt.Sprint(edit["exclude_tools"])
		}
	}
	t.Fatalf("no clear_tool_uses edit in %s", body)
	return ""
}

func TestAFailedRequestIsLoggedAsAnError(t *testing.T) {
	m, _ := manager(t, echo(false), mockapi.Turn{Status: 400})
	logged := capture(m)

	if _, err := m.Dispatch(context.Background(), "echo", "x", "", agent.Callbacks{}); err == nil {
		t.Fatal("dispatch succeeded against a 400")
	}
	rec := logged()[0]
	if rec.Stop != StopError || rec.Err == "" {
		t.Errorf("stop %q err %q, want error with a message", rec.Stop, rec.Err)
	}
}

func TestSchemaConformanceIsLogged(t *testing.T) {
	for _, tc := range []struct {
		reply string
		want  bool
	}{{`{"answer":"42"}`, true}, {`{"answer":"trunc`, false}} {
		m, _ := manager(t, reporter(func(json.RawMessage) string { return "ok" }), mockapi.Turn{Text: tc.reply})
		logged := capture(m)
		if _, err := m.Dispatch(context.Background(), "echo", "x", "", agent.Callbacks{}); err != nil {
			t.Fatal(err)
		}
		rec := logged()[0]
		if rec.Conformed == nil || *rec.Conformed != tc.want {
			t.Errorf("reply %s: conformed = %v, want %v", tc.reply, rec.Conformed, tc.want)
		}
	}
}

func TestABackgroundRunIsLoggedWhenItLands(t *testing.T) {
	m, _ := manager(t, echo(true), mockapi.Turn{Text: "Filed it."})
	logged := capture(m)

	if _, err := m.Dispatch(context.Background(), "echo", "file it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("background run never finished")
	}
	recs := logged()
	if len(recs) != 1 || !recs[0].Background || recs[0].Report != "Filed it." {
		t.Errorf("records = %+v", recs)
	}
}

func TestEachRunIsLoggedUnderTheIDItsRosterRowCarries(t *testing.T) {
	m, _ := manager(t, echo(false), mockapi.Turn{Text: "one"}, mockapi.Turn{Text: "two"})
	m.Session = "the-session"
	logged := capture(m)

	for range 2 {
		if _, err := m.Dispatch(context.Background(), "echo", "x", "", agent.Callbacks{}); err != nil {
			t.Fatal(err)
		}
	}
	recs, runs := logged(), m.Roster.Runs()
	if len(recs) != 2 || len(runs) != 2 {
		t.Fatalf("%d records and %d runs, want 2 of each", len(recs), len(runs))
	}
	for i := range recs {
		if recs[i].ID == "" || recs[i].ID != runs[i].Dispatch {
			t.Errorf("run %d: logged as %q, roster says %q", i, recs[i].ID, runs[i].Dispatch)
		}
		if recs[i].Session != "the-session" {
			t.Errorf("run %d: session %q", i, recs[i].Session)
		}
	}
	if recs[0].ID == recs[1].ID {
		t.Errorf("two runs share the ID %q", recs[0].ID)
	}
}

func TestFileLogAppendsOneJSONLinePerRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dispatches.jsonl")
	write := FileLog(path)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			write(Dispatch{Agent: "echo", Steps: i})
		}()
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	for sc := bufio.NewScanner(f); sc.Scan(); lines++ {
		var rec Dispatch
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line %d is not a record: %v", lines, err)
		}
	}
	if lines != 20 {
		t.Errorf("%d lines, want 20", lines)
	}
}
