package ui

import (
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/workers"
)

type answer struct {
	pid     int
	id      string
	approve bool
}

// parkWorker has pid 99 report p as parked and returns where m's answers to it land.
func parkWorker(m *Model, p workers.Prompt) *[]answer {
	var answers []answer
	m.ParkedPrompt = func(pid int) (workers.Prompt, bool) { return p, pid == 99 }
	m.AnswerPrompt = func(pid int, id string, approve bool, _ *ask.Answer) (bool, error) {
		answers = append(answers, answer{pid, id, approve})
		return true, nil
	}
	return &answers
}

func workerWaiting(key, promptID string) SessionsMsg {
	return SessionsMsg{Sessions: []registry.Info{{PID: 99, Name: "headless", Worker: key, PromptID: promptID, State: registry.Waiting}}}
}

func TestYAnswersTheWorkerPromptOnScreenWhenTheOwnSessionParksBehindIt(t *testing.T) {
	m, s := harness(t, 80, 40)
	answers := parkWorker(m, workers.Prompt{ID: "99-1", Kind: "approval", Label: "$ make worker"})
	send(m, workerWaiting("ABC-1", "99-1"))
	if out := screen(m); !strings.Contains(out, "[ABC-1]") || !strings.Contains(out, "make worker") {
		t.Fatalf("the worker prompt is not on screen:\n%s", out)
	}

	own := s.ParkForTest("$ own command")
	settle(m)
	if out := screen(m); !strings.Contains(out, "[ABC-1]") || strings.Contains(out, "own command") {
		t.Fatalf("the own park took the screen from the worker prompt:\n%s", out)
	}

	press(m, "y")
	if len(*answers) != 1 || (*answers)[0] != (answer{99, "99-1", true}) {
		t.Errorf("answers %+v, want 99-1 approved at pid 99", *answers)
	}
	select {
	case ok := <-own:
		t.Fatalf("y approved the own prompt (%v) behind the worker's", ok)
	default:
	}
	if s.Snapshot().Pending == "" {
		t.Fatal("the own prompt was resolved by the worker's answer")
	}
	settle(m)
	if out := screen(m); !strings.Contains(out, "own command") || strings.Contains(out, "[ABC-1]") {
		t.Errorf("the own prompt did not come up once the worker was answered:\n%s", out)
	}
}

func TestTheOwnPromptIsShownBeforeAWaitingWorkerPrompt(t *testing.T) {
	m, s := harness(t, 80, 40)
	parkWorker(m, workers.Prompt{ID: "99-1", Kind: "approval", Label: "$ make worker"})
	s.ParkForTest("$ own command")
	settle(m)
	send(m, workerWaiting("ABC-1", "99-1"))
	if out := screen(m); !strings.Contains(out, "own command") || strings.Contains(out, "[ABC-1]") {
		t.Errorf("the worker prompt was shown ahead of the own one:\n%s", out)
	}
}

func TestAWorkerPromptTheRegistryNoLongerNamesIsNotShown(t *testing.T) {
	m, _ := harness(t, 80, 40)
	parkWorker(m, workers.Prompt{ID: "99-1", Kind: "approval", Label: "$ make worker"})
	send(m, workerWaiting("ABC-1", "99-2"))
	if out := screen(m); strings.Contains(out, "[ABC-1]") {
		t.Errorf("a prompt the worker has moved past was shown:\n%s", out)
	}
}

func TestAWorkerPromptFromAnotherRepoIsNotShown(t *testing.T) {
	m, _ := harness(t, 80, 40)
	parkWorker(m, workers.Prompt{ID: "99-1", Kind: "approval", Label: "$ make worker"})
	m.Origin = "/repo"
	msg := workerWaiting("ABC-1", "99-1")
	msg.Sessions[0].Origin = "/other"
	send(m, msg)
	if out := screen(m); strings.Contains(out, "[ABC-1]") {
		t.Errorf("another repo's worker prompt was shown:\n%s", out)
	}
}
