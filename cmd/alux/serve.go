package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/attach"
	"github.com/alexbieg/alux/internal/commands"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/programstatus"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/resolve"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/ui"
	"github.com/alexbieg/alux/internal/workers"
	"github.com/charmbracelet/x/ansi"
)

// serveSession keeps the session running for whichever TUI attaches, until SIGTERM or SIGINT, or for a
// worker until it finishes.
func serveSession(s *session.Session, srv *attach.Server, run *workerRun) error {
	signal.Ignore(syscall.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	var finished <-chan workers.Final
	if run != nil {
		finished = run.finished
	}
	fmt.Fprintf(os.Stderr, "alux: serving pid %d; attach with: alux attach %d\n", os.Getpid(), os.Getpid())
	var err error
	select {
	case <-ctx.Done():
		if run != nil {
			err = workers.MarkStopped(run.key)
		}
	case err = <-served:
	case final := <-finished:
		fmt.Fprintf(os.Stderr, "alux: %s %s (%s); exiting\n", run.key, final.State, final.How)
	}
	s.Cancel()
	return errors.Join(err, srv.Close())
}

// servedState returns a publisher of the session's state for other panes, which runs whether or not a
// TUI is attached; a finish with none attached stays unseen until one attaches and is focused.
func servedState(s *session.Session, srv *attach.Server, reg *registry.Registry) func() {
	var mu sync.Mutex
	last, lastMessage, lastTasks, lastTopic := registry.Idle, "", 0, ""
	return func() {
		mu.Lock()
		defer mu.Unlock()
		snap := s.Snapshot()
		state, message, tasks, topic := ui.SessionState(snap), ui.LastMessage(snap), ui.SessionTasks(snap), snap.Topic
		if state == last && message == lastMessage && tasks == lastTasks && topic == lastTopic {
			return
		}
		lastTasks, lastTopic = tasks, topic
		patch := registry.Patch{State: &state, LastMessage: &message, Tasks: &tasks, Topic: &topic}
		if last == registry.Working && state == registry.Idle && !srv.Attached() {
			unseen := true
			patch.Unseen = &unseen
		}
		last, lastMessage = state, message
		reg.Publish(patch)
	}
}

// mailbox returns a send that never blocks, delivering in order once ready is closed. Unbounded on
// purpose: the client's reader calls send, and the event loop may be waiting on that reader for a reply.
func mailbox(ready <-chan struct{}, deliver func(tea.Msg)) func(tea.Msg) {
	var mu sync.Mutex
	var queue []tea.Msg
	wake := make(chan struct{}, 1)
	go func() {
		<-ready
		for range wake {
			mu.Lock()
			batch := queue
			queue = nil
			mu.Unlock()
			for _, msg := range batch {
				deliver(msg)
			}
		}
	}()
	return func(msg tea.Msg) {
		mu.Lock()
		queue = append(queue, msg)
		mu.Unlock()
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// reachWorkers lets a TUI show, answer and message the workers other sessions run, through their control sockets.
func reachWorkers(model *ui.Model) {
	model.ParkedPrompt, model.AnswerPrompt = attach.ParkedPrompt, attach.AnswerPrompt
	commands.SendWorker = attach.SendMessage
	model.StopSession = attach.StopSession
}

// landSuggestions is what /land completes to, from list.
func landSuggestions(list func() ([]orchestrator.Unlanded, error)) func() []commands.Suggestion {
	return func() []commands.Suggestion {
		unlanded, err := list()
		if err != nil {
			return []commands.Suggestion{{Label: "listing unlanded work failed", Description: err.Error()}}
		}
		var out []commands.Suggestion
		for _, u := range unlanded {
			out = append(out, commands.Suggestion{Value: u.Change, Label: u.Change, Description: u.Description + " (" + u.Age + ")"})
		}
		return out
	}
}

// attachCommand returns the exit code of alux attach [-new [-here]] [pid].
func attachCommand(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fresh := fs.Bool("new", false, "start a session in this directory and attach to it")
	here := fs.Bool("here", false, "with -new, run the session in this checkout instead of a pool workspace")
	if fs.Parse(args) != nil || fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: alux attach [-new [-here]] [pid]")
		return 2
	}
	pid, err := pickServer(*fresh, *here, fs.Arg(0))
	if err == nil {
		err = attachTo(pid)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "alux:", err)
		return 1
	}
	return 0
}

// stopCommand returns the exit code of alux stop pid, which ends a served session as SIGTERM would.
func stopCommand(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: alux stop pid")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err == nil && !slices.Contains(servedPIDs(), pid) {
		err = fmt.Errorf("no served session has pid %d", pid)
	}
	if err == nil {
		err = syscall.Kill(pid, syscall.SIGTERM)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "alux:", err)
		return 1
	}
	return 0
}

