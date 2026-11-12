package ui

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/charmbracelet/x/ansi"
)

// safeBuf is written by the renderer goroutine and read by the test.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The unit tests above drive the model directly, which skips the program loop, the renderer and
// the session's own goroutines. This runs the real thing over a pipe: a prompt goes in, a turn
// runs against a scripted API, and the reply has to appear on screen.
func TestEndToEndTurnReachesTheScreen(t *testing.T) {
	server := mockapi.New(
		mockapi.Turn{
			Thinking: "considering it",
			Tools:    []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo from-the-tool; seq 1 12"}}},
		},
		mockapi.Turn{Text: "All done."},
	)
	defer server.Close()

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

	var program *tea.Program
	s := session.New(a, func() {
		if program != nil {
			program.Send(sessionChanged{})
		}
	})

	in, inWriter := io.Pipe()
	out := &safeBuf{}
	program = tea.NewProgram(New(s), tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(100, 30))
	s.Start()

	finished := make(chan struct{})
	go func() {
		_, _ = program.Run()
		close(finished)
	}()

	waitFor(t, out, "›", "the prompt never rendered")

	_, _ = inWriter.Write([]byte("say hello\r"))

	waitFor(t, out, "All done.", "the model's reply never reached the screen")

	screen := ansi.Strip(out.String())
	if !strings.Contains(screen, "$ echo from-the-tool") {
		t.Errorf("the tool call was not shown as a box:\n%s", tail(screen))
	}
	if !strings.Contains(screen, "thinking") {
		t.Errorf("the thinking box never appeared:\n%s", tail(screen))
	}
	// Long output starts collapsed: the tool ran, but its output stays folded until asked for.
	if strings.Contains(screen, "from-the-tool\n") && strings.Contains(screen, "[exit 0]") {
		t.Errorf("tool output was shown expanded:\n%s", tail(screen))
	}

	program.Quit()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit")
	}
}

// A dispatch crosses five packages on its way to the screen — the manager runs it, the roster
// records it, the agent's callback announces it, the session makes a box for it and the view
// draws the row — and every unit test above stubs out at least one of them. This drives the
// real thing: the main agent asks for a subagent, the subagent answers against its own scripted
// turn, and both the row and the box have to appear.
func TestDispatchedSubagentReachesTheScreen(t *testing.T) {
	// One server for both agents. The main agent's turns come first, so the subagent's reply is
	// third: turns are consumed in order whoever asks.
	server := mockapi.New(
		mockapi.Turn{Tools: []mockapi.ToolUse{{
			ID: "t1", Name: "task",
			Input: map[string]any{"agent": "echo", "name": "thing hunt", "objective": "find the thing", "done": "d", "report": "r"},
		}}},
		mockapi.Turn{Text: "The agent says 42."},
	)
	defer server.Close()

	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	roster := subagent.NewRoster(nil)
	// A foreground definition with no tools: it replies once and is done, which is all this
	// test needs the subagent itself to do.
	agents, err := subagent.NewManager(roster, &subagent.Definition{
		Name:        "echo",
		Description: "test agent",
		System:      func() string { return "be a test" },
		Model:       func() string { return "mock" },
		MaxSteps:    3,
		Commands:    &guardian.Criteria{Agent: "echo", Allowed: []guardian.Category{guardian.Read}, Threshold: 0.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	agents.Client = &client

	a := &agent.Agent{
		Client: &client, Model: "mock", System: "test", Standing: agent.NewRuleInjector(""),
		Tools: func() []anthropic.ToolUnionParam {
			return append(agent.BuiltinTools(), agents.Tools()...)
		},
		Handlers:   agents.Handlers(),
		Fallback:   agent.RunBash,
		Concurrent: agents.Concurrent(),
	}

	var program *tea.Program
	s := session.New(a, func() {
		if program != nil {
			program.Send(sessionChanged{})
		}
	})
	s.Roster = roster

	in, inWriter := io.Pipe()
	out := &safeBuf{}
	program = tea.NewProgram(New(s), tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(100, 30))
	s.Start()

	finished := make(chan struct{})
	go func() {
		_, _ = program.Run()
		close(finished)
	}()

	waitFor(t, out, "›", "the prompt never rendered")
	_, _ = inWriter.Write([]byte("go and look\r"))
	waitFor(t, out, "The agent says 42.", "the main agent's reply never reached the screen")

	screen := ansi.Strip(out.String())
	if !strings.Contains(screen, "thing hunt · echo") {
		t.Errorf("the dispatch never appeared as a box:\n%s", tail(screen))
	}
	// Finished, so the header must say so rather than freeze at dispatch.
	if !strings.Contains(screen, "done in") {
		t.Errorf("the box never reported the run finishing:\n%s", tail(screen))
	}
	if got := roster.Runs(); len(got) != 1 || got[0].State != subagent.Done {
		t.Errorf("roster = %+v, want one finished run", got)
	}

	program.Quit()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit")
	}
}

// The gauge crosses three packages — reported usage, the session snapshot, the status line — so
// it is tested over the whole path rather than from the model alone.
func TestContextUsedReachesTheStatusLine(t *testing.T) {
	server := mockapi.New(mockapi.Turn{
		Text:  "counted",
		Usage: mockapi.Usage{InputTokens: 40_000, CacheRead: 10_000, OutputTokens: 2_000},
	})
	defer server.Close()

	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	a := &agent.Agent{Client: &client, Model: "mock", System: "test", Tools: agent.BuiltinTools, Window: 200_000,
		Standing: agent.NewRuleInjector("")}

	var program *tea.Program
	s := session.New(a, func() {
		if program != nil {
			program.Send(sessionChanged{})
		}
	})

	in, inWriter := io.Pipe()
	out := &safeBuf{}
	program = tea.NewProgram(New(s), tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(100, 30))
	s.Start()

	finished := make(chan struct{})
	go func() {
		_, _ = program.Run()
		close(finished)
	}()

	waitFor(t, out, "›", "the prompt never rendered")

	if strings.Contains(ansi.Strip(out.String()), "ctx ") {
		t.Fatal("the context gauge showed a figure before any request was answered")
	}

	_, _ = inWriter.Write([]byte("hello\r"))
	waitFor(t, out, "counted", "the model's reply never reached the screen")

	// 52k of a 200k window: the prompt's cached and fresh tokens plus the output.
	waitFor(t, out, "ctx 26% (52k/200k)", "the context gauge never reached the status line")

	program.Quit()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit")
	}
}

// Mouse reporting is a claim about bytes on the wire — DECSET 1002 on startup, and never the
// matching reset — so it is tested over the wire rather than by reading View().MouseMode. The
// old ctrl+m toggle is checked dead here too: the key is the carriage return in the legacy
// encoding, so a stale binding would be reachable as plain enter on a terminal without kitty
// disambiguation.
func TestTheMouseIsTakenOnStartupAndNeverGivenBack(t *testing.T) {
	server := mockapi.New(mockapi.Turn{Text: "enter still submits"})
	defer server.Close()
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	a := &agent.Agent{Client: &client, Model: "mock", System: "test", Tools: agent.BuiltinTools, Standing: agent.NewRuleInjector("")}

	var program *tea.Program
	s := session.New(a, func() {
		if program != nil {
			program.Send(sessionChanged{})
		}
	})

	in, inWriter := io.Pipe()
	out := &safeBuf{}
	program = tea.NewProgram(New(s), tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(100, 30))
	s.Start()

	finished := make(chan struct{})
	go func() {
		_, _ = program.Run()
		close(finished)
	}()

	waitFor(t, out, "›", "the prompt never rendered")

	waitForRaw(t, out, enableMouse, "the pane never asked the terminal for mouse reporting")

	// ctrl+m arrives as CSI 109;5u where kitty disambiguation is honoured, and as a bare \r
	// where it is not. Neither may give the mouse back, and the legacy spelling must still
	// submit — a reply can only arrive if the prompt was sent.
	_, _ = inWriter.Write([]byte("\x1b[109;5u"))
	_, _ = inWriter.Write([]byte("say something\r"))
	waitFor(t, out, "enter still submits", "enter did not submit the prompt")

	if strings.Contains(out.String(), disableMouse) {
		t.Error("something gave mouse reporting back mid-session; there is only one mode now")
	}

	program.Quit()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit")
	}
}

