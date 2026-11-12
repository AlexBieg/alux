package procs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

func withForegroundLimit(t *testing.T, d time.Duration) {
	t.Helper()
	was := foregroundLimit
	foregroundLimit = d
	t.Cleanup(func() { foregroundLimit = was })
}

func call(t *testing.T, name string, input any) agent.Call {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return agent.Call{ID: "call_1", Name: name, Input: raw}
}

func approving() agent.Callbacks {
	return agent.Callbacks{
		Approve:      func(context.Context, string, string) bool { return true },
		OnToolStart:  func(string, string, agent.ToolKind) {},
		OnToolResult: func(string, string, string, agent.ToolKind) {},
	}
}

// text pulls the tool result's body out, which is the only part the model ever sees.
func text(t *testing.T, b anthropic.ContentBlockParamUnion) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range shape.Content {
		out = append(out, c.Text)
	}
	return strings.Join(out, "\n")
}

func TestAForegroundCommandReturnsItsOutput(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	got := text(t, r.Bash()(context.Background(), call(t, "bash", map[string]any{"command": "echo hello"}), approving()))

	if !strings.Contains(got, "hello") {
		t.Fatalf("result = %q, want it to contain hello", got)
	}
	if !strings.Contains(got, "exit 0") {
		t.Fatalf("result = %q, want the exit status", got)
	}
	if len(r.Running()) != 0 {
		t.Fatal("a command that finished in the foreground should not have been adopted")
	}
}

func TestACommandThatOverrunsIsHeldRatherThanKilled(t *testing.T) {
	withForegroundLimit(t, 150*time.Millisecond)
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })

	got := text(t, r.Bash()(context.Background(),
		call(t, "bash", map[string]any{"command": "echo starting; sleep 30"}), approving()))

	for _, want := range []string{"starting", "still running", "NOT killed", "last output", "background_proc(1", "kill_shell(1", "killed when this turn ends"} {
		if !strings.Contains(got, want) {
			t.Fatalf("result = %q, want it to contain %q", got, want)
		}
	}

	running := r.Running()
	if len(running) != 1 {
		t.Fatalf("%d processes running, want the overrunning one to have been held", len(running))
	}
	// Checked after the signal would have landed: a kill is asynchronous.
	time.Sleep(200 * time.Millisecond)
	if running[0].Exited() {
		t.Fatalf("the overrunning command was killed, status %q — holding has to leave it running", running[0].Status())
	}
}

func overrun(t *testing.T, r *Registry, command string) *Proc {
	t.Helper()
	withForegroundLimit(t, 150*time.Millisecond)
	r.Bash()(context.Background(), call(t, "bash", map[string]any{"command": command}), approving())
	p, ok := r.Get(1)
	if !ok {
		t.Fatal("the overrunning command was not held")
	}
	return p
}

func TestAHeldCommandThatExitsWakesNobody(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	woken := make(chan struct{}, 1)
	r.OnExit = func() { woken <- struct{}{} }

	p := overrun(t, r, "sleep 0.4; echo late")
	waitFor(t, "the held command to exit", p.Exited)

	select {
	case <-woken:
		t.Fatal("a held command woke the session; only a backgrounded one may")
	case <-time.After(200 * time.Millisecond):
	}
	if exits := r.TakeExits(); len(exits) != 0 {
		t.Fatalf("got %d queued exits, want none for a held command", len(exits))
	}
}

func TestKillHeldStopsHeldCommandsAndSparesBackgroundedOnes(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	held := overrun(t, r, "sleep 30")
	r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "sleep 30", "check_in": 600, "lifetime": "finite"}), approving())
	started, _ := r.Get(2)

	killed := r.KillHeld()
	if len(killed) != 1 || killed[0] != held {
		t.Fatalf("killed %d processes, want only the held one", len(killed))
	}
	waitFor(t, "the held command to die", held.Exited)
	time.Sleep(200 * time.Millisecond)
	if started.Exited() {
		t.Fatal("KillHeld stopped a command started with run_background")
	}
	if exits := r.TakeExits(); len(exits) != 0 {
		t.Fatalf("got %d queued exits, want the killed held command not reported", len(exits))
	}
}

func TestBashSecondsHoldsTheTurnPastTheDefaultLimit(t *testing.T) {
	withForegroundLimit(t, 150*time.Millisecond)
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })

	got := text(t, r.Bash()(context.Background(),
		call(t, "bash", map[string]any{"command": "sleep 0.5; echo THE-RESULT; exit 2", "seconds": 10}), approving()))
	if !strings.Contains(got, "THE-RESULT") || !strings.Contains(got, "exit 2") {
		t.Fatalf("result = %q, want the command's result rather than a hold", got)
	}
	if _, held := r.Get(1); held {
		t.Fatal("a command inside its seconds was held")
	}
}

