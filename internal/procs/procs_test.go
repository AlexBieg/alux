package procs

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexbieg/alux/internal/clip"
	"github.com/alexbieg/alux/internal/sandbox"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestOutputIsReadableWhileTheProcessIsStillRunning(t *testing.T) {
	t.Parallel()
	p, err := Start(context.Background(), "echo first; sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()

	waitFor(t, "the first line", func() bool { return strings.Contains(p.Buffered(), "first") })
	if p.Exited() {
		t.Fatal("process exited; it should still be running")
	}
	if got := p.Status(); got != "running" {
		t.Fatalf("status = %q, want running", got)
	}
}

func TestEachReadReturnsOnlyWhatIsNew(t *testing.T) {
	t.Parallel()
	p, err := Start(context.Background(), "echo one; sleep 0.2; echo two; sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()

	waitFor(t, "the first line", func() bool { return strings.Contains(p.Buffered(), "one") })
	if first := p.Take(); !strings.Contains(first, "one") {
		t.Fatalf("first read = %q, want it to contain one", first)
	}
	waitFor(t, "the second line", func() bool { return strings.Contains(p.Buffered(), "two") })

	second := p.Take()
	if !strings.Contains(second, "two") {
		t.Fatalf("second read = %q, want it to contain two", second)
	}
	if strings.Contains(second, "one") {
		t.Fatalf("second read = %q, want the already-read line gone", second)
	}
}

func TestBackgroundCommandsRunWithAFailingJJEditor(t *testing.T) {
	t.Parallel()
	p, err := Start(context.Background(), "echo $JJ_EDITOR", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the exit", p.Exited)
	if got := strings.TrimSpace(p.Buffered()); got != "false" {
		t.Errorf("JJ_EDITOR in a background command = %q, want false", got)
	}
}

func TestKillTakesGrandchildrenWithIt(t *testing.T) {
	t.Parallel()
	killTakesGrandchildren(t, nil)
}

func TestKillTakesGrandchildrenWithItUnderAWrap(t *testing.T) {
	t.Parallel()
	killTakesGrandchildren(t, &sandbox.Wrap{Args: []string{"bash", "-c", `"$@"; status=$?; exit $status`, "wrap"}})
}

func killTakesGrandchildren(t *testing.T, w *sandbox.Wrap) {
	t.Helper()
	p, err := Start(context.Background(), "sleep 300 & echo $!; wait", w)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the grandchild pid", func() bool { return strings.TrimSpace(p.Buffered()) != "" })

	pid, err := strconv.Atoi(strings.TrimSpace(p.Buffered()))
	if err != nil {
		t.Fatalf("reading grandchild pid from %q: %v", p.Buffered(), err)
	}
	if !alive(pid) {
		t.Fatalf("grandchild %d was never running", pid)
	}

	p.Kill()
	waitFor(t, "the grandchild to die", func() bool { return !alive(pid) })
}

func TestCancellingTheContextKillsTheProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, "sleep 300", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()

	cancel()
	waitFor(t, "the process to die", p.Exited)
	if got := p.Status(); got != "killed" {
		t.Fatalf("status = %q, want killed", got)
	}
}

func TestAnExitIsQueuedAndWakesTheSession(t *testing.T) {
	t.Parallel()
	woken := make(chan struct{}, 1)
	r := NewRegistry()
	r.OnExit = func() { woken <- struct{}{} }

	p, err := Start(context.Background(), "echo done; exit 3", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Adopt(p, "call_1")

	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("OnExit never fired")
	}

	exits := r.TakeExits()
	if len(exits) != 1 {
		t.Fatalf("got %d exits, want 1", len(exits))
	}
	if exits[0].ID != id {
		t.Fatalf("exit id = %d, want %d", exits[0].ID, id)
	}
	if exits[0].Status != "exit 3" {
		t.Fatalf("status = %q, want exit 3", exits[0].Status)
	}
	if !strings.Contains(exits[0].Output, "done") {
		t.Fatalf("output = %q, want it to contain done", exits[0].Output)
	}
	if rest := r.TakeExits(); len(rest) != 0 {
		t.Fatalf("got %d exits on a second drain, want the first to have consumed them", len(rest))
	}
}

func TestAProcessAdoptedBeforeAClearDoesNotReportAfterIt(t *testing.T) {
	t.Parallel()
	fired := make(chan struct{}, 1)
	r := NewRegistry()
	r.OnExit = func() { fired <- struct{}{} }

	p, err := Start(context.Background(), "sleep 300", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Adopt(p, "call_1")
	r.DiscardExits()

	p.Kill()
	waitFor(t, "the process to die", p.Exited)

	select {
	case <-fired:
		t.Fatal("a process from the cleared conversation reported into the new one")
	case <-time.After(250 * time.Millisecond):
	}
	if exits := r.TakeExits(); len(exits) != 0 {
		t.Fatalf("got %d exits, want none after a clear", len(exits))
	}
}

func TestKillAllStopsEveryLiveProcess(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	for range 3 {
		p, err := Start(context.Background(), "sleep 300", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Adopt(p, "call_1")
	}
	waitFor(t, "all three to register", func() bool { return len(r.Running()) == 3 })

	if n := r.KillAll(); n != 3 {
		t.Fatalf("killed %d, want 3", n)
	}
	waitFor(t, "the registry to empty", func() bool { return len(r.Running()) == 0 })
}

func TestTheBufferKeepsTheTailAndSaysWhatItDropped(t *testing.T) {
	t.Parallel()
	var r ring
	first := strings.Repeat("a", maxBuffered)
	if _, err := r.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("TAIL")); err != nil {
		t.Fatal(err)
	}

	out, missed := r.take()
	if missed != int64(len("TAIL")) {
		t.Fatalf("missed = %d, want %d", missed, len("TAIL"))
	}
	if !strings.HasSuffix(out, "TAIL") {
		t.Fatalf("output does not end with the newest bytes: %q", out[max(0, len(out)-20):])
	}
	if len(out) != maxBuffered {
		t.Fatalf("buffered %d bytes, want the cap of %d", len(out), maxBuffered)
	}
}

// The buffer holds more than one read is allowed to hand over, so a command that fills it does not
// spend the agent's context on everything it printed.
func TestAnAgentReadIsClippedToItsBudget(t *testing.T) {
	t.Parallel()
	p := &Proc{out: &ring{}}
	if _, err := p.out.Write([]byte(strings.Repeat("filler line\n", maxBuffered/12))); err != nil {
		t.Fatal(err)
	}

	out := p.Take()
	if len(out) > takeMaxChars+200 {
		t.Errorf("read is %d bytes, want roughly %d", len(out), takeMaxChars)
	}
	if !strings.Contains(out, "cut from the middle") {
		t.Error("an oversized read was not clipped, or did not say so")
	}
}

// The ring keeps only its newest 64 KB, so before the spill a long command lost its start.
func TestACommandThatOutrunsTheBufferKeepsItsFirstLines(t *testing.T) {
	was := clip.SpillDir
	clip.SpillDir = t.TempDir()
	t.Cleanup(func() { clip.SpillDir = was })
	p, err := Start(context.Background(), "echo FIRST; yes filler | head -100000; echo LAST", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()

	out := p.Take()
	if !strings.HasPrefix(out, "FIRST\n") || !strings.HasSuffix(out, "filler\nLAST\n") {
		t.Errorf("Take lost an end: starts %q, ends %q", out[:min(20, len(out))], out[max(0, len(out)-20):])
	}
	if !strings.Contains(out, "the full output is in "+clip.SpillDir) {
		t.Errorf("Take does not name the spill file")
	}
}

// A progress bar redraws one line thousands of times. The pane collapses those frames, and the
// agent's read is no more use with them left in.
func TestAnAgentReadKeepsOnlyTheLastFrameOfARedrawnLine(t *testing.T) {
	t.Parallel()
	p := &Proc{out: &ring{}}
	if _, err := p.out.Write([]byte("working 1%\rworking 2%\rworking 3%\ndone\n")); err != nil {
		t.Fatal(err)
	}

	if got, want := p.Take(), "working 3%\ndone\n"; got != want {
		t.Errorf("Take = %q, want %q", got, want)
	}
}

// Two readers share one buffer: the pane, which redraws the tail several times a second, and the
// tool the agent reads output with. Only the tool's read may consume, or whichever of them looked
// last would decide what the other one ever sees.
func TestWatchingAProcessTakesNoOutputFromTheAgent(t *testing.T) {
	t.Parallel()
	p, err := Start(context.Background(), "echo one; echo two; sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()

	waitFor(t, "both lines", func() bool { return strings.Contains(p.Buffered(), "two") })
	for range 5 {
		p.Tail(TailLines)
	}

	out := p.Take()
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the agent's read = %q, want it to still contain %q after the pane watched", out, want)
		}
	}
	if got := p.Tail(TailLines); !strings.Contains(got, "two") {
		t.Fatalf("tail = %q, want the pane to still see output the agent has taken", got)
	}
}

// The tail is bounded in lines because it is copied into every snapshot, and a snapshot is taken
// on every change for any reason.
func TestTheTailIsTheLastFewLinesOnly(t *testing.T) {
	t.Parallel()
	var r ring
	for i := range 40 {
		if _, err := r.Write([]byte(strconv.Itoa(i) + "\n")); err != nil {
			t.Fatal(err)
		}
	}

	got := r.tail(8)
	if lines := strings.Count(got, "\n") + 1; lines != 8 {
		t.Fatalf("tail is %d lines, want 8:\n%s", lines, got)
	}
	if got != "32\n33\n34\n35\n36\n37\n38\n39" {
		t.Fatalf("tail = %q, want the last eight lines with no blank one on the end", got)
	}
}

// A line is not itself bounded, so the line count alone is not a bound: a progress bar redraws one
// line with carriage returns and can write the whole buffer without ever ending it.
func TestAnUnbrokenLineStillComesBackShort(t *testing.T) {
	t.Parallel()
	var r ring
	for range 200 {
		if _, err := r.Write([]byte(strings.Repeat("=", 100) + " 50%\r")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Write([]byte("done")); err != nil {
		t.Fatal(err)
	}

	if got := r.tail(TailLines); got != "done" {
		t.Fatalf("tail = %q (%d bytes), want only what the last carriage return left standing", got, len(got))
	}
}

// Where the byte bound cuts is decided by a byte count, so in anything but ASCII it lands inside a
// rune about as often as not, and a broken rune puts the cell grid out of step with the screen.
func TestABoundedTailIsStillValidText(t *testing.T) {
	t.Parallel()
	var r ring
	// Three bytes each, against a bound that is not a multiple of three.
	if _, err := r.Write([]byte(strings.Repeat("→", maxTail))); err != nil {
		t.Fatal(err)
	}

	got := r.tail(TailLines)
	if len(got) > maxTail {
		t.Fatalf("tail is %d bytes, want no more than the bound of %d", len(got), maxTail)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("tail starts mid-rune: %q", got[:min(12, len(got))])
	}
}

func TestExitBlockNamesEveryFinishedCommand(t *testing.T) {
	t.Parallel()
	block := ExitBlock([]Exit{
		{ID: 1, Command: "go test ./...", Status: "exit 1", Output: "FAIL"},
		{ID: 2, Command: "npm run build", Status: "exit 0"},
	})
	for _, want := range []string{"<background-commands>", "proc 1", "go test ./...", "exit 1", "FAIL", "proc 2", "no further output"} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
	if ExitBlock(nil) != "" {
		t.Fatal("an empty batch should render nothing")
	}
}
