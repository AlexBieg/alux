package subagent

import (
	"testing"

	"github.com/alexbieg/alux/internal/awake"
)

func TestARunKeepsTheMacAwakeOnlyWhileItWorks(t *testing.T) {
	r := NewRoster(nil)
	r.Awake = awake.CounterForTest()
	count := func(want int, when string) {
		t.Helper()
		if got := r.Awake.Count(); got != want {
			t.Fatalf("count %d %s, want %d", got, when, want)
		}
	}

	driver := r.start("pair", "fix", "fix the thing", 10, true)
	count(1, "once a run started")
	r.startPeer(driver.run.ID, "", "navigator", "fix", 4)
	count(1, "with a resting navigator beside its driver")
	driver.waiting(true)
	count(0, "while the run waits on the user")
	driver.waiting(false)
	count(1, "once the user answered")
	other := r.start("research", "look", "look into it", 10, true)
	count(2, "with two runs working")
	other.fail("boom")
	driver.finish("done")
	count(0, "once both runs ended")
}
