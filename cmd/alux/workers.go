package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/workers"
	"github.com/anthropics/anthropic-sdk-go"
)

const (
	workersTool     = "workers"
	workerStartTool = "worker_start"
	workerStopTool  = "worker_stop"
)

func workersParam() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: workersTool,
		Description: anthropic.String("This repo's headless workers, one line each: key · stage · state · ⚑ when it waits on " +
			"a prompt · mode · time since its last activity · workspace · audit log path. State is running, waiting, " +
			"stalled (idle with a skill run open), idle (finished its turn with no skill run, still live), crashed (its process is gone with a prompt still parked), done or " +
			"failed: <reason> (its skill run closed and it exited), or failed alone (it exited before it got going; its log says why).\n\n" +
			"Read-only, quick, and no approval is asked for it."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties:  map[string]any{},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}
}

func workersHandler(origin string) agent.Handler {
	return func(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		label := "workers"
		if cb.OnToolStart != nil {
			cb.OnToolStart(call.ID, label, agent.KindSearch)
		}
		out := workers.Lines(workers.Read(origin), time.Now())
		if cb.OnToolResult != nil {
			cb.OnToolResult(call.ID, label, out, agent.KindSearch)
		}
		return anthropic.NewToolResultBlock(call.ID, out, false)
	}
}

func workerStartParam() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: workerStartTool,
		Description: anthropic.String("Only when the user asks: start a headless alux in a pool workspace of its own, its first " +
			"message being task as written. For ticketed work, have the task run the dev-cycle skill on the ticket " +
			"(\"Run the dev-cycle skill on ABC-123: <goal>\"); otherwise give the task in plain words. It exits when its skill " +
			"run closes; with none it stays idle, taking follow-ups, until stopped. It stops for the user's yes, then returns the key, pid and log path; " +
			"follow it with the workers tool. Refused past max_workers live workers in this repo (settings.json, default 1)."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"key":  map[string]any{"type": "string", "description": "The worker's label, such as the ticket key ABC-123. Leave it out for a one-off, numbered WORKER-N."},
				"task": map[string]any{"type": "string", "description": "The session's first message, sent as written."},
				"base": map[string]any{"type": "string", "enum": []string{orchestrator.BaseMain, "@-"},
					"description": "main (the default) starts on the freshly fetched trunk; @- starts on this session's @- commit."},
			},
			Required:    []string{"task"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}
}

func workerStopParam() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: workerStopTool,
		Description: anthropic.String("Only when the user asks: stop a worker of this repo, stopping for the user's yes. A live " +
			"worker is sent SIGTERM and releases its workspace, keeping its described work; a crashed or failed one has its channel removed."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"key": map[string]any{"type": "string", "description": "The worker key, as the workers tool lists it."},
			},
			Required:    []string{"key"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}
}

type workerStartInput struct {
	Key  string `json:"key"`
	Task string `json:"task"`
	Base string `json:"base"`
}

// workerDeps is what starting a worker reads from the running TUI.
type workerDeps struct {
	// origin is the pool's default workspace, empty when the repo has no pool.
	origin, exe, mode string
	// resolveAt is the TUI's @- commit id.
	resolveAt func(ctx context.Context) (string, error)
	// spawn starts cmd, leaving cmd.Process set.
	spawn func(cmd *exec.Cmd) error
	// checkTrunk and await are skipped when nil.
	checkTrunk func(ctx context.Context) error
	await      func(key string, pid int, logPath string) error
}

const (
	awaitFor   = 15 * time.Second
	awaitEvery = 200 * time.Millisecond
)

// awaitWorker waits for key to register, failing with its log's tail if pid exits first; a worker still preparing its slot counts as started.
func awaitWorker(key string, pid int, logPath string) error {
	for deadline := time.Now().Add(awaitFor); time.Now().Before(deadline); time.Sleep(awaitEvery) {
		for _, info := range registry.Default("", "").Others() {
			if info.Worker == key && info.PID == pid {
				return nil
			}
		}
		if !registry.Alive(pid) {
			return fmt.Errorf("worker %s (pid %d) exited before it registered: %s\nlog: %s", key, pid, logTail(logPath, 6), logPath)
		}
	}
	return nil
}

func logTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type requireFunc func(label, body, reason string) bool

