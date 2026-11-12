package workers

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTwoTUIsClaimingOneFinalDeliverItOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	channel(t, "ABC-1", map[string]any{
		"worker.json": Meta{V: Version, Origin: "/a", PID: deadPID(t), Session: "s1"},
		"final.json":  Final{V: Version, State: Done, How: "finished", Message: "PR is up."},
	})
	first, second := Claim("/a", 101), Claim("/a", 102)
	if len(first) != 1 || first[0].Key != "ABC-1" || first[0].Final.Message != "PR is up." || len(second) != 0 {
		t.Fatalf("claims = %+v then %+v, want ABC-1 once", first, second)
	}
	dir, _ := Dir("ABC-1")
	if _, err := os.Stat(filepath.Join(dir, "final.delivered.101.json")); err != nil {
		t.Errorf("the winning claim left no delivered file: %v", err)
	}
}

func TestTheLedgerStillListsAWorkerAfterItsFinalIsDelivered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	channel(t, "ABC-1", map[string]any{
		"worker.json": Meta{V: Version, Origin: "/a", PID: deadPID(t), Session: "s1"},
		"final.json":  Final{V: Version, State: Done, How: "finished", Message: "PR is up."},
	})
	if len(Claim("/a", 101)) != 1 {
		t.Fatal("nothing was claimed")
	}
	got := Read("/a")
	if len(got) != 1 || got[0].State != Done || got[0].Message != "PR is up." {
		t.Fatalf("Read(/a) = %+v, want ABC-1 done with its message", got)
	}
}

func TestAFollowUpReachesAnIdleWorkerAndNotAFinishedOne(t *testing.T) {
	var got []string
	deliver := func(pid int, text string) error {
		got = append(got, fmt.Sprintf("%d %s", pid, text))
		return nil
	}
	list := []Worker{{Key: "WORKER-1", PID: 41, State: "idle"}, {Key: "WORKER-2", PID: 42, State: Done}}
	if _, err := Send(list, "worker-1", "now the tests", deliver); err != nil {
		t.Fatalf("sending to an idle worker: %v", err)
	}
	if _, err := Send(list, "WORKER-2", "too late", deliver); err == nil {
		t.Error("a finished worker accepted a message")
	}
	if len(got) != 1 || got[0] != "41 now the tests" {
		t.Errorf("delivered %v, want the idle worker's one message", got)
	}
}
