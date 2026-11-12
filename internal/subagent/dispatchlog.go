package subagent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/paths"
)

// Dispatch is one finished subagent run: what it was asked, what it ran on, and how it ended.
type Dispatch struct {
	ID         string             `json:"id"`
	Session    string             `json:"session"`
	Time       time.Time          `json:"time"`
	PID        int                `json:"pid"`
	Cwd        string             `json:"cwd"`
	Agent      string             `json:"agent"`
	Model      string             `json:"model"`
	Tier       string             `json:"tier"`
	TierScores map[string]float64 `json:"tier_scores,omitempty"`
	Background bool               `json:"background"`
	Task       string             `json:"task"`
	Context    string             `json:"context,omitempty"`
	// Inputs are a skill run's bound inputs, which hold its real brief.
	Inputs map[string]any `json:"inputs,omitempty"`
	// SeedsAsked is the dispatch's seed input; Seeds is what was actually read in, after missing files, bad ranges and the byte cap.
	SeedsAsked []Seed `json:"seeds_asked,omitempty"`
	Seeds      []Seed `json:"seeds,omitempty"`

	Stop      string `json:"stop"`
	Err       string `json:"err,omitempty"`
	Report    string `json:"report,omitempty"`
	Conformed *bool  `json:"conformed,omitempty"`

	Steps      int   `json:"steps"`
	MaxSteps   int   `json:"max_steps"`
	Requests   int   `json:"requests"`
	Input      int64 `json:"input_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Output     int64 `json:"output_tokens"`
	DurationMS int64 `json:"duration_ms"`
}

// Stop values: the agent's stop reasons folded into a closed set.
const (
	StopDone        = "done"
	StopStepLimit   = "step_limit"
	StopContextFull = "context_full"
	StopCancelled   = "cancelled"
	StopError       = "error"
)

// tally accumulates one run's record from callbacks that may fire on other goroutines.
type tally struct {
	mu         sync.Mutex
	rec        Dispatch
	from       time.Time
	transcript *recorder
}

func (t *tally) step(n int) {
	t.mu.Lock()
	t.rec.Steps = n
	t.mu.Unlock()
}

func (t *tally) usage(u agent.Usage) {
	t.mu.Lock()
	t.rec.Requests++
	t.rec.Input += u.Prompt - u.CacheRead - u.CacheWrite
	t.rec.CacheRead += u.CacheRead
	t.rec.CacheWrite += u.CacheWrite
	t.rec.Output += u.Output
	t.mu.Unlock()
}

func (t *tally) set(fn func(*Dispatch)) {
	t.mu.Lock()
	fn(&t.rec)
	t.mu.Unlock()
}

func (t *tally) record() Dispatch {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec := t.rec
	rec.DurationMS = time.Since(t.from).Milliseconds()
	return rec
}

// DefaultDispatchLog is state, not cache: nothing can regenerate it.
func DefaultDispatchLog() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.State, "dispatches.jsonl")
}

// Followup is one thing that happened around a subagent's report reaching the main agent.
type Followup struct {
	Time    time.Time `json:"time"`
	Session string    `json:"session"`
	Event   string    `json:"event"`
	Run     string    `json:"run,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	Label   string    `json:"label,omitempty"`
	Error   bool      `json:"error,omitempty"`
	Text    string    `json:"text,omitempty"`
	Steer   bool      `json:"steer,omitempty"`
}

const (
	EventReport  = "report"
	EventTool    = "tool"
	EventMessage = "message"
	EventReset   = "reset"
)

// PeerExchange is one message a pair's navigator was sent, exactly as sent, and what came back.
type PeerExchange struct {
	Time     time.Time `json:"time"`
	Session  string    `json:"session"`
	Dispatch string    `json:"dispatch"`
	Kind     string    `json:"kind"`
	Sent     string    `json:"sent"`
	Reply    string    `json:"reply"`
	Thinking string    `json:"thinking,omitempty"`
	Verdict  string    `json:"verdict"`
	Stop     string    `json:"stop,omitempty"`
	Err      string    `json:"err,omitempty"`
	Steps    int       `json:"steps,omitempty"`
	Prompt   int64     `json:"prompt_tokens,omitempty"`
	Output   int64     `json:"output_tokens,omitempty"`
}

// PeerExchange kinds: which of the pair's conversations the message belonged to.
const (
	ExchangePlan       = "plan"
	ExchangeCheckpoint = "checkpoint"
	ExchangeEnd        = "end"
	ExchangeQuestion   = "question"
	ExchangeClosing    = "closing"
)

// PeerExchange verdicts. Empty is kept apart from none: a reply with no text is not a pass.
const (
	VerdictNone   = "none"
	VerdictEmpty  = "empty"
	VerdictSaid   = "said"
	VerdictFailed = "failed"
)

func DefaultPeerLog() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.State, "navigator.jsonl")
}

func PeerLog(path string) func(PeerExchange) { return jsonLines[PeerExchange](path) }

// NewID is a random identifier that does not repeat across processes.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func DefaultOutcomeLog() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.State, "outcomes.jsonl")
}

// FileLog appends one JSON line per record, dropping failures so logging never fails a run.
func FileLog(path string) func(Dispatch) { return jsonLines[Dispatch](path) }

func FollowupLog(path string) func(Followup) { return jsonLines[Followup](path) }

func jsonLines[T any](path string) func(T) {
	var mu sync.Mutex
	return func(rec T) {
		line, err := json.Marshal(rec)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(append(line, '\n'))
	}
}
