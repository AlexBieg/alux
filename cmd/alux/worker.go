package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/attach"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/skillrun"
	"github.com/alexbieg/alux/internal/workers"
)

// checkWorker validates a worker's flags, returning the upper-cased key and the mode.
func checkWorker(here, serve bool, worker, mode string) (string, session.Mode, error) {
	if !serve {
		return "", "", errors.New("--worker runs with --serve")
	}
	if here {
		return "", "", errors.New("a worker runs in a pool workspace of its own and cannot be combined with --here")
	}
	key := strings.ToUpper(strings.TrimSpace(worker))
	if strings.ContainsAny(key, `/\`) || key == "." || key == ".." {
		return "", "", fmt.Errorf("--worker %q is not a worker key", worker)
	}
	m := session.Mode(mode)
	if !slices.Contains(session.Modes, m) {
		return "", "", fmt.Errorf("a worker needs --mode read-only, local or auto, got %q", mode)
	}
	return key, m, nil
}

func requireSlot(worker bool, slot *orchestrator.Slot) error {
	if worker && slot == nil {
		return errors.New("a worker needs a pool workspace: run it from the default workspace of a repo with a pool")
	}
	return nil
}

// workerRun is a served session running as a worker: it publishes the worker's status as the session
// changes, and once the skill run has closed and the session is idle it writes final.json and ends.
type workerRun struct {
	key, dir string
	s        *session.Session
	reg      *registry.Registry
	finished chan workers.Final

	mu     sync.Mutex
	ended  string
	last   activity
	lastAt int64
	done   bool
}

// startWorkerRun writes worker.json and the registry's worker fields, then hands the session its task.
func startWorkerRun(s *session.Session, reg *registry.Registry, key string, mode session.Mode, prompt, logPath, sessionID string) (*workerRun, error) {
	dir, err := workers.Dir(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	origin, m, transcript := project.Origin(), string(mode), s.Transcript.Path()
	reg.Publish(registry.Patch{Worker: &key, Mode: &m, Transcript: &transcript, Origin: &origin, Log: &logPath})
	meta := workers.Meta{V: workers.Version, Origin: origin, PID: os.Getpid(), Session: sessionID, Workspace: project.Workspace(),
		Mode: m, Transcript: transcript, Log: logPath}
	if err := workers.WriteJSON(filepath.Join(dir, "worker.json"), meta); err != nil {
		return nil, err
	}
	t := &workerRun{key: key, dir: dir, s: s, reg: reg, finished: make(chan workers.Final, 1), lastAt: time.Now().UnixMilli()}
	s.OnSkillClosed = func(how string) {
		t.mu.Lock()
		t.ended = how
		t.mu.Unlock()
	}
	s.SetMode(mode)
	fmt.Fprintf(os.Stderr, "alux: worker %s in %s mode; channel %s\n", key, mode, dir)
	if prompt != "" {
		s.Submit(prompt)
	}
	return t, nil
}

// publish is called on every change to the session.
func (t *workerRun) publish() {
	snap := t.s.Snapshot()
	id := attach.PromptID(os.Getpid(), snap)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	// Ended only once the closing reply is written, so the final message is the agent's last word.
	if t.ended != "" && !snap.Busy && id == "" {
		t.done = true
		final := finalOf(t.ended, lastReply(snap))
		if err := workers.WriteJSON(filepath.Join(t.dir, "final.json"), final); err != nil {
			fmt.Fprintln(os.Stderr, "alux: writing final.json:", err)
		}
		t.finished <- final
		return
	}
	if now := activityOf(snap); now != t.last {
		t.last, t.lastAt = now, time.Now().UnixMilli()
	}
	stage, state := t.s.SkillStage(), workerState(snap, id, liveChildren(t.s))
	t.reg.Publish(registry.Patch{PromptID: &id, Stage: &stage, WorkerState: &state, LastActivity: &t.lastAt})
}

func finalOf(how, message string) workers.Final {
	state := workers.Failed
	if how == skillrun.Finished {
		state = workers.Done
	}
	return workers.Final{V: workers.Version, State: state, How: how, Message: message}
}

func lastReply(snap session.Snapshot) string {
	for i := len(snap.Items) - 1; i >= 0; i-- {
		if snap.Items[i].Kind == session.KindAssistant {
			return strings.TrimSpace(snap.Items[i].Text)
		}
	}
	return ""
}

// workerState is "waiting" while a prompt is parked; when not busy, queued or awaiting a subagent of its own, "stalled" with a skill run open and "idle" without; else "running".
func workerState(snap session.Snapshot, id string, children int) string {
	switch {
	case id != "":
		return "waiting"
	case snap.Busy || len(snap.Queued) > 0 || children > 0:
		return "running"
	case snap.Skill != nil:
		return "stalled"
	default:
		return "idle"
	}
}

func liveChildren(s *session.Session) int {
	if s.Roster == nil {
		return 0
	}
	n := 0
	for _, r := range s.Roster.Active() {
		if r.Live() {
			n++
		}
	}
	return n
}

// activity is what changes when the session takes a step, compared change to change for LastActivity.
type activity struct {
	items, prompt int
	busy          bool
	live          int
}

func activityOf(snap session.Snapshot) activity {
	return activity{len(snap.Items), snap.PromptSeq, snap.Busy, len(snap.Live)}
}

// auditLog appends each unprompted escalated command to path, one line per command.
func auditLog(path string) func(command string) {
	return func(command string) {
		if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s unprompted %s\n", time.Now().Format(time.RFC3339), command)
	}
}

// promptLog appends each settled prompt to path as one JSON line.
func promptLog(path string) func(session.PromptRecord) {
	return func(r session.PromptRecord) {
		line, err := json.Marshal(r)
		if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(append(line, '\n'))
	}
}
