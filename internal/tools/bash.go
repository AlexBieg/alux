package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/clip"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/shell"
)

const (
	bashTimeout  = 120 * time.Second
	bashMaxChars = 30_000
	// 128+SIGPIPE: under pipefail, what a stage writing into an early-exiting | head reports.
	sigpipeExit = 141
)

// How long a cancelled command gets to die politely before it is killed outright.
var killGrace = 2 * time.Second

// RunShell runs a command in the agent's live shell, killing it, and so the shell, past bashTimeout.
// It returns the output with its trailer, and how the command ended, which is empty when it never started.
func RunShell(ctx context.Context, sh *shell.Shell, command string, head, tail int) (string, string) {
	out := clip.NewSpill(bashMaxChars)
	defer out.Close()
	w, sandboxed := sandbox.From(ctx)
	var wrap *sandbox.Wrap
	if sandboxed {
		wrap = &w
	}
	c, err := sh.Start(ctx, command, wrap, out)
	if err != nil {
		return err.Error(), ""
	}
	timer := time.NewTimer(bashTimeout)
	defer timer.Stop()
	select {
	case <-c.Done():
	case <-ctx.Done():
		c.Cancel("cancelled by the user")
	case <-timer.C:
		c.Cancel("timed out")
	}
	<-c.Done()
	out.Close()
	result := c.Result()
	status := result.Trailer(sh.Root())
	if strings.HasPrefix(status, fmt.Sprintf("exit %d", sigpipeExit)) {
		status += "; a pipe stage was cut off early, usually by | head; the head and tail parameters trim output without that"
	}
	return wrap.Hint(strings.TrimSpace(out.Whole(head, tail) + "\n[" + status + "]")), result.Status
}

// RunBash runs a command in a fresh shell and returns its combined output with a status trailer.
// It never returns an error: a failed command is a result the model should see, not a tool fault.
//
// The command gets its own process group so cancelling takes its children (npm, docker, tail…)
// with it. That group kill is the feature — "it got stuck" almost always means a grandchild is
// still running — and it is why this does not simply use exec.CommandContext, which signals only
// the direct child.
//
// Output past head+tail lines or the character budget keeps its first head and last tail lines,
// and the whole of it is saved to a file the notice names.
//
// The second value is how the command ended, as RunShell's is.
func RunBash(ctx context.Context, command string, head, tail int) (string, string) {
	cmd, sandboxed := sandbox.Cmd(ctx, "set -o pipefail\n"+command)
	cmd.Dir = Dir(ctx)
	// Setpgid makes the child a group leader, so a negative pid signals the whole group below.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out := clip.NewSpill(bashMaxChars)
	defer out.Close()
	cmd.Stdout = out
	cmd.Stderr = out
	// stdin is left nil so commands cannot steal keystrokes from the TUI.

	if err := cmd.Start(); err != nil {
		return err.Error(), ""
	}

	grace := killGrace
	var cancelled, timedOut bool
	var stateMu sync.Mutex
	done := make(chan struct{})

	kill := func(markTimeout bool) {
		stateMu.Lock()
		if markTimeout {
			timedOut = true
		} else {
			cancelled = true
		}
		stateMu.Unlock()
		pgid := -cmd.Process.Pid
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		go func() {
			select {
			case <-time.After(grace):
				_ = syscall.Kill(pgid, syscall.SIGKILL)
			case <-done:
			}
		}()
	}

	go func() {
		timer := time.NewTimer(bashTimeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			kill(false)
		case <-timer.C:
			kill(true)
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)

	out.Close()
	text := out.Whole(head, tail)

	stateMu.Lock()
	wasCancelled, wasTimeout := cancelled, timedOut
	stateMu.Unlock()

	var status, note string
	switch {
	case wasCancelled:
		status = "cancelled by the user"
	case wasTimeout:
		status = "timed out"
	case waitErr != nil && cmd.ProcessState.ExitCode() < 0:
		status = fmt.Sprintf("killed by %v", cmd.ProcessState.Sys())
	default:
		status = fmt.Sprintf("exit %d", cmd.ProcessState.ExitCode())
		if cmd.ProcessState.ExitCode() == sigpipeExit {
			note = ": a pipe stage was cut off early, usually by | head; the head and tail parameters trim output without that"
		}
	}
	result := strings.TrimSpace(text + "\n[" + status + note + "]")
	if w, ok := sandbox.From(ctx); ok && sandboxed {
		return w.Hint(result), status
	}
	return result, status
}
