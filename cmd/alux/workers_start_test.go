package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWorkerStartRefusesAWorkerPastMaxWorkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := filepath.Join(home, ".config", "alux")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "settings.json"), []byte(`{"max_workers": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := workerDeps{origin: t.TempDir(), exe: "/bin/true", mode: "local", spawn: func(cmd *exec.Cmd) error {
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			return err
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		cmd.Process = child.Process
		return nil
	}}
	yes := func(string, string, string) bool { return true }

	out, err := startWorker(context.Background(), workerStartInput{Key: "abc-1", Task: "first"}, d, yes)
	if err != nil || !strings.HasPrefix(out, "worker ABC-1 started: pid ") {
		t.Fatalf("the first worker gave %q, %v", out, err)
	}
	_, err = startWorker(context.Background(), workerStartInput{Key: "ABC-2", Task: "second"}, d, yes)
	if err == nil || !strings.Contains(err.Error(), "MaxWorkers is 1") || !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("a second worker past max_workers 1 gave %v", err)
	}
}

func TestAWorkerThatExitsBeforeRegisteringIsReportedWithItsLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logPath := filepath.Join(t.TempDir(), "T-1.log")
	if err := os.WriteFile(logPath, []byte("alux: moving to base main: trunk main@origin does not resolve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := startProcess(cmd); err != nil {
		t.Fatal(err)
	}
	err := awaitWorker("T-1", cmd.Process.Pid, logPath)
	if err == nil || !strings.Contains(err.Error(), "exited before it registered") || !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("a worker that died at start gave %v", err)
	}
}

func TestAWorkerRunsInItsOwnSessionFromTheDefaultWorkspace(t *testing.T) {
	cmd := workerCommand("/bin/alux", "/repo", "ABC-1", "auto", "main", "fix it")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("the worker shares the TUI's session, so closing its pane would hang it up")
	}
	if cmd.Dir != "/repo" {
		t.Errorf("the worker starts in %s, want the default workspace", cmd.Dir)
	}
	want := []string{"/bin/alux", "--serve", "--worker", "ABC-1", "--mode", "auto", "--base", "main", "--prompt", "fix it"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("argv %q, want %q", cmd.Args, want)
	}
}
