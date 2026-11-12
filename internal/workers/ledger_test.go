package workers

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/registry"
)

// register writes a registry entry under HOME for a pid that is alive and not this process.
func register(t *testing.T, info registry.Info) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), paths.Cache, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(info.PID)+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func channel(t *testing.T, key string, files map[string]any) {
	t.Helper()
	dir, err := Dir(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, v := range files {
		if err := WriteJSON(filepath.Join(dir, name), v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadListsOnlyTheWorkersOfTheGivenOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	register(t, registry.Info{PID: os.Getppid(), Worker: "ABC-1", Origin: "/a", PromptID: "1-1", WorkerState: "waiting"})
	register(t, registry.Info{PID: 1, Worker: "XYZ-2", Origin: "/b"})
	got := Read("/a")
	if len(got) != 1 || got[0].Key != "ABC-1" || !got[0].Waiting || got[0].State != "waiting" {
		t.Fatalf("Read(/a) = %+v, want only ABC-1, waiting", got)
	}
}

func TestAWorkerThatDiedMidRunReadsAsCrashedUnlessItWasStopped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	channel(t, "ABC-1", map[string]any{"worker.json": Meta{V: Version, Origin: "/a", PID: deadPID(t), Session: "s1", Log: "/l/ABC-1.log"}})
	channel(t, "ABC-2", map[string]any{"worker.json": Meta{V: Version, Origin: "/a", PID: deadPID(t), Session: "s2"}})
	channel(t, "XYZ-3", map[string]any{"worker.json": Meta{V: Version, Origin: "/b", PID: deadPID(t), Session: "s3"}})
	if err := MarkStopped("ABC-2"); err != nil {
		t.Fatal(err)
	}
	got := Read("/a")
	if len(got) != 1 || got[0].Key != "ABC-1" || got[0].State != Crashed || got[0].Waiting || got[0].Log != "/l/ABC-1.log" {
		t.Fatalf("Read(/a) = %+v, want only ABC-1, crashed", got)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestAWorkerThatExitedWithAFinalReadsAsItsEndAndIsNotLive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	channel(t, "WORKER-3", map[string]any{
		"worker.json": Meta{V: Version, Origin: "/a", PID: deadPID(t), Session: "s3"},
		"final.json":  Final{V: Version, State: Failed, How: "failed: stopped at intake", Message: "Nothing changed."},
	})
	got := Read("/a")
	if len(got) != 1 || got[0].State != Failed || got[0].Ended != "failed: stopped at intake" || got[0].Message != "Nothing changed." {
		t.Fatalf("Read(/a) = %+v, want WORKER-3 failed with its final message", got)
	}
	if live := Live("/a"); len(live) != 0 {
		t.Errorf("a worker that exited counted against MaxWorkers: %v", live)
	}
}

func TestAWorkerThatDiedBeforeItStartedReadsAsFailedAndACleanStopIsNotListed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	channel(t, "WORKER-1", map[string]any{"worker.json": Meta{V: Version, Origin: "/a", PID: deadPID(t), Log: "/l/WORKER-1-9.log"}})
	channel(t, "WORKER-2", map[string]any{"worker.json": Meta{V: Version, Origin: "/a", PID: deadPID(t), Session: "s2"}})
	if err := MarkStopped("WORKER-2"); err != nil {
		t.Fatal(err)
	}
	got := Read("/a")
	if len(got) != 1 || got[0].Key != "WORKER-1" || got[0].State != Failed || got[0].Log != "/l/WORKER-1-9.log" {
		t.Fatalf("Read(/a) = %+v, want only WORKER-1, failed, with its start log", got)
	}
	if live := Live("/a"); len(live) != 0 {
		t.Errorf("a failed worker counted against MaxWorkers: %v", live)
	}
}
