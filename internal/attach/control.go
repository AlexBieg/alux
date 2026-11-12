package attach

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/ui"
	"github.com/alexbieg/alux/internal/workers"
)

// controlTimeout bounds one control request, which a TUI makes from its event loop.
const controlTimeout = time.Second

// controlRequest is the one line a control connection sends; Op is "prompt", "answer", "message", "stop" or "watched".
type controlRequest struct {
	Op      string      `json:"op"`
	ID      string      `json:"id,omitempty"`
	Approve bool        `json:"approve,omitempty"`
	Answer  *ask.Answer `json:"answer,omitempty"`
	Text    string      `json:"text,omitempty"`
}

type controlReply struct {
	Prompt   *workers.Prompt `json:"prompt,omitempty"`
	Resolved bool            `json:"resolved,omitempty"`
	Watched  bool            `json:"watched,omitempty"`
	Err      string          `json:"err,omitempty"`
}

// ControlPath is the socket another pane reaches the session in pid through without attaching.
func ControlPath(pid int) (string, error) {
	sock, err := SocketPath(pid)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(sock, ".sock") + ".ctl.sock", nil
}

// PromptID names the prompt snap is parked on, "" when none is; it is what the registry publishes.
func PromptID(pid int, snap session.Snapshot) string {
	if snap.PromptSeq == 0 {
		return ""
	}
	return fmt.Sprintf("%d-%d", pid, snap.PromptSeq)
}

// Control answers one request per connection from panes that are not attached: what the session is
// parked on, an answer to it, a message for it, or a stop.
type Control struct {
	// Stop ends the session for another pane, or says why it will not; nil refuses every stop.
	Stop func() error
	// Watched reports whether a TUI is attached; nil answers that none is.
	Watched func() bool
	session ui.SessionClient
	ln      *net.UnixListener
	refuse  func(pid int) error
}

// ListenControl opens the control socket at path, replacing a stale one.
func ListenControl(path string, s ui.SessionClient) (*Control, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Control{session: s, ln: ln, refuse: refuseSessionDescendants(filepath.Dir(path))}, nil
}

// Serve answers until Close.
func (ctl *Control) Serve() {
	for {
		nc, err := ctl.ln.AcceptUnix()
		if err != nil {
			return
		}
		go ctl.handle(nc)
	}
}

// Close stops answering and removes the socket.
func (ctl *Control) Close() error {
	path := ctl.ln.Addr().String()
	err := ctl.ln.Close()
	os.Remove(path)
	return err
}

func (ctl *Control) handle(nc *net.UnixConn) {
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(controlTimeout))
	pid, err := peerPID(nc)
	if err == nil {
		err = ctl.refuse(pid)
	}
	if err != nil {
		return
	}
	var r controlRequest
	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &r) != nil {
		return
	}
	json.NewEncoder(nc).Encode(ctl.do(r))
}

func (ctl *Control) do(r controlRequest) controlReply {
	s, self := ctl.session, os.Getpid()
	snap := s.Snapshot()
	id := PromptID(self, snap)
	switch r.Op {
	case "prompt":
		if id == "" {
			return controlReply{}
		}
		p := &workers.Prompt{ID: id, Kind: "approval", Label: snap.Pending, Body: snap.PendingBody}
		if snap.Question != nil {
			p = &workers.Prompt{ID: id, Kind: "question", Label: snap.Question.Question, Question: snap.Question}
		}
		return controlReply{Prompt: p}
	case "answer":
		// An answer names the prompt it was given for; one for any other is dropped.
		if id == "" || r.ID != id {
			return controlReply{}
		}
		if snap.Question != nil {
			return controlReply{Resolved: s.AnswerID(snap.QuestionSeq, r.Answer)}
		}
		return controlReply{Resolved: s.ApproveID(snap.ApprovalSeq, r.Approve)}
	case "message":
		if strings.TrimSpace(r.Text) == "" {
			return controlReply{Err: "an empty message"}
		}
		s.Submit(r.Text)
		return controlReply{}
	case "stop":
		if ctl.Stop == nil {
			return controlReply{Err: "this session cannot be stopped from another pane"}
		}
		if err := ctl.Stop(); err != nil {
			return controlReply{Err: err.Error()}
		}
		return controlReply{}
	case "watched":
		return controlReply{Watched: ctl.Watched != nil && ctl.Watched()}
	default:
		return controlReply{Err: fmt.Sprintf("unknown op %q", r.Op)}
	}
}

func control(pid int, r controlRequest) (controlReply, error) {
	path, err := ControlPath(pid)
	if err != nil {
		return controlReply{}, err
	}
	nc, err := net.DialTimeout("unix", path, controlTimeout)
	if errors.Is(err, os.ErrNotExist) {
		return controlReply{}, fmt.Errorf("session %d is not served; it runs in a UI of its own", pid)
	}
	if err != nil {
		return controlReply{}, err
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(controlTimeout))
	if err := json.NewEncoder(nc).Encode(r); err != nil {
		return controlReply{}, err
	}
	var reply controlReply
	if err := json.NewDecoder(nc).Decode(&reply); err != nil {
		return controlReply{}, fmt.Errorf("session %d did not answer: %w", pid, err)
	}
	if reply.Err != "" {
		return reply, errors.New(reply.Err)
	}
	return reply, nil
}

// ParkedPrompt is what the session in pid is parked on, false when nothing is or it could not be asked.
func ParkedPrompt(pid int) (workers.Prompt, bool) {
	reply, err := control(pid, controlRequest{Op: "prompt"})
	if err != nil || reply.Prompt == nil {
		return workers.Prompt{}, false
	}
	return *reply.Prompt, true
}

// AnswerPrompt answers the prompt id in the session in pid, reporting whether it was still the one parked.
func AnswerPrompt(pid int, id string, approve bool, a *ask.Answer) (bool, error) {
	reply, err := control(pid, controlRequest{Op: "answer", ID: id, Approve: approve, Answer: a})
	return reply.Resolved, err
}

// StopSession ends the session in pid, which refuses while a TUI is attached to it.
func StopSession(pid int) error {
	_, err := control(pid, controlRequest{Op: "stop"})
	return err
}

// Watched reports whether a TUI is attached to the session in pid.
func Watched(pid int) (bool, error) {
	reply, err := control(pid, controlRequest{Op: "watched"})
	return reply.Watched, err
}

// SendMessage submits text to the session in pid as a user message.
func SendMessage(pid int, text string) error {
	_, err := control(pid, controlRequest{Op: "message", Text: text})
	return err
}
