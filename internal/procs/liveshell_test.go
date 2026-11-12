package procs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/anthropics/anthropic-sdk-go"
)

func liveShell(t *testing.T) context.Context {
	t.Helper()
	sh, err := shell.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	return shell.With(context.Background(), sh)
}

func bash(t *testing.T, r *Registry, ctx context.Context, input map[string]any) string {
	t.Helper()
	return text(t, r.Bash()(ctx, call(t, "bash", input), approving()))
}

type passAll struct{}

func (passAll) Screen(context.Context, string) guardian.Verdict {
	return guardian.Verdict{Agent: "test", Top: guardian.Read, Threshold: 0.5}
}
func (passAll) Record(string, guardian.Verdict, string) {}
func (passAll) Suggest(context.Context, string, []string) guardian.Suggestion {
	return guardian.Suggestion{}
}
func (passAll) RecordSuggested(string, guardian.Verdict, guardian.Suggestion, string, string) {}

func TestAFinishedCommandHandsOnCallItsBareExitStatus(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	ctx := liveShell(t)
	var exit string
	cb := approving()
	cb.OnCall = func(c agent.Call, _ anthropic.ContentBlockParamUnion) { exit = c.Exit }
	a := &agent.Agent{Handlers: map[string]agent.Handler{"bash": r.Bash()}, Screen: passAll{}}

	for command, want := range map[string]string{
		"mkdir -p sub && cd sub && echo moved": "exit 0",
		"false":                                "exit 1",
	} {
		exit = ""
		got := text(t, a.Run(ctx, call(t, "bash", map[string]any{"command": command}), cb))
		if exit != want {
			t.Fatalf("%s: OnCall saw Exit %q, want %q (result %q)", command, exit, want, got)
		}
	}
}

func TestAHeldCommandTakesItsShellWithItAndTheNextCallGetsANewOne(t *testing.T) {
	withForegroundLimit(t, 300*time.Millisecond)
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	ctx := liveShell(t)

	bash(t, r, ctx, map[string]any{"command": "cd /tmp; export KEPT=1"})
	held := bash(t, r, ctx, map[string]any{"command": "echo started; sleep 2; echo finished"})
	if !strings.Contains(held, "started") || !strings.Contains(held, "still running") {
		t.Fatalf("the command was not held: %q", held)
	}
	p, ok := r.Get(1)
	if !ok {
		t.Fatal("the held command was not registered")
	}
	if got := bash(t, r, ctx, map[string]any{"command": `pwd -P; echo "${KEPT:-}"`}); !strings.HasPrefix(got, "/private/tmp\n1") {
		t.Fatalf("the next call did not start where the shell was: %q", got)
	}
	if p.Exited() {
		t.Fatal("the next call waited for the held command instead of getting a shell of its own")
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the held command did not finish")
	}
	if out := p.Final(); !strings.Contains(out, "finished") || !strings.Contains(out, "exit 0") {
		t.Fatalf("the held command's output was lost: %q", out)
	}
}

func TestAGhReadPollRunsThePinnedGhNotOneTheShellPutOnItsPath(t *testing.T) {
	pinned, fake := t.TempDir(), t.TempDir()
	fakeRan := filepath.Join(fake, "ran")
	writeScript(t, filepath.Join(pinned, "gh"), "echo pinned-gh \"$@\"")
	writeScript(t, filepath.Join(fake, "gh"), "touch '"+fakeRan+"'; echo fake-gh")
	base := os.Getenv("PATH")
	t.Setenv("PATH", pinned+string(os.PathListSeparator)+base)
	if _, ok := agent.GhRead("gh pr checks 1 --repo o/r"); !ok {
		t.Fatal("no gh was pinned")
	}
	// A gh that turns up first on the path after the pin, as a mise shim would.
	t.Setenv("PATH", fake+string(os.PathListSeparator)+base)
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	ctx := liveShell(t)

	bash(t, r, ctx, map[string]any{"command": "export PATH='" + fake + "':$PATH"})
	got := text(t, r.runPoll(ctx, call(t, "poll", map[string]any{"command": "gh pr checks 7 --repo o/r",
		"sandbox": false, "background": false, "until": "gh"}), approving()))
	if _, err := os.Stat(fakeRan); err == nil || !strings.Contains(got, "pinned-gh pr checks 7 --repo o/r") {
		t.Fatalf("the shell's gh ran, or the pinned one did not: %q", got)
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestACallOutsideTheSandboxStartsWhereTheShellIsWithoutItsExports(t *testing.T) {
	r := NewRegistry()
	ctx := liveShell(t)

	bash(t, r, ctx, map[string]any{"command": "cd /tmp; export CARRIED=1"})
	got := bash(t, r, ctx, map[string]any{"command": `pwd -P; echo "${CARRIED:-unset}"`, "sandbox": false})
	if !strings.HasPrefix(got, "/private/tmp\nunset") {
		t.Fatalf("got %q", got)
	}
}