func pickServer(fresh, here bool, arg string) (int, error) {
	if fresh {
		return startServer(here, "", os.Stderr)
	}
	if arg != "" {
		return strconv.Atoi(arg)
	}
	pids := servedPIDs()
	if free := unwatched(pids); len(free) > 0 {
		pids = free
	}
	switch len(pids) {
	case 0:
		return 0, errors.New("no served sessions; start one with alux attach -new")
	case 1:
		return pids[0], nil
	}
	cwd := map[int]string{}
	for _, info := range registry.Default("", "").Others() {
		cwd[info.PID] = info.Cwd
	}
	var b strings.Builder
	b.WriteString("several served sessions; name one:")
	for _, pid := range pids {
		fmt.Fprintf(&b, "\n  alux attach %d    %s", pid, cwd[pid])
	}
	return 0, errors.New(b.String())
}

// unwatched is the sessions in pids with no TUI attached; one that cannot be asked counts as attached.
func unwatched(pids []int) []int {
	var free []int
	for _, pid := range pids {
		if watched, err := attach.Watched(pid); err == nil && !watched {
			free = append(free, pid)
		}
	}
	return free
}

// detachGrace is how long a session takes to notice this pane left; a stop still refused after it is another pane attached.
const detachGrace = 2 * time.Second

// stopDetached stops the session in pid this pane just left, retrying while the session still counts it as attached.
func stopDetached(pid int) error {
	var err error
	for deadline := time.Now().Add(detachGrace); time.Now().Before(deadline); time.Sleep(awaitEvery) {
		if err = attach.StopSession(pid); err == nil {
			return nil
		}
	}
	return err
}

// nextSession is a served session other than except to move to, one with no UI on it first, 0 when there is none.
func nextSession(except int) int {
	pids := slices.DeleteFunc(servedPIDs(), func(pid int) bool { return pid == except })
	if free := unwatched(pids); len(free) > 0 {
		return free[0]
	}
	if len(pids) > 0 {
		return pids[0]
	}
	return 0
}

// servedPIDs is every live process with a socket in the registry's directory.
func servedPIDs() []int {
	sock, err := attach.SocketPath(0)
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(sock), "*.sock"))
	var pids []int
	for _, m := range matches {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(m), ".sock"))
		if err == nil && registry.Alive(pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// showAfter is how long a launch stays quiet before the session's progress lines are shown.
const showAfter = time.Second

// startServer runs alux --serve in dir ("" for this one) in its own session, so closing this pane does not
// reach it, and waits for its socket for as long as it lives, copying its log to progress once the wait is slow.
// progress is nil when a TUI owns the terminal.
func startServer(here bool, dir string, progress io.Writer) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	logDir := filepath.Join(home, paths.State, "serve")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, err
	}
	log, err := os.CreateTemp(logDir, "serve-*.log")
	if err != nil {
		return 0, err
	}
	defer log.Close()
	cmd := exec.Command(exe, "--serve")
	if here {
		cmd.Args = append(cmd.Args, "--here")
	}
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := startProcess(cmd); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	sock, err := attach.SocketPath(pid)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	var shown int
	for ; ; time.Sleep(awaitEvery) {
		if progress != nil && time.Since(start) >= showAfter {
			shown = copyLines(progress, log.Name(), shown)
		}
		if _, err := os.Stat(sock); err == nil {
			return pid, nil
		}
		if !registry.Alive(pid) {
			return 0, fmt.Errorf("the session (pid %d) exited before it listened: %s\nlog: %s", pid, logTail(log.Name(), 6), log.Name())
		}
	}
}

// copyLines writes the whole lines of path past byte offset from to w and returns the offset after them.
func copyLines(w io.Writer, path string, from int) int {
	data, err := os.ReadFile(path)
	if err != nil || len(data) <= from {
		return from
	}
	end := from + bytes.LastIndexByte(data[from:], '\n') + 1
	_, _ = w.Write(data[from:end])
	return end
}

// attachTo runs the TUI against the session in pid, then against each session the user switches to, until one quits or ends.
func attachTo(pid int) error {
	for pid != 0 {
		next, err := attachOnce(pid)
		if err != nil {
			fmt.Fprint(os.Stdout, ansi.ResetModeAltScreenSaveCursor)
			return err
		}
		pid = next
	}
	return nil
}

// served reports whether pid has an attach socket, which an --inline session lacks.
func served(pid int) bool {
	sock, err := attach.SocketPath(pid)
	if err != nil {
		return false
	}
	_, err = os.Stat(sock)
	return err == nil && registry.Alive(pid)
}

