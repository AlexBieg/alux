package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/clip"
	"github.com/alexbieg/alux/internal/sandbox"
)

func TestACommandUnderWithDirRunsThere(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := run(WithDir(context.Background(), dir), "pwd -P")
	if got := strings.TrimSpace(strings.TrimSuffix(out, "[exit 0]")); got != dir {
		t.Errorf("pwd under WithDir printed %q, want %q", got, dir)
	}
}

// The group kill is the whole reason RunBash does not use exec.CommandContext. "It got stuck"
// almost always means a grandchild is still running, and signalling only the direct child leaves
// it behind. This drives the case that actually happens: a shell that ignores SIGTERM itself and
// owns a background grandchild.
func TestCancelKillsTheWholeProcessGroup(t *testing.T) {
	cancelKillsTheGroup(t, context.Background(), t.TempDir())
}

// passThrough stands in for sandbox-exec: a parent that stays alive between RunBash and the command.
var passThrough = sandbox.Wrap{Args: []string{"bash", "-c", `"$@"; status=$?; exit $status`, "wrap"}}

func TestCancelKillsTheWholeProcessGroupUnderAWrap(t *testing.T) {
	dir := t.TempDir()
	cancelKillsTheGroup(t, WithDir(sandbox.With(context.Background(), passThrough), dir), dir)
}

func TestASandboxedDenialCarriesTheHint(t *testing.T) {
	t.Parallel()
	out := run(sandbox.With(context.Background(), passThrough), "echo 'touch: denied: Operation not permitted' >&2; exit 1")
	if !strings.Contains(out, "Operation not permitted") || !strings.Contains(out, "sandbox: false") {
		t.Errorf("a sandboxed denial did not carry the hint: %q", out)
	}
}

func TestAFailingStageEarlyInAPipeFailsTheCall(t *testing.T) {
	t.Parallel()
	if out := run(context.Background(), "false | tail -1"); !strings.HasSuffix(out, "[exit 1]") {
		t.Errorf("false | tail -1 reported %q, want [exit 1]", out)
	}
}

func TestAPipeCutOffByHeadSaysToUseTheHeadParameter(t *testing.T) {
	t.Parallel()
	if out := run(context.Background(), "yes | head -1"); !strings.Contains(out, "exit 141: a pipe stage was cut off early") {
		t.Errorf("yes | head -1 gave no SIGPIPE explanation: %q", out)
	}
}

func TestBashCommandsRunWithAFailingJJEditor(t *testing.T) {
	t.Parallel()
	for _, ctx := range []context.Context{context.Background(), sandbox.With(context.Background(), passThrough)} {
		if out := run(ctx, "echo $JJ_EDITOR"); !strings.HasPrefix(out, "false\n") {
			t.Errorf("JJ_EDITOR in a bash command = %q, want false", out)
		}
	}
}

func cancelKillsTheGroup(t *testing.T, parent context.Context, dir string) {
	t.Helper()
	was := killGrace
	killGrace = 100 * time.Millisecond
	t.Cleanup(func() { killGrace = was })

	started := filepath.Join(dir, "started")

	script := fmt.Sprintf(`
trap '' TERM
sleep 30 >/dev/null 2>&1 &
echo working
echo $! > %[1]q.tmp; mv %[1]q.tmp %[1]q
wait
`, started)

	ctx, cancel := context.WithCancel(parent)
	out := make(chan string, 1)
	go func() { out <- run(ctx, script) }()

	// Let the grandchild get going, so the test proves it was killed rather than never started.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, err := os.ReadFile(started)
	if err != nil {
		t.Fatal("grandchild never started; the test would pass vacuously")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("grandchild pid %q: %v", raw, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	cancelledAt := time.Now()
	cancel()

	select {
	case got := <-out:
		if waited := time.Since(cancelledAt); waited < killGrace {
			t.Errorf("returned after %v, before the %v grace: SIGKILL escalation was not what ended it", waited, killGrace)
		}
		if !strings.Contains(got, "working") {
			t.Errorf("partial output was lost, want it returned to the model: %q", got)
		}
		if !strings.Contains(got, "[cancelled by the user]") {
			t.Errorf("status trailer = %q, want a cancellation", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunBash did not return after cancellation")
	}

	deadline = time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatal("grandchild survived cancellation; only the direct child was signalled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExitCodeReachesTheModel(t *testing.T) {
	t.Parallel()
	got := run(context.Background(), "echo out; echo err >&2; exit 3")
	for _, want := range []string{"out", "err", "[exit 3]"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}

func run(ctx context.Context, command string) string {
	out, _ := RunBash(ctx, command, clip.DefaultHead, clip.DefaultTail)
	return out
}

// The tail is the assertion that matters: a test or build run reports why it failed last, and
// keeping only the head throws that away while still looking truncated.
func TestLongOutputKeepsBothEndsAndSavesTheMiddle(t *testing.T) {
	was := clip.SpillDir
	clip.SpillDir = t.TempDir()
	t.Cleanup(func() { clip.SpillDir = was })
	got, _ := RunBash(context.Background(), "echo FIRST-LINE; seq 1 20000; echo LAST-LINE", 3, 5)

	if !strings.HasPrefix(got, "FIRST-LINE\n1\n2\n[… ") {
		t.Errorf("the first 3 lines were not kept: %q", got[:min(len(got), 80)])
	}
	if !strings.HasSuffix(got, "19997\n19998\n19999\n20000\nLAST-LINE\n\n[exit 0]") {
		t.Errorf("the last 5 lines were not kept: %q", got[max(0, len(got)-80):])
	}
	files, _ := filepath.Glob(filepath.Join(clip.SpillDir, "*.log"))
	if len(files) != 1 || !strings.Contains(got, files[0]+", lines 4–19997 cut") {
		t.Fatalf("the notice does not name the spill file %v and the cut lines: %q", files, got)
	}
	saved, _ := os.ReadFile(files[0])
	if lines := strings.Count(string(saved), "\n"); lines != 20002 {
		t.Errorf("the spill file holds %d lines, want all 20002", lines)
	}
}
