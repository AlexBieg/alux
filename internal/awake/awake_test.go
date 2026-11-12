package awake

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fake is a keeper whose caffeinate is a script logging each start's pid and arguments to log.
func fake(t *testing.T, power *Power) (*Keeper, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	bin := filepath.Join(dir, "caffeinate")
	script := "#!/bin/sh\necho \"$$ $*\" >> " + log + "\nexec sleep 300\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	k := &Keeper{bin: bin, pid: 4242, floor: Floor, every: time.Hour, power: func() (Power, error) {
		mu.Lock()
		defer mu.Unlock()
		return *power, nil
	}}
	t.Cleanup(k.Close)
	return k, log
}

// starts is each caffeinate started so far, as "pid args".
func starts(t *testing.T, log string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(log)
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.Split(s, "\n")
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func pidOf(t *testing.T, line string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.Fields(line)[0])
	if err != nil {
		t.Fatalf("log line %q has no pid", line)
	}
	return pid
}

func TestOverlappingWorkTakesOneAssertionAndTheLastToFinishReleasesIt(t *testing.T) {
	k, log := fake(t, &Power{})
	turn := k.Hold()
	subagent := k.Hold()
	if k.Count() != 2 || k.State() != Holding {
		t.Fatalf("count %d state %v, want 2 holding", k.Count(), k.State())
	}
	got := starts(t, log)
	if len(got) != 1 {
		t.Fatalf("caffeinate started %d times for two overlapping holds, want once: %q", len(got), got)
	}
	pid := pidOf(t, got[0])

	turn.Release()
	turn.Release()
	if k.Count() != 1 || k.State() != Holding || !alive(pid) {
		t.Fatalf("after the first release: count %d state %v alive %v, want 1 holding alive", k.Count(), k.State(), alive(pid))
	}
	subagent.Release()
	if k.Count() != 0 || k.State() != Off {
		t.Fatalf("after the last release: count %d state %v, want 0 off", k.Count(), k.State())
	}
	if alive(pid) {
		t.Fatal("caffeinate still running once nothing is in flight")
	}
}

func TestCaffeinateIsOnlyEverGivenIdleSleep(t *testing.T) {
	for _, pid := range []int{1, 4242, 99999} {
		args := Args(pid)
		if !slices.Equal(args, []string{"-i", "-w", strconv.Itoa(pid)}) {
			t.Fatalf("Args(%d) = %q, want -i -w %d and nothing else", pid, args, pid)
		}
	}
	k, log := fake(t, &Power{})
	k.Hold().Release()
	k.Hold()
	for _, line := range starts(t, log) {
		if got := strings.Fields(line)[1:]; !slices.Equal(got, []string{"-i", "-w", "4242"}) {
			t.Fatalf("caffeinate ran with %q, want -i -w 4242 alone", got)
		}
	}
}

func TestOnBatteryItHoldsAboveTheFloorAndLetsTheMacSleepBelowIt(t *testing.T) {
	power := &Power{Battery: true, Percent: Floor - 1}
	k, log := fake(t, power)
	k.Hold()
	if k.State() != LowBattery {
		t.Fatalf("state %v on %d%% battery, want low battery", k.State(), power.Percent)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("caffeinate started under the floor: %q", b)
	}

	power.Percent = Floor
	k.recheck()
	got := starts(t, log)
	if k.State() != Holding || len(got) != 1 {
		t.Fatalf("state %v starts %q at the floor, want holding once", k.State(), got)
	}

	power.Percent = Floor - 1
	k.recheck()
	if k.State() != LowBattery || alive(pidOf(t, got[0])) {
		t.Fatalf("state %v, caffeinate alive %v after dropping under the floor; want it released", k.State(), alive(pidOf(t, got[0])))
	}
}

func TestWaitingOnTheUserIsNotCounted(t *testing.T) {
	k, _ := fake(t, &Power{})
	h := k.Hold()
	h.Pause()
	if k.Count() != 0 || k.State() != Off {
		t.Fatalf("count %d state %v while paused, want 0 off", k.Count(), k.State())
	}
	h.Resume()
	if k.Count() != 1 {
		t.Fatalf("count %d after resuming, want 1", k.Count())
	}
	h.Pause()
	h.Release()
	h.Resume()
	if k.Count() != 0 {
		t.Fatalf("count %d after a release while paused then a resume, want 0", k.Count())
	}
}

func TestCaffeinateLetsGoWhenTheProcessItWatchesDies(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("caffeinate is macOS only")
	}
	watched := exec.Command("sleep", "300")
	if err := watched.Start(); err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{}, 4)
	k := New()
	k.pid = watched.Process.Pid
	k.OnChange = func() { changed <- struct{}{} }
	t.Cleanup(k.Close)
	k.power = func() (Power, error) { return Power{}, nil }
	k.Hold()
	<-changed
	if k.State() != Holding {
		t.Fatalf("state %v, want holding", k.State())
	}

	_ = watched.Process.Kill()
	_ = watched.Wait()
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("caffeinate kept holding after the process it watches died")
	}
	if k.State() != Off {
		t.Fatalf("state %v after the watched process died, want off", k.State())
	}
}

func TestParsePower(t *testing.T) {
	for out, want := range map[string]Power{
		"Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged; 0:00 remaining present: true": {},
		"Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t18%; discharging; 1:02 remaining":       {Battery: true, Percent: 18},
		"Now drawing from 'AC Power'\n": {},
	} {
		if got := parsePower(out); got != want {
			t.Errorf("parsePower(%q) = %+v, want %+v", out, got, want)
		}
	}
}