func decodeInput(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func checkKey(raw string) (string, error) {
	key := strings.ToUpper(strings.TrimSpace(raw))
	if strings.ContainsAny(key, `/\`) || strings.HasPrefix(key, ".") || strings.ContainsAny(key, " \t\n") {
		return "", fmt.Errorf("%q is not a worker key", raw)
	}
	return key, nil
}

// workerCommand is the served alux a worker runs as, in its own session so the TUI's hangup does not reach it.
func workerCommand(exe, origin, key, mode, base, task string) *exec.Cmd {
	cmd := exec.Command(exe, "--serve", "--worker", key, "--mode", mode, "--base", base, "--prompt", task)
	cmd.Dir = origin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

func startProcess(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped here, or a finished worker stays a zombie that kill(pid, 0) still finds alive.
	go func() { _ = cmd.Wait() }()
	return nil
}

func startWorker(ctx context.Context, in workerStartInput, d workerDeps, require requireFunc) (string, error) {
	if d.origin == "" {
		return "", errors.New("this repo has no workspace pool, and a worker runs in a pool workspace of its own")
	}
	key, err := checkKey(in.Key)
	if err != nil {
		return "", err
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return "", errors.New("a worker needs a task")
	}
	base, named := orchestrator.BaseMain, orchestrator.BaseMain
	switch strings.TrimSpace(in.Base) {
	case "", orchestrator.BaseMain:
		if d.checkTrunk != nil {
			if err := d.checkTrunk(ctx); err != nil {
				return "", fmt.Errorf("%w; start it with base @- instead", err)
			}
		}
	case "@-":
		if base, err = d.resolveAt(ctx); err != nil {
			return "", fmt.Errorf("resolving this session's @-: %w", err)
		}
		named = "@- (" + base + ")"
	default:
		return "", fmt.Errorf("base is main or @-, not %q", in.Base)
	}
	shown := key
	if shown == "" {
		shown = "WORKER-N"
	}
	label := "start worker " + shown + " on " + named
	body := fmt.Sprintf("key: %s\ntask: %s\nbase: %s\nmode: %s\n%s", shown, task, base, d.mode,
		strings.Join(workerCommand(d.exe, d.origin, shown, d.mode, base, task).Args, " "))
	if !require(label, body, "a worker runs an agent of its own in the background") {
		return "", errors.New("the user declined to " + label + "; nothing started")
	}

	locked, err := workers.StartLock()
	if err != nil {
		return "", err
	}
	unlock := sync.OnceFunc(locked)
	defer unlock()
	if key != "" && workers.LiveKey(key) {
		return "", fmt.Errorf("worker %s is already running", key)
	}
	if live, limit := workers.Live(d.origin), workers.MaxWorkers(); len(live) >= limit {
		return "", fmt.Errorf("this repo has %d live worker(s) (%s) and MaxWorkers is %d; raise max_workers in ~/.config/alux/settings.json to run more",
			len(live), strings.Join(live, ", "), limit)
	}
	root, err := workers.Dir("")
	if err != nil {
		return "", err
	}
	if key == "" {
		if key, err = workers.AllocOneoff(root); err != nil {
			return "", err
		}
	} else {
		if err := workers.Archive(root, key); err != nil {
			return "", fmt.Errorf("archiving %s's old channel: %w", key, err)
		}
		if err := os.MkdirAll(filepath.Join(root, key), 0o700); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(root, key)
	pid, logPath, err := spawnWorker(d, key, base, task)
	if err != nil {
		_ = os.Remove(dir)
		return "", err
	}
	meta := workers.Meta{V: workers.Version, Origin: d.origin, PID: pid, Mode: d.mode, Log: logPath}
	if err := workers.WriteJSON(filepath.Join(dir, "worker.json"), meta); err != nil {
		return "", fmt.Errorf("worker %s started as pid %d, but its worker.json was not written: %w", key, pid, err)
	}
	unlock()
	if d.await != nil {
		if err := d.await(key, pid, logPath); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("worker %s started: pid %d, log %s", key, pid, logPath), nil
}

func spawnWorker(d workerDeps, key, base, task string) (int, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, "", err
	}
	logDir := filepath.Join(home, paths.State, "workers")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, "", err
	}
	starting := filepath.Join(logDir, fmt.Sprintf("%s-start-%d.log", key, time.Now().UnixNano()))
	f, err := os.OpenFile(starting, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	cmd := workerCommand(d.exe, d.origin, key, d.mode, base, task)
	cmd.Stdout, cmd.Stderr = f, f
	if err := d.spawn(cmd); err != nil {
		_ = os.Remove(starting)
		return 0, "", err
	}
	pid := cmd.Process.Pid
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", key, pid))
	if os.Rename(starting, logPath) != nil {
		logPath = starting
	}
	return pid, logPath, nil
}

func stopWorker(raw, origin string, require requireFunc) (string, error) {
	key, err := checkKey(raw)
	if err != nil || key == "" {
		return "", fmt.Errorf("%q is not a worker key", raw)
	}
	pid, crashed, found := 0, false, false
	for _, t := range workers.Read(origin) {
		if t.Key == key {
			pid, found = t.PID, true
			crashed = t.State == workers.Crashed || t.State == workers.Failed || t.State == workers.Done
		}
	}
	if !found {
		if m, ok := workers.Starting(key); ok && m.Origin == origin {
			pid, found = m.PID, true
		}
	}
	if !found {
		return "", fmt.Errorf("no worker %s in this repo", key)
	}
	dir, err := workers.Dir(key)
	if err != nil {
		return "", err
	}
	label, body := "stop worker "+key, fmt.Sprintf("kill -TERM %d", pid)
	if crashed {
		body = "rm -r " + dir
	}
	if !require(label, body, "stopping a worker ends its agent") {
		return "", errors.New("the user declined to " + label + "; nothing changed")
	}
	if crashed {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		return "removed crashed worker " + key + "'s channel " + dir, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return "", fmt.Errorf("signalling worker %s (pid %d): %w", key, pid, err)
	}
	return fmt.Sprintf("sent SIGTERM to worker %s (pid %d); it releases its workspace as it exits", key, pid), nil
}

// gated adapts cb.Require, refusing outright when nobody is there to answer.
func gated(ctx context.Context, cb agent.Callbacks) requireFunc {
	return func(label, body, reason string) bool {
		return cb.Require != nil && cb.Require(ctx, label, body, reason)
	}
}

func toolResult(id string, out string, err error) anthropic.ContentBlockParamUnion {
	if err != nil {
		return anthropic.NewToolResultBlock(id, "Refused: "+err.Error(), true)
	}
	return anthropic.NewToolResultBlock(id, out, false)
}

func workerStartHandler(mode func() string) agent.Handler {
	return func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in workerStartInput
		if err := decodeInput(call.Input, &in); err != nil {
			return toolResult(call.ID, "", err)
		}
		if cb.Require == nil {
			return toolResult(call.ID, "", errors.New("starting a worker needs the user's yes, and there is nobody here to give it"))
		}
		d := workerDeps{mode: mode(), spawn: startProcess, resolveAt: func(ctx context.Context) (string, error) {
			commits, err := orchestrator.JJ{}.Resolve(ctx, project.Root(), "@-")
			if err == nil && len(commits) != 1 {
				err = fmt.Errorf("@- is %d commits, not one", len(commits))
			}
			if err != nil {
				return "", err
			}
			return commits[0], nil
		}}
		if pool, err := orchestrator.PoolFor(ctx, project.Origin()); err != nil {
			return toolResult(call.ID, "", err)
		} else if pool != nil {
			d.origin = pool.Origin
			d.checkTrunk = func(ctx context.Context) error { return pool.CheckTrunk(ctx, pool.Origin) }
			d.await = awaitWorker
		}
		exe, err := os.Executable()
		if err != nil {
			return toolResult(call.ID, "", err)
		}
		d.exe = exe
		out, err := startWorker(ctx, in, d, gated(ctx, cb))
		return toolResult(call.ID, out, err)
	}
}

func workerStopHandler(origin string) agent.Handler {
	return func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Key string `json:"key"`
		}
		if err := decodeInput(call.Input, &in); err != nil {
			return toolResult(call.ID, "", err)
		}
		if cb.Require == nil {
			return toolResult(call.ID, "", errors.New("stopping a worker needs the user's yes, and there is nobody here to give it"))
		}
		out, err := stopWorker(in.Key, origin, gated(ctx, cb))
		return toolResult(call.ID, out, err)
	}
}
