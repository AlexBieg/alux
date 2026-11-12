package procs

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/clip"
	"github.com/alexbieg/alux/internal/netfail"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/wake"
)

const (
	minPollEvery     = 5 * time.Second
	defaultPollEvery = 30 * time.Second
	defaultPollFor   = 30 * time.Minute
	maxPollFor       = 6 * time.Hour
	// pollFailures in a row stop a poll: a check that cannot run will not start to.
	pollFailures = 3
	// pollShown is how much of one check's output the log keeps for each change.
	pollShown = 2000
	// maxOfflineWait caps the wait between checks that cannot reach the network, which doubles from Every.
	maxOfflineWait = 5 * time.Minute
)

// checkTimeout bounds one run of the check, which is meant to be a quick look, not the work itself.
var checkTimeout = time.Minute

// Poll is a check run on an interval until its output says to stop.
type Poll struct {
	Check string
	Every time.Duration
	For   time.Duration
	// Until stops at the first output it matches, While at the first it does not, neither at an exit 0.
	Until, While *regexp.Regexp
	// Unreachable replaces netfail's test for output that says the check could not reach the network.
	Unreachable *regexp.Regexp
	At          Where

	sleep func(ctx context.Context, d time.Duration) (asleep time.Duration, ok bool)
}

// unreachable is the line of output saying the check could not reach the network, or "" when it reached it.
func (s Poll) unreachable(output string) string {
	if s.Unreachable == nil {
		return netfail.Line(output)
	}
	loc := s.Unreachable.FindStringIndex(output)
	if loc == nil {
		return ""
	}
	line, _, _ := strings.Cut(output[strings.LastIndexByte(output[:loc[0]], '\n')+1:], "\n")
	if line = strings.TrimSpace(line); line == "" {
		return "(no output)"
	}
	return line
}

// wait sleeps d of awake time, cut short by the machine waking from sleep (asleep says how long) or by ctx ending (not ok).
func (s Poll) wait(ctx context.Context, d time.Duration) (asleep time.Duration, ok bool) {
	if s.sleep != nil {
		return s.sleep(ctx, d)
	}
	return wake.Sleep(ctx, d)
}

func (s Poll) describe() string {
	head := "poll every " + brief(s.Every)
	switch {
	case s.Until != nil:
		head += " until /" + s.Until.String() + "/"
	case s.While != nil:
		head += " while /" + s.While.String() + "/"
	default:
		head += " until exit 0"
	}
	return head + ", gives up after " + brief(s.For) + ": " + s.Check
}

func (s Poll) met(output string, code int) bool {
	switch {
	case s.Until != nil:
		return s.Until.MatchString(output)
	case s.While != nil:
		return !s.While.MatchString(output)
	}
	return code == 0
}

func (s Poll) condition() string {
	switch {
	case s.Until != nil:
		return "until /" + s.Until.String() + "/"
	case s.While != nil:
		return "while /" + s.While.String() + "/"
	}
	return "exit 0"
}

func (s Poll) unmet(code int) string {
	switch {
	case s.Until != nil:
		return s.condition() + " did not match"
	case s.While != nil:
		return s.condition() + " still matches"
	}
	return fmt.Sprintf("exit %d, waiting for exit 0", code)
}

// Stop is how a poll ended.
type Stop int

const (
	StopMet Stop = iota + 1
	StopTimedOut
	StopFailed
	StopKilled
)

// StartPoll runs s as a Proc whose output is a timestamped log of each change in the check's output.
func StartPoll(ctx context.Context, s Poll, w *sandbox.Wrap) *Proc {
	pctx, cancel := context.WithCancel(ctx)
	out := &ring{}
	p := &Proc{
		Command:   s.describe(),
		Started:   time.Now(),
		Sandboxed: w != nil,
		wrap:      w,
		Poll:      true,
		out:       out,
		done:      make(chan struct{}),
		checked:   make(chan struct{}),
		signal:    func(syscall.Signal) { cancel() },
	}
	first := func(unmet string) {
		p.unmet = unmet
		close(p.checked)
	}
	go func() {
		status, stop := s.run(pctx, out, w, first)
		cancel()
		p.mu.Lock()
		p.status, p.stop = status, stop
		p.mu.Unlock()
		out.finish()
		close(p.done)
	}()
	return p
}

