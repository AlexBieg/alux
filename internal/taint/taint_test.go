package taint

import (
	"sync"
	"testing"
)

// A latch that answers no until something sets it, and yes from then on. The one-way half is the
// point: the package offers nothing that puts it back, so a conversation cannot argue its way out
// of a mark it has already read.
func TestTheLatchOnlyGoesOneWay(t *testing.T) {
	t.Cleanup(ResetForTest)
	ResetForTest()

	if Tainted() {
		t.Fatal("the latch starts set")
	}
	Set()
	if !Tainted() {
		t.Fatal("Set did not latch")
	}
	Set()
	if !Tainted() {
		t.Error("a second Set cleared the latch")
	}
}

// Several alux sessions, their subagents and their background filings run as goroutines in one
// process, so the latch is read and set concurrently.
func TestSettingAndReadingConcurrentlyIsSafe(t *testing.T) {
	t.Cleanup(ResetForTest)
	ResetForTest()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Set()
			Tainted()
		}()
	}
	wg.Wait()
	if !Tainted() {
		t.Error("the latch is not set after concurrent Sets")
	}
}
