// Package shell keeps one live bash per agent, so a cd, a variable, a function or an alias carries
// from one call to the next. Whenever the process is lost or its sandbox profile changes, the next
// one starts from the directory and exported environment left by the last call that finished.
package shell

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/sandbox"
)

// The command arrives on fd 4 and the answer leaves on fd 3; both are closed to the command itself,
// so nothing it leaves running can hold the control pipe or read the next command.
const prelude = `set -o pipefail
shopt -s expand_aliases
while IFS= builtin read -r -d '' __alux_cmd <&4 && IFS= builtin read -r -d '' __alux_out <&4; do
	builtin eval "$__alux_cmd" </dev/null >|"$__alux_out" 2>&1 3>&- 4<&-
	__alux_st=$?
	builtin set +eCux -o pipefail
	builtin trap - EXIT ERR DEBUG RETURN
	builtin printf 'done %s\0%s\0' "$__alux_st" "$(builtin pwd -P 2>/dev/null)" >&3
	/usr/bin/env -0 >&3
	builtin printf '\0' >&3
done
`

var (
	// How long a signalled shell gets to die politely before it is killed outright.
	killGrace = 2 * time.Second
	tick      = 50 * time.Millisecond
	// maxOutput stops a call whose output would otherwise fill the temp directory.
	maxOutput int64 = 256 << 20
)

// Shell is one agent's live bash. Calls run one at a time.
type Shell struct {
	root  string
	fence []string
	dir   string

	// busy is held by the running call; live is touched only by its holder.
	busy chan struct{}
	live *proc

	mu   sync.Mutex
	snap snapshot
}

type snapshot struct {
	dir string
	env []string
}

// New makes a shell rooted at root; its process starts with the first call.
func New(root string) (*Shell, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("the shell's root: %w", err)
	}
	fence := []string{resolved}
	for _, tmp := range []string{os.TempDir(), "/private/tmp"} {
		if r, err := filepath.EvalSymlinks(tmp); err == nil {
			fence = append(fence, r)
		}
	}
	dir, err := os.MkdirTemp("", "alux-shell-")
	if err != nil {
		return nil, err
	}
	return &Shell{root: resolved, fence: fence, dir: dir, busy: make(chan struct{}, 1),
		snap: snapshot{dir: resolved, env: sandbox.Env()}}, nil
}

type key struct{}

// With makes s the shell of every bash call run under ctx.
func With(ctx context.Context, s *Shell) context.Context { return context.WithValue(ctx, key{}, s) }

// From is the shell With put on ctx, if any.
func From(ctx context.Context) (*Shell, bool) {
	s, ok := ctx.Value(key{}).(*Shell)
	return s, ok && s != nil
}

// Fork is a new shell with s's root, directory and exported environment, and a process of its own.
func (s *Shell) Fork() (*Shell, error) {
	f, err := New(s.root)
	if err != nil {
		return nil, err
	}
	f.write(s.read())
	return f, nil
}

// Root is where the shell starts and where it returns after leaving its fence.
func (s *Shell) Root() string { return s.root }

// Dir is the directory the next call starts in.
func (s *Shell) Dir() string { return s.read().dir }

// Env is the environment the last finished call left, for a fresh process that should share it.
func (s *Shell) Env() []string { return carried(s.read().env) }

func (s *Shell) read() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *Shell) write(snap snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
}

// Restart ends the process and forgets everything, so the next call starts at the root.
func (s *Shell) Restart() {
	s.busy <- struct{}{}
	defer func() { <-s.busy }()
	s.stop()
	s.write(snapshot{dir: s.root, env: sandbox.Env()})
}

// Close ends the process and removes the shell's output files.
func (s *Shell) Close() {
	s.busy <- struct{}{}
	defer func() { <-s.busy }()
	s.stop()
	_ = os.RemoveAll(s.dir)
}

func (s *Shell) stop() {
	if s.live != nil {
		s.live.end()
		s.live = nil
	}
}

type proc struct {
	cmd    *exec.Cmd
	wrap   *sandbox.Wrap
	in     *os.File
	frames chan []string
	done   chan struct{}
}

