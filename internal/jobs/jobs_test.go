package jobs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/report"
)

func TestParseDefaultsToTheCuratorWithNoJobsBlock(t *testing.T) {
	list, err := Parse([]byte(`{"prs": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != (Job{Name: "curator", Agent: "curator", Every: 3 * time.Hour}) {
		t.Fatalf("jobs = %+v, want the curator every 3h", list)
	}
}

func TestParseLeavesOutADisabledJob(t *testing.T) {
	list, err := Parse([]byte(`{"jobs": {"curator": {"every": "3h", "enabled": false}, "tidy": {"agent": "curator", "every": "1h"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != (Job{Name: "tidy", Agent: "curator", Every: time.Hour}) {
		t.Fatalf("jobs = %+v, want tidy alone", list)
	}
}

func TestParseRefusesAnEveryUnderTenMinutes(t *testing.T) {
	if _, err := Parse([]byte(`{"jobs": {"curator": {"every": "5m"}}}`)); err == nil {
		t.Fatal("want an error for every 5m")
	}
}

func TestLoopNotifiesOnceForARepeatedErrorAndAgainWhenItChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	errs := []error{errors.New("a"), errors.New("a"), errors.New("b")}
	calls := make(chan struct{})
	start := func(Job, string) error {
		defer func() { calls <- struct{}{} }()
		err := errs[0]
		errs = errs[1:]
		return err
	}
	var notices []string
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		Loop(ctx, []Job{{Name: "curator", Agent: "curator", Every: time.Hour}}, tick, start, func(s string) { notices = append(notices, s) })
		close(done)
	}()
	<-calls
	tick <- time.Now()
	<-calls
	tick <- time.Now()
	<-calls
	cancel()
	<-done
	want := []string{"job curator: not started — a", "job curator: not started — b"}
	if strings.Join(notices, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notices = %q, want %q", notices, want)
	}
}

func TestStatusShowsARunningEntryAsUnknownOnceItsProcessHasExited(t *testing.T) {
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	list := []Job{{Name: "curator", Agent: "curator", Every: time.Hour}}
	for _, c := range []struct {
		pid  int
		want string
	}{
		{os.Getpid(), "running · run 4"},
		{gone.Process.Pid, "the harness running it exited before it finished"},
	} {
		t.Setenv("HOME", t.TempDir())
		if err := Append(Entry{Job: "curator", Run: 4, PID: c.pid, Started: time.Now(), Outcome: Running}); err != nil {
			t.Fatal(err)
		}
		if got := Table(list, time.Now()).Plain(); !strings.Contains(got, c.want) {
			t.Errorf("pid %d: status lacks %q:\n%s", c.pid, c.want, got)
		}
	}
}

func TestStatusShowsAFailedRunsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	if err := Append(Entry{Job: "curator", Run: 3, Started: now, Ended: now, Outcome: "failed", Err: "model overloaded"}); err != nil {
		t.Fatal(err)
	}
	got := Table([]Job{{Name: "curator", Agent: "curator", Every: time.Hour}}, now).Plain()
	if !strings.Contains(got, "failed") || !strings.Contains(got, "model overloaded") {
		t.Fatalf("status does not show the failure:\n%s", got)
	}
}

func TestAFailedLastRunReadsAsBad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	if err := Append(Entry{Job: "curator", Run: 3, Started: now, Ended: now, Outcome: "failed", Err: "model overloaded"}); err != nil {
		t.Fatal(err)
	}
	row := Table([]Job{{Name: "curator", Agent: "curator", Every: time.Hour}}, now).Rows[0]
	if row.Cells[3].Tone != report.Bad {
		t.Fatalf("last run %q has tone %q, want bad", row.Cells[3].Text, row.Cells[3].Tone)
	}
}
