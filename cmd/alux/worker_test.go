package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/skillrun"
	"github.com/alexbieg/alux/internal/workers"
)

func TestAnIdleWorkerWithASkillOpenIsStalledOnlyWhenNoSubagentOfItsOwnIsLive(t *testing.T) {
	idle := session.Snapshot{Skill: &session.SkillView{Name: "dev-cycle"}}
	if got := workerState(idle, "", 1); got != "running" {
		t.Errorf("idle while its coder runs reads %q, want running", got)
	}
	if got := workerState(idle, "", 0); got != "stalled" {
		t.Errorf("idle with nothing running reads %q, want stalled", got)
	}
}

func TestAnIdleWorkerWithNoSkillRunReadsIdleOnlyOnceNothingIsQueued(t *testing.T) {
	if got := workerState(session.Snapshot{}, "", 0); got != "idle" {
		t.Errorf("a finished skill-less turn reads %q, want idle", got)
	}
	queued := session.Snapshot{Queued: []session.Queued{{Text: "the task"}}}
	if got := workerState(queued, "", 0); got != "running" {
		t.Errorf("a task not yet picked up reads %q, want running", got)
	}
}

func TestAWorkerEndsOnlyOnceItsSkillRunClosedAndTheSessionIsIdle(t *testing.T) {
	dir := t.TempDir()
	s := session.New(&agent.Agent{}, nil)
	run := &workerRun{key: "ABC-1", dir: dir, s: s, reg: registry.New(filepath.Join(dir, "sessions"), "t", dir), finished: make(chan workers.Final, 1)}
	s.SetBusyForTest(true)
	run.ended = skillrun.Finished
	run.publish()
	if _, err := os.Stat(filepath.Join(dir, "final.json")); err == nil {
		t.Fatal("the worker ended while its closing turn was still running")
	}
	s.PushAssistantForTest("PR is up.")
	s.SetBusyForTest(false)
	run.publish()
	run.publish()
	var final workers.Final
	if err := workers.ReadJSON(filepath.Join(dir, "final.json"), &final); err != nil || final.State != workers.Done || final.Message != "PR is up." {
		t.Fatalf("final.json = %+v, %v", final, err)
	}
	if got := <-run.finished; got != final || len(run.finished) != 0 {
		t.Fatalf("finished %+v, and %d more", got, len(run.finished))
	}
}

func TestWorkerFlagsAreRefusedWithoutServeOrModeWithHereOrWithoutASlot(t *testing.T) {
	cases := []struct {
		name         string
		here, serve  bool
		worker, mode string
		slot         *orchestrator.Slot
		wantErr      bool
	}{
		{"no serve", false, false, "abc-1", "local", &orchestrator.Slot{}, true},
		{"no mode", false, true, "abc-1", "", &orchestrator.Slot{}, true},
		{"unknown mode", false, true, "abc-1", "yolo", &orchestrator.Slot{}, true},
		{"with --here", true, true, "abc-1", "local", &orchestrator.Slot{}, true},
		{"no slot", false, true, "abc-1", "local", nil, true},
		{"complete", false, true, "abc-1", "read-only", &orchestrator.Slot{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, mode, err := checkWorker(c.here, c.serve, c.worker, c.mode)
			if err == nil {
				err = requireSlot(true, c.slot)
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("err %v, want error %t", err, c.wantErr)
			}
			if !c.wantErr && (key != "ABC-1" || mode != session.ModeReadOnly) {
				t.Fatalf("got %q %q", key, mode)
			}
		})
	}
}
