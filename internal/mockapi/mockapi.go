// Package mockapi serves a local stand-in for the Anthropic Messages API, so turns can be driven
// end to end without an API key. Point the client at it with ANTHROPIC_BASE_URL.
//
// It validates no request shapes. That is a real limitation worth remembering: a new tool type or
// a new request parameter has to be confirmed against the real API once, because this will
// happily accept a request the server would reject.
package mockapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Turn is one scripted reply. Blocks are emitted in the order thinking, text, tools.
type Turn struct {
	Thinking string
	Text     string
	Tools    []ToolUse
	// PauseMidThinking holds the stream open partway through the thinking block. Pausing after
	// the block closes is too late to observe anything streaming, because the UI has already
	// collapsed it.
	PauseMidThinking time.Duration
	// Status, when non-zero, fails the request instead of streaming.
	Status int
	// Usage is what this reply claims to have consumed; zero fields take the defaults below.
	Usage Usage
	// Stop overrides the stop reason, which otherwise follows from whether Tools is empty.
	Stop string
}

func (t Turn) stop() string {
	switch {
	case t.Stop != "":
		return t.Stop
	case len(t.Tools) > 0:
		return "tool_use"
	}
	return "end_turn"
}

// Usage scripts a reply's token counts. CacheRead is separate because the API reports cached
// prompt tokens in their own field.
type Usage struct {
	InputTokens  int64
	CacheRead    int64
	OutputTokens int64
}

const (
	defaultInputTokens  = 1
	defaultOutputTokens = 5
)

// usage fills in the defaults for whatever the turn left at zero.
func (u Usage) usage() map[string]any {
	input, output := u.InputTokens, u.OutputTokens
	if input == 0 && u.CacheRead == 0 {
		input = defaultInputTokens
	}
	if output == 0 {
		output = defaultOutputTokens
	}
	return map[string]any{
		"input_tokens":            input,
		"cache_read_input_tokens": u.CacheRead,
		"output_tokens":           output,
	}
}

type ToolUse struct {
	ID    string
	Name  string
	Input any
}

// Server is a scripted Messages API. Turns are consumed in order; the last one repeats.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	turns    []Turn
	at       int
	Requests []json.RawMessage
	headers  []http.Header
}

func New(turns ...Turn) *Server {
	s := &Server{turns: turns}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handle)
	mux.HandleFunc("/v1/messages/count_tokens", s.countTokens)
	s.Server = httptest.NewServer(mux)
	return s
}

// countTokens bills one token per four bytes of request body. The absolute number is meaningless;
// what a caller can rely on is that adding content to a request raises it, which is what makes a
// breakdown built from differences testable without the network.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.Requests = append(s.Requests, body)
	s.headers = append(s.headers, r.Header)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": len(body) / 4})
}

// Next returns the turn for this request and records the body and headers, so a test can assert
// what history was sent back — where the extended-thinking replay rules bite — and which betas
// the request opted into.
func (s *Server) next(body json.RawMessage, header http.Header) Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, body)
	s.headers = append(s.headers, header)
	if len(s.turns) == 0 {
		return Turn{Text: "ok"}
	}
	t := s.turns[min(s.at, len(s.turns)-1)]
	s.at++
	return t
}

// RequestCount is how many times the model was called.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Requests)
}

// LastRequest returns the most recent request body decoded into a generic map.
func (s *Server) LastRequest() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Requests) == 0 {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(s.Requests[len(s.Requests)-1], &out)
	return out
}

// LastHeaderValues returns every value the most recent request sent for a header, which for
// anthropic-beta is one entry per opted-in feature.
func (s *Server) LastHeaderValues(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.headers) == 0 {
		return nil
	}
	return s.headers[len(s.headers)-1].Values(name)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	var body json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	turn := s.next(body, r.Header.Clone())

	if turn.Status != 0 {
		w.WriteHeader(turn.Status)
		_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":"mock failure"}}`)
		return
	}

	// The coding loop streams; the vault subagent does not. Which one a caller wants is in the
	// request, so both are served rather than making the caller adapt to the mock.
	var envelope struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &envelope)
	if !envelope.Stream {
		writeJSON(w, message(turn))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	send := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_mock", "type": "message", "role": "assistant", "model": "mock",
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			// Prompt tokens arrive here; message_delta below corrects the output count, as the
			// real API does.
			"usage": turn.Usage.usage(),
		},
	})

	index := 0
	block := func(start any, deltas []any) {
		send("content_block_start", merge(start, map[string]any{"type": "content_block_start", "index": index}))
		for _, d := range deltas {
			send("content_block_delta", merge(d, map[string]any{"type": "content_block_delta", "index": index}))
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}

	if turn.Thinking != "" {
		send("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})
		half := len(turn.Thinking) / 2
		for _, part := range []string{turn.Thinking[:half], turn.Thinking[half:]} {
			send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": part},
			})
			if turn.PauseMidThinking > 0 {
				time.Sleep(turn.PauseMidThinking)
				turn.PauseMidThinking = 0
			}
		}
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "signature_delta", "signature": "mock-signature"},
		})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}

	if turn.Text != "" {
		block(
			map[string]any{"content_block": map[string]any{"type": "text", "text": ""}},
			[]any{map[string]any{"delta": map[string]any{"type": "text_delta", "text": turn.Text}}},
		)
	}

	for _, tool := range turn.Tools {
		input, _ := json.Marshal(tool.Input)
		block(
			map[string]any{"content_block": map[string]any{
				"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{},
			}},
			[]any{map[string]any{"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}}},
		)
	}

	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": turn.stop(), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": turn.Usage.usage()["output_tokens"]},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func merge(a any, b map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := a.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// message builds the non-streaming form of a scripted turn.
func message(turn Turn) map[string]any {
	content := []any{}
	if turn.Thinking != "" {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": turn.Thinking, "signature": "mock-signature",
		})
	}
	if turn.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": turn.Text})
	}
	for _, tool := range turn.Tools {
		input := tool.Input
		if input == nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": input,
		})
	}
	return map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": "mock",
		"content": content, "stop_reason": turn.stop(), "stop_sequence": nil,
		"usage": turn.Usage.usage(),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
