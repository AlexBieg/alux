package mine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/anthropics/anthropic-sdk-go"
)

// manualRun is how a skill started from /skills run opens its prompt.
var manualRun = regexp.MustCompile(`^Run the skill \*\*([\w-]+)\*\*`)

// Recorder appends one line per user turn to the sequence log; a nil Recorder records nothing.
type Recorder struct {
	path    string
	session string

	mu           sync.Mutex
	conversation int
	turn         int
	skill        string
	cur          *turnBuf
}

type turnBuf struct {
	skill         string
	calls         []string
	model, prompt string
}

func NewRecorder(dir, session string) *Recorder {
	if dir == "" || session == "" {
		return nil
	}
	return &Recorder{path: SequencesFile(dir), session: session}
}

// Watch starts a turn and wraps cb so its tool calls are encoded into it. The returned func writes
// the turn's line; it does nothing once a later turn or a Reset has taken over, so late calls from
// an abandoned turn are dropped and cannot change the skill.
func (r *Recorder) Watch(prompt, model, promptVersion string, cb *agent.Callbacks) (end func()) {
	if r == nil {
		return func() {}
	}
	buf := &turnBuf{model: model, prompt: promptVersion}
	r.mu.Lock()
	if m := manualRun.FindStringSubmatch(prompt); m != nil {
		r.skill = m[1]
	}
	buf.skill = r.skill
	r.cur = buf
	r.mu.Unlock()

	onCall := cb.OnCall
	cb.OnCall = func(call agent.Call, res anthropic.ContentBlockParamUnion) {
		r.call(buf, call)
		if onCall != nil {
			onCall(call, res)
		}
	}
	return func() { r.end(buf) }
}

func (r *Recorder) call(buf *turnBuf, call agent.Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != buf {
		return
	}
	switch call.Name {
	case "start_skill":
		var in struct {
			Skill string `json:"skill"`
		}
		if json.Unmarshal(call.Input, &in) == nil && in.Skill != "" {
			r.skill = in.Skill
			buf.skill = in.Skill
		}
	case "finish_skill", "fail_run":
		r.skill = ""
	}
	buf.calls = append(buf.calls, Encode(call.Name, call.Input)...)
}

// end writes buf's line, unless buf is no longer the current turn or made no call the miner keeps.
func (r *Recorder) end(buf *turnBuf) {
	r.mu.Lock()
	if r.cur != buf || len(buf.calls) == 0 {
		r.mu.Unlock()
		return
	}
	r.cur = nil
	t := Turn{Session: r.session, Conversation: r.conversation, Turn: r.turn, Time: time.Now(), Skill: buf.skill, Calls: buf.calls,
		Model: buf.model, Prompt: buf.prompt}
	r.turn++
	r.mu.Unlock()
	appendLine(r.path, t)
}

// Reset starts a new conversation, as /new does.
func (r *Recorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.conversation++
	r.turn = 0
	r.skill = ""
	r.cur = nil
	r.mu.Unlock()
}

var appendMu sync.Mutex

func appendLine(path string, t Turn) {
	line, err := json.Marshal(t)
	if err != nil {
		return
	}
	appendMu.Lock()
	defer appendMu.Unlock()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}
