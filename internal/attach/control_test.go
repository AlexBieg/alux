package attach

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/session"
)

func TestAnotherPaneAnswersOnlyThePromptItWasShownAndCanSendAMessage(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	pid := os.Getpid()
	path, err := ControlPath(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	s := session.New(&agent.Agent{}, nil)
	ctl, err := ListenControl(path, s)
	if err != nil {
		t.Fatal(err)
	}
	ctl.refuse = func(int) error { return nil }
	go ctl.Serve()
	t.Cleanup(func() { ctl.Close() })

	if _, ok := ParkedPrompt(pid); ok {
		t.Fatal("a prompt was reported with nothing parked")
	}
	parked := s.ParkForTest("$ make")
	p, ok := ParkedPrompt(pid)
	if !ok || p.Kind != "approval" || p.Label != "$ make" || p.ID != PromptID(pid, s.Snapshot()) {
		t.Fatalf("parked prompt %+v, %v", p, ok)
	}
	if resolved, err := AnswerPrompt(pid, p.ID+"0", true, nil); err != nil || resolved {
		t.Fatalf("an answer for another prompt resolved it: %v, %v", resolved, err)
	}
	if resolved, err := AnswerPrompt(pid, p.ID, true, nil); err != nil || !resolved {
		t.Fatalf("the answer for the parked prompt: %v, %v", resolved, err)
	}
	select {
	case okay := <-parked:
		if !okay {
			t.Fatal("the approval arrived as a decline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked approval was never resolved")
	}

	if err := SendMessage(pid, "now the tests"); err != nil {
		t.Fatal(err)
	}
	if q := s.Snapshot().Queued; len(q) != 1 || q[0].Text != "now the tests" {
		t.Fatalf("queued %+v, want the message", q)
	}

	watched := true
	stops := 0
	ctl.Stop = func() error {
		if watched {
			return errors.New("a UI is attached to it")
		}
		stops++
		return nil
	}
	ctl.Watched = func() bool { return watched }
	if w, err := Watched(pid); err != nil || !w {
		t.Fatalf("watched while a UI is attached: %v, %v", w, err)
	}
	if err := StopSession(pid); err == nil || !strings.Contains(err.Error(), "a UI is attached") {
		t.Fatalf("a stop while watched returned %v", err)
	}
	watched = false
	if w, err := Watched(pid); err != nil || w {
		t.Fatalf("watched with no UI attached: %v, %v", w, err)
	}
	if err := StopSession(pid); err != nil || stops != 1 {
		t.Fatalf("a stop while unwatched: %v, %d stops", err, stops)
	}
}
