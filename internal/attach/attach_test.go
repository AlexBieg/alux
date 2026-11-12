package attach

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/ui"
)

var _ ui.SessionClient = (*Client)(nil)

// serve listens under /tmp, since a unix socket path is capped at 104 bytes on macOS and t.TempDir is longer.
func serve(t *testing.T) (*session.Session, string) {
	return serveRefusing(t, func(int) error { return nil })
}

func serveRefusing(t *testing.T, refuse func(pid int) error) (*session.Session, string) {
	s, path, _ := serveWith(t, func(srv *Server) { srv.refuse = refuse })
	return s, path
}

// serveWith configures the server before it starts serving, which is when its hooks must be set.
func serveWith(t *testing.T, configure func(*Server)) (*session.Session, string, *Server) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "attach")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	var srv *Server
	s := session.New(&agent.Agent{}, func() {
		if srv != nil {
			srv.Changed()
		}
	})
	srv, err = Listen(path, s)
	if err != nil {
		t.Fatal(err)
	}
	srv.refuse = func(int) error { return nil }
	configure(srv)
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return s, path, srv
}

func dial(t *testing.T, path string) (*Client, chan struct{}) {
	t.Helper()
	changed := make(chan struct{}, 1)
	c, err := Dial(path, Events{Changed: func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, changed
}

func waitFor(t *testing.T, changed chan struct{}, what string, ok func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !ok() {
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestTheMirrorMatchesTheSessionAfterAnEditBehindLaterItems(t *testing.T) {
	s, path := serve(t)
	s.PushUserForTest("first")
	c, changed := dial(t, path)

	s.StartToolForTest("call-1", "$ ls", agent.KindBash)
	s.PushAssistantForTest("after the tool")
	waitFor(t, changed, "the running box and the item after it", func() bool {
		items := c.Snapshot().Items
		return len(items) == 3 && items[1].Running
	})
	s.ResolveToolForTest("call-1", "$ ls", "a.go\nb.go", agent.KindBash)
	s.PushUserForTest("last")

	want := s.Snapshot().Items
	waitFor(t, changed, "the mirror to catch up", func() bool {
		return reflect.DeepEqual(c.Snapshot().Items, want)
	})
	if got := c.Snapshot().Items[1]; got.Running || got.Body != "a.go\nb.go" {
		t.Fatalf("the resolved tool box reached the client as %+v", got)
	}
}

func TestAKeyedApprovalResolvesOnlyThePromptItNames(t *testing.T) {
	s, path := serve(t)
	parked := s.ParkForTest("$ rm -rf build")
	c, changed := dial(t, path)
	waitFor(t, changed, "the approval to show", func() bool { return c.Snapshot().ApprovalSeq != 0 })
	seq := c.Snapshot().ApprovalSeq

	if c.ApproveID(seq+1, true) {
		t.Fatal("an approval for a seq that is not parked was accepted")
	}
	if !c.ApproveID(seq, true) {
		t.Fatal("the approval for the parked seq was refused")
	}
	select {
	case okay := <-parked:
		if !okay {
			t.Fatal("the parked approval was declined")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked approval was never resolved")
	}
}

func TestASecondAttachViewsAndLeavesTheFirstDriving(t *testing.T) {
	s, path := serve(t)
	first, firstChanged := dial(t, path)
	second, secondChanged := dial(t, path)
	if first.Viewing() || !second.Viewing() {
		t.Fatalf("first viewing %v, second viewing %v; want the first driving", first.Viewing(), second.Viewing())
	}

	s.PushUserForTest("for both")
	for _, tui := range []struct {
		c       *Client
		changed chan struct{}
	}{{first, firstChanged}, {second, secondChanged}} {
		waitFor(t, tui.changed, "the note in every mirror", func() bool { return len(tui.c.Snapshot().Items) == 1 })
	}
	select {
	case <-first.Done():
		t.Fatal("the second attach detached the first")
	default:
	}
}

func TestAViewerIsRefusedASessionChangeUntilItTakesTheSession(t *testing.T) {
	_, path := serve(t)
	dial(t, path)
	viewer, _ := dial(t, path)
	if err := viewer.quick(opCancel, args{}, nil); err == nil || err.Error() != errViewing {
		t.Fatalf("a viewer's cancel returned %v, want the refusal", err)
	}
	if err := viewer.quick(opTake, args{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := viewer.quick(opCancel, args{}, nil); err != nil {
		t.Fatalf("the cancel after taking the session returned %v", err)
	}
}

func TestTakingTheSessionLeavesTheOldDriverViewingItsSnapshots(t *testing.T) {
	s, path := serve(t)
	demoted := make(chan struct{}, 1)
	firstChanged := make(chan struct{}, 1)
	first, err := Dial(path, Events{
		Changed: func() {
			select {
			case firstChanged <- struct{}{}:
			default:
			}
		},
		Demoted: func() { demoted <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, _ := dial(t, path)

	second.Show("taken")
	select {
	case <-demoted:
	case <-time.After(5 * time.Second):
		t.Fatal("the old driver was never told it was demoted")
	}
	if !first.Viewing() || second.Viewing() {
		t.Fatalf("first viewing %v, second viewing %v; want the second driving", first.Viewing(), second.Viewing())
	}
	s.PushUserForTest("after")
	waitFor(t, firstChanged, "the old driver's mirror to follow", func() bool { return len(first.Snapshot().Items) == 2 })
	select {
	case <-first.Done():
		t.Fatal("taking the session detached the old driver")
	default:
	}
}

func TestAViewerHoldsItsPaneBackUntilItTakesTheSession(t *testing.T) {
	reports := make(chan Pane, 4)
	_, path, _ := serveWith(t, func(srv *Server) { srv.OnPane = func(p Pane) { reports <- p } })
	dial(t, path)
	viewer, _ := dial(t, path)
	bookmark := "held"
	viewer.Publish(registry.Patch{Bookmark: &bookmark})
	if len(reports) != 0 || !viewer.Viewing() {
		t.Fatalf("a viewer's pane report took the session: %d reports, viewing %v", len(reports), viewer.Viewing())
	}
	viewer.Show("taking")
	select {
	case p := <-reports:
		if p.Bookmark != "held" {
			t.Fatalf("after taking, the pane arrived as %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held pane never arrived after taking the session")
	}
}

func TestARefusedPeerIsNeverServed(t *testing.T) {
	_, path := serveRefusing(t, func(int) error { return errors.New("no") })
	if _, err := Dial(path, Events{}); err == nil {
		t.Fatal("a refused peer attached")
	}
}

func TestAnyProcessASessionStartedIsRefusedAndOthersAreNot(t *testing.T) {
	sessions := t.TempDir()
	refuse := refuseSessionDescendants(sessions)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })

	if refuse(child.Process.Pid) == nil {
		t.Error("a process this one started was let in")
	}
	if err := refuse(os.Getppid()); err != nil {
		t.Errorf("this process's parent, started by no session, was refused: %v", err)
	}
	grandparent, err := parentPID(os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", grandparent)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if refuse(os.Getppid()) == nil {
		t.Error("a process another session started was let in")
	}
}

func TestANoticeReachesTheAttachedTUI(t *testing.T) {
	_, path, srv := serveWith(t, func(*Server) {})
	got := make(chan string, 1)
	c, err := Dial(path, Events{Notice: func(text string) { got <- text }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	srv.Notice("mcp: a server failed to start")
	select {
	case text := <-got:
		if text != "mcp: a server failed to start" {
			t.Fatalf("the notice arrived as %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notice never arrived")
	}
}

func TestThePaneIsReportedOncePerChange(t *testing.T) {
	reports := make(chan Pane, 4)
	_, path, _ := serveWith(t, func(srv *Server) { srv.OnPane = func(p Pane) { reports <- p } })
	c, _ := dial(t, path)
	unseen, bookmark := true, "attach-pane"
	c.Publish(registry.Patch{Unseen: &unseen, Bookmark: &bookmark})
	c.Publish(registry.Patch{Unseen: &unseen})
	seen := false
	c.Publish(registry.Patch{Unseen: &seen})

	want := []Pane{{Unseen: true, Bookmark: "attach-pane"}, {Unseen: false, Bookmark: "attach-pane"}}
	for i, w := range want {
		select {
		case p := <-reports:
			if p != w {
				t.Fatalf("report %d = %+v, want %+v", i, p, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("report %d never arrived", i)
		}
	}
	if len(reports) != 0 {
		t.Fatalf("an unchanged pane was reported again: %+v", <-reports)
	}
}

func TestTheSessionIsUnattachedOnceItsTUIGoes(t *testing.T) {
	_, path, srv := serveWith(t, func(*Server) {})
	c, _ := dial(t, path)
	if !srv.Attached() {
		t.Fatal("not attached with a TUI connected")
	}
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for srv.Attached() {
		if time.Now().After(deadline) {
			t.Fatal("still attached after the TUI disconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOnWatchFiresWhenTheFirstTUIComesAndTheLastGoes(t *testing.T) {
	flips := make(chan bool, 4)
	_, path, _ := serveWith(t, func(s *Server) { s.OnWatch = func(w bool) { flips <- w } })
	first, _ := dial(t, path)
	second, _ := dial(t, path)
	first.Close()
	second.Close()
	for i, want := range []bool{true, false} {
		select {
		case got := <-flips:
			if got != want {
				t.Fatalf("flip %d = %v, want %v", i, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("flip %d never arrived", i)
		}
	}
	if len(flips) != 0 {
		t.Fatalf("flipped again with no change: %v", <-flips)
	}
}

type fakeLand struct{ ui.LandSource }

func (fakeLand) Workspaces() (orchestrator.RepoView, error) {
	return orchestrator.RepoView{Name: "alux", Trunk: "main", Slug: "o/alux"}, nil
}

func (fakeLand) Sweep() ([]string, map[string]orchestrator.Held, error) {
	return []string{"abandoned kx"}, map[string]orchestrator.Held{"qp": {PR: 7, Why: "head moved"}}, nil
}

func TestTheLanderReachesTheTUIWithItsResults(t *testing.T) {
	_, path, _ := serveWith(t, func(srv *Server) {
		srv.Land, srv.Hello = fakeLand{}, Hello{Land: true, PRFlow: true, VCSSlug: "o/alux"}
	})
	c, _ := dial(t, path)
	land := c.Land()
	if land == nil || !land.PRFlow() || c.Hello().VCSSlug != "o/alux" {
		t.Fatalf("lander %v, hello %+v", land, c.Hello())
	}
	view, err := land.Workspaces()
	if err != nil || view.Name != "alux" || view.Slug != "o/alux" {
		t.Fatalf("Workspaces = %+v, %v", view, err)
	}
	said, held, err := land.Sweep()
	if err != nil || !reflect.DeepEqual(said, []string{"abandoned kx"}) || held["qp"] != (orchestrator.Held{PR: 7, Why: "head moved"}) {
		t.Fatalf("Sweep = %v, %v, %v", said, held, err)
	}
}

func TestASessionWithNoLanderOffersNone(t *testing.T) {
	_, path := serve(t)
	c, _ := dial(t, path)
	if c.Land() != nil {
		t.Fatal("a session outside a pool workspace offered a lander")
	}
}

type fakePRs struct {
	ui.PRSource
	labelled chan string
}

func (f fakePRs) MergeReady(_ context.Context, repo string, number int) (prs.MergeCheck, error) {
	return prs.MergeCheck{Label: fmt.Sprintf("merge %s#%d", repo, number), Pending: []string{"ci"}}, nil
}

func (f fakePRs) AddLabel(_ context.Context, _ string, number int, label string) error {
	f.labelled <- fmt.Sprintf("#%d %s", number, label)
	return nil
}

func TestThePRQueueArrivesOnAttachAndThenAsItChanges(t *testing.T) {
	queue := []prs.RepoState{{Repo: "o/alux", Err: errors.New("gh: rate limited")}}
	_, path, srv := serveWith(t, func(srv *Server) {
		srv.PRs, srv.PRStates, srv.Hello.PRs = fakePRs{}, func() []prs.RepoState { return queue }, true
	})
	states := make(chan []prs.RepoState, 4)
	reviews := make(chan reviewstore.Review, 1)
	c, err := Dial(path, Events{
		PRs:    func(s []prs.RepoState) { states <- s },
		Review: func(r reviewstore.Review) { reviews <- r },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if got := <-states; len(got) != 1 || got[0].Repo != "o/alux" || got[0].Err == nil || got[0].Err.Error() != "gh: rate limited" {
		t.Fatalf("on attach the queue was %+v", got)
	}

	srv.PRsChanged(nil)
	srv.ReviewLoaded(reviewstore.Review{Repo: "o/alux", Number: 9})
	select {
	case got := <-states:
		if len(got) != 0 {
			t.Fatalf("the emptied queue arrived as %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queue's change never arrived")
	}
	select {
	case r := <-reviews:
		if r.Number != 9 {
			t.Fatalf("the review arrived as %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the review never arrived")
	}
}

func TestThePollerAnswersTheTUIsCalls(t *testing.T) {
	labelled := make(chan string, 1)
	_, path, _ := serveWith(t, func(srv *Server) { srv.PRs, srv.Hello.PRs = fakePRs{labelled: labelled}, true })
	c, _ := dial(t, path)
	p := c.PRs()
	if p == nil {
		t.Fatal("a session tracking PRs offered no poller")
	}
	check, err := p.MergeReady(context.Background(), "o/alux", 12)
	if err != nil || check.Label != "merge o/alux#12" || !reflect.DeepEqual(check.Pending, []string{"ci"}) {
		t.Fatalf("MergeReady = %+v, %v", check, err)
	}
	if err := p.AddLabel(context.Background(), "o/alux", 12, "ready"); err != nil || <-labelled != "#12 ready" {
		t.Fatalf("AddLabel: %v", err)
	}
}

func TestTheSessionNamesIDsForTheTUI(t *testing.T) {
	_, path, _ := serveWith(t, func(srv *Server) {
		srv.Name = func(id string) string { return map[string]string{"#4242": "Fix the importer"}[id] }
	})
	c, _ := dial(t, path)
	if got := c.LookupName(context.Background(), "#4242"); got != "Fix the importer" {
		t.Fatalf("LookupName = %q", got)
	}
	if got := c.LookupName(context.Background(), "#9999"); got != "" {
		t.Fatalf("an unknown id was named %q", got)
	}
}

func TestTheGuardianCheckReachesATUIAttachingBeforeOrAfterIt(t *testing.T) {
	_, path, srv := serveWith(t, func(srv *Server) { srv.Hello.Setup = &ui.Setup{Project: "alux"} })
	checks := make(chan error, 2)
	before, err := Dial(path, Events{Guardian: func(err error) { checks <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if setup := before.Hello().Setup; setup == nil || setup.Project != "alux" {
		t.Fatalf("setup on attach: %+v", setup)
	}
	srv.GuardianChecked(errors.New("ollama is not running"))
	if got := <-checks; got == nil || got.Error() != "ollama is not running" {
		t.Fatalf("the TUI attached before the check got %v", got)
	}
	before.Close()
	deadline := time.Now().Add(5 * time.Second)
	for srv.Attached() {
		if time.Now().After(deadline) {
			t.Fatal("still attached after the first TUI left")
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := Dial(path, Events{Guardian: func(err error) { checks <- err }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { after.Close() })
	select {
	case got := <-checks:
		if got == nil || got.Error() != "ollama is not running" {
			t.Fatalf("the TUI attached after the check got %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a TUI attaching after the check never got it")
	}
}

func TestEverySnapshotFieldSurvivesJSON(t *testing.T) {
	seen := map[reflect.Type]bool{}
	// shadowed names the fields an outer struct declares, which JSON takes over an embedded one's.
	var walk func(t reflect.Type, path string, shadowed map[string]bool) []string
	walk = func(typ reflect.Type, path string, shadowed map[string]bool) []string {
		if seen[typ] {
			return nil
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Func, reflect.Chan, reflect.Interface, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
			return []string{path + " is a " + typ.Kind().String()}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			return walk(typ.Elem(), path+"[]", nil)
		case reflect.Map:
			return walk(typ.Elem(), path+"{}", nil)
		case reflect.Struct:
			if typ == reflect.TypeFor[time.Time]() {
				return nil
			}
			own := map[string]bool{}
			for name := range shadowed {
				own[name] = true
			}
			for i := range typ.NumField() {
				if f := typ.Field(i); !f.Anonymous {
					own[f.Name] = true
				}
			}
			var bad []string
			for i := range typ.NumField() {
				f := typ.Field(i)
				if shadowed[f.Name] {
					continue
				}
				if f.Anonymous {
					bad = append(bad, walk(f.Type, path+"."+f.Name, own)...)
					continue
				}
				if !f.IsExported() {
					bad = append(bad, path+"."+f.Name+" is unexported")
					continue
				}
				if f.Tag.Get("json") == "-" {
					bad = append(bad, path+"."+f.Name+" is left out of JSON")
					continue
				}
				bad = append(bad, walk(f.Type, path+"."+f.Name, nil)...)
			}
			return bad
		}
		return nil
	}
	for name, typ := range map[string]reflect.Type{
		"Snapshot": reflect.TypeFor[session.Snapshot](),
		"Hello":    reflect.TypeFor[Hello](),
		"RepoView": reflect.TypeFor[orchestrator.RepoView](),
		"Unlanded": reflect.TypeFor[orchestrator.Unlanded](),
		"Sweep":    reflect.TypeFor[sweepResult](),
		"PRs":      reflect.TypeFor[prStates](),
		"Review":   reflect.TypeFor[reviewstore.Review](),
		"App":      reflect.TypeFor[prs.ReviewApp](),
		"Merge":    reflect.TypeFor[prs.MergeCheck](),
		"Guardian": reflect.TypeFor[guardianResult](),
	} {
		for _, problem := range walk(typ, name, nil) {
			t.Error(problem)
		}
	}
}
