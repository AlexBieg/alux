package ui

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/desktop"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/report"
	"github.com/alexbieg/alux/internal/resolve"
	"github.com/alexbieg/alux/internal/reviewgraph"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/alexbieg/alux/internal/workers"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/charmbracelet/x/ansi"
)

// The model is driven directly rather than through a pty. Everything worth asserting here is a
// pure function of the snapshot and the messages, and a pty rig would only add flakiness.
func harness(t *testing.T, width, height int) (*Model, *session.Session) {
	t.Helper()
	s := session.New(&agent.Agent{}, nil)
	m := New(s)
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return m, s
}

func roster(m *Model) *subagent.Roster { return m.session.(*session.Session).Roster }

func screen(m *Model) string {
	return ansi.Strip(m.View().Content)
}

func send(m *Model, msg tea.Msg) {
	m.Update(msg)
}

func settle(m *Model) {
	m.Update(sessionChanged{})
}

// The mouse gestures, spelled as the terminal delivers them: a press, a motion while held, a
// release. x defaults to 3 where a test only cares which row it hit.

func click(m *Model, row int) {
	clickAt(m, 3, row)
}

func clickAt(m *Model, x, row int) {
	send(m, tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: row})
}

func releaseAt(m *Model, x, row int) {
	send(m, tea.MouseReleaseMsg{Button: tea.MouseLeft, X: x, Y: row})
}

// expand is the click that toggles a box: a press and a release in the same place.
func expand(t *testing.T, m *Model, row int) {
	t.Helper()
	if _, ok := m.pointAt(3, row); !ok {
		t.Fatalf("row %d is not in the transcript", row)
	}
	click(m, row)
	releaseAt(m, 3, row)
}

// dragOver selects exactly want on the given row.
func dragOver(t *testing.T, m *Model, row int, want string) {
	t.Helper()
	x := screenXOf(t, m, row, want)
	dragSelect(m, x, row, x+lipgloss.Width(want)-1, row)
}

// dragSelect presses, drags and releases, which is how a range is selected and copied.
func dragSelect(m *Model, x1, y1, x2, y2 int) {
	clickAt(m, x1, y1)
	send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: x2, Y: y2})
	releaseAt(m, x2, y2)
}

// screenXOf is the screen column the given text starts at on a row, so a test can click on a
// word rather than count cells by hand. Measured in display cells, not bytes.
func screenXOf(t *testing.T, m *Model, row int, want string) int {
	t.Helper()
	rows := strings.Split(screen(m), "\n")
	if row < 0 || row >= len(rows) {
		t.Fatalf("row %d is off screen", row)
	}
	i := strings.Index(rows[row], want)
	if i < 0 {
		t.Fatalf("row %d does not contain %q: %q", row, want, rows[row])
	}
	return lipgloss.Width(rows[row][:i])
}

// pointLineAt is the transcript line a screen row maps to, for tests that need to name a box
// without reimplementing the mapping.
func (m *Model) pointLineAt(t *testing.T, row int) int {
	t.Helper()
	pt, ok := m.pointAt(3, row)
	if !ok {
		t.Fatalf("row %d is not in the transcript", row)
	}
	return pt.line
}

// selectedTextForTest is what a copy would put on the clipboard right now. The clipboard itself
// is a terminal escape sequence, so the text is asserted here and the escape sequence is
// asserted once, over the wire, in the e2e test.
func (m *Model) selectedTextForTest() string {
	if m.sel.empty() {
		return ""
	}
	from, to := m.sel.span()
	return m.content.selectedText(from, to)
}