func TestBackgroundProcMakesTheExitWakeTheSession(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	woken := make(chan struct{}, 1)
	r.OnExit = func() { woken <- struct{}{} }
	p := overrun(t, r, "sleep 0.5; echo BACKGROUNDED")

	got := text(t, r.runBackground(context.Background(), call(t, "background_proc", map[string]any{"id": 1, "check_in": 600}), approving()))
	if !strings.Contains(got, "woken when it exits") {
		t.Fatalf("result = %q, want it to say the exit will be reported", got)
	}
	if killed := r.KillHeld(); len(killed) != 0 {
		t.Fatal("a backgrounded command was still treated as held")
	}

	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("the backgrounded command's exit never woke the session")
	}
	exits := r.TakeExits()
	if len(exits) != 1 || exits[0].ID != p.ID || !strings.Contains(exits[0].Output, "BACKGROUNDED") {
		t.Fatalf("exits = %+v, want the backgrounded command's exit with its output", exits)
	}
}

func TestCancellingTheTurnKillsAStillForegroundCommand(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	turn, cancel := context.WithCancel(context.Background())

	done := make(chan string, 1)
	go func() {
		done <- text(t, r.Bash()(turn, call(t, "bash", map[string]any{"command": "sleep 30"}), approving()))
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if !strings.Contains(got, "cancelled") {
			t.Fatalf("result = %q, want it to say the command was cancelled", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the turn did not return the tool call")
	}
	if len(r.Running()) != 0 {
		t.Fatal("cancelling the turn left the command running")
	}
}

func TestDecliningACommandRunsNothing(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	cb := approving()
	cb.Approve = func(context.Context, string, string) bool { return false }

	got := text(t, r.Bash()(context.Background(), call(t, "bash", map[string]any{"command": "echo nope"}), cb))
	if !strings.Contains(got, "declined") {
		t.Fatalf("result = %q, want it to report the decline", got)
	}
	if len(r.Running()) != 0 {
		t.Fatal("a declined command should not have run")
	}
}

func TestRunBackgroundReturnsAHandleAtOnce(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })

	start := time.Now()
	got := text(t, r.runStart(context.Background(),
		call(t, "run_background", map[string]any{"command": "sleep 30", "check_in": 600, "lifetime": "finite"}), approving()))

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %s to return, want it to come back at once", elapsed)
	}
	if !strings.Contains(got, "proc 1") {
		t.Fatalf("result = %q, want it to name the process id", got)
	}
	if len(r.Running()) != 1 {
		t.Fatalf("%d processes running, want 1", len(r.Running()))
	}
}

func TestBashOutputReadsOnlyWhatIsNew(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "echo once; sleep 30", "check_in": 600, "lifetime": "finite"}), approving())

	waitFor(t, "the first line", func() bool {
		return strings.Contains(text(t, r.runOutput(context.Background(), call(t, "bash_output", map[string]any{"id": 1}), approving())), "once")
	})

	again := text(t, r.runOutput(context.Background(), call(t, "bash_output", map[string]any{"id": 1}), approving()))
	if strings.Contains(again, "once") {
		t.Fatalf("second read = %q, want the already-read line gone", again)
	}
	if !strings.Contains(again, "Nothing new") {
		t.Fatalf("second read = %q, want it to say there is nothing new", again)
	}
}

func TestAnUnknownIdSaysWhatIsActuallyRunning(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "sleep 30", "check_in": 600, "lifetime": "finite"}), approving())

	got := text(t, r.runOutput(context.Background(), call(t, "bash_output", map[string]any{"id": 99}), approving()))
	if !strings.Contains(got, "no proc 99") {
		t.Fatalf("result = %q, want it to say there is no such process", got)
	}
	if !strings.Contains(got, "proc 1") {
		t.Fatalf("result = %q, want it to name what is running instead", got)
	}
}

func TestKillShellStopsTheProcess(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "sleep 30", "check_in": 600, "lifetime": "finite"}), approving())

	got := text(t, r.runKill(context.Background(), call(t, "kill_shell", map[string]any{"id": 1}), approving()))
	if !strings.Contains(got, "Killed proc 1") {
		t.Fatalf("result = %q, want it to confirm the kill", got)
	}
	waitFor(t, "the process to die", func() bool { return len(r.Running()) == 0 })
}

func TestTheBackgroundToolsAreOfferedWithTheirSchemas(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for _, tool := range NewRegistry().Tools() {
		names[tool.OfTool.Name] = true
	}
	for _, want := range []string{"run_background", "bash_output", "kill_shell", "poll", "background_proc"} {
		if !names[want] {
			t.Fatalf("%s is not offered; got %v", want, names)
		}
	}
	if names["wait_proc"] {
		t.Fatal("wait_proc is offered; backgrounding sets a wake instead")
	}
	if names["bash"] {
		t.Fatal("bash must not be re-declared here: it keeps the server-side schema")
	}
}