// What a terminal is told when the pane takes the mouse, and gives it back.
const (
	enableMouse  = "\x1b[?1002h"
	disableMouse = "\x1b[?1002l"
)

// Dragging to select and copy is a claim about bytes in both directions: SGR mouse reports in,
// an OSC 52 clipboard write out. Both ends are asserted over the wire, through the real parser,
// because nothing else proves that a drag's motion report actually carries the held button —
// the unit tests construct MouseMotionMsg themselves and so assume exactly what is in question.
func TestDraggingSelectsAndCopiesOverTheWire(t *testing.T) {
	server := mockapi.New(mockapi.Turn{Text: "pick me up"})
	defer server.Close()
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	a := &agent.Agent{Client: &client, Model: "mock", System: "test", Tools: agent.BuiltinTools, Standing: agent.NewRuleInjector("")}

	var program *tea.Program
	s := session.New(a, func() {
		if program != nil {
			program.Send(sessionChanged{})
		}
	})

	in, inWriter := io.Pipe()
	out := &safeBuf{}
	program = tea.NewProgram(New(s), tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(100, 30))
	s.Start()

	finished := make(chan struct{})
	go func() {
		_, _ = program.Run()
		close(finished)
	}()

	waitFor(t, out, "›", "the prompt never rendered")
	s.PushAssistantForTest("selectable text here")
	waitFor(t, out, "selectable text here", "the line to select never rendered")

	// Coordinates cannot be read back off the output stream — bubbletea repaints incrementally,
	// so the stream is frames, not a screen. They are derived from the layout instead: with no
	// top bar (no usage yet) the transcript starts at the first screen row, and a transcript
	// shorter than the viewport sits at its top, so the only line is row 1 in SGR's 1-based
	// coordinates. The one column of transcript indent puts its first character at column 2.
	const row = 1
	const firstCol = 2
	const tenthCol = firstCol + 9

	// Button 0 is the left button; bit 32 marks a motion report, which is how a drag is spelled.
	// The release repeats the button with a lowercase terminator.
	press := fmt.Sprintf("\x1b[<0;%d;%dM", firstCol, row)
	drag := fmt.Sprintf("\x1b[<32;%d;%dM", tenthCol, row)
	release := fmt.Sprintf("\x1b[<0;%d;%dm", tenthCol, row)
	_, _ = inWriter.Write([]byte(press + drag + release))

	// OSC 52 to the system clipboard, carrying the selected text base64-encoded.
	want := ansi.SetSystemClipboard("selectable")
	waitForRaw(t, out, want, "the selection was not written to the clipboard")

	program.Quit()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit")
	}
}

// waitForRaw is waitFor over the unstripped stream, for asserting on escape sequences rather
// than on what they draw.
func waitForRaw(t *testing.T, out *safeBuf, want, whenMissing string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s (waiting for %q on the wire)", whenMissing, want)
}

func waitFor(t *testing.T, out *safeBuf, want, whenMissing string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(ansi.Strip(out.String()), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s (waiting for %q)\n%s", whenMissing, want, tail(ansi.Strip(out.String())))
}

func tail(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}
