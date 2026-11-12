// Package classify asks a local model whether a shell command writes to disk.
//
// The alux prompt forbids editing files through bash, and nothing enforces that. This
// reports on it after the fact: a command the model reads as a write is tinted in the
// transcript, so a rule the prompt only states is at least visible when it is broken.
//
// The model is a fine-tuned Laya checkpoint served by the Python sidecar in sidecar/, since
// the weights are PyTorch and alux is Go. A verdict is advisory and often absent —
// the sidecar is optional, starts slowly and is wrong about one novel command in seven — so
// every caller here treats "no answer" as the normal case rather than an error.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

const (
	// Long enough for a cold import of torch plus an 808 MB checkpoint, which is the whole of
	// the wait: inference afterwards is tens of milliseconds.
	startupWait = 90 * time.Second
	// A classification that has not answered by now has lost its race with the command itself,
	// and the box it would have tinted is already drawn.
	classifyTimeout = 3 * time.Second
	killGrace       = 2 * time.Second
	defaultPort     = 8899
)

// Verdict is what the model made of one command.
type Verdict int

const (
	Unknown Verdict = iota // not classified: sidecar off, still loading, too slow, or errored
	Reads
	Writes
)

type Conf struct {
	Python string
	Model  string
	Script string
	Port   int
	Device string
}

// Config reads {"classify": {"model": "...", "python": "...", "script": "...", "port": 8899,
// "device": "mps"}} and reports false when the feature is not configured. A pointer field so a
// file with no classify block is told apart from one that leaves it empty.
func Config() (Conf, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Conf{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return Conf{}, false
	}
	var settings struct {
		Classify *struct {
			Python string `json:"python"`
			Model  string `json:"model"`
			Script string `json:"script"`
			Port   int    `json:"port"`
			Device string `json:"device"`
		} `json:"classify"`
	}
	if json.Unmarshal(data, &settings) != nil || settings.Classify == nil {
		return Conf{}, false
	}
	c := Conf{
		Python: expand(settings.Classify.Python),
		Model:  expand(settings.Classify.Model),
		Script: expand(settings.Classify.Script),
		Port:   settings.Classify.Port,
		Device: expand(settings.Classify.Device),
	}
	// Without an interpreter and weights there is nothing to run, and a half-configured block is
	// likelier a typo than an intention.
	if c.Python == "" || c.Model == "" {
		return Conf{}, false
	}
	if c.Port == 0 {
		c.Port = defaultPort
	}
	if c.Device == "" {
		c.Device = "mps"
	}
	return c, true
}

func expand(raw string) string {
	return strings.TrimSpace(os.Expand(raw, os.Getenv))
}

// Client is a handle on the sidecar. The zero value is a working no-op client, which is what
// alux uses when the feature is unconfigured.
type Client struct {
	conf    Conf
	http    *http.Client
	cmd     *exec.Cmd
	ready   chan struct{}
	mu      sync.Mutex
	failed  bool
	onError func(string)
}

// Attach is a client for a sidecar someone else is running, which is what the tests use and
// what a hand-started server on the configured port gets. It owns no process, so Close is a
// no-op and nothing waits for a model to load.
func Attach(port int, onError func(string)) *Client {
	c := &Client{
		conf:    Conf{Port: port},
		http:    &http.Client{Timeout: classifyTimeout},
		ready:   make(chan struct{}),
		onError: onError,
	}
	close(c.ready)
	return c
}

// Start boots the sidecar and returns immediately; the model loads in the background. A nil
// Client is valid and classifies nothing, so callers never branch on whether the feature is on.
func Start(ctx context.Context, conf Conf, onError func(string)) *Client {
	c := &Client{
		conf:    conf,
		http:    &http.Client{Timeout: classifyTimeout},
		ready:   make(chan struct{}),
		onError: onError,
	}
	script := conf.Script
	if script == "" {
		script = "sidecar/classify_server.py"
	}
	cmd := exec.Command(conf.Python, script,
		"--model", conf.Model,
		"--port", fmt.Sprint(conf.Port),
		"--device", conf.Device)
	// Its own process group, so stopping alux takes the sidecar's children with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Discarded for the same reason the MCP transports discard theirs: torch warnings on stderr
	// would paint over the alternate screen.
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		c.fail("command classifier: " + err.Error())
		close(c.ready)
		return c
	}
	c.cmd = cmd
	go c.await(ctx)
	return c
}

// await polls /health rather than reading the sidecar's READY line, so a server already
// listening from a previous run is adopted instead of colliding with it.
func (c *Client) await(ctx context.Context) {
	defer close(c.ready)
	deadline := time.Now().Add(startupWait)
	probe := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/health"), nil)
		if err != nil {
			return
		}
		resp, err := probe.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
	}
	c.fail("command classifier: sidecar did not come up within " + startupWait.String())
}

func (c *Client) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", c.conf.Port, path)
}

// Classify blocks until the sidecar answers, and reports Unknown rather than an error for every
// failure: a missing verdict is the normal case, not a fault worth surfacing per command.
func (c *Client) Classify(ctx context.Context, command string) Verdict {
	if c == nil || c.http == nil {
		return Unknown
	}
	select {
	case <-c.ready:
	case <-ctx.Done():
		return Unknown
	}
	c.mu.Lock()
	failed := c.failed
	c.mu.Unlock()
	if failed {
		return Unknown
	}
	body, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return Unknown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/classify"), bytes.NewReader(body))
	if err != nil {
		return Unknown
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.fail("command classifier: " + err.Error())
		return Unknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.fail(fmt.Sprintf("command classifier: sidecar returned %s", resp.Status))
		return Unknown
	}
	var out struct {
		Writes *float64 `json:"writes"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Writes == nil {
		return Unknown
	}
	if *out.Writes >= 0.5 {
		return Writes
	}
	return Reads
}

// fail reports the first failure and silences every one after it. The sidecar being down is one
// condition, not one per command, and the transcript is not the place to say so repeatedly.
func (c *Client) fail(msg string) {
	c.mu.Lock()
	first := !c.failed
	c.failed = true
	c.mu.Unlock()
	if first && c.onError != nil {
		c.onError(msg)
	}
}

func (c *Client) Close() {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return
	}
	pgid := -c.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(killGrace):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}
}

// Command recovers the shell command from a transcript label, and reports false for a bash box
// that reports on a process rather than running one ("output of proc 3", "kill proc 3").
func Command(label string) (string, bool) {
	if !strings.HasPrefix(label, "$ ") {
		return "", false
	}
	cmd := strings.TrimSpace(label[2:])
	// A background command's head carries its wake after the &.
	if i := strings.Index(cmd, " & · "); i >= 0 {
		cmd = cmd[:i]
	}
	cmd = strings.TrimSuffix(cmd, " &")
	if cmd == "" {
		return "", false
	}
	return cmd, true
}