func (s *Shell) spawn(w *sandbox.Wrap) (*proc, string, error) {
	note := ""
	snap := s.read()
	if info, err := os.Stat(snap.dir); err != nil || !info.IsDir() {
		note = fmt.Sprintf("%s no longer exists, so the shell started at %s", snap.dir, s.root)
		snap.dir = s.root
		s.write(snap)
	}
	cmd := sandbox.Bash(w, "--noprofile", "--norc", "-c", prelude)
	cmd.Dir = snap.dir
	cmd.Env = append(carried(snap.env), "PWD="+snap.dir)
	// Setsid leaves the shell no terminal, so a command that opens /dev/tty fails rather than stops.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		ctlR.Close()
		ctlW.Close()
		return nil, "", err
	}
	cmd.ExtraFiles = []*os.File{ctlW, cmdR}
	err = cmd.Start()
	ctlW.Close()
	cmdR.Close()
	if err != nil {
		ctlR.Close()
		cmdW.Close()
		return nil, "", err
	}
	p := &proc{cmd: cmd, wrap: w, in: cmdW, frames: make(chan []string), done: make(chan struct{})}
	go p.read(ctlR)
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, note, nil
}

// read turns the control pipe into one frame per call: status, directory, then the environment.
func (p *proc) read(ctl *os.File) {
	defer close(p.frames)
	defer ctl.Close()
	r := bufio.NewReader(ctl)
	for {
		var frame []string
		for {
			tok, err := r.ReadString(0)
			if err != nil {
				return
			}
			tok = strings.TrimSuffix(tok, "\x00")
			if tok == "" && len(frame) >= 2 {
				break
			}
			frame = append(frame, tok)
		}
		p.frames <- frame
	}
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *proc) signal(sig syscall.Signal) { _ = syscall.Kill(-p.cmd.Process.Pid, sig) }

// kill takes the shell's whole process group, so whatever a command started goes with it.
func (p *proc) kill() {
	if p.exited() {
		return
	}
	p.signal(syscall.SIGTERM)
	go func() {
		select {
		case <-time.After(killGrace):
			p.signal(syscall.SIGKILL)
		case <-p.done:
		}
	}()
}

// end lets an idle shell leave on its own, and kills it if it does not.
func (p *proc) end() {
	p.in.Close()
	select {
	case <-p.done:
	case <-time.After(killGrace):
		p.kill()
	}
}

// send runs one command and returns its frame, or false once the shell is gone.
func (p *proc) send(command, out string) ([]string, bool) {
	if _, err := io.WriteString(p.in, command+"\x00"+out+"\x00"); err != nil {
		return nil, false
	}
	frame, ok := <-p.frames
	return frame, ok
}

// Result is how a call ended.
type Result struct {
	// Status is "exit N", or how the shell itself ended when the call took it down.
	Status string
	// Lost is set when the shell ended during the call, so its changes are gone.
	Lost bool
	// Notes are what the agent should be told about its shell, beyond the status.
	Notes []string
	// Dir is where the next call starts.
	Dir string
}

// Trailer is the status line the agent reads: how it ended, where the shell is now when that is
// not root, and the notes.
func (r Result) Trailer(root string) string {
	parts := []string{r.Status}
	if r.Dir != "" && r.Dir != root {
		where := r.Dir
		if rel, err := filepath.Rel(root, r.Dir); err == nil && !strings.HasPrefix(rel, "..") {
			where = rel
		}
		parts[0] += " · in " + where
	}
	return strings.Join(append(parts, r.Notes...), "; ")
}

// Call is one command running in the shell.
type Call struct {
	s    *Shell
	p    *proc
	out  string
	done chan struct{}

	stopTail chan struct{}
	tailed   chan struct{}

	mu        sync.Mutex
	detached  bool
	cancelled string
	res       Result
}

// ErrNUL refuses a command bash could not receive whole.
var ErrNUL = errors.New("the command contains a NUL byte, which a shell cannot receive")

// Start runs command under w, writing its output to sink as it arrives. The call holds the shell
// until it finishes or is detached.
func (s *Shell) Start(ctx context.Context, command string, w *sandbox.Wrap, sink io.Writer) (*Call, error) {
	if strings.ContainsRune(command, 0) {
		return nil, ErrNUL
	}
	select {
	case s.busy <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var notes []string
	if s.live != nil && (s.live.exited() || !sandbox.Same(s.live.wrap, w)) {
		s.stop()
	}
	if s.live == nil {
		p, note, err := s.spawn(w)
		if err != nil {
			<-s.busy
			return nil, err
		}
		s.live = p
		if note != "" {
			notes = append(notes, note)
		}
	}
	f, err := os.CreateTemp(s.dir, "call-")
	if err != nil {
		<-s.busy
		return nil, err
	}
	f.Close()

	c := &Call{s: s, p: s.live, out: f.Name(), done: make(chan struct{}),
		stopTail: make(chan struct{}), tailed: make(chan struct{})}
	c.res.Notes = notes
	go c.tail(sink)
	go c.wait(command)
	return c, nil
}

// Done closes once the command has finished and all its output has reached the sink.
func (c *Call) Done() <-chan struct{} { return c.done }

// Result is how the call ended; read it after Done.
func (c *Call) Result() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.res
}

