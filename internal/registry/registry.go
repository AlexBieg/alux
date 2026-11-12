// Package registry lets concurrently running alux sessions see each other.
//
// Awareness only: each session publishes a small JSON file and reads the others'. There are no
// sockets and no way to drive another session — that was considered and deliberately left out,
// so the failure mode of this package is a stale dot, never a hung pane.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

type State string

const (
	Idle    State = "idle"
	Working State = "working"
	Waiting State = "waiting"
)

type Info struct {
	PID         int    `json:"pid"`
	Name        string `json:"name"`
	Topic       string `json:"topic,omitempty"`
	Session     string `json:"session"`
	Cwd         string `json:"cwd"`
	Workspace   string `json:"workspace"`
	Bookmark    string `json:"bookmark"`
	Change      string `json:"change"`
	State       State  `json:"state"`
	LastMessage string `json:"lastMessage"`
	// Unseen is a finish that landed while the pane was unfocused, held until it is focused.
	Unseen     bool   `json:"unseen"`
	UpdatedAt  int64  `json:"updatedAt"`
	Worker     string `json:"worker,omitempty"`
	PromptID   string `json:"promptId,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Origin     string `json:"origin,omitempty"`
	Stage      string `json:"stage,omitempty"`
	// WorkerState is a headless worker's "running", "waiting", "stalled" or "idle".
	WorkerState string `json:"workerState,omitempty"`
	// LastActivity is the unix ms of the session's last step or change.
	LastActivity int64  `json:"lastActivity,omitempty"`
	Log          string `json:"log,omitempty"`
	// Detached is a served session with no TUI connected; false for a pane that is its own UI.
	Detached bool `json:"detached,omitempty"`
	// Tasks is the subagents and background commands still running for the session.
	Tasks int `json:"tasks,omitempty"`
}

// Registry is this session's view of every running alux.
type Registry struct {
	dir string
	pid int
	// forward, when set, takes each patch in place of writing the entry: an attached TUI's view of
	// the session it drives, whose entry only the session's own process writes.
	forward func(Patch)

	mu  sync.Mutex
	own Info
}

func New(dir, name, cwd string) *Registry {
	return &Registry{
		dir: dir,
		pid: os.Getpid(),
		own: Info{PID: os.Getpid(), Name: name, Cwd: cwd, State: Idle, UpdatedAt: time.Now().UnixMilli()},
	}
}

// Attached is the registry as seen by a TUI attached to the session in pid: that session is its own,
// read from its entry, and Publish hands each patch to forward.
func Attached(pid int, forward func(Patch)) *Registry {
	r := Default("", "")
	r.pid, r.forward = pid, forward
	r.own.PID = pid
	if data, err := os.ReadFile(r.ownFile()); err == nil {
		_ = json.Unmarshal(data, &r.own)
	}
	return r
}

// Own is this session's entry as last published or read.
func (r *Registry) Own() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.own
}

// Default puts the registry where every alux on this machine looks for it.
func Default(name, cwd string) *Registry {
	home, _ := os.UserHomeDir()
	return New(filepath.Join(home, paths.Cache, "sessions"), name, cwd)
}

func (r *Registry) ownFile() string {
	return filepath.Join(r.dir, strconv.Itoa(r.pid)+".json")
}

func (r *Registry) Name() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.own.Name
}

// Patch is the part of a session's entry that changes as it works.
type Patch struct {
	Session      *string
	Name         *string
	Topic        *string
	State        *State
	LastMessage  *string
	Workspace    *string
	Bookmark     *string
	Change       *string
	Unseen       *bool
	Worker       *string
	PromptID     *string
	Mode         *string
	Transcript   *string
	Origin       *string
	Stage        *string
	WorkerState  *string
	LastActivity *int64
	Log          *string
	Detached     *bool
	Tasks        *int
}

// Publish updates this session's entry. Written to a temporary file and renamed, so a reader
// never sees a half-written one.
func (r *Registry) Publish(patch Patch) {
	r.mu.Lock()
	if patch.Session != nil {
		r.own.Session = *patch.Session
	}
	if patch.Name != nil {
		r.own.Name = *patch.Name
	}
	if patch.Topic != nil {
		r.own.Topic = *patch.Topic
	}
	if patch.State != nil {
		r.own.State = *patch.State
	}
	if patch.LastMessage != nil {
		r.own.LastMessage = *patch.LastMessage
	}
	if patch.Workspace != nil {
		r.own.Workspace = *patch.Workspace
	}
	if patch.Bookmark != nil {
		r.own.Bookmark = *patch.Bookmark
	}
	if patch.Change != nil {
		r.own.Change = *patch.Change
	}
	if patch.Unseen != nil {
		r.own.Unseen = *patch.Unseen
	}
	if patch.Worker != nil {
		r.own.Worker = *patch.Worker
	}
	if patch.PromptID != nil {
		r.own.PromptID = *patch.PromptID
	}
	if patch.Mode != nil {
		r.own.Mode = *patch.Mode
	}
	if patch.Transcript != nil {
		r.own.Transcript = *patch.Transcript
	}
	if patch.Origin != nil {
		r.own.Origin = *patch.Origin
	}
	if patch.Stage != nil {
		r.own.Stage = *patch.Stage
	}
	if patch.WorkerState != nil {
		r.own.WorkerState = *patch.WorkerState
	}
	if patch.LastActivity != nil {
		r.own.LastActivity = *patch.LastActivity
	}
	if patch.Log != nil {
		r.own.Log = *patch.Log
	}
	if patch.Detached != nil {
		r.own.Detached = *patch.Detached
	}
	if patch.Tasks != nil {
		r.own.Tasks = *patch.Tasks
	}
	r.own.UpdatedAt = time.Now().UnixMilli()
	own := r.own
	r.mu.Unlock()

	if r.forward != nil {
		r.forward(patch)
		return
	}
	if os.MkdirAll(r.dir, 0o700) != nil {
		return
	}
	data, err := json.Marshal(own)
	if err != nil {
		return
	}
	tmp := r.ownFile() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, r.ownFile())
}

// Remove deletes this session's entry on the way out.
func (r *Registry) Remove() {
	if r.forward != nil {
		return
	}
	_ = os.Remove(r.ownFile())
}

// Alive says a process with pid exists, whoever owns it.
func Alive(pid int) bool { return alive(pid) }

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// Others lists every running alux but this one, pruning entries left behind by sessions that
// crashed — nothing else ever cleans those up.
func (r *Registry) Others() []Info {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	var out []Info
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil || pid == r.pid {
			continue
		}
		if !alive(pid) {
			_ = os.Remove(filepath.Join(r.dir, name))
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.dir, name))
		if err != nil {
			continue
		}
		var info Info
		if json.Unmarshal(data, &info) == nil {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

// RunningSessions is the session id of every other live alux on this machine.
func RunningSessions() map[string]bool {
	running := map[string]bool{}
	for _, info := range Default("", "").Others() {
		if info.Session != "" {
			running[info.Session] = true
		}
	}
	return running
}

func less(a, b Info) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.PID < b.PID
}

// All is every running session including this one, in the order the dot bar shows them. The
// live state is passed in rather than read off the stored entry, which only catches up when
// Publish runs and would otherwise render the bar one frame stale.
func (r *Registry) All(others []Info, state State) []Info {
	r.mu.Lock()
	own := r.own
	r.mu.Unlock()
	own.State = state

	all := append([]Info{own}, others...)
	sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	return all
}

func (r *Registry) OwnPID() int { return r.pid }

// Watch polls for changes and calls onChange whenever the set of other sessions differs.
//
// Polling rather than a filesystem watch: what this feeds is a row of coloured dots, a second
// of lag is imperceptible, and a watcher would add a dependency plus its own missed-event
// backstop.
func (r *Registry) Watch(interval time.Duration, onChange func([]Info)) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var last string
		first := true
		for {
			others := r.Others()
			if key := fingerprint(others); first || key != last {
				first, last = false, key
				onChange(others)
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
}

func fingerprint(sessions []Info) string {
	var b strings.Builder
	for _, s := range sessions {
		fmt.Fprintf(&b, "%d:%s:%s:%s:%s:%t:%s:%s:%s:%s:%s:%s:%s:%t;", s.PID, s.Name, s.Topic, s.Workspace, s.State, s.Unseen, s.Change, s.LastMessage, s.Worker, s.PromptID, s.Origin, s.Stage, s.WorkerState, s.Detached)
	}
	return b.String()
}

// Label names a session, disambiguating with the pid only when two share a name.
func Label(session Info, all []Info) string {
	count := 0
	for _, s := range all {
		if s.Name == session.Name {
			count++
		}
	}
	if count > 1 {
		return fmt.Sprintf("%s#%d", session.Name, session.PID)
	}
	return session.Name
}

// DescribeTransition is the one definition of "this session needs attention". It drives both the
// in-pane notices about other sessions and this session's desktop notifications, so the two can
// never disagree about what is worth interrupting for. Deliberately label-free: each caller
// names the session its own way.
func DescribeTransition(prev State, next Info) string {
	if next.State == Waiting && prev != Waiting {
		return "is waiting for approval: " + next.LastMessage
	}
	if prev == Working && next.State == Idle {
		if next.LastMessage != "" {
			return "finished: " + next.LastMessage
		}
		return "finished"
	}
	return ""
}