// run checks until the condition is met or s.For of awake time has passed; time asleep is not charged to the limit.
// first gets why the condition is not met once the first check is logged, unless that check ended the poll.
func (s Poll) run(ctx context.Context, out *ring, w *sandbox.Wrap, first func(unmet string)) (string, Stop) {
	start := time.Now()
	deadline := start.Add(s.For)
	last, checks, failures := "", 0, 0
	var offlineSince time.Time
	var offlineWait, slept time.Duration
	took := func() string {
		d := short(time.Since(start))
		if slept > 0 {
			d += " awake (plus " + wake.Brief(slept) + " asleep)"
		}
		return d
	}
	for {
		checks++
		output, code, err := check(ctx, s.Check, w, s.At)
		if ctx.Err() != nil {
			return "stopped", StopKilled
		}
		now := time.Now()
		stamp := now.Format("15:04:05")
		line, unmet := "", ""
		if err == nil {
			line = s.unreachable(output)
		}
		switch {
		case err != nil:
			unmet = "the check failed, so " + s.condition() + " was not tested"
			failures++
			fmt.Fprintf(out, "%s check failed: %v\n", stamp, err)
			if failures >= pollFailures {
				return fmt.Sprintf("stopped: the check failed %d times in a row", failures), StopFailed
			}
		case line != "":
			unmet = "the check could not reach the network, so " + s.condition() + " was not tested"
			failures = 0
			if offlineSince.IsZero() {
				offlineSince = now
				fmt.Fprintf(out, "%s offline: %s\n", stamp, line)
			}
			if offlineWait == 0 {
				offlineWait = s.Every
			} else {
				offlineWait = min(2*offlineWait, max(maxOfflineWait, s.Every))
			}
		default:
			failures = 0
			if !offlineSince.IsZero() {
				fmt.Fprintf(out, "%s back online after %s\n", stamp, short(now.Sub(offlineSince)))
				offlineSince, offlineWait, last = time.Time{}, 0, ""
			}
			shown := output
			if code != 0 {
				shown = strings.TrimSpace(shown + fmt.Sprintf(" [exit %d]", code))
			}
			if shown == "" {
				shown = "(no output)"
			}
			if shown != last {
				fmt.Fprintf(out, "%s %s\n", stamp, shown)
				last = shown
			}
			if s.met(output, code) {
				return fmt.Sprintf("condition met after %s, %d checks", took(), checks), StopMet
			}
			unmet = s.unmet(code)
		}
		wait := s.Every
		if !offlineSince.IsZero() {
			wait = offlineWait
		}
		if time.Now().Add(wait).After(deadline) {
			gave := fmt.Sprintf("gave up after %s without the condition, %d checks", took(), checks)
			if !offlineSince.IsZero() {
				gave += ", offline since " + offlineSince.Format("15:04:05")
			}
			return gave, StopTimedOut
		}
		if checks == 1 && first != nil {
			first(unmet)
		}
		asleep, ok := s.wait(ctx, wait)
		if !ok {
			return "stopped", StopKilled
		}
		if asleep > 0 {
			slept += asleep
			offlineWait = 0
			fmt.Fprintf(out, "%s resumed after ~%s asleep\n", time.Now().Format("15:04:05"), wake.Brief(asleep))
		}
	}
}

// check runs the command once; a non-zero exit is its answer, not a failure, since grep and gh pr checks talk that way.
func check(ctx context.Context, command string, w *sandbox.Wrap, at Where) (string, int, error) {
	cmd := sandbox.Shell(command, w)
	at.apply(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return "", 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(checkTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return "", 0, fmt.Errorf("it ran past %s", short(checkTimeout))
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return "", 0, ctx.Err()
	}
	output := clip.Middle(strings.TrimSpace(cleanLine(buf.Bytes())), pollShown)
	return output, cmd.ProcessState.ExitCode(), nil
}
