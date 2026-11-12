package procs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/awake"
)

func counting(t *testing.T) (*Registry, *awake.Keeper) {
	t.Helper()
	r := NewRegistry()
	r.Awake = awake.CounterForTest()
	t.Cleanup(func() { r.KillAll() })
	return r, r.Awake
}

func held(k *awake.Keeper, n int) func() bool { return func() bool { return k.Count() == n } }

func TestAFiniteBackgroundCommandHoldsUntilItExitsPastItsCheckIn(t *testing.T) {
	t.Parallel()
	r, k := counting(t)
	r.checkInUnit = testCheckInUnit
	ch := make(chan struct{}, 8)
	r.OnExit = func() { ch <- struct{}{} }
	exit := filepath.Join(t.TempDir(), "exit")
	r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "until [ -e '" + exit +
		"' ]; do sleep 0.01; done", "check_in": 5, "lifetime": "finite"}), approving())
	if k.Count() != 1 {
		t.Fatalf("count %d once a finite command started, want 1", k.Count())
	}
	if e := news(t, r, ch); e.Event != EventCheckIn {
		t.Fatalf("news = %+v, want its check-in to pass while it still runs", e)
	}
	if k.Count() != 1 {
		t.Fatalf("count %d after its check-in passed with the command still running, want 1", k.Count())
	}
	if err := os.WriteFile(exit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the hold to be released at exit", held(k, 0))
}

func TestAnOngoingBackgroundCommandNeverHolds(t *testing.T) {
	t.Parallel()
	r, k := counting(t)
	r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "sleep 30", "check_in": 600,
		"lifetime": "ongoing"}), approving())
	if len(r.Running()) != 1 {
		t.Fatal("the ongoing command did not start")
	}
	time.Sleep(100 * time.Millisecond)
	if k.Count() != 0 {
		t.Fatalf("count %d for an ongoing command, want 0", k.Count())
	}
}

func TestRunBackgroundWithoutALifetimeIsRefused(t *testing.T) {
	t.Parallel()
	r, _ := counting(t)
	for _, lifetime := range []any{nil, "", "forever"} {
		input := map[string]any{"command": "sleep 30", "check_in": 600}
		if lifetime != nil {
			input["lifetime"] = lifetime
		}
		got := text(t, r.runStart(context.Background(), call(t, "run_background", input), approving()))
		if !strings.Contains(got, "lifetime is required") {
			t.Errorf("lifetime %v answered %q, want it refused", lifetime, got)
		}
	}
	if len(r.Running()) != 0 {
		t.Fatal("a refused call started something")
	}
}

func TestAForegroundCommandHoldsWhileItRunsAndAfterItIsHeld(t *testing.T) {
	r, k := counting(t)
	withForegroundLimit(t, 150*time.Millisecond)
	done := make(chan struct{})
	go func() {
		r.Bash()(context.Background(), call(t, "bash", map[string]any{"command": "sleep 30"}), approving())
		close(done)
	}()
	waitFor(t, "the foreground command to hold", held(k, 1))
	<-done
	if k.Count() != 1 {
		t.Fatalf("count %d once the command was held at its limit, want 1", k.Count())
	}
	r.KillHeld()
	waitFor(t, "the killed command to release its hold", held(k, 0))
}

func TestAPollHoldsUntilItStops(t *testing.T) {
	t.Parallel()
	r, k := counting(t)
	r.runPoll(context.Background(), call(t, "poll", map[string]any{"command": "echo pending", "until": "done",
		"check_in": 600}), approving())
	if k.Count() != 1 {
		t.Fatalf("count %d while polling, want 1", k.Count())
	}
	r.KillAll()
	waitFor(t, "the poll to stop and release its hold", held(k, 0))
}