// holdScreen re-enters the alternate screen when there is a next session, so the shell stays hidden until its TUI draws.
func holdScreen(next int) int {
	if next != 0 {
		fmt.Fprint(os.Stdout, ansi.SetModeAltScreenSaveCursor)
	}
	return next
}

// attachOnce runs the TUI against the session in pid until the user quits, which detaches and leaves the session running,
// and returns the session the user switched to, or 0.
func attachOnce(pid int) (int, error) {
	clear(commands.Unconfigured)
	commands.LandCandidates = nil
	sock, err := attach.SocketPath(pid)
	if err != nil {
		return 0, err
	}
	var client *attach.Client
	reg := registry.Attached(pid, func(p registry.Patch) { client.Publish(p) })
	// Before anything resolves the project: the session may run in a pool slot this pane is not in,
	// and the status line, file paths and workspaces all read the working directory.
	if cwd := reg.Own().Cwd; cwd != "" {
		if err := os.Chdir(cwd); err != nil {
			return 0, fmt.Errorf("moving to the session's directory: %w", err)
		}
	}
	var program *tea.Program
	ready := make(chan struct{})
	notify := coalesce(25*time.Millisecond, func() {
		<-ready
		program.Send(ui.Changed())
	})
	send := mailbox(ready, func(msg tea.Msg) { program.Send(msg) })
	client, err = attach.Dial(sock, attach.Events{
		Changed:  notify,
		Notice:   func(text string) { send(ui.NoticeMsg{Text: text}) },
		PRs:      func(states []prs.RepoState) { send(ui.PRsMsg{States: states}) },
		Review:   func(r reviewstore.Review) { send(ui.ReviewMsg{Review: r}) },
		Guardian: func(err error) { send(ui.GuardianMsg{Err: err}) },
		Demoted:  func() { send(ui.NoticeMsg{Text: "viewing — another pane took over"}) },
	})
	if err != nil {
		return 0, fmt.Errorf("attaching to %d: %w", pid, err)
	}
	defer client.Close()
	model := ui.New(client)
	model.Registry, model.Origin, model.Viewing, model.CanAttach = reg, reg.Own().Origin, client.Viewing, served
	// From the default workspace, so a pool hands the new session a slot rather than this one's.
	model.Spawn = func() (int, error) { return startServer(false, reg.Own().Origin, nil) }
	model.CanKill = true
	reachWorkers(model)
	if setup := client.Hello().Setup; setup != nil {
		model.Setup = setup
		commands.Unconfigured["vault"] = !setup.Vault
	}
	model.Names = &resolve.Resolver{
		Remote: client.LookupName,
		OnName: coalesce(25*time.Millisecond, func() {
			<-ready
			program.Send(ui.NamesMsg{})
		}),
	}
	if land := client.Land(); land != nil {
		hello := client.Hello()
		model.Land, model.VCSScope, model.VCSSlug = land, hello.VCSScope, hello.VCSSlug
		if !hello.PRFlow {
			commands.LandCandidates = landSuggestions(client.LandCandidates)
		}
	} else {
		commands.Unconfigured["land"], commands.Unconfigured["resume"] = true, true
	}
	if p := client.PRs(); p != nil {
		model.PRs = p
	} else {
		commands.Unconfigured["prs"] = true
	}
	program = tea.NewProgram(model)
	close(ready)
	if client.Viewing() {
		send(ui.NoticeMsg{Text: "viewing — another pane is driving"})
	}
	stopWatching := reg.Watch(time.Second, func(others []registry.Info) {
		program.Send(ui.SessionsMsg{Sessions: others})
	})
	defer stopWatching()
	go func() {
		<-client.Done()
		program.Quit()
	}()
	_, err = program.Run()
	fmt.Fprint(os.Stdout, programstatus.Clear)
	select {
	case <-client.Done():
		fmt.Fprintf(os.Stderr, "alux: session %d ended\n", pid)
		return 0, err
	default:
	}
	if err == nil && model.Kill {
		client.Close()
		if err := stopDetached(pid); err != nil {
			return 0, fmt.Errorf("not killing session %d: %w", pid, err)
		}
		fmt.Fprintf(os.Stderr, "alux: killed session %d\n", pid)
		if model.KillOnly {
			return 0, nil
		}
		return holdScreen(nextSession(pid)), nil
	}
	if err == nil && model.SwitchTo != 0 {
		return holdScreen(model.SwitchTo), nil
	}
	fmt.Fprintf(os.Stderr, "alux: detached; session %d is still running (alux attach %d, alux stop %d)\n", pid, pid, pid)
	return 0, err
}
