package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/sandbox"
)

type buffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newShell(t *testing.T) *Shell {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func start(t *testing.T, s *Shell, command string, w *sandbox.Wrap) (*Call, *buffer) {
	t.Helper()
	var out buffer
	c, err := s.Start(context.Background(), command, w, &out)
	if err != nil {
		t.Fatal(err)
	}
	return c, &out
}

func run(t *testing.T, s *Shell, command string, w *sandbox.Wrap) (string, Result) {
	t.Helper()
	c, out := start(t, s, command, w)
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("%q did not finish", command)
	}
	return strings.TrimSpace(out.String()), c.Result()
}

func TestADirectoryChangeCarriesToTheNextCall(t *testing.T) {
	s := newShell(t)
	if err := os.Mkdir(filepath.Join(s.Root(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, s, "cd sub", nil)
	if out, _ := run(t, s, "pwd -P", nil); out != filepath.Join(s.Root(), "sub") {
		t.Fatalf("pwd = %q", out)
	}
}

func TestVariablesFunctionsAndAliasesCarry(t *testing.T) {
	s := newShell(t)
	run(t, s, "x=plain; export Y=exported; f() { echo fn; }; alias a='echo aliased'", nil)
	if out, _ := run(t, s, `echo "$x $Y"; f; a`, nil); out != "plain exported\nfn\naliased" {
		t.Fatalf("got %q", out)
	}
}

func TestAnExitStatusIsReportedAndTheShellSurvivesIt(t *testing.T) {
	s := newShell(t)
	if _, res := run(t, s, "false", nil); res.Status != "exit 1" || res.Lost {
		t.Fatalf("got %+v", res)
	}
	run(t, s, "kept=1", nil)
	if out, _ := run(t, s, "echo $kept", nil); out != "1" {
		t.Fatalf("got %q", out)
	}
}

func TestALostShellComesBackWithTheStateFromBeforeTheCall(t *testing.T) {
	s := newShell(t)
	if err := os.Mkdir(filepath.Join(s.Root(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, s, "cd sub; export KEPT=1", nil)
	_, res := run(t, s, "cd ..; export LOST=1; set -u; echo $unset_variable", nil)
	if !res.Lost {
		t.Fatalf("set -u should end the shell: %+v", res)
	}
	if out, _ := run(t, s, `pwd -P; echo "${KEPT:-}${LOST:-}"`, nil); out != filepath.Join(s.Root(), "sub")+"\n1" {
		t.Fatalf("got %q", out)
	}
}

func TestAShellThatLeavesItsFenceIsMovedBack(t *testing.T) {
	s := newShell(t)
	_, res := run(t, s, `cd "$(echo /usr)"`, nil)
	if len(res.Notes) == 0 || res.Dir != s.Root() {
		t.Fatalf("got %+v", res)
	}
	if out, _ := run(t, s, "pwd -P", nil); out != s.Root() {
		t.Fatalf("pwd = %q", out)
	}
}

func TestADetachedCommandKeepsRunningAndTheNextCallGetsAFreshShell(t *testing.T) {
	s := newShell(t)
	run(t, s, "export KEPT=1", nil)
	held, out := start(t, s, "export LOST=1; echo started; sleep 1; echo finished", nil)
	time.Sleep(300 * time.Millisecond)
	if !held.Detach() {
		t.Fatal("a running call should detach")
	}
	if got, res := run(t, s, `echo "${KEPT:-}${LOST:-}"`, nil); got != "1" || res.Lost {
		t.Fatalf("got %q %+v", got, res)
	}
	select {
	case <-held.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the detached command did not finish")
	}
	if got := out.String(); got != "started\nfinished\n" || held.Result().Status != "exit 0" {
		t.Fatalf("got %q %+v", got, held.Result())
	}
}

func TestCancelKillsTheCommandAndLosesTheShell(t *testing.T) {
	s := newShell(t)
	c, _ := start(t, s, "sleep 30", nil)
	time.Sleep(100 * time.Millisecond)
	c.Cancel("cancelled by the user")
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the call")
	}
	if res := c.Result(); res.Status != "cancelled by the user" || !res.Lost {
		t.Fatalf("got %+v", res)
	}
	if out, _ := run(t, s, "echo again", nil); out != "again" {
		t.Fatalf("got %q", out)
	}
}

func TestExecIsReportedWhenTheCommandEndsNotWhenTheShellIsReplaced(t *testing.T) {
	s := newShell(t)
	began := time.Now()
	_, res := run(t, s, "exec sleep 1", nil)
	if time.Since(began) < time.Second || res.Status != "exit 0" || !res.Lost {
		t.Fatalf("after %s: %+v", time.Since(began), res)
	}
}

func TestRestartReturnsToTheRootWithTheBaseEnvironment(t *testing.T) {
	s := newShell(t)
	run(t, s, "cd /tmp; export GONE=1", nil)
	s.Restart()
	if out, _ := run(t, s, `pwd -P; echo "${GONE:-unset}"`, nil); out != s.Root()+"\nunset" {
		t.Fatalf("got %q", out)
	}
}

func TestAForkStartsWhereTheShellIsButChangesOnlyItself(t *testing.T) {
	s := newShell(t)
	run(t, s, "cd /tmp; export A=1", nil)
	f, err := s.Fork()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	if out, _ := run(t, f, `pwd -P; echo "${A:-}"; export B=1`, nil); out != "/private/tmp\n1" {
		t.Fatalf("got %q", out)
	}
	if out, _ := run(t, s, `echo "${B:-unset}"`, nil); out != "unset" {
		t.Fatalf("got %q", out)
	}
}

func TestACarriedEnvironmentNeverRunsCodeInANewShell(t *testing.T) {
	got := carried([]string{"A=1", "BASH_ENV=/x", "ENV=/y", "BASH_FUNC_f%%=() { :; }", "SHLVL=2", "_=/bin/x", "PWD=/z"})
	if len(got) != 1 || got[0] != "A=1" {
		t.Fatalf("got %q", got)
	}
}
