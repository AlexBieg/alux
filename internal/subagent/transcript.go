package subagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/anthropics/anthropic-sdk-go"
)

// TranscriptEvent is one thing a subagent run did, in the order it did it.
type TranscriptEvent struct {
	Time   time.Time       `json:"time"`
	Kind   string          `json:"kind"`
	ID     string          `json:"id,omitempty"`
	Name   string          `json:"name,omitempty"`
	Label  string          `json:"label,omitempty"`
	Tool   agent.ToolKind  `json:"tool,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Failed bool            `json:"failed,omitempty"`
	Text   string          `json:"text,omitempty"`
}

// TranscriptEvent kinds.
const (
	TranscriptTask   = "task"
	TranscriptModel  = "model"
	TranscriptText   = "text"
	TranscriptTool   = "tool"
	TranscriptReport = "report"
	TranscriptError  = "error"
	TranscriptUser   = "user"
	TranscriptReset  = "reset"
	TranscriptSeed   = "seed"
)

const (
	transcriptOutputCap = 64 << 10
	transcriptInputCap  = 16 << 10
	transcriptsKept     = 300
)

func DefaultTranscriptDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.State, "transcripts")
}

// DefaultSessionDir holds main-session transcripts, apart from subagent runs so neither prunes the other.
func DefaultSessionDir() string { return agent.SessionDir() }

// Transcript records a conversation the roster does not own, such as the main session; nil records nothing.
type Transcript struct {
	r    *recorder
	path string
}

func NewTranscript(dir, id string) *Transcript {
	r := newRecorder(dir, id)
	if r == nil {
		return nil
	}
	return &Transcript{r: r, path: transcriptPath(dir, id)}
}

// Path is the transcript file; "" for a nil transcript.
func (t *Transcript) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

// User records the prompt exactly as the model was sent it.
func (t *Transcript) User(text string) {
	if t != nil {
		t.r.add(TranscriptEvent{Kind: TranscriptUser, Text: text})
	}
}

func (t *Transcript) Watch(cb *agent.Callbacks) {
	if t != nil {
		t.r.watch(cb)
	}
}

func (t *Transcript) Flush() {
	if t != nil {
		t.r.flush()
	}
}

func (t *Transcript) Reset() {
	if t != nil {
		t.r.add(TranscriptEvent{Kind: TranscriptReset})
	}
}

func transcriptPath(dir, dispatch string) string {
	return filepath.Join(dir, filepath.Base(dispatch)+".jsonl")
}

// ReadTranscript is every event recorded for dispatch, skipping lines it cannot parse.
func ReadTranscript(dir, dispatch string) ([]TranscriptEvent, error) {
	f, err := os.Open(transcriptPath(dir, dispatch))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []TranscriptEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4*(transcriptOutputCap+transcriptInputCap))
	for sc.Scan() {
		var e TranscriptEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			events = append(events, e)
		}
	}
	return events, sc.Err()
}

// recorder appends one run's events to its transcript file; a nil recorder records nothing.
type recorder struct {
	write  func(TranscriptEvent)
	mu     sync.Mutex
	text   strings.Builder
	result map[string]TranscriptEvent
}

func newRecorder(dir, dispatch string) *recorder {
	if dir == "" || dispatch == "" {
		return nil
	}
	pruneTranscripts(dir, transcriptsKept-1)
	return &recorder{write: jsonLines[TranscriptEvent](transcriptPath(dir, dispatch)), result: map[string]TranscriptEvent{}}
}

func (r *recorder) add(e TranscriptEvent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushText()
	e.Time = time.Now()
	r.write(e)
}

// finish records how the run ended. The report is the run's last text, so that text is not written twice.
func (r *recorder) finish(report string, err error) {
	if r == nil {
		return
	}
	if err != nil {
		r.add(TranscriptEvent{Kind: TranscriptError, Text: err.Error()})
		return
	}
	r.mu.Lock()
	r.text.Reset()
	r.mu.Unlock()
	r.add(TranscriptEvent{Kind: TranscriptReport, Text: report})
}

// flush writes the model text gathered since the last event.
func (r *recorder) flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushText()
}

// flushText writes the model text gathered since the last event; r.mu is held.
func (r *recorder) flushText() {
	if text := strings.TrimSpace(r.text.String()); text != "" {
		r.write(TranscriptEvent{Time: time.Now(), Kind: TranscriptText, Text: text})
	}
	r.text.Reset()
}

// watch wraps cb so the run's text and every tool call reach the transcript.
func (r *recorder) watch(cb *agent.Callbacks) {
	if r == nil {
		return
	}
	onText, onResult, onCall := cb.OnText, cb.OnToolResult, cb.OnCall
	cb.OnText = func(delta string) {
		r.mu.Lock()
		r.text.WriteString(delta)
		r.mu.Unlock()
		if onText != nil {
			onText(delta)
		}
	}
	cb.OnToolResult = func(id, label, output string, kind agent.ToolKind) {
		r.mu.Lock()
		r.result[id] = TranscriptEvent{Label: label, Output: output, Tool: kind}
		r.mu.Unlock()
		if onResult != nil {
			onResult(id, label, output, kind)
		}
	}
	cb.OnCall = func(call agent.Call, res anthropic.ContentBlockParamUnion) {
		r.mu.Lock()
		e, shown := r.result[call.ID]
		delete(r.result, call.ID)
		r.mu.Unlock()
		e.Kind, e.ID, e.Name, e.Input = TranscriptTool, call.ID, call.Name, capInput(call.Input)
		if e.Label == "" {
			e.Label = call.Name
		}
		if tr := res.OfToolResult; tr != nil {
			e.Failed = tr.IsError.Or(false)
			if !shown {
				e.Output = toolResultText(tr)
			}
		}
		e.Output = capText(e.Output, transcriptOutputCap)
		r.add(e)
		if onCall != nil {
			onCall(call, res)
		}
	}
}

func toolResultText(tr *anthropic.ToolResultBlockParam) string {
	var parts []string
	for _, c := range tr.Content {
		if c.OfText != nil {
			parts = append(parts, c.OfText.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[…cut]"
}

func capInput(raw json.RawMessage) json.RawMessage {
	if len(raw) <= transcriptInputCap {
		return raw
	}
	cut, _ := json.Marshal(capText(string(raw), transcriptInputCap))
	return cut
}

// pruneTranscripts removes all but the keep most recently written transcripts in dir.
func pruneTranscripts(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= keep {
		return
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		files = append(files, file{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	slices.SortFunc(files, func(a, b file) int { return b.mod.Compare(a.mod) })
	for _, f := range files[min(keep, len(files)):] {
		os.Remove(f.path)
	}
}
