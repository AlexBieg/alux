package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeEntry plants another session's file. The pid decides whether it counts as running.
func writeEntry(t *testing.T, dir string, info Info) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, itoa(info.PID)+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// A crashed session never removes its own file, and nothing else is going to. Leaving them
// would slowly fill the dot bar with sessions that are not running.
func TestOthersPrunesDeadSessions(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "self", "/tmp")

	writeEntry(t, dir, Info{PID: 999999, Name: "crashed", State: Idle})
	writeEntry(t, dir, Info{PID: os.Getppid(), Name: "running", State: Working})

	others := r.Others()

	if len(others) != 1 || others[0].Name != "running" {
		t.Fatalf("others = %+v, want only the running session", others)
	}
	if _, err := os.Stat(filepath.Join(dir, "999999.json")); err == nil {
		t.Error("the dead session's file was left behind")
	}
}

func TestOthersExcludesThisSession(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "self", "/tmp")
	r.Publish(Patch{})

	if others := r.Others(); len(others) != 0 {
		t.Errorf("this session appeared in its own list: %+v", others)
	}
}

func TestAnAttachedTUINeverWritesTheEntryOfTheSessionItDrives(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".cache", "alux", "sessions")
	server := os.Getppid()
	writeEntry(t, dir, Info{PID: server, Name: "served", State: Working, Origin: "/repo"})
	before, _ := os.ReadFile(filepath.Join(dir, itoa(server)+".json"))

	var forwarded []Patch
	r := Attached(server, func(p Patch) { forwarded = append(forwarded, p) })
	idle := Idle
	r.Publish(Patch{State: &idle})
	r.Remove()

	if after, _ := os.ReadFile(filepath.Join(dir, itoa(server)+".json")); string(after) != string(before) {
		t.Errorf("the served session's entry was rewritten:\n%s", after)
	}
	if len(forwarded) != 1 || *forwarded[0].State != Idle {
		t.Errorf("forwarded %+v, want the one patch", forwarded)
	}
	if r.Name() != "served" || r.Own().Origin != "/repo" || r.OwnPID() != server {
		t.Errorf("the attached registry is not the served session's: %+v pid %d", r.Own(), r.OwnPID())
	}
	if others := r.Others(); len(others) != 0 {
		t.Errorf("the served session is listed among the others: %+v", others)
	}
}

// The bar has to show what a session is doing now, not what it last wrote to disk. Reading the
// stored entry instead would render one frame stale on every transition.
func TestAllUsesTheLiveStateNotTheStoredOne(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "self", "/tmp")
	idle := Idle
	r.Publish(Patch{State: &idle})

	all := r.All(nil, Working)

	if len(all) != 1 {
		t.Fatalf("all = %+v", all)
	}
	if all[0].State != Working {
		t.Errorf("state = %q, want the live %q", all[0].State, Working)
	}
}

func TestPublishLeavesNoPartialFiles(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "self", "/tmp")
	name := "renamed"
	r.Publish(Patch{Name: &name})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
	if r.Name() != "renamed" {
		t.Errorf("name = %q", r.Name())
	}
}

func TestLabelDisambiguatesOnlyOnCollision(t *testing.T) {
	a := Info{PID: 1, Name: "alux"}
	b := Info{PID: 2, Name: "alux"}
	c := Info{PID: 3, Name: "acme"}

	if got := Label(c, []Info{a, b, c}); got != "acme" {
		t.Errorf("unique name = %q, want it left alone", got)
	}
	if got := Label(a, []Info{a, b, c}); got != "alux#1" {
		t.Errorf("colliding name = %q, want the pid appended", got)
	}
}

// One definition of "needs attention" drives both the in-pane notices and the desktop ones, so
// what it does and does not fire on is the whole contract.
func TestDescribeTransitionFiresOnlyOnRealChanges(t *testing.T) {
	cases := []struct {
		name string
		prev State
		next Info
		want string
	}{
		{"newly waiting", Working, Info{State: Waiting, LastMessage: "$ rm -rf /"}, "is waiting for approval: $ rm -rf /"},
		{"still waiting", Waiting, Info{State: Waiting, LastMessage: "$ rm -rf /"}, ""},
		{"finished with a reply", Working, Info{State: Idle, LastMessage: "all done"}, "finished: all done"},
		{"finished silently", Working, Info{State: Idle}, "finished"},
		{"still working", Working, Info{State: Working}, ""},
		{"still idle", Idle, Info{State: Idle}, ""},
		{"started working", Idle, Info{State: Working}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DescribeTransition(tc.prev, tc.next); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The watch must report a change once and then stay quiet, or every poll would raise a notice
// about sessions that have not moved.
func TestWatchReportsChangesNotPolls(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "self", "/tmp")

	updates := make(chan []Info, 16)
	stop := r.Watch(20*time.Millisecond, func(others []Info) { updates <- others })
	defer stop()

	// The first read is a baseline and arrives immediately.
	select {
	case <-updates:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial read")
	}

	writeEntry(t, dir, Info{PID: os.Getppid(), Name: "other", State: Working})

	select {
	case got := <-updates:
		if len(got) != 1 || got[0].Name != "other" {
			t.Fatalf("update = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the new session was never reported")
	}

	// Nothing has changed since, so nothing more should arrive.
	select {
	case got := <-updates:
		t.Errorf("an unchanged poll raised an update: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatchReportsAnAttachOrDetach(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "self", "/tmp")
	writeEntry(t, dir, Info{PID: os.Getppid(), Name: "other", State: Idle, Detached: true})

	updates := make(chan []Info, 16)
	stop := r.Watch(20*time.Millisecond, func(others []Info) { updates <- others })
	defer stop()
	<-updates

	writeEntry(t, dir, Info{PID: os.Getppid(), Name: "other", State: Idle})

	select {
	case got := <-updates:
		if len(got) != 1 || got[0].Detached {
			t.Fatalf("update = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a UI attaching to the session was never reported")
	}
}

func TestStopEndsTheWatch(t *testing.T) {
	r := New(t.TempDir(), "self", "/tmp")
	updates := make(chan []Info, 8)
	stop := r.Watch(10*time.Millisecond, func(others []Info) { updates <- others })
	<-updates
	stop()
	stop() // must be safe twice; main defers it alongside other cleanup
}