// Cancel kills the shell and everything it started; why becomes the call's status.
func (c *Call) Cancel(why string) {
	c.mu.Lock()
	if c.cancelled == "" {
		c.cancelled = why
	}
	c.mu.Unlock()
	c.p.kill()
}

// Detach hands the running command, shell and all, to the caller, and frees the agent's shell to
// start a new process from the state before this call. It reports false once the call has finished.
func (c *Call) Detach() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished() {
		return false
	}
	c.detached = true
	if c.s.live == c.p {
		c.s.live = nil
	}
	// Closing the command pipe lets the loop end once the command does, rather than wait for another.
	c.p.in.Close()
	<-c.s.busy
	return true
}

// Pid is the shell's process, which is also its process group.
func (c *Call) Pid() int { return c.p.cmd.Process.Pid }

func (c *Call) finished() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Call) wait(command string) {
	frame, ok := c.p.send(command, c.out)
	if !ok {
		// The control pipe closes when the shell dies, and also when exec replaced it with the command.
		<-c.p.done
	}
	close(c.stopTail)
	<-c.tailed
	_ = os.Remove(c.out)

	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.cancelled != "":
		c.res.Status = c.cancelled
		c.res.Lost = true
	case ok:
		c.res.Status = "exit " + strings.TrimPrefix(frame[0], "done ")
	default:
		c.res.Status = exitOf(c.p.cmd)
		c.res.Lost = true
	}
	if c.detached {
		close(c.done)
		return
	}
	s := c.s
	if c.res.Lost {
		if s.live == c.p {
			s.live = nil
		}
		c.res.Notes = append(c.res.Notes, "the shell ended during this call, so the next call gets a new one in "+
			s.read().dir+" with the exported variables from before it; functions, aliases and this call's changes are gone")
	} else {
		s.write(snapshot{dir: frame[1], env: frame[2:]})
		if !s.inside(frame[1]) {
			c.res.Notes = append(c.res.Notes, s.home(frame[1]))
		}
	}
	c.res.Dir = s.read().dir
	<-s.busy
	close(c.done)
}

// home moves a shell that left its fence, or whose directory was deleted, back to the root.
func (s *Shell) home(was string) string {
	why := was + " is outside " + s.root + " and the temp directories"
	if was == "" {
		why = "the shell's directory was deleted"
	}
	snap := s.read()
	snap.dir = s.root
	if frame, ok := s.live.send("builtin cd -- "+quote(s.root), os.DevNull); ok {
		snap.env = frame[2:]
	} else {
		s.live = nil
	}
	s.write(snap)
	return why + ", so the shell moved back to " + s.root
}

func (s *Shell) inside(dir string) bool {
	if dir == "" {
		return false
	}
	for _, f := range s.fence {
		if dir == f || strings.HasPrefix(dir, f+"/") {
			return true
		}
	}
	return false
}

// tail copies the output file to sink while the command runs, and stops a runaway at maxOutput.
func (c *Call) tail(sink io.Writer) {
	defer close(c.tailed)
	f, err := os.Open(c.out)
	if err != nil {
		return
	}
	defer f.Close()
	var written int64
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		n, _ := io.Copy(sink, f)
		written += n
		if written > maxOutput {
			c.Cancel(fmt.Sprintf("killed after %d MB of output", maxOutput>>20))
		}
		select {
		case <-c.stopTail:
			_, _ = io.Copy(sink, f)
			return
		case <-t.C:
		}
	}
}

// carried is an environment a new process can start from: bash recomputes its own bookkeeping, and
// a variable that makes bash run code at start is never carried into one.
func carried(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "_", name == "SHLVL", name == "PWD", name == "BASH_ENV", name == "ENV",
			strings.HasPrefix(name, "BASH_FUNC_"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

func exitOf(cmd *exec.Cmd) string {
	if cmd.ProcessState == nil {
		return "the shell ended"
	}
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return "exit " + strconv.Itoa(code)
	}
	return "killed by " + fmt.Sprint(cmd.ProcessState.Sys())
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
