package commands

import (
	"fmt"
	"testing"

	"github.com/alexbieg/alux/internal/workers"
)

func TestWorkersSendHandsTheRestOfTheLineToTheNamedWorker(t *testing.T) {
	var sent []string
	SendWorker = func(pid int, text string) error {
		sent = append(sent, fmt.Sprintf("%d %s", pid, text))
		return nil
	}
	t.Cleanup(func() { SendWorker = nil })
	ctx := Context{Workers: func() []workers.Worker {
		return []workers.Worker{{Key: "ABC-1", PID: 41, State: "running"}}
	}}
	if got := run(t, "workers", "send abc-1 hi there", ctx); got != "sent to ABC-1" {
		t.Errorf("send to a live worker showed %q", got)
	}
	if len(sent) != 1 || sent[0] != "41 hi there" {
		t.Errorf("sent %v, want hi there to pid 41", sent)
	}
}