func typeText(m *Model, text string) {
	for _, r := range text {
		send(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// Reported by a user: what they typed appeared on the line *below* the prompt marker. glamour's
// stock document style opens every block with a blank line and indents it two columns, which is
// right for rendering a document and wrong for a line that has to sit beside a marker.
func TestUserPromptSitsBesideItsMarker(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.PushUserForTest("hello there")
	settle(m)

	for _, line := range strings.Split(screen(m), "\n") {
		if !strings.Contains(line, "›") {
			continue
		}
		if !strings.Contains(line, "hello there") {
			t.Errorf("the marker line does not carry the text: %q", line)
		}
		return
	}
	t.Fatalf("no marker found:\n%s", screen(m))
}

// The same defect one line down: a prompt long enough to wrap must keep its continuation under
// the text rather than under the marker.
func TestWrappedPromptKeepsItsHangingIndent(t *testing.T) {
	m, s := harness(t, 40, 24)
	s.PushUserForTest(strings.Repeat("wrap ", 30))
	settle(m)

	var body []string
	for _, line := range strings.Split(screen(m), "\n") {
		if strings.Contains(line, "wrap") {
			body = append(body, unframe(line))
		}
	}
	if len(body) < 2 {
		t.Fatalf("prompt did not wrap:\n%s", screen(m))
	}
	if !strings.HasPrefix(body[0], "› ") {
		t.Fatalf("the prompt's first line does not open with its marker: %q", body[0])
	}
	for i, line := range body[1:] {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			t.Errorf("continuation line %d is not indented under the text: %q", i+1, line)
		}
	}
}

// Reported by a user: what they typed had a black background. textarea highlights the cursor's line
// with a background colour, which suits a multi-line editor and paints the prompt in a chat box.
func TestTypedTextHasNoBackground(t *testing.T) {
	m, _ := harness(t, 60, 12)
	for _, r := range "hello" {
		send(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	for _, line := range strings.Split(m.View().Content, "\n") {
		if !strings.Contains(ansi.Strip(line), "hello") {
			continue
		}
		if setsBackground(line) {
			t.Errorf("the typed line carries a background: %q", line)
		}
		return
	}
	t.Fatalf("typed text never rendered:\n%s", ansi.Strip(m.View().Content))
}

var sgrPattern = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// setsBackground reports whether a line contains an SGR sequence that sets a background colour:
// 40-47 legacy, 100-107 bright, or 48 followed by its own arguments.
//
// Those arguments are why this is a parser and not a pattern. An extended *foreground* is
// 38;5;n, and for n in 40..47 a naive search finds a number in the background range inside a
// sequence that sets no background at all.
func setsBackground(line string) bool {
	return len(backgroundsOf(line)) > 0
}

// backgroundsOf is every background a line sets, each as its SGR parameters, e.g. "48;2;37;45;67".
func backgroundsOf(line string) []string {
	var out []string
	for _, match := range sgrPattern.FindAllStringSubmatch(line, -1) {
		params := strings.Split(match[1], ";")
		for i := 0; i < len(params); i++ {
			n, err := strconv.Atoi(params[i])
			if err != nil {
				continue
			}
			args := 0
			if (n == 38 || n == 48) && i+1 < len(params) {
				switch params[i+1] {
				case "5":
					args = 2
				case "2":
					args = 4
				}
			}
			switch {
			case n == 48:
				out = append(out, strings.Join(params[i:min(len(params), i+1+args)], ";"))
			case n >= 40 && n <= 47, n >= 100 && n <= 107:
				out = append(out, params[i])
			}
			// Skip an extended colour's arguments so they are not read as codes.
			i += args
		}
	}
	return out
}

// unframe is a row with any bar and the indent before it taken off.
func unframe(row string) string {
	if i := strings.Index(row, "│ "); i >= 0 && strings.TrimSpace(row[:i]) == "" {
		row = strings.TrimRight(row[i+len("│ "):], " ")
	}
	return row
}

// The terminal's own cursor is used rather than textarea's drawn one, which means the position
// handed to bubbletea has to be right — and textarea reports nil while its virtual cursor is on,
// so this also catches that being switched back by accident.
func TestCursorSitsAfterTheTypedText(t *testing.T) {
	m, _ := harness(t, 60, 12)
	for _, r := range "abc" {
		send(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	cursor := m.View().Cursor
	if cursor == nil {
		t.Fatal("no cursor reported; textarea's virtual cursor is masking the real one")
	}
	// Frame margin, border and padding, then the prompt marker, then what has been typed.
	wantX := 3 + lipgloss.Width("› ") + 3
	if cursor.X != wantX {
		t.Errorf("cursor X = %d, want %d", cursor.X, wantX)
	}
	// The single input row: above the frame's lower border, which carries the status line.
	wantY := 12 - 2
	if cursor.Y != wantY {
		t.Errorf("cursor Y = %d, want %d", cursor.Y, wantY)
	}
}

// Tab inserts the top row and a space, which opens the next argument's list in place of the old one.
func TestTabCompletesTheTopRowAndOpensTheNextLevel(t *testing.T) {
	m, _ := harness(t, 80, 16)
	for _, r := range "/va" {
		send(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	send(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if got := m.input.Value(); got != "/vault " {
		t.Fatalf("after tab the box holds %q, want %q", got, "/vault ")
	}
	if !strings.Contains(screen(m), "rebuild") {
		t.Fatalf("the argument list did not open:\n%s", screen(m))
	}
	send(m, tea.KeyPressMsg{Code: 'a', Text: "a"})
	send(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if got := m.input.Value(); got != "/vault aliases " {
		t.Errorf("second tab left %q, want %q", got, "/vault aliases ")
	}
}

// Reported by a user: typing a slash command moved the input box up but left the cursor behind.
// The completion list has to grow upwards, which keeps the box — and the cursor's row — fixed.
func TestCommandListAppearsAboveTheInput(t *testing.T) {
	m, _ := harness(t, 60, 12)
	for _, r := range "/cl" {
		send(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	lines := strings.Split(screen(m), "\n")
	list, input := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "/clear") {
			list = i
		}
		if strings.Contains(line, "›") {
			input = i
		}
	}
	if list < 0 {
		t.Fatalf("no completion shown for a typed command:\n%s", screen(m))
	}
	if input < 0 {
		t.Fatalf("no input row found:\n%s", screen(m))
	}
	if list > input {
		t.Errorf("completion is on row %d, below the input on row %d", list, input)
	}
	if input != 12-2 {
		t.Errorf("input row = %d, want %d: the box moved", input, 12-2)
	}

	cursor := m.View().Cursor
	if cursor == nil {
		t.Fatal("no cursor reported")
	}
	if cursor.Y != input {
		t.Errorf("cursor Y = %d, but the input is on row %d", cursor.Y, input)
	}
}

// rowOf finds the screen row a piece of text is drawn on.
func rowOf(t *testing.T, m *Model, want string) int {
	t.Helper()
	for i, line := range strings.Split(screen(m), "\n") {
		if strings.Contains(line, want) {
			return i
		}
	}
	t.Fatalf("%q is not on screen:\n%s", want, screen(m))
	return -1
}

// The box stays while a turn runs, and what is typed into it is queued.
func TestTheInputBoxStaysWhileTheAgentWorks(t *testing.T) {
	m, s := harness(t, 60, 14)
	m.snap.Busy = true
	m.layout()

	typeText(m, "a follow-up")
	if !strings.Contains(screen(m), "a follow-up") {
		t.Errorf("typing during a turn is invisible:\n%s", screen(m))
	}

	send(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.input.Value() != "" {
		t.Errorf("enter did not clear the box: %q", m.input.Value())
	}
	queued := s.Snapshot().Queued
	if len(queued) != 1 || queued[0].Text != "a follow-up" {
		t.Fatalf("enter did not queue the prompt: %v", queued)
	}

	if strings.Contains(screen(m), "working…") == false {
		t.Errorf("nothing says a turn is running:\n%s", screen(m))
	}
}

// A click maps back to a prompt by counting rows, so the row the list draws one on and the row
// the hit test resolves it from have to agree. Taken from the rendered screen rather than
// computed, since an arithmetic guess here would agree with a wrong implementation.
func TestClickingAQueuedPromptResolvesToIt(t *testing.T) {
	m, _ := harness(t, 60, 16)
	m.snap.Busy, m.snap.Steerable = true, true
	m.snap.Steering = []session.Queued{{ID: 1, Text: "already on its way"}}
	m.snap.Queued = []session.Queued{{ID: 7, Text: "first waiting"}, {ID: 8, Text: "second waiting"}}
	m.layout()

	for _, want := range []struct {
		text string
		id   int
	}{{"first waiting", 7}, {"second waiting", 8}} {
		row := rowOf(t, m, want.text)
		got, ok := m.queuedAt(row)
		if !ok {
			t.Errorf("row %d draws %q but resolves to no queued prompt", row, want.text)
			continue
		}
		if got != want.id {
			t.Errorf("row %d draws %q but resolves to id %d, want %d", row, want.text, got, want.id)
		}
	}

	// Already sent, so there is nothing a click on it could do.
	if _, ok := m.queuedAt(rowOf(t, m, "already on its way")); ok {
		t.Error("a message already sent into the turn is offered for sending again")
	}
	// And nothing outside the list is in it.
	if _, ok := m.queuedAt(rowOf(t, m, "›")); ok {
		t.Error("the input row resolves to a queued prompt")
	}
}

// The queue can hold more than the pane has rows for, and the input box must survive it. The
// hit test has to clip with the list, or a click would resolve to a prompt that is not drawn.
func TestALongQueueIsCappedAndKeepsTheInputBox(t *testing.T) {
	m, _ := harness(t, 60, 14)
	m.snap.Busy, m.snap.Steerable = true, true
	for i := range 20 {
		m.snap.Queued = append(m.snap.Queued, session.Queued{ID: i, Text: fmt.Sprintf("prompt %d", i)})
	}
	m.layout()

	if !strings.Contains(screen(m), "›") {
		t.Fatalf("a long queue pushed the input box off the pane:\n%s", screen(m))
	}
	if !strings.Contains(screen(m), fmt.Sprintf("… %d more", 20-queueShown)) {
		t.Errorf("the pane does not say how much it is not showing:\n%s", screen(m))
	}
	if strings.Contains(screen(m), fmt.Sprintf("prompt %d", queueShown)) {
		t.Errorf("more than %d queued prompts are drawn:\n%s", queueShown, screen(m))
	}

	// The last drawn row resolves; nothing past it does.
	last := rowOf(t, m, fmt.Sprintf("prompt %d", queueShown-1))
	if id, ok := m.queuedAt(last); !ok || id != queueShown-1 {
		t.Errorf("last drawn row resolved to (%d, %v)", id, ok)
	}
	if _, ok := m.queuedAt(last + 1); ok {
		t.Error("the overflow row resolves to a queued prompt")
	}
}

// A slash command runs locally, so steering one into a turn would send the model the literal
// text. It queues like anything else but is not offered for sending.
func TestAQueuedCommandIsNotClickable(t *testing.T) {
	m, s := harness(t, 60, 16)
	s.Submit("/context")
	s.Submit("a real prompt")
	settle(m)
	m.snap.Busy, m.snap.Steerable = true, true
	m.layout()

	if _, ok := m.queuedAt(rowOf(t, m, "/context")); ok {
		t.Error("a queued slash command is offered for steering")
	}
	if _, ok := m.queuedAt(rowOf(t, m, "a real prompt")); !ok {
		t.Error("a queued prompt beside a command stopped being clickable")
	}
}

// Steering is not on offer when there is no turn to fold it into, however the pane is drawn.
func TestAQueuedPromptIsNotClickableWithNoTurnRunning(t *testing.T) {
	m, _ := harness(t, 60, 16)
	m.snap.Queued = []session.Queued{{ID: 3, Text: "just waiting"}}
	m.layout()

	if _, ok := m.queuedAt(rowOf(t, m, "just waiting")); ok {
		t.Error("a queued prompt is clickable with no turn to steer")
	}
}

func TestShiftEnterInsertsANewlineInsteadOfSubmitting(t *testing.T) {
	m, s := harness(t, 60, 14)
	typeText(m, "first")
	send(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	typeText(m, "second")

	if got := m.input.Value(); got != "first\nsecond" {
		t.Errorf("input = %q, want %q", got, "first\nsecond")
	}
	if queued := s.Snapshot().Queued; len(queued) != 0 {
		t.Errorf("shift+enter submitted: %v", queued)
	}
}

func TestBackspaceOnAnEmptyBoxTakesBackTheLastQueuedPrompt(t *testing.T) {
	m, s := harness(t, 60, 14)
	s.Submit("first")
	s.Submit("second")
	settle(m)

	send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.input.Value(); got != "second" {
		t.Errorf("input = %q, want the last queued prompt back", got)
	}
	if queued := s.Snapshot().Queued; len(queued) != 1 || queued[0].Text != "first" {
		t.Errorf("queue = %v, want only the first prompt left", queued)
	}

	// With something in the box, backspace is editing again.
	send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.input.Value(); got != "secon" {
		t.Errorf("input = %q: backspace stopped editing the box", got)
	}
	if queued := s.Snapshot().Queued; len(queued) != 1 {
		t.Errorf("backspace took another prompt off the queue: %v", queued)
	}
}

// Reported by a user: a line long enough to wrap left the box one row tall, so only the row being
// typed on was visible. The box has to grow upwards instead of scrolling inside itself.
func TestWrappedInputGrowsTheBox(t *testing.T) {
	m, _ := harness(t, 40, 14)
	typeText(m, "the first word and enough of the rest to force a wrap")

	if m.input.Height() < 2 {
		t.Fatalf("the box stayed %d row(s) tall:\n%s", m.input.Height(), screen(m))
	}
	if !strings.Contains(screen(m), "the first word") {
		t.Errorf("the start of the line scrolled out of the box:\n%s", screen(m))
	}
	if cursor := m.View().Cursor; cursor.Y != 14-2 {
		t.Errorf("cursor Y = %d, want %d: it is not on the last row of the box", cursor.Y, 14-2)
	}
}

// Reported by a user: at the end of a full row the cursor was sometimes drawn on the frame's
// border instead of wrapping with the text. It belongs between the borders at every length.
func TestCursorStaysInsideTheInputBox(t *testing.T) {
	m, _ := harness(t, 40, 14)
	for i := range 80 {
		send(m, tea.KeyPressMsg{Code: 'x', Text: "x"})
		c := m.View().Cursor
		left, right := boxEdges(t, m, c.Y)
		if c.X <= left || c.X >= right {
			t.Fatalf("after %d characters the cursor is at column %d, outside the box's %d..%d:\n%s",
				i+1, c.X, left, right, screen(m))
		}
	}
}

// boxEdges returns the columns of the input frame's own borders on the given row.
func boxEdges(t *testing.T, m *Model, row int) (left, right int) {
	t.Helper()
	lines := strings.Split(screen(m), "\n")
	if row < 0 || row >= len(lines) {
		t.Fatalf("row %d is off the %d-row screen", row, len(lines))
	}
	left, right = -1, -1
	for i, r := range []rune(lines[row]) {
		if r == '\u2502' {
			if left < 0 {
				left = i
			}
			right = i
		}
	}
	if left < 0 || left == right {
		t.Fatalf("row %d is not a row of the input box: %q", row, lines[row])
	}
	return left, right
}

// A tool box whose output runs past the collapse threshold shows its command and hides its output until asked.
func TestLongToolOutputIsCollapsedUntilOpened(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("ready")
	pushTool(s, "$ echo hi", numberedLines(12))
	settle(m)

	out := screen(m)
	if !strings.Contains(out, "$ echo hi") {
		t.Fatalf("command not shown:\n%s", out)
	}
	if strings.Contains(out, "line 12") {
		t.Errorf("body should be collapsed:\n%s", out)
	}
	if !strings.Contains(out, "· 12 lines") {
		t.Errorf("line count missing:\n%s", out)
	}

	expand(t, m, rowContaining(t, m, "$ echo hi"))
	if out := screen(m); !strings.Contains(out, "line 12") {
		t.Errorf("body should be open after a click:\n%s", out)
	}
}

// A body of exactly the collapse threshold starts open, with no line count on its head.
func TestAToolBodyAtTheThresholdStartsOpen(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", numberedLines(session.BoxCollapseLines))
	settle(m)

	out := screen(m)
	if !strings.Contains(out, "line 5") {
		t.Errorf("a five-line body should start open:\n%s", out)
	}
	if strings.Contains(out, "5 lines") {
		t.Errorf("an open box is offering a line count:\n%s", out)
	}
	if !strings.Contains(ansi.Strip(rowWith(t, m, "$ echo hi")), "▾") {
		t.Errorf("the marker of an open box is not the open one:\n%s", out)
	}
	if len(m.expanded) != 0 {
		t.Errorf("a default was recorded as a choice: %v", m.expanded)
	}
}

// One line past the threshold starts the box collapsed, with the count on its head.
func TestAToolBodyPastTheThresholdStartsCollapsed(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", numberedLines(session.BoxCollapseLines+1))
	settle(m)

	out := screen(m)
	if strings.Contains(out, "line 6") {
		t.Errorf("a six-line body should start collapsed:\n%s", out)
	}
	if !strings.Contains(out, "· 6 lines") {
		t.Errorf("line count missing:\n%s", out)
	}
	if !strings.Contains(ansi.Strip(rowWith(t, m, "$ echo hi")), "▸") {
		t.Errorf("the marker of a collapsed box is not the closed one:\n%s", out)
	}
}

// Clicking a box that started open closes it, and clicking it again opens it back up.
func TestClickingAShortOpenBoxCollapsesIt(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", "hi\nthere\nfriend")
	settle(m)
	if !strings.Contains(screen(m), "friend") {
		t.Fatalf("a short body should start open:\n%s", screen(m))
	}

	expand(t, m, rowContaining(t, m, "$ echo hi"))
	out := screen(m)
	if strings.Contains(out, "friend") {
		t.Errorf("clicking an open box did not collapse it:\n%s", out)
	}
	if !strings.Contains(out, "· 3 lines") {
		t.Errorf("a box the user closed has no line count:\n%s", out)
	}
	if got, ok := m.expanded[m.snap.Items[0].ID]; !ok || got {
		t.Errorf("the closing click was not recorded as a choice to close: %v", m.expanded)
	}

	expand(t, m, rowContaining(t, m, "$ echo hi"))
	if out := screen(m); !strings.Contains(out, "friend") {
		t.Errorf("a second click did not reopen the box:\n%s", out)
	}
}

// A head longer than the screen wraps instead of running off the right edge, and the line
// count — the one thing a collapsed box exists to tell you — survives the wrap.
func TestALongBoxHeadWraps(t *testing.T) {
	m, s := harness(t, 60, 24)
	pushTool(s, "$ echo "+strings.Repeat("abcdefgh ", 12)+"tail", numberedLines(11))
	settle(m)

	out := screen(m)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 60 {
			t.Fatalf("row is %d columns wide on a 60-column screen: %q", w, line)
		}
	}
	if !strings.Contains(out, "tail") {
		t.Errorf("the end of the command never made it onto the screen:\n%s", out)
	}
	if !strings.Contains(out, "· 11 lines") {
		t.Errorf("line count missing:\n%s", out)
	}
}

// Continuation lines hang under the head's text, so a wrapped command reads as one block rather
// than as a second box that lost its marker.
func TestAWrappedHeadHangsUnderItsFirstLine(t *testing.T) {
	m, s := harness(t, 40, 24)
	pushTool(s, "$ echo "+strings.Repeat("abcdefgh ", 6)+"tail", numberedLines(11))
	settle(m)

	rows := strings.Split(screen(m), "\n")
	first := rowContaining(t, m, "▸ $ echo")
	marker := strings.Index(unframe(rows[first]), "▸")
	for _, row := range rows[first+1:] {
		if strings.TrimSpace(row) == "" {
			break
		}
		row = unframe(row)
		if got := len(row) - len(strings.TrimLeft(row, " ")); got != marker+2 {
			t.Errorf("continuation indented to column %d, want %d: %q", got, marker+2, row)
		}
	}
}

// A wrapped head is still one box: clicking its second line opens the body, the same as
// clicking the line with the marker on it.
func TestClickingAWrappedHeadLineExpandsTheBox(t *testing.T) {
	m, s := harness(t, 60, 24)
	pushTool(s, "$ echo "+strings.Repeat("abcdefgh ", 12)+"tail", hiddenBody)
	settle(m)

	expand(t, m, rowContaining(t, m, "tail"))

	if out := screen(m); !strings.Contains(out, "hidden body") {
		t.Errorf("clicking the wrapped line did not expand the box:\n%s", out)
	}
}

// The interaction the port was motivated by. A click lands on the box under the cursor with no
// escape-sequence parsing and no layout measurement — bubbletea gives cell coordinates and the
// row-to-item map is built during rendering.
func TestClickingABoxExpandsIt(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", numberedLines(11))
	// With the top bar showing, which moves every transcript row down one.
	s.SetUsageForTest(agent.Usage{Prompt: 1_000, Output: 100, Window: 200_000})
	settle(m)

	expand(t, m, rowContaining(t, m, "$ echo hi"))

	out := screen(m)
	if !strings.Contains(out, "line 11") {
		t.Errorf("click did not expand the box:\n%s", out)
	}
	if !strings.Contains(out, "▾") {
		t.Errorf("marker did not flip to open:\n%s", out)
	}

	expand(t, m, rowContaining(t, m, "$ echo hi"))
	if strings.Contains(screen(m), "line 11") {
		t.Error("clicking again did not collapse the box")
	}
}

// The box toggles the moment the button comes up. The press alone does nothing, because it may
// still become a drag.
func TestExpansionHappensOnRelease(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", hiddenBody)
	settle(m)

	row := rowContaining(t, m, "$ echo hi")
	click(m, row)
	if len(m.expanded) != 0 {
		t.Errorf("the press expanded the box before the release: %v", m.expanded)
	}

	_, cmd := m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 3, Y: row})
	if cmd != nil {
		t.Error("the release scheduled work instead of toggling at once")
	}
	if out := screen(m); !strings.Contains(out, "hidden body") {
		t.Errorf("the release did not expand the box:\n%s", out)
	}
}

// A drag selects a range across rows, and the release copies it. Asking the terminal for the
// mouse takes its own selection away, so the pane owes the user this itself.
func TestDraggingSelectsARangeAcrossRows(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("first line")
	s.Show("second line")
	settle(m)

	top := rowContaining(t, m, "first line")
	bottom := rowContaining(t, m, "second line")
	dragSelect(m, screenXOf(t, m, top, "first"), top, screenXOf(t, m, bottom, "line")+3, bottom)

	got := m.selectedTextForTest()
	if !strings.HasPrefix(got, "first line") || !strings.HasSuffix(got, "line") {
		t.Errorf("drag selected %q, want it to run from \"first line\" to the end of \"line\"", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("a drag across rows produced one line: %q", got)
	}
	if out := screen(m); !strings.Contains(out, "copied") {
		t.Errorf("the release did not report a copy:\n%s", out)
	}
}

// A press that turns into a drag was a selection, not a click, so the box it started in must not
// open under the selection.
func TestADragDoesNotExpandTheBoxItStartedIn(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", hiddenBody)
	settle(m)

	row := rowContaining(t, m, "$ echo hi")
	dragSelect(m, 3, row, 9, row)

	if len(m.expanded) != 0 {
		t.Errorf("a drag expanded the box it started in: %v", m.expanded)
	}
	if m.sel.empty() {
		t.Error("the drag did not select anything")
	}
}

// The selection has to be visible, which means a background painted under the selected cells and
// nowhere else.
func TestTheSelectionIsPaintedOnScreen(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("alpha beta gamma")
	settle(m)

	row := rowContaining(t, m, "alpha beta gamma")
	if setsBackground(rowWith(t, m, "alpha beta gamma")) {
		t.Fatal("the row had a background before anything was selected")
	}

	dragOver(t, m, row, "beta")
	m.notices = nil

	if !setsBackground(rowWith(t, m, "alpha beta gamma")) {
		t.Errorf("the selected row is not painted:\n%q", rowWith(t, m, "alpha beta gamma"))
	}
}

// The highlight is drawn on the way to the screen and never stored in the transcript, so a copy
// reads the text and not the decoration laid over it.
func TestCopyingDoesNotPickUpItsOwnHighlight(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("alpha beta gamma")
	settle(m)

	row := rowContaining(t, m, "alpha beta gamma")
	dragOver(t, m, row, "beta")

	if got := m.selectedTextForTest(); got != "beta" {
		t.Errorf("copied %q, want \"beta\"", got)
	}
	for i, line := range m.content.lines {
		if setsBackground(line) {
			t.Errorf("transcript line %d was mutated by the highlight: %q", i, line)
		}
	}
}

// esc is the obvious way to dismiss a selection, and while one is up it must not also cancel the
// turn — two things on one key, with the selection taking precedence because it is on screen.
func TestEscClearsTheSelectionBeforeCancelling(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("alpha beta gamma")
	settle(m)

	row := rowContaining(t, m, "alpha beta gamma")
	dragOver(t, m, row, "beta")
	m.notices = nil
	if m.sel.empty() {
		t.Fatal("nothing was selected to clear")
	}

	send(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if !m.sel.empty() {
		t.Error("esc did not clear the selection")
	}
	if setsBackground(rowWith(t, m, "alpha beta gamma")) {
		t.Error("the highlight survived the selection being cleared")
	}
}

// Clicking anywhere drops the previous selection — this is what keeps a stale highlight off the
// screen, and it is also why toggling a box does not have to clear the selection itself.
func TestAPressDropsThePreviousSelection(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("alpha beta gamma")
	settle(m)

	row := rowContaining(t, m, "alpha beta gamma")
	dragOver(t, m, row, "beta")
	m.notices = nil
	if m.sel.empty() {
		t.Fatal("nothing was selected")
	}

	clickAt(m, screenXOf(t, m, row, "gamma"), row)

	if !m.sel.empty() {
		t.Errorf("a new press left the old selection in place: %+v", m.sel)
	}
	if setsBackground(rowWith(t, m, "alpha beta gamma")) {
		t.Error("the old highlight is still painted")
	}
}

// A press outside the transcript — on the prompt, say — also drops the selection, because
// clicking away from a selection is how every other application dismisses one.
func TestPressingOutsideTheTranscriptClearsTheSelection(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("alpha beta gamma")
	settle(m)

	row := rowContaining(t, m, "alpha beta gamma")
	dragOver(t, m, row, "beta")
	if m.sel.empty() {
		t.Fatal("nothing was selected")
	}

	clickAt(m, 3, m.height-2) // the input frame, below the transcript

	if !m.sel.empty() {
		t.Errorf("a press on the chrome left the selection up: %+v", m.sel)
	}
}

// Reporting is all-or-nothing per terminal, and alux keeps it on permanently: there is no
// mode to be in the wrong one of. The status line names both gestures, because an invisible
// mouse convention is the thing that made the old toggle annoying.
func TestMouseReportingIsAlwaysOn(t *testing.T) {
	m, _ := harness(t, 80, 24)

	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("a fresh pane did not ask for the mouse: mode = %v", got)
	}

	// Nothing may move it — least of all ctrl+m, the key a toggle would most plausibly sit on.
	send(m, tea.KeyPressMsg{Code: 'm', Mod: tea.ModCtrl})
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("ctrl+m still changes the mouse mode: mode = %v", got)
	}
}

// The user's call: the tab selection cursor is gone, so expanding a box is a click and nothing else.
// Tab must not walk the boxes, must not leave a marker highlighted — and must not have become a
// character in the prompt now that the key is no longer intercepted.
func TestTabNoLongerReachesTheBoxes(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", hiddenBody)
	settle(m)

	send(m, tea.KeyPressMsg{Code: tea.KeyTab})
	send(m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if strings.Contains(screen(m), "hidden body") {
		t.Error("tab then enter still expanded a box")
	}
	if got := m.input.Value(); got != "" {
		t.Errorf("tab fell through into the prompt as %q", got)
	}
}

// /context lays its numbers out in coloured columns before the pane ever sees them. Markdown
// treats those lines as one paragraph and rewraps them into prose, which ran every count
// together — so a report has to reach the screen exactly as it was built, colour and all.
func TestAColumnedReportIsNotRewrappedIntoProse(t *testing.T) {
	m, s := harness(t, 80, 24)
	report := agent.Report{
		Rows:  []agent.Row{{Label: "system prompt", Tokens: 12_982}, {Label: "request framing", Tokens: 7}},
		Total: 12_989, Window: 200_000,
	}.String()
	s.ShowBlock(report)
	settle(m)

	stripped := screen(m)
	for _, line := range strings.Split(ansi.Strip(report), "\n") {
		if !strings.Contains(stripped, line) {
			t.Errorf("the line %q was reflowed away:\n%s", line, stripped)
		}
	}
	// The bars are the only colour in the report, and a pane that strips or rewrites the escapes
	// would leave the layout intact while flattening every bar to one shade.
	if !strings.Contains(m.View().Content, "\x1b[38;2;") {
		t.Error("the bar colours did not survive the pane")
	}
}

// A click on prose must be inert: prose has no body to show, so a click near one must not toggle
// whatever box happens to be nearby.
func TestClickingProseIsInert(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.Show("just some prose")
	pushTool(s, "$ echo hi", hiddenBody)
	settle(m)

	expand(t, m, rowContaining(t, m, "just some prose"))

	if strings.Contains(screen(m), "hidden body") {
		t.Error("a click on prose expanded a box")
	}
	if len(m.expanded) != 0 {
		t.Errorf("a click on prose recorded an expansion: %v", m.expanded)
	}
}

// Expansion is UI state, never session state: two panes mirroring one conversation must be able
// to disagree about what is open.
func TestExpansionIsNotInTheSnapshot(t *testing.T) {
	m, s := harness(t, 80, 24)
	pushTool(s, "$ echo hi", hiddenBody)
	settle(m)
	expand(t, m, rowContaining(t, m, "$ echo hi"))

	other := New(s)
	other.Init()
	other.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	settle(other)

	if strings.Contains(ansi.Strip(other.View().Content), "hidden body") {
		t.Error("a second view inherited the first one's expansion")
	}
}

// Scrolling up has to stop the transcript yanking itself back to the tail on the next delta.
func TestScrollingUpStopsFollowingUntilEnd(t *testing.T) {
	m, s := harness(t, 80, 12)
	for i := range 40 {
		s.Show(strings.Repeat("line ", 3) + string(rune('a'+i%26)))
	}
	settle(m)

	if !m.follow {
		t.Fatal("should start following the tail")
	}
	send(m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.follow {
		t.Error("wheel up should stop following")
	}
	offset := m.viewport.YOffset()

	s.Show("new output arrives")
	settle(m)
	if m.viewport.YOffset() != offset {
		t.Errorf("view jumped from %d to %d while scrolled up", offset, m.viewport.YOffset())
	}

	send(m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if !m.follow {
		t.Error("end should resume following")
	}
}

// A fast flick must cost one redraw a frame, not one per wheel event, and still land where every event put it.
func TestWheelBurstIsAppliedOnceAFrame(t *testing.T) {
	m, s := harness(t, 80, 12)
	for i := range 200 {
		s.Show(fmt.Sprintf("line %d", i))
	}
	settle(m)
	start := m.viewport.YOffset()

	if m.onWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp}) == nil {
		t.Fatal("the first wheel event of a burst scheduled no frame")
	}
	first := screen(m)
	if m.viewport.YOffset() != start-wheelStep {
		t.Fatalf("the first wheel event did not scroll at once: offset %d, want %d", m.viewport.YOffset(), start-wheelStep)
	}

	for range 10 {
		send(m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		if got := screen(m); got != first {
			t.Fatal("a banked wheel event redrew the view")
		}
	}
	send(m, tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.viewport.YOffset() != start-wheelStep {
		t.Fatal("a banked wheel event scrolled before its frame")
	}

	send(m, wheelFrameMsg{})
	if want := start - 10*wheelStep; m.viewport.YOffset() != want {
		t.Errorf("after the frame the offset is %d, want %d", m.viewport.YOffset(), want)
	}
	if screen(m) == first {
		t.Error("the frame did not redraw the view")
	}

	send(m, wheelFrameMsg{})
	send(m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if want := start - 11*wheelStep; m.viewport.YOffset() != want {
		t.Errorf("a wheel event after the burst settled was banked: offset %d, want %d", m.viewport.YOffset(), want)
	}
}

func TestScrolledUpShowsHowMuchIsBelowJustAboveTheInput(t *testing.T) {
	m, s := harness(t, 80, 12)
	for i := range 40 {
		s.Show("line " + string(rune('a'+i%26)))
	}
	settle(m)
	if strings.Contains(screen(m), "below") {
		t.Fatal("the indicator showed while following the tail")
	}

	send(m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	lines := strings.Split(screen(m), "\n")
	row := m.transcriptTop() + m.viewport.Height()
	if !strings.Contains(lines[row], "below · click or end to follow") {
		t.Fatalf("row %d above the input is %q:\n%s", row, lines[row], strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[row+1]), "╭") {
		t.Errorf("the indicator is not directly above the input frame: %q", lines[row+1])
	}

	send(m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if strings.Contains(screen(m), "below") {
		t.Error("the indicator outlived returning to the tail")
	}
}

func TestClickingTheScrolledIndicatorFollowsTheTail(t *testing.T) {
	m, s := harness(t, 80, 12)
	for i := range 40 {
		s.Show("line " + string(rune('a'+i%26)))
	}
	settle(m)
	send(m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	_, x, y, ok := m.scrolledPill()
	if !ok {
		t.Fatal("no indicator after scrolling up")
	}

	send(m, tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: y})
	if m.follow {
		t.Fatal("a click beside the indicator followed the tail")
	}

	send(m, tea.MouseClickMsg{Button: tea.MouseLeft, X: x + 2, Y: y})
	if !m.follow || !m.viewport.AtBottom() {
		t.Error("clicking the indicator did not return to the tail")
	}
	if strings.Contains(screen(m), "below") {
		t.Error("the indicator stayed up after it was clicked")
	}
}

// Nothing from a series reaches the transcript until the last answer, so the panel is the only
// place the user can see where they are in one and what they have already said.
func TestSeriesPanelSaysWhereYouAreAndWhatYouSaid(t *testing.T) {
	m, _ := harness(t, 80, 24)
	m.snap.Question = &ask.Question{
		Question:   "Which format?",
		Options:    []ask.Option{{Label: "JSON"}},
		AllowOther: true,
		Step:       2,
		Total:      4,
		Prior:      []string{"Session state"},
	}
	m.layout()

	out := screen(m)
	for _, want := range []string{"2 of 4", "✓ Session state", "keeps your answers"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cancels the turn") {
		t.Error("panel still promises to cancel the turn, which would discard the first answer")
	}

	// A gate still ahead means the total is a ceiling, and saying "4" flatly would be a lie.
	m.snap.Question.Approx = true
	m.layout()
	if out := screen(m); !strings.Contains(out, "2 of up to 4") {
		t.Errorf("panel reports a conditional total as exact:\n%s", out)
	}

	// A lone question is not a series and should say nothing about steps.
	m.snap.Question = &ask.Question{Question: "Which way?", Step: 1, Total: 1}
	m.layout()
	if out := screen(m); strings.Contains(out, "1 of 1") || strings.Contains(out, "keeps your answers") {
		t.Errorf("lone question presented as a series:\n%s", out)
	}
}

// Every surface that shows text it did not write wraps it: long text ending in a marker word must
// leave the marker on screen, and no row may run past the pane. A new surface belongs in this table.
func TestLongTextWrapsOnEverySurface(t *testing.T) {
	words := func(n int, end string) string { return strings.Repeat("lorem ipsum ", n) + end }
	cases := []struct {
		name    string
		setup   func(m *Model, s *session.Session)
		markers []string
	}{
		{"approval", func(m *Model, _ *session.Session) {
			m.snap.Pending = "$ " + words(15, "ENDCOMMAND")
			m.snap.PendingBody = "needs your say-so: " + words(10, "ENDREASON")
			m.snap.PendingSummary = words(6, "ENDSUMMARY")
		}, []string{"ENDCOMMAND", "ENDREASON", "ENDSUMMARY", "allow?"}},
		{"worker prompt", func(m *Model, _ *session.Session) {
			parkWorker(m, workers.Prompt{ID: "99-1", Kind: "approval", Label: "$ " + words(15, "ENDWORKER"), Body: words(10, "ENDWORKERBODY")})
			send(m, workerWaiting("ABC-1", "99-1"))
		}, []string{"[ABC-1]", "ENDWORKER", "ENDWORKERBODY", "allow?"}},
		// Fills 40 rows at width 40, so the greeting at the top is checked in the callout case.
		{"welcome", func(m *Model, _ *session.Session) {
			m.Setup = &Setup{Project: "acme", Missing: []string{"rg", "fd"}, AuthWarning: words(3, "ENDAUTH")}
			m.guardian = guardianCheck{done: true, err: errors.New("guardian: " + words(3, "ENDGUARDIAN"))}
		}, []string{"tip:", "ctrl+c kill", "ENDGUARDIAN", "brew install ripgrep fd", "ENDAUTH", `add "calendar": true`}},
		{"welcome collapsed checklist", func(m *Model, _ *session.Session) {
			m.Setup = &Setup{Project: words(8, "ENDPROJECT"), Records: "/tmp/records.db", Vault: true, PRs: true}
			m.guardian = guardianCheck{done: true}
		}, []string{"ENDPROJECT", "✓ guardian", "✓ records", "calendar"}},
		{"welcome mode callout", func(m *Model, s *session.Session) {
			s.SetMode(session.ModeReadOnly)
			settle(m)
			m.Setup = &Setup{Project: "acme"}
			m.guardian = guardianCheck{done: true}
			m.now = func() time.Time { return time.Date(2026, 10, 29, 20, 0, 0, 0, time.Local) }
			m.account = func() string { return "Jane Doe" }
		}, []string{"good evening, Jane", "read-only mode:", "change.", "ctrl+c kill"}},
		// Single-row chrome, cut rather than wrapped, so its start is what must stay.
		{"sensitive marker", func(m *Model, _ *session.Session) {
			taint.Set()
			t.Cleanup(taint.ResetForTest)
			m.session.SetMode(session.ModeAuto)
			settle(m)
		}, []string{"sensitive read"}},
		{"question", func(m *Model, _ *session.Session) {
			m.snap.Question = &ask.Question{
				Question: words(8, "ENDQUESTION"),
				Context:  words(8, "ENDCONTEXT"),
				Options: []ask.Option{
					{Label: words(6, "ENDCURSOR"), Detail: words(6, "ENDDETAIL")},
					{Label: words(6, "ENDOTHER")},
				},
				Step: 2, Total: 3, Prior: []string{words(4, "ENDPRIOR")},
			}
		}, []string{"ENDQUESTION", "ENDCONTEXT", "ENDCURSOR", "ENDDETAIL", "ENDOTHER", "ENDPRIOR", "answer"}},
		// Cut to one row, and dropped before the status when even that does not fit.
		{"session topic", func(m *Model, _ *session.Session) {
			m.snap.Topic = words(10, "ENDTOPIC")
		}, []string{"mode"}},
		{"notice", func(m *Model, _ *session.Session) {
			m.Notify(words(3, "ENDNOTICE"))
		}, []string{"ENDNOTICE"}},
		// The running box's rows are live heads and are cut rather than wrapped, so a long name's start is what they keep.
		{"agent row", func(m *Model, s *session.Session) {
			s.Roster = subagent.NewRoster(nil)
			s.Roster.AddForTest(subagent.Run{Agent: "research", Name: "STARTNAME " + words(8, ""),
				State: subagent.Running, Step: 3, MaxSteps: 40})
			settle(m)
		}, []string{"STARTNAME"}},
		{"jobs table", func(m *Model, s *session.Session) {
			s.ShowTable(report.Table{Head: []string{"job", "agent", "every", "last run", "started", "next due"},
				Rows: []report.Row{{Cells: []report.Cell{{Text: "curator"}, {Text: "curator"}, {Text: "3h"},
					{Text: "failed · 2m", Tone: report.Bad}, {Text: "10:04, 2h ago"}, {Text: "13:04, in 1h"}},
					Detail: words(12, "ENDDETAIL"), DetailTone: report.Bad}},
				Footer: words(6, "ENDFOOTER")})
			settle(m)
		}, []string{"ENDDETAIL", "ENDFOOTER"}},
		// Five columns leave the name a few columns at width 40, so its marker is kept that short.
		{"agents table", func(m *Model, s *session.Session) {
			s.ShowTable(report.Table{Preamble: "  research  ·  40 turns, background\n      " + words(10, "ENDCATALOGUE"),
				Head: []string{"agent", "model", "state", "took", "name"},
				Rows: []report.Row{{Cells: []report.Cell{{Text: "research"}, {Text: "sonnet-4-5"},
					{Text: "running", Tone: report.Active}, {Text: "3/40"}, {Text: words(4, "ZZZ")}}}},
				Footer: "1 run this session, 1 still going."})
			settle(m)
		}, []string{"ENDCATALOGUE", "ZZZ", "still going."}},
		{"focused agent row", func(m *Model, s *session.Session) {
			s.Roster = subagent.NewRoster(nil)
			s.Roster.AddForTest(subagent.Run{Agent: "research", Name: "STARTNAME " + words(8, ""),
				State: subagent.Running, Step: 3, MaxSteps: 40})
			settle(m)
			m.focusSidebar()
		}, []string{"STARTNAME", "enter opens"}},
		{"session row topic", func(m *Model, _ *session.Session) {
			m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
			m.others = []registry.Info{{PID: 1, Name: "other", State: registry.Working, Topic: words(4, "ENDTOPIC")}}
			settle(m)
		}, []string{"ENDTOPIC"}},
		{"running box", func(m *Model, s *session.Session) {
			s.Roster = subagent.NewRoster(nil)
			s.Roster.AddForTest(subagent.Run{Agent: "coder", Name: "STARTLAND " + words(8, ""), State: subagent.Done,
				Ended: time.Now(), Kept: []string{"a.go"}})
			settle(m)
			m.snap.Procs = []procs.Info{{Command: "STARTCOMMAND " + words(8, ""), Call: "c1"}}
		}, []string{"STARTLAND", "STARTCOMMAND"}},
		// A file row is cut from the left, so its name is what must stay.
		{"file row", func(m *Model, _ *session.Session) {
			m.snap.Files = []session.File{{Path: strings.Repeat("deep/", 12) + "ENDFILE.go", Created: true}}
		}, []string{"ENDFILE.go"}},
		{"file diff", func(m *Model, _ *session.Session) {
			m.fileDiff = &fileDiffPane{path: "internal/ui/ENDDIFFPATH.go", loaded: true,
				patch: "@@ -1 +1 @@\n-x\n+STARTCODE " + words(12, "ENDCODE") + "\n"}
			m.layout()
		}, []string{"ENDDIFFPATH.go", "first edited it", "STARTCODE", "ENDCODE"}},
		{"file diff error", func(m *Model, _ *session.Session) {
			m.fileDiff = &fileDiffPane{path: "a.go", loaded: true, err: errors.New(words(12, "ENDFILEDIFFERR"))}
			m.layout()
		}, []string{"ENDFILEDIFFERR"}},
		{"expanded run head", func(m *Model, s *session.Session) {
			s.Roster = subagent.NewRoster(nil)
			id := s.Roster.AddForTest(subagent.Run{Agent: "research", Name: "STARTNAME " + words(8, "ENDNAME"),
				State: subagent.Running, Step: 3, MaxSteps: 40, Task: words(10, "ENDTASK")})
			settle(m)
			m.openRun(id)
		}, []string{"STARTNAME", "ENDNAME · research", "ENDTASK"}},
		{"expanded skill head", func(m *Model, _ *session.Session) {
			m.snap.Skill = &session.SkillView{Name: words(8, "ENDSKILL"), Nodes: []skills.NodeResult{{Name: "only", State: skills.Pending}}}
			m.openSkill()
		}, []string{"skill lorem", "ENDSKILL · 0 of 1 done"}},
		{"skill waivers", func(m *Model, _ *session.Session) {
			m.snap.Skill = &session.SkillView{Name: "s",
				Nodes:   []skills.NodeResult{{Name: "check", State: skills.Pending, Stopped: words(10, "ENDSTOP")}},
				Waivers: []skills.Waiver{{Step: strings.Repeat("each[ch-2].", 6) + "ENDPATH", Reason: words(12, "ENDREASON")}}}
			if cmd := m.openSkill(); cmd != nil {
				m.Update(cmd())
			}
		}, []string{"Stopped", "ENDSTOP", "Waived", "ENDPATH", "ENDREASON"}},
		{"poll box", func(m *Model, s *session.Session) {
			head := "poll every 30s until /x/, gives up after 1h: STARTPOLL " + words(10, "ENDPOLL") + " · check-in 1h"
			s.StartToolForTest("p1", head, agent.KindPoll)
			s.ResolveToolForTest("p1", head, "10:00:00 STARTLOG "+words(10, "ENDLOG")+"\n[gave up after 1h without the condition, "+words(4, "ENDVERDICT")+"]", agent.KindPoll)
			settle(m)
		}, []string{"STARTPOLL", "ENDPOLL", "check-in 1h", "STARTLOG", "ENDLOG", "ENDVERDICT"}},
		{"compose steps", func(m *Model, s *session.Session) {
			s.StartToolForTest("t9.1", "$ STARTSTEP "+words(10, "ENDSTEP"), agent.KindBash)
			s.ResolveToolForTest("t9.1", "$ STARTSTEP "+words(10, "ENDSTEP"), words(10, "ENDSTEPOUT"), agent.KindBash)
			settle(m)
		}, []string{"STARTSTEP", "ENDSTEP", "ENDSTEPOUT"}},
		{"queue", func(m *Model, _ *session.Session) {
			m.snap.Busy, m.snap.Steerable = true, true
			m.snap.Steering = []session.Queued{{ID: 1, Text: words(3, "ENDSENT")}}
			m.snap.Queued = []session.Queued{{ID: 2, Text: words(3, "ENDQUEUED")}}
		}, []string{"ENDSENT", "sent", "ENDQUEUED"}},
		{"workspace view", func(m *Model, _ *session.Session) {
			m.Land = &fakeLand{}
			m.wsPane = &workspacePane{loaded: true, view: orchestrator.RepoView{Name: words(6, "ENDREPO"), Workspaces: []orchestrator.WorkspaceView{
				{Name: "pool-1", Changes: []orchestrator.WorkspaceChange{{Change: "abcdefgh", WorkingCopy: true, Description: words(12, "ENDWSDESC"), Age: "2m"}}},
			}}}
			m.onWorkspaces(workspacesMsg{view: m.wsPane.view})
			m.layout()
		}, []string{"ENDREPO", "abcdefgh", "ENDWSDESC", "free"}},
		{"workspace PR row", func(m *Model, _ *session.Session) {
			m.Land = &fakeLand{pr: true}
			m.wsPane = &workspacePane{}
			m.onWorkspaces(workspacesMsg{view: orchestrator.RepoView{Name: "repo", Trunk: "main@origin", Workspaces: []orchestrator.WorkspaceView{
				{Name: "pool-1", Behind: 3, Changes: []orchestrator.WorkspaceChange{{Change: "abcdefgh", Description: words(12, "ENDPRDESC"), Age: "2m",
					Ship: orchestrator.Ship{Bookmark: "STARTBOOKMARK-" + strings.Repeat("x", 30) + "-ENDBOOKMARK", Ahead: 2}}}},
			}}})
			m.layout()
		}, []string{"ENDPRDESC", "STARTBOOKMARK", "ENDBOOKMARK", "↑2 unpushed", "main ↓3"}},
		{"workspace view error", func(m *Model, _ *session.Session) {
			m.Land = &fakeLand{}
			m.wsPane = &workspacePane{loaded: true, err: errors.New(words(12, "ENDWSERR"))}
			m.layout()
		}, []string{"ENDWSERR"}},
		{"workspace view also lands", func(m *Model, _ *session.Session) {
			m.Land = &fakeLand{}
			m.wsPane = &workspacePane{}
			m.onWorkspaces(workspacesMsg{
				view: orchestrator.RepoView{Name: "repo", Workspaces: []orchestrator.WorkspaceView{
					{Name: "pool-1", Changes: []orchestrator.WorkspaceChange{{Change: "abcdefgh", Description: "work", Age: "2m"}}},
				}},
				landable: []orchestrator.Unlanded{{Change: "abcdefgh", With: []string{"STARTWITH", words(10, "ENDWITH")}}},
			})
			m.layout()
		}, []string{"STARTWITH", "ENDWITH"}},
		{"workspace diff", func(m *Model, _ *session.Session) {
			m.Land = &fakeLand{}
			m.wsPane = &workspacePane{loaded: true, diff: &wsDiff{change: "abcdefgh", loaded: true, stat: "    " + words(12, "ENDSTAT"),
				expanded: map[int]bool{}, open: map[int]bool{}}}
			m.layout()
		}, []string{"diff of abcdefgh", "ENDSTAT", "esc back"}},
		{"workspace diff error", func(m *Model, _ *session.Session) {
			m.Land = &fakeLand{}
			m.wsPane = &workspacePane{loaded: true, diff: &wsDiff{change: "abcdefgh", loaded: true, err: errors.New(words(12, "ENDDIFFERR"))}}
			m.layout()
		}, []string{"ENDDIFFERR"}},
		{"file graph list", func(m *Model, _ *session.Session) {
			long := strings.Repeat("dir/", 12) + "ENDPATH.go"
			m.fileGraph = &fileGraphPane{changed: []int{0}, title: words(8, "ENDTITLE"), pics: map[int]neighbours{}, names: map[string]string{long: "ENDPATH.go"}, g: reviewgraph.Graph{
				Repo: "acme/web", Number: 7,
				Files:      []reviewgraph.File{{Path: long, Changed: true, Status: "M"}},
				Unexamined: []string{words(10, "ENDGAP")},
			}}
			m.layout()
		}, []string{"ENDTITLE", "ENDPATH", "ENDGAP"}},
		{"file graph findings", func(m *Model, _ *session.Session) {
			m.fileGraph = &fileGraphPane{changed: []int{0}, inFindings: true, pics: map[int]neighbours{}, names: map[string]string{"a.go": "a.go"},
				g: reviewgraph.Graph{Repo: "acme/web", Number: 7, Head: "h",
					Files: []reviewgraph.File{{Path: "a.go", Changed: true, Status: "M"}},
					Diff:  "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+STARTCODE " + words(12, "ENDCODE") + "\n",
				},
				review: &reviewstore.Review{Head: "h", Findings: []reviewstore.Finding{
					{Severity: "blocking", Class: "c", Path: "a.go", StartLine: 1, EndLine: 1, Text: words(12, "ENDFINDING")},
				}}}
			m.layout()
		}, []string{"STARTCODE", "ENDFINDING"}},
		{"finding question input", func(m *Model, _ *session.Session) {
			in := newFindingInput()
			in.SetValue(words(12, "ENDTYPED"))
			m.fileGraph = &fileGraphPane{changed: []int{0}, inFindings: true, chatting: true, input: in, pics: map[int]neighbours{}, names: map[string]string{"a.go": "a.go"},
				g:      reviewgraph.Graph{Repo: "acme/web", Number: 7, Head: "h", Files: []reviewgraph.File{{Path: "a.go", Changed: true, Status: "M"}}},
				review: &reviewstore.Review{Head: "h", Findings: []reviewstore.Finding{{Severity: "blocking", Class: "c", Path: "a.go", Text: "finding"}}}}
			m.layout()
		}, []string{"ENDTYPED"}},
		{"file graph diff", func(m *Model, _ *session.Session) {
			m.fileGraph = &fileGraphPane{changed: []int{0}, diff: true, pics: map[int]neighbours{}, names: map[string]string{"a.go": "a.go"}, g: reviewgraph.Graph{
				Repo: "acme/web", Number: 7,
				Files: []reviewgraph.File{{Path: "a.go", Changed: true, Status: "M"}},
				Diff:  "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+" + words(12, "ENDDIFF") + "\n",
			}}
			m.layout()
		}, []string{"ENDDIFF"}},
		{"box head", func(m *Model, s *session.Session) {
			pushTool(s, "$ "+words(8, "ENDHEAD"), "output")
			settle(m)
		}, []string{"ENDHEAD"}},
		{"boxed prompt", func(m *Model, s *session.Session) {
			s.PushUserForTest(words(10, "ENDPROMPT"))
			settle(m)
		}, []string{"ENDPROMPT"}},
		// The pinned prompt is one row over the transcript, so it is cut and must keep the prompt's start.
		{"pinned prompt", func(m *Model, s *session.Session) {
			s.PushUserForTest("STARTPIN " + words(10, ""))
			s.Show(paragraphs(60))
			settle(m)
			m.jumpPrompt(-1)
			m.scrollBy(10)
		}, []string{"› STARTPIN"}},
		{"boxed error", func(m *Model, s *session.Session) {
			s.PushErrorForTest(words(10, "ENDERROR"))
			settle(m)
		}, []string{"ENDERROR"}},
		{"rules box", func(m *Model, s *session.Session) {
			s.PushRulesForTest([]agent.FiredRule{{Subject: "Code Comments", Title: words(8, "")},
				{Subject: words(6, "ENDCATEGORY"), Title: words(8, "ENDRULE")}})
			settle(m)
			m.expanded[m.snap.Items[0].ID] = true
		}, []string{"ENDCATEGORY", "ENDRULE"}},
		{"boxed tool body", func(m *Model, s *session.Session) {
			pushTool(s, "$ cat notes", words(12, "ENDBODY"))
			settle(m)
			m.expanded[m.snap.Items[0].ID] = true
		}, []string{"ENDBODY"}},
		{"record box body", func(m *Model, s *session.Session) {
			s.PushToolForTest("records: search · "+words(8, "ENDLABEL"),
				"### "+words(8, "ENDTITLE")+"\n`rec_1` · **claim** about **alux**\n\n"+words(12, "ENDTEXT"), agent.KindRecord)
			settle(m)
			m.expanded[m.snap.Items[0].ID] = true
		}, []string{"ENDLABEL", "ENDTITLE", "ENDTEXT"}},
		{"declined suggestion", func(m *Model, s *session.Session) {
			input, _ := json.Marshal(map[string]string{"id": "s1", "reason": words(10, "ENDREASON")})
			s.CalledForTest(agent.Call{ID: "t2", Name: agent.DeclineSuggestion, Input: input},
				anthropic.NewToolResultBlock("t2", "Not run: "+words(10, "ENDRESULT"), true))
			settle(m)
		}, []string{"decline suggestion s1", "ENDREASON", "ENDRESULT"}},
		{"tool group", func(m *Model, s *session.Session) {
			for i := range 4 {
				pushTool(s, fmt.Sprintf("$ %s ENDHEAD%d", words(3, ""), i), "out")
			}
			settle(m)
		}, []string{"ENDHEAD0", "ENDHEAD1", "ENDHEAD2", "ENDHEAD3"}},
		{"mermaid diagram", func(m *Model, s *session.Session) {
			s.Show("```mermaid\ngraph TD\n    A[Start] --> B[ENDBOX]\n```")
			settle(m)
		}, []string{"Start", "ENDBOX"}},
		{"labelled flowchart", func(m *Model, s *session.Session) {
			s.Show("```mermaid\nflowchart TD\n  start -->|then| ENDFLOW\n```")
			settle(m)
		}, []string{"start", "then", "ENDFLOW"}},
		{"er diagram", func(m *Model, s *session.Session) {
			s.Show("```mermaid\nerDiagram\n    CUSTOMER ||--o{ ENDENTITY : places\n    CUSTOMER {\n        string id PK\n    }\n```")
			settle(m)
		}, []string{"CUSTOMER", "places", "ENDENTITY"}},
		{"sequence diagram", func(m *Model, s *session.Session) {
			s.Show("```mermaid\nsequenceDiagram\n    Client->>ENDSEQ: request\n    ENDSEQ-->>Client: reply\n```")
			settle(m)
		}, []string{"Client", "request", "reply", "ENDSEQ"}},
		{"flowchart too wide", func(m *Model, s *session.Session) {
			s.Show("```mermaid\nflowchart TD\n  Request --> Router --> Authenticate\n  Router --> LoadSession\n  Router --> RunAgent\n  Router --> RenderReply\n  Router --> ENDFLOWWIDE\n```")
			settle(m)
		}, []string{"Authenticate", "ENDFLOWWIDE"}},
		{"review app offer", func(m *Model, _ *session.Session) {
			it := pr(42, "t")
			m.picker = &prPicker{rows: []pickerRow{{section: "mine", item: it}}}
			m.onE2E(e2eMsg{item: it, state: prs.EnvNone, label: "deploy-preview"})
		}, []string{"#42 has no", "cancels"}},
		{"merge label offer", func(m *Model, _ *session.Session) {
			it := pr(42, "t")
			m.picker = &prPicker{rows: []pickerRow{{section: "mine", item: it}}}
			m.onMergeCheck(mergeCheckMsg{item: it, check: prs.MergeCheck{Label: "merge-queue", Pending: []string{"a", "b"}}})
		}, []string{"#42 is yours", "cancels"}},
		{"skill section", func(m *Model, _ *session.Session) {
			m.snap.Skill = &session.SkillView{Name: "STARTSKILL-" + strings.Repeat("lorem-", 10),
				Nodes: []skills.NodeResult{{Name: "only", State: skills.Pending}}}
		}, []string{"STARTSKILL", "0/1"}},
		{"named id", func(m *Model, s *session.Session) {
			m.Names = &resolve.Resolver{}
			m.Names.Learn("#4321", words(3, "ENDNAME"))
			s.PushAssistantForTest(words(4, "") + "#4321 " + words(4, "ENDPROSE"))
			settle(m)
			send(m, NamesMsg{})
		}, []string{"#4321", "ENDPROSE"}},
		{"command output", func(m *Model, s *session.Session) {
			s.Show(words(30, "ENDNOTE"))
			settle(m)
		}, []string{"ENDNOTE"}},
		{"mermaid too wide", func(m *Model, s *session.Session) {
			s.Show("```mermaid\ngraph LR\n    Request --> Router --> Authenticate --> LoadSession --> RunAgent --> RenderReply\n    RenderReply --> ENDNODE\n```")
			settle(m)
		}, []string{"Authenticate", "ENDNODE"}},
	}
	// The running box's cases run with it open, at widths that allow it.
	inBox := map[string]bool{"notice": true, "agent row": true, "focused agent row": true, "running box": true, "skill section": true, "session row topic": true, "file row": true}
	for _, c := range cases {
		widths := []int{40, 80}
		if inBox[c.name] {
			widths = []int{sidebarMinScreen, 140}
		}
		for _, width := range widths {
			m, s := harness(t, width, 40)
			m.sidebarOpen = inBox[c.name]
			c.setup(m, s)
			m.layout()
			out := screen(m)
			for i, line := range strings.Split(out, "\n") {
				if w := ansi.StringWidth(line); w > width {
					t.Errorf("%s at %d: row %d is %d columns: %q", c.name, width, i, w, line)
				}
			}
			// The expanded running box scrolls, so what it holds is read whole rather than off the screen.
			if m.panelOpen() {
				held := ansi.Strip(m.pane.GetContent())
				out += "\n" + held
				for i, line := range strings.Split(held, "\n") {
					if w := ansi.StringWidth(line); w > m.pane.Width() {
						t.Errorf("%s at %d: panel row %d is %d columns in %d: %q", c.name, width, i, w, m.pane.Width(), line)
					}
				}
			}
			flat := strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " ")
			for _, want := range c.markers {
				if !strings.Contains(flat, want) {
					t.Errorf("%s at %d: %q was cut off:\n%s", c.name, width, want, out)
				}
			}
		}
	}
}

// The input frame sits in the transcript's columns, one in from either edge of the pane.
func TestTheFrameLinesUpWithTheTranscript(t *testing.T) {
	m, s := harness(t, 60, 16)
	s.PushUserForTest("hello")
	settle(m)
	rows := strings.Split(screen(m), "\n")
	input := -1
	for i, row := range rows {
		if strings.Contains(row, "›") {
			input = i
		}
	}
	left, right := boxEdges(t, m, input)
	if left != 1 || right != 58 {
		t.Errorf("frame borders at columns %d and %d, want 1 and 58:\n%s", left, right, screen(m))
	}
	prompt := []rune(rows[rowOf(t, m, "hello")])
	if left >= len(prompt) || prompt[left] != '│' {
		t.Errorf("the prompt's bar is not in the input frame's left column %d:\n%s", left, screen(m))
	}
}

func transcriptText(m *Model) string { return ansi.Strip(strings.Join(m.content.lines, "\n")) }

// groupsIn is the number of runs of consecutive barred rows in the transcript.
func groupsIn(m *Model) int {
	n, in := 0, false
	for _, line := range m.content.lines {
		bar := strings.HasPrefix(ansi.Strip(line), "│")
		if bar && !in {
			n++
		}
		in = bar
	}
	return n
}

func TestConsecutiveCallsToOneToolShareABox(t *testing.T) {
	m, s := harness(t, 80, 30)
	pushTool(s, "$ echo one", numberedLines(11))
	pushTool(s, "$ echo two", numberedLines(11))
	settle(m)

	if n := groupsIn(m); n != 1 {
		t.Fatalf("two bash calls drew %d bars, want one:\n%s", n, transcriptText(m))
	}
	if one, two := rowContaining(t, m, "$ echo one"), rowContaining(t, m, "$ echo two"); two != one+1 {
		t.Errorf("the heads in a shared box are rows %d and %d, want adjacent:\n%s", one, two, screen(m))
	}
}

func TestADifferentToolStartsANewBox(t *testing.T) {
	m, s := harness(t, 80, 30)
	pushTool(s, "$ echo one", "1")
	s.PushToolForTest("edit internal/ui/view.go", "- a\n+ b", agent.KindEdit)
	settle(m)

	if n := groupsIn(m); n != 2 {
		t.Errorf("bash then edit drew %d bars, want two:\n%s", n, transcriptText(m))
	}
}

func TestAssistantProseIsNotBoxed(t *testing.T) {
	m, s := harness(t, 80, 30)
	s.PushAssistantForTest("just some prose")
	pushTool(s, "$ echo one", "1")
	settle(m)

	if row := rowWith(t, m, "just some prose"); strings.Contains(ansi.Strip(row), "│") {
		t.Errorf("prose is drawn with a bar: %q", ansi.Strip(row))
	}
	if groupsIn(m) != 1 {
		t.Errorf("the tool after the prose has no bar:\n%s", screen(m))
	}
}

func TestCommandOutputIsBarred(t *testing.T) {
	m, s := harness(t, 80, 30)
	s.Show("Landed abc under your working copy")
	settle(m)

	if row := rowWith(t, m, "Landed abc"); !strings.Contains(ansi.Strip(row), "│") {
		t.Errorf("command output is drawn like the model's prose: %q", ansi.Strip(row))
	}
}

func TestExpandingTheMiddleOfAGroupKeepsOneBox(t *testing.T) {
	m, s := harness(t, 80, 30)
	pushTool(s, "$ echo one", numberedLines(11))
	pushTool(s, "$ echo two", "MIDDLE-BODY\n"+numberedLines(10))
	pushTool(s, "$ echo three", numberedLines(11))
	settle(m)

	expand(t, m, rowContaining(t, m, "$ echo two"))

	if n := groupsIn(m); n != 1 {
		t.Fatalf("expanding the middle item split the group into %d bars:\n%s", n, transcriptText(m))
	}
	two, body, three := rowContaining(t, m, "$ echo two"), rowContaining(t, m, "MIDDLE-BODY"), rowContaining(t, m, "$ echo three")
	if !(two < body && body < three) {
		t.Errorf("the body is not between its head and the next item: rows %d, %d, %d:\n%s", two, body, three, screen(m))
	}
}

func TestClicksInAGroupToggleTheItemUnderThem(t *testing.T) {
	m, s := harness(t, 80, 30)
	pushTool(s, "$ echo one", numberedLines(11))
	pushTool(s, "$ echo two", numberedLines(11))
	pushTool(s, "$ echo three", numberedLines(11))
	settle(m)
	ids := map[string]int{}
	for _, item := range m.snap.Items {
		ids[item.Text] = item.ID
	}

	for _, head := range []string{"$ echo two", "$ echo one", "$ echo three"} {
		expand(t, m, rowContaining(t, m, head))
		if !m.expanded[ids[head]] {
			t.Errorf("clicking %q did not open it: %v", head, m.expanded)
		}
	}
	if len(m.expanded) != 3 {
		t.Errorf("three clicks opened %v, want each item once", m.expanded)
	}
}

func TestCopyingAGroupLeavesTheFrameBehind(t *testing.T) {
	m, s := harness(t, 80, 30)
	pushTool(s, "$ echo one", numberedLines(11))
	pushTool(s, "$ echo two", numberedLines(11))
	settle(m)

	dragSelect(m, 1, rowContaining(t, m, "$ echo one"), 70, rowContaining(t, m, "$ echo two"))
	got := m.selectedTextForTest()
	if strings.Contains(got, "│") {
		t.Errorf("the copy carries the bar:\n%q", got)
	}
	for _, want := range []string{"▸ $ echo one", "▸ $ echo two"} {
		if !strings.Contains(got, want) {
			t.Errorf("the copy lost %q:\n%q", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, " ") {
			t.Errorf("a copied line keeps the frame's padding: %q", line)
		}
	}
}

// A wrapped queued message is one click target across all its rows.
func TestEveryRowOfAWrappedQueuedPromptIsClickable(t *testing.T) {
	m, _ := harness(t, 40, 20)
	m.snap.Busy, m.snap.Steerable = true, true
	m.snap.Queued = []session.Queued{{ID: 9, Text: strings.Repeat("lorem ipsum ", 4) + "ENDQUEUED"}, {ID: 10, Text: "next"}}
	m.layout()

	first, last := rowOf(t, m, "1. lorem"), rowOf(t, m, "ENDQUEUED")
	if first == last {
		t.Fatalf("the message did not wrap:\n%s", screen(m))
	}
	for row := first; row <= last; row++ {
		if id, ok := m.queuedAt(row); !ok || id != 9 {
			t.Errorf("row %d resolved to (%d, %v), want 9", row, id, ok)
		}
	}
	if id, ok := m.queuedAt(rowOf(t, m, "2. next")); !ok || id != 10 {
		t.Errorf("the message after a wrapped one resolved to (%d, %v), want 10", id, ok)
	}
}

// A question owns the keyboard: j/k move the cursor rather than typing, which is only safe
// because the prompt is not live while the panel is up.
func TestQuestionPanelTakesTheKeyboard(t *testing.T) {
	m, s := harness(t, 80, 24)
	go s.Answer(nil) // nothing parked yet; proves Answer is safe to call spuriously
	m.snap.Question = &ask.Question{
		Question:   "Which way?",
		Options:    []ask.Option{{Label: "Left"}, {Label: "Right"}},
		AllowOther: true,
	}
	m.layout()

	out := screen(m)
	for _, want := range []string{"Which way?", "Left", "Right", "Other…"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel missing %q:\n%s", want, out)
		}
	}

	send(m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 1 {
		t.Errorf("j moved the cursor to %d, want 1", m.cursor)
	}
	if strings.Contains(screen(m), "› j") {
		t.Error("j was typed into the prompt instead of moving the cursor")
	}
}

func TestAnOpenPanelTakesKeysAheadOfAQuestion(t *testing.T) {
	m, _ := harness(t, 80, 24)
	m.snap.Question = &ask.Question{Question: "Which way?", Options: []ask.Option{{Label: "Left"}, {Label: "Right"}}}
	m.skillPane = &skillPane{}
	m.layout()

	send(m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 0 {
		t.Errorf("j reached the question behind the panel: cursor %d", m.cursor)
	}
	send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.panelOpen() {
		t.Fatal("esc did not close the panel")
	}
	send(m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 1 {
		t.Errorf("with the panel closed j moved the cursor to %d, want 1", m.cursor)
	}
}

func TestAPasteLandsInTheNoteOnAQuestion(t *testing.T) {
	m, _ := harness(t, 80, 24)
	m.snap.Question = &ask.Question{Question: "Which way?", Options: []ask.Option{{Label: "Left"}}}
	m.layout()

	send(m, tea.KeyPressMsg{Code: 'l', Text: "l"})
	send(m, tea.PasteMsg{Content: "PASTED"})
	if m.note.Value() != "PASTED" || m.input.Value() != "" {
		t.Errorf("paste went astray; note %q, main %q", m.note.Value(), m.input.Value())
	}
}

func TestAnOpenQuestionTakesTypingStraightIntoItsField(t *testing.T) {
	m, _ := harness(t, 80, 24)
	m.snap.Question = &ask.Question{Question: "What name?", AllowOther: true}
	m.layout()

	if strings.Contains(screen(m), "Other…") {
		t.Errorf("an open question still offers an Other row:\n%s", screen(m))
	}
	send(m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	send(m, tea.PasteMsg{Content: "oe"})
	if m.note.Value() != "joe" || m.input.Value() != "" {
		t.Errorf("typing went astray; note %q, main %q", m.note.Value(), m.input.Value())
	}
}

func TestAPromptArrivingTakesFocusFromTheRunningBox(t *testing.T) {
	m, s := harness(t, 130, 40)
	s.Roster = subagent.NewRoster(nil)
	s.Roster.AddForTest(subagent.Run{Agent: "research", Name: "probe", State: subagent.Running, Step: 1, MaxSteps: 40})
	settle(m)
	m.focusSidebar()
	if !m.sideFocused() {
		t.Fatal("super+e did not focus the running box")
	}

	s.ParkForTest("bash ls")
	settle(m)
	if m.sideFocused() {
		t.Error("the running box kept focus with an approval waiting")
	}
}

// Approving an edit is approving its diff, so the diff has to be on screen with the prompt.
func TestApprovalPromptShowsTheChange(t *testing.T) {
	m, _ := harness(t, 80, 24)
	m.snap.Pending = "edit internal/ui/view.go"
	m.snap.PendingBody = "- old line\n+ new line"
	m.layout()

	out := screen(m)
	if !strings.Contains(out, "- old line") || !strings.Contains(out, "+ new line") {
		t.Errorf("approval prompt hides the change:\n%s", out)
	}
	if !strings.Contains(out, "allow? (y/n") {
		t.Errorf("approval prompt lost its question:\n%s", out)
	}
	// It is the same diff as in the transcript, so it is drawn the same way.
	var tinted int
	for _, line := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(ansi.Strip(line), "old line") || strings.Contains(ansi.Strip(line), "new line") {
			if !setsBackground(line) {
				t.Errorf("the change is shown untinted: %q", ansi.Strip(line))
			}
			tinted++
		}
	}
	if tinted != 2 {
		t.Errorf("found %d diff rows, want 2", tinted)
	}
}

func TestAnApprovalThatIsNotAnEditDrawsItsListItemsUntinted(t *testing.T) {
	m, _ := harness(t, 80, 24)
	m.snap.Pending = "pr_create alux#1"
	m.snap.PendingBody = "## Summary\n- first point\n+ not an addition"
	m.layout()

	for _, line := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(ansi.Strip(line), "point") || strings.Contains(ansi.Strip(line), "addition") {
			if setsBackground(line) {
				t.Errorf("a PR body line is tinted like a diff: %q", ansi.Strip(line))
			}
		}
	}
	if !strings.Contains(screen(m), "- first point") {
		t.Errorf("the body lost its list marker:\n%s", screen(m))
	}
}

// The approval prompt has to swallow y/n rather than letting them reach the text input.
func TestApprovalPromptOwnsYAndN(t *testing.T) {
	m, s := harness(t, 80, 24)
	_ = s
	m.snap.Pending = "$ rm -rf /"
	m.layout()

	if !strings.Contains(screen(m), "allow? (y/n") {
		t.Fatalf("approval prompt not shown:\n%s", screen(m))
	}
	send(m, tea.KeyPressMsg{Code: 'y', Text: "y"})
	if m.input.Value() != "" {
		t.Errorf("y reached the text input: %q", m.input.Value())
	}
}

// Bracketed paste arrives as one message, so a multi-line paste is never mistaken for a submit.
func TestMultiLinePasteDoesNotSubmit(t *testing.T) {
	m, _ := harness(t, 80, 24)
	send(m, tea.PasteMsg{Content: "first line\nsecond line"})

	if got := m.input.Value(); got != "first line\nsecond line" {
		t.Errorf("pasted value = %q", got)
	}
}

func TestStatusLineShowsMode(t *testing.T) {
	m, _ := harness(t, 80, 24)
	if !strings.Contains(screen(m), "auto mode") {
		t.Errorf("status line missing mode:\n%s", screen(m))
	}
	for _, want := range []string{"read-only mode", "local mode", "auto mode"} {
		send(m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		settle(m)
		if !strings.Contains(screen(m), want) {
			t.Errorf("shift+tab did not reach %s:\n%s", want, screen(m))
		}
	}
}

// Marked only while the latch drops auto; the PR summary slides clear of it.
func TestTopBarMarksASensitiveReadOnlyWhenItDropsTheMode(t *testing.T) {
	m, s := harness(t, 60, 24)
	send(m, PRsMsg{States: []prs.RepoState{{Repo: "acme/web", Queue: prs.Queue{Review: []prs.Item{pr(1, "first")}}}}})
	s.SetMode(session.ModeAuto)
	settle(m)
	if top := strings.Split(screen(m), "\n")[0]; strings.Contains(top, sensitiveMarker) {
		t.Errorf("marker shown before any sensitive read:\n%q", top)
	}

	taint.Set()
	t.Cleanup(taint.ResetForTest)
	s.SetMode(session.ModeLocal)
	settle(m)
	if top := strings.Split(screen(m), "\n")[0]; strings.Contains(top, sensitiveMarker) {
		t.Errorf("marker shown in local mode, which the read did not change:\n%q", top)
	}

	s.SetMode(session.ModeAuto)
	settle(m)
	top := strings.Split(screen(m), "\n")[0]
	if !strings.HasPrefix(top, " "+sensitiveMarker) {
		t.Fatalf("top bar does not open with the marker:\n%q", top)
	}
	if row := m.topRow(); row.center == "" || row.centerAt < 1+ansi.StringWidth(sensitiveMarker)+topBarGap {
		t.Errorf("PR summary at column %d overlaps the marker:\n%q", row.centerAt, top)
	}
	if strings.Contains(screen(m), "(PHI read)") {
		t.Errorf("status line still carries the old suffix:\n%s", screen(m))
	}
}

// The gauge closes the input box's upper border, after the status line and before the corner.
func TestContextGaugeClosesTheInputBoxsUpperBorder(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.SetUsageForTest(agent.Usage{Prompt: 50_000, Output: 2_000, Window: 200_000, CacheRead: 48_000})
	settle(m)

	edge := edgeRow(t, m)
	if !strings.HasSuffix(edge, "ctx 26% (52k/200k) · cache 96% ─╮") || !strings.Contains(edge, "auto mode") {
		t.Errorf("upper border does not carry the status then the gauge:\n%q", edge)
	}
	if got := ansi.StringWidth(edge); got != 79 {
		t.Errorf("the border ends at column %d of 80, want the box's own right edge", got)
	}
	if strings.Contains(strings.Split(screen(m), "\n")[0], "ctx ") {
		t.Errorf("the gauge is still on the top row:\n%s", screen(m))
	}
}

// The turn's own state outranks the gauge when the border cannot hold both.
func TestTheGaugeYieldsToTheTurnsStateWhenNarrow(t *testing.T) {
	m, s := harness(t, 45, 24)
	s.SetUsageForTest(agent.Usage{Prompt: 50_000, Output: 2_000, Window: 200_000, CacheRead: 48_000})
	settle(m)
	m.snap.Busy = true
	m.layout()
	if edge := edgeRow(t, m); !strings.Contains(edge, "working…") || strings.Contains(edge, "ctx ") {
		t.Errorf("narrow border = %q, want the turn's state and no gauge", edge)
	}
}

func TestTheTopicSitsCentredInTheBorderAndYieldsFirst(t *testing.T) {
	for _, c := range []struct {
		width int
		shown bool
	}{{80, true}, {60, false}} {
		m, s := harness(t, c.width, 24)
		s.SetUsageForTest(agent.Usage{Prompt: 50_000, Output: 2_000, Window: 200_000, CacheRead: 48_000})
		settle(m)
		m.snap.Topic = "status border tweaks"
		m.layout()
		edge := ansi.Strip(edgeRow(t, m))
		if !strings.Contains(edge, "auto mode") || !strings.Contains(edge, "ctx 26%") {
			t.Errorf("at %d the topic pushed out the status or the gauge: %q", c.width, edge)
		}
		before, after, found := strings.Cut(edge, " status border tweaks ")
		if found != c.shown {
			t.Errorf("at %d the topic shown = %v, want %v: %q", c.width, found, c.shown, edge)
		}
		if !found {
			continue
		}
		left := len(before) - len(strings.TrimRight(before, "─"))
		right := len(after) - len(strings.TrimLeft(after, "─"))
		if left/len("─")-right/len("─") > 1 || right/len("─")-left/len("─") > 1 {
			t.Errorf("topic off centre, %d dashes before and %d after: %q", left/len("─"), right/len("─"), edge)
		}
	}
}

func TestTheGaugeFlagsAPromptThatCachedNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		u    agent.Usage
		want string
	}{
		{"uncached", agent.Usage{Prompt: 50_000, Output: 2_000, Window: 200_000}, "cache off"},
		{"first write", agent.Usage{Prompt: 50_000, Output: 2_000, Window: 200_000, CacheWrite: 50_000}, "cache 0%"},
		{"too small to cache", agent.Usage{Prompt: 1_000, Output: 100, Window: 200_000}, "cache 0%"},
		{"counted before any request", agent.Usage{Prompt: 20_000, Window: 1_000_000, Counted: true}, "ctx 2% (20k/1000k)"},
	} {
		m, s := harness(t, 80, 24)
		s.SetUsageForTest(c.u)
		settle(m)
		if edge := strings.TrimSuffix(edgeRow(t, m), " ─╮"); !strings.HasSuffix(edge, c.want) {
			t.Errorf("%s: upper border %q, want the gauge to end %q", c.name, edge, c.want)
		}
	}
}

// A pane narrower than the content must not emit lines wider than itself, or the terminal
// rewraps them and the layout comes apart.
func TestNothingOverflowsThePane(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		m, s := harness(t, width, 20)
		s.Show("a reply with a very long unbroken token: " + strings.Repeat("x", 200))
		pushTool(s, "$ "+strings.Repeat("long-command ", 20), strings.Repeat("output ", 50))
		s.SetUsageForTest(agent.Usage{Prompt: 150_000, Output: 9_000, Window: 200_000})
		settle(m)

		for i, line := range strings.Split(screen(m), "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("width %d: line %d is %d columns:\n%s", width, i, w, line)
			}
		}
	}
}

// An expanded body wraps to the pane rather than being clipped by the viewport at its edge.
func TestExpandedBodyWraps(t *testing.T) {
	m, s := harness(t, 40, 24)
	words := make([]string, 30)
	for i := range words {
		words[i] = fmt.Sprintf("w%02d", i)
	}
	pushTool(s, "$ think", numberedLines(10)+"\n"+strings.Join(words, " "))
	settle(m)
	expand(t, m, rowContaining(t, m, "$ think"))

	if !strings.Contains(screen(m), "w29") {
		t.Errorf("the end of the body was clipped:\n%s", screen(m))
	}
}

// Reported by a user: an expanded edit was grey text on a grey box. A write is a diff, so it is
// drawn as one — a tint per side, the code on it still coloured as code.
func TestAnExpandedEditIsATintedDiff(t *testing.T) {
	m, s := harness(t, 80, 24)
	long := strings.Repeat("filled(argument) + ", 8)
	s.PushToolForTest("edit internal/ui/view.go", "- was := 1\n+ now := "+long+"\n"+numberedLines(9), agent.KindEdit)
	settle(m)
	expand(t, m, rowContaining(t, m, "edit internal/ui/view.go"))

	var tinted int
	for i, line := range strings.Split(m.View().Content, "\n") {
		if !strings.Contains(ansi.Strip(line), "was := 1") && !strings.Contains(ansi.Strip(line), "filled") {
			continue
		}
		if !setsBackground(line) {
			t.Errorf("diff row %d has no tint: %q", i, ansi.Strip(line))
		}
		if w := ansi.StringWidth(line); w > 80 {
			t.Errorf("diff row %d is %d columns wide", i, w)
		}
		tinted++
	}
	if tinted < 3 {
		t.Fatalf("want the removed line and a wrapped added line, found %d rows:\n%s", tinted, screen(m))
	}
}

// What a slow command looks like while it is still going: its own row, naming it, with no line
// count — there is no output to count yet, and claiming one line of it would be a lie.
func TestARunningCommandShowsItsHeadWithNoLineCount(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.StartToolForTest("t1", "$ go test ./...", agent.KindBash)
	settle(m)

	row := rowWith(t, m, "go test ./...")
	if strings.Contains(row, "line") {
		t.Errorf("a command that has not answered is offering a line count: %q", row)
	}
	if strings.Contains(row, "▸") {
		t.Errorf("a running command's row says it can be opened: %q", row)
	}

	// The shimmer is baked into the transcript rather than drawn over it, so a tick that does not
	// reach the transcript leaves a frozen head that looks exactly like nothing happening.
	for range 5 {
		m.Update(m.spin.Tick())
		if next := rowWith(t, m, "go test ./..."); next != row {
			return
		}
	}
	t.Errorf("the row did not change over five ticks, so it is frozen: %q", ansi.Strip(row))
}

// A rendered item is remembered between layouts, so anything that still changes has to be left out
// of that. A thought grows with every delta, and a frozen one would look exactly like the stall
// this whole area is about.
func TestAGrowingThoughtIsNotFrozenByTheBlockCache(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.ThinkForTest(numberedLines(10) + "\nfirst the one thing")
	settle(m)
	expand(t, m, rowContaining(t, m, "thinking"))
	if !strings.Contains(screen(m), "first the one thing") {
		t.Fatalf("the thought never reached the screen:\n%s", screen(m))
	}

	s.ThinkForTest(" and then the other thing")
	settle(m)
	if !strings.Contains(screen(m), "and then the other thing") {
		t.Errorf("the thought froze at its first delta:\n%s", screen(m))
	}
}

// A thought often arrives with blank lines around it, and counting them promises lines that open
// onto nothing.
func TestAThoughtCountsOnlyTheLinesItShows(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.ThinkForTest("\n" + numberedLines(11) + "\n\n")
	settle(m)
	row := ansi.Strip(rowWith(t, m, "thinking"))
	if !strings.Contains(row, "· 11 lines") {
		t.Errorf("an eleven-line thought should say 11 lines: %q", row)
	}
}

// A thought starts collapsed however short it is.
func TestAShortThoughtStartsCollapsed(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.ThinkForTest("just one idea")
	settle(m)
	if strings.Contains(screen(m), "just one idea") {
		t.Errorf("a one-line thought should start collapsed:\n%s", screen(m))
	}
	if !strings.Contains(ansi.Strip(rowWith(t, m, "thinking")), "▸") {
		t.Errorf("the thinking box is not marked closed:\n%s", screen(m))
	}
}

// Width is part of what a remembered block was rendered for, so a resize cannot serve the old one
// back: the wrapping would be wrong and the selection grid would disagree with the screen.
func TestAResizeRewrapsRememberedBlocks(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.PushUserForTest(strings.Repeat("a long prompt that has to wrap somewhere ", 4))
	settle(m)
	wide := len(m.content.lines)

	send(m, tea.WindowSizeMsg{Width: 40, Height: 24})
	for _, line := range m.content.lines {
		if w := ansi.StringWidth(line); w > 40 {
			t.Fatalf("a remembered block kept its old width: %d columns in %q", w, line)
		}
	}
	if narrow := len(m.content.lines); narrow <= wide {
		t.Errorf("the prompt occupied %d lines at 80 columns and %d at 40, so it was not re-wrapped",
			wide, narrow)
	}
}

// The same tool also views files, and numbered lines are not a change: painting them green
// would say something happened that did not.
func TestAnExpandedFileViewIsNotTinted(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.PushToolForTest("view internal/ui/view.go", "  1\tpackage ui\n  2\t"+strings.Repeat("\n  3\t// pad", 9), agent.KindEdit)
	settle(m)
	expand(t, m, rowContaining(t, m, "view internal/ui/view.go"))

	for i, line := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(ansi.Strip(line), "package ui") && setsBackground(line) {
			t.Errorf("row %d of a file view is painted like a change: %q", i, ansi.Strip(line))
		}
	}
}

// It is still code, though, so it is still coloured like code — the box reaches internal/code for
// a view exactly as it reaches internal/diff for a write.
func TestAnExpandedFileViewIsHighlighted(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.PushToolForTest("view internal/ui/view.go", "  1\tfunc render() string {"+strings.Repeat("\n  2\t// pad", 10), agent.KindEdit)
	settle(m)
	expand(t, m, rowContaining(t, m, "view internal/ui/view.go"))

	row := rowWith(t, m, "func render()")
	if got := countColours(row); got < 3 {
		t.Errorf("a viewed Go line came back in %d colours, so it was not lexed: %q", got, row)
	}
}

// The search tools report matches, which are code in whatever file they came from. Nothing says
// which language that is except the path on each line, so the box hands the block over whole.
func TestExpandedSearchResultsAreHighlighted(t *testing.T) {
	m, s := harness(t, 80, 24)
	s.PushToolForTest("grep render — *.go", "internal/ui/view.go:12:func render() string {"+strings.Repeat("\ninternal/ui/view.go:13:// pad", 10), agent.KindSearch)
	settle(m)
	expand(t, m, rowContaining(t, m, "grep render"))

	row := rowWith(t, m, "func render()")
	if got := countColours(row); got < 3 {
		t.Errorf("a search hit came back in %d colours, so it was not lexed: %q", got, row)
	}
	if !strings.Contains(ansi.Strip(row), "internal/ui/view.go:12:") {
		t.Errorf("the hit lost the path that locates it: %q", ansi.Strip(row))
	}
}

// rowWith is the first transcript row containing text, escapes and all; the chrome above it, tiles included, is skipped.
func rowWith(t *testing.T, m *Model, text string) string {
	t.Helper()
	for _, line := range strings.Split(m.View().Content, "\n")[m.transcriptTop():] {
		if strings.Contains(ansi.Strip(line), text) {
			return line
		}
	}
	t.Fatalf("no row contains %q:\n%s", text, screen(m))
	return ""
}

var truecolourFg = regexp.MustCompile(`38;2;(\d+;\d+;\d+)`)

// countColours reports how many distinct truecolour foregrounds a row sets.
func countColours(row string) int {
	seen := map[string]bool{}
	for _, match := range truecolourFg.FindAllStringSubmatch(row, -1) {
		seen[match[1]] = true
	}
	return len(seen)
}

// The bubble sits in the sidebar, never over the transcript, and its tail follows him as he walks.
func TestANoticeIsABubbleOverTheAluxWhoseTailFollowsHim(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Notify("NOTICETEXT")
	for _, at := range []int{0, sidebarWide - aluxWide} {
		m.alux.x = at
		x, y := aluxCell(m)
		lines := strings.Split(screen(m), "\n")
		if col := strings.Index(lines[y-4], "NOTICETEXT"); col < 0 || ansi.StringWidth(lines[y-4][:col]) < m.mainWidth() {
			t.Fatalf("with him at %d the notice is not in the sidebar just above his tail:\n%s", at, screen(m))
		}
		tail := strings.TrimSpace(ansi.Cut(lines[y-2], x-1, x+aluxWide+1))
		if tail != "╲" && tail != "╱" {
			t.Errorf("with him at %d there is no tail over his hat, got %q:\n%s", at, tail, screen(m))
		}
	}
}

// One notice is said at a time; the next waits its turn with its full life, and the bubble counts it.
func TestNoticesAreSaidOneAtATime(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Notify("FIRST")
	if cmd := m.Notify("SECOND"); cmd != nil {
		t.Error("a waiting notice started its timer before it was shown")
	}
	if out := screen(m); !strings.Contains(out, "FIRST") || strings.Contains(out, "SECOND") || !strings.Contains(out, "+1") {
		t.Fatalf("want FIRST shown with +1 waiting:\n%s", out)
	}
	_, cmd := m.Update(noticeExpired{id: m.notices[0].id})
	if cmd == nil {
		t.Fatal("the next notice reached the front with no timer of its own")
	}
	if out := screen(m); strings.Contains(out, "FIRST") || !strings.Contains(out, "SECOND") || strings.Contains(out, "+1") {
		t.Errorf("after FIRST expired want SECOND alone:\n%s", out)
	}
}

// With him hidden the notice falls back to a card over the transcript, so it is never lost.
func TestANoticeFallsBackToACardWhenTheAluxIsHidden(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.setSidebar(false)
	m.Notify("NOTICETEXT")
	row := rowContaining(t, m, "NOTICETEXT")
	line := strings.Split(screen(m), "\n")[row]
	if col := ansi.StringWidth(line[:strings.Index(line, "NOTICETEXT")]); col >= m.mainWidth() {
		t.Errorf("the fallback card is at column %d, want over the transcript:\n%s", col, screen(m))
	}
	if strings.Contains(screen(m), "╲") || strings.Contains(screen(m), "╱") {
		t.Errorf("the fallback card has a speech tail with nobody to speak:\n%s", screen(m))
	}
}

func TestTheAluxStopsWalkingWhileHeSpeaks(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.snap.Busy = true
	m.Notify("NOTICETEXT")
	start := aluxAt(m)
	ticks(m, 4*aluxStepEvery)
	if aluxAt(m) != start {
		t.Errorf("he walked from %d to %d while a notice was up", start, aluxAt(m))
	}
}

// A notice is cut short rather than growing up the sidebar.
func TestAVeryLongNoticeIsCutShort(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Notify(strings.Repeat("spill ", 400))
	out := screen(m)
	if lines := strings.Split(out, "\n"); len(lines) > 30 {
		t.Errorf("the notice grew the screen to %d rows", len(lines))
	}
	if strings.Count(out, "spill") > noticeMaxTall*panelInner(sidebarWide)/len("spill ") || !strings.Contains(out, "more lines") {
		t.Errorf("the notice was not cut short:\n%s", out)
	}
}

func pushTool(s *session.Session, label, body string) {
	s.PushToolForTest(label, body, agent.KindBash)
}

// numberedLines is n lines of distinct text, long enough to start a box collapsed once n passes session.BoxCollapseLines.
func numberedLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	return strings.Join(lines, "\n")
}

// hiddenBody is eleven lines, so a box holding it starts collapsed with "hidden body" out of sight.
var hiddenBody = "hidden body\n" + numberedLines(10)

// A run starting must not move the transcript: the box sits beside it, and clicks are mapped from the screen row.
func TestARunAppearingLeavesTheTranscriptAndItsClicksWhereTheyWere(t *testing.T) {
	m, s := harness(t, 140, 24)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	pushTool(s, "$ echo hi", numberedLines(11))
	m.sidebarOpen = true
	settle(m)
	before, width := m.transcriptTop(), m.contentWidth()

	roster.AddForTest(subagent.Run{Agent: "research", Task: "look", State: subagent.Running, MaxSteps: 25})
	settle(m)

	if got := m.transcriptTop(); got != before {
		t.Fatalf("transcript top = %d, want %d: the running box must not push the transcript down", got, before)
	}
	if got := m.contentWidth(); got != width {
		t.Errorf("a run starting rewrapped the transcript from %d to %d columns", width, got)
	}

	expand(t, m, rowContaining(t, m, "$ echo hi"))
	if !strings.Contains(screen(m), "line 11") {
		t.Errorf("the click missed its box once the run appeared:\n%s", screen(m))
	}
}

// A second ctrl+c quits only while the first one's warning is still on screen.
func TestCtrlCOverARunningSubagentAsksTwice(t *testing.T) {
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	// The warning's cmd is a notice timer, so only a model that says it is quitting has its cmd run.
	quits := func(m *Model, cmd tea.Cmd) bool {
		if !m.quit || cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}

	idle, _ := harness(t, 80, 24)
	if _, cmd := idle.Update(ctrlC); !quits(idle, cmd) {
		t.Fatal("ctrl+c with nothing running should quit at once")
	}

	m, s := harness(t, 80, 24)
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	driver := roster.AddForTest(subagent.Run{Agent: "pair", State: subagent.Running, MaxSteps: 60})
	roster.AddForTest(subagent.Run{Agent: "navigator", State: subagent.Idle, MaxSteps: 6, Parent: driver})
	settle(m)

	if _, cmd := m.Update(ctrlC); quits(m, cmd) {
		t.Fatal("the first ctrl+c over a running subagent quit")
	}
	if !strings.Contains(screen(m), "1 subagent still running") {
		t.Errorf("no warning, or the navigator was counted as its own agent:\n%s", screen(m))
	}

	clock = clock.Add(noticeLife + time.Second)
	if _, cmd := m.Update(ctrlC); quits(m, cmd) {
		t.Fatal("a ctrl+c after the warning faded quit without warning again")
	}

	clock = clock.Add(time.Second)
	if _, cmd := m.Update(ctrlC); !quits(m, cmd) {
		t.Fatal("a second ctrl+c inside the warning did not quit")
	}
}

func TestProgramStatusSaysWhatTheSessionWaitsOn(t *testing.T) {
	m, _ := harness(t, 80, 24)
	cases := []struct {
		name  string
		setup func()
		want  string
	}{
		{"fresh", func() {}, "state=idle:app=alux"},
		{"working", func() { m.snap.Busy = true }, "state=working:app=alux"},
		{"approval", func() { m.snap.Pending = "bash: make\n  && make test" }, "state=blocked:app=alux:kind=permission:msg=" + b64("bash: make   && make test")},
		{"question", func() { m.snap.Question = &ask.Question{Question: "which?"} }, "state=blocked:app=alux:kind=question:msg=" + b64("which?")},
		{"replied", func() { m.snap.Items = []session.Item{{Kind: session.KindAssistant, Text: "all green"}} }, "state=done:app=alux:msg=" + b64("all green")},
	}
	for _, c := range cases {
		m.snap.Busy, m.snap.Pending, m.snap.Question, m.snap.Items = false, "", nil, nil
		c.setup()
		if got, want := m.programStatus(), "\x1b]7501;"+c.want+"\x1b\\"; got != want {
			t.Errorf("%s: got %q, want %q", c.name, got, want)
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// A finish is unseen only when it lands in an unfocused pane, and stays so until that pane is focused.
func TestFinishWhileUnfocusedIsUnseenUntilFocus(t *testing.T) {
	notifyDesktop = func(string, string) {}
	t.Cleanup(func() { notifyDesktop = desktop.Notify })

	m, _ := harness(t, 80, 24)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	unseen := func() bool { return m.Registry.All(nil, registry.Idle)[0].Unseen }
	finish := func() {
		m.snap.Busy = true
		m.publishState()
		m.snap.Busy = false
		m.publishState()
	}

	finish()
	if unseen() {
		t.Fatal("a finish in the focused pane was marked unseen")
	}

	send(m, tea.BlurMsg{})
	finish()
	if !unseen() {
		t.Fatal("a finish in an unfocused pane was not marked unseen")
	}

	send(m, tea.FocusMsg{})
	if unseen() {
		t.Error("focusing the pane did not clear unseen")
	}
}

// A focused pane still notifies when the terminal is in the background, since a multiplexer may
// never report that the terminal lost focus.
func TestFocusedPaneNotifiesOnlyWhenTerminalIsAway(t *testing.T) {
	var sent []string
	inFront := true
	asked := 0
	notifyDesktop = func(_, body string) { sent = append(sent, body) }
	terminalInFront = func() bool { asked++; return inFront }
	t.Cleanup(func() {
		notifyDesktop = desktop.Notify
		terminalInFront = desktop.TerminalInFront
	})

	m, _ := harness(t, 80, 24)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	finish := func() {
		m.snap.Busy = true
		run(m.publishState())
		m.snap.Busy = false
		run(m.publishState())
	}

	finish()
	if len(sent) != 0 {
		t.Fatalf("notified while focused with the terminal in front: %q", sent)
	}

	inFront = false
	finish()
	if len(sent) == 0 {
		t.Fatal("no notification while focused with the terminal in the background")
	}

	sent, asked = nil, 0
	send(m, tea.BlurMsg{})
	finish()
	if len(sent) == 0 || asked != 0 {
		t.Errorf("an unfocused pane sent %q after asking macOS %d times; want a notification without asking", sent, asked)
	}
}

func TestQuietBusySpellSendsNoNotification(t *testing.T) {
	var sent []string
	notifyDesktop = func(_, body string) { sent = append(sent, body) }
	t.Cleanup(func() { notifyDesktop = desktop.Notify })

	m, _ := harness(t, 80, 24)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	send(m, tea.BlurMsg{})
	m.snap.Busy, m.snap.Quiet = true, true
	run(m.publishState())
	m.snap.Busy, m.snap.Quiet = false, false
	run(m.publishState())
	if len(sent) != 0 {
		t.Errorf("a quiet busy spell sent %q", sent)
	}
}

func TestAViewingPaneLeavesNotificationsToTheDriver(t *testing.T) {
	var sent []string
	notifyDesktop = func(_, body string) { sent = append(sent, body) }
	t.Cleanup(func() { notifyDesktop = desktop.Notify })

	m, _ := harness(t, 80, 24)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	viewing := true
	m.Viewing = func() bool { return viewing }
	send(m, tea.BlurMsg{})
	m.snap.Busy = true
	run(m.publishState())
	m.snap.Busy = false
	run(m.publishState())
	if len(sent) != 0 {
		t.Errorf("a viewing pane sent %q", sent)
	}

	viewing = false
	m.snap.Busy = true
	run(m.publishState())
	m.snap.Busy = false
	run(m.publishState())
	if len(sent) != 1 {
		t.Errorf("the driving pane sent %q, want one notification", sent)
	}
}

func TestWakeTurnsNotifyOnceWhenTheLastRunLands(t *testing.T) {
	var sent []string
	notifyDesktop = func(_, body string) { sent = append(sent, body) }
	t.Cleanup(func() { notifyDesktop = desktop.Notify })

	m, _ := harness(t, 80, 24)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	send(m, tea.BlurMsg{})
	wake := func(runs ...subagent.Run) {
		m.snap.Runs, m.snap.Woken = runs, true
		m.snap.Busy = true
		run(m.publishState())
		m.snap.Busy = false
		run(m.publishState())
	}
	research := subagent.Run{ID: 1, Name: "research", Background: true, State: subagent.Done}
	review := subagent.Run{ID: 2, Name: "review", Background: true, State: subagent.Running}

	wake(research, review)
	if len(sent) != 0 {
		t.Fatalf("a wake turn with a run still going sent %q", sent)
	}
	review.State = subagent.Failed
	wake(research, review)
	if want := []string{"2 subagents finished: research ✓, review ✗ failed"}; !slices.Equal(sent, want) {
		t.Errorf("sent %q, want %q", sent, want)
	}
}

func TestAHeldWakeTurnNotifiesOnceTheBatchHoldRunsOut(t *testing.T) {
	var sent []string
	notifyDesktop = func(_, body string) { sent = append(sent, body) }
	t.Cleanup(func() { notifyDesktop = desktop.Notify })

	m, _ := harness(t, 80, 24)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	send(m, tea.BlurMsg{})
	m.snap.Runs = []subagent.Run{{ID: 1, Agent: "pair", Background: true, State: subagent.Running}}
	m.snap.Woken = true
	wake := func() {
		m.snap.Busy = true
		run(m.publishState())
		m.snap.Busy = false
		run(m.publishState())
	}

	wake()
	clock = clock.Add(batchHold)
	wake()
	if len(sent) != 1 {
		t.Errorf("sent %q after the hold ran out, want one notification", sent)
	}
}

// Other panes move from the top-left dots into the box: a waiting one under NEEDS YOU, the rest
// under SESSIONS with this pane marked, and nothing left in the top bar's corner.
func TestOtherSessionsAreListedInTheBox(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	own := "pool-1"
	m.Registry.Publish(registry.Patch{Workspace: &own})
	m.others = []registry.Info{
		{PID: 1, Name: "aaa", Cwd: "/tmp/a", State: registry.Waiting},
		{PID: 2, Name: "zzz", Topic: "sidebar topic rows", Cwd: "/tmp/z", Workspace: "default", State: registry.Idle, Unseen: true},
	}
	m.setSidebar(false)
	settle(m)
	if rail := plainBox(m); !strings.Contains(rail, needGlyph+"1") || !strings.Contains(rail, sessionGlyph+"1") {
		t.Errorf("the rail reads\n%s\nwant one waiting pane under ! and one other under ◎", rail)
	}
	cmdS(m)
	box := plainBox(m)
	need, sessions := strings.Index(box, "NEEDS YOU"), strings.Index(box, "SESSIONS")
	if need < 0 || sessions < 0 {
		t.Fatalf("the open box has no NEEDS YOU or SESSIONS group:\n%s", box)
	}
	if a := strings.Index(box, "aaa"); a < need || a > sessions {
		t.Errorf("the waiting pane is not under NEEDS YOU:\n%s", box)
	}
	if !strings.Contains(box[sessions:], "here") || strings.Contains(box, "(this)") || !strings.Contains(box[sessions:], "zzz") || !strings.Contains(box[sessions:], "done") {
		t.Errorf("SESSIONS lacks this pane or the finished one:\n%s", box)
	}
	if strings.Contains(box[sessions:], "@") {
		t.Errorf("SESSIONS still shows a pane's workspace:\n%s", box)
	}
	if !strings.Contains(box[sessions:], "● sidebar topic rows") {
		t.Errorf("a named pane's row does not lead with its topic:\n%s", box)
	}
	for _, line := range strings.Split(box[sessions:], "\n") {
		if strings.Contains(line, "sidebar topic rows") && strings.Contains(line, "done") {
			t.Errorf("the state word shares the topic's line instead of the project line:\n%s", box)
		}
	}
	if !slices.ContainsFunc(strings.Split(box[sessions:], "\n"), func(line string) bool {
		return strings.HasPrefix(strings.TrimSpace(strings.Trim(line, " │")), "zzz") && strings.HasSuffix(strings.TrimRight(line, " │"), "done")
	}) {
		t.Errorf("the topic row has no project line under it with the state word flush right:\n%s", box)
	}
	if top := ansi.Strip(strings.Split(screen(m), "\n")[0]); strings.Contains(top, dot) {
		t.Errorf("the top bar still draws session dots: %q", top)
	}
}

func TestSessionsWithNoUIAreListedUnderBackground(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	m.others = []registry.Info{
		{PID: 1, Name: "pane", State: registry.Working},
		{PID: 2, Name: "headless", Worker: "ABC-42", Stage: "plan", WorkerState: "stalled", Workspace: "pool-3", State: registry.Idle},
		{PID: 3, Name: "orphan", Detached: true, State: registry.Idle},
	}
	m.sidebarOpen = true
	settle(m)
	box := plainBox(m)
	background, sessions := strings.Index(box, "BACKGROUND"), strings.Index(box, "SESSIONS")
	if background < 0 || sessions < background {
		t.Fatalf("the box has no BACKGROUND group above SESSIONS:\n%s", box)
	}
	listed := box[background:sessions]
	for _, want := range []string{"ABC-42", "stalled", "plan", "pool-3@", "orphan"} {
		if !strings.Contains(listed, want) {
			t.Errorf("BACKGROUND lacks %q:\n%s", want, box)
		}
	}
	attached := box[sessions:]
	if strings.Contains(attached, "headless") || strings.Contains(attached, "orphan") || !strings.Contains(attached, "pane") {
		t.Errorf("SESSIONS should hold only the pane with a UI:\n%s", box)
	}
}

func TestALoneSessionListsNoSessions(t *testing.T) {
	m, _ := harness(t, 140, 30)
	m.Registry = registry.New(t.TempDir(), "here", "/tmp/here")
	m.sidebarOpen = true
	settle(m)
	if box := plainBox(m); strings.Contains(box, "SESSIONS") || !strings.Contains(box, "nothing running") {
		t.Errorf("with no other pane the box reads\n%s", box)
	}
}

// A finished run leaves at once: its result is already a box in the transcript.
func TestFinishedRunsLeaveTheBox(t *testing.T) {
	m, s := harness(t, 140, 24)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	roster.AddForTest(subagent.Run{Agent: "research", State: subagent.Done, Ended: time.Now()})
	m.sidebarOpen = true
	settle(m)

	if box := plainBox(m); !strings.Contains(box, "nothing running") {
		t.Errorf("a finished run is still in the box:\n%s", box)
	}
}

func rowContaining(t *testing.T, m *Model, want string) int {
	t.Helper()
	for i, line := range strings.Split(screen(m), "\n") {
		if strings.Contains(line, want) {
			return i
		}
	}
	t.Fatalf("no row contains %q:\n%s", want, screen(m))
	return -1
}
