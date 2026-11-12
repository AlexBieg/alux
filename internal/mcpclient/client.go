package mcpclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/web"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxResultChars = 50_000
	// Servers still connecting after this — waiting on a browser sign-in, say — join on a later
	// turn. The budget is spent once rather than per turn.
	startupWait = 10 * time.Second
)

type remoteTool struct {
	server  string
	name    string
	session *mcp.ClientSession
	def     anthropic.ToolUnionParam
	// readOnly is the server's own readOnlyHint. Advisory: the MCP spec says these annotations
	// "are not guaranteed to provide a faithful description of tool behavior" and that clients
	// "should never make tool use decisions based on" them. So nothing here authorizes on it —
	// see ToolInfo for what it is actually used for.
	readOnly bool
}

// ToolInfo is what alux knows about one MCP tool beside its schema. It exists so a caller
// deciding what an agent may reach can see the server's own claim about each tool without being
// able to mistake that claim for a guarantee.
type ToolInfo struct {
	// Name is namespaced, as the model sees it.
	Name   string
	Server string
	// ReadOnly is the server's readOnlyHint, defaulting to false when it says nothing. False is
	// the safe default here: a server that declares nothing reads as "may write".
	ReadOnly bool
}

// Inventory is every connected tool, sorted by name. Empty before the servers connect, which is
// why anything deciding capability from it has to be evaluated per run rather than at startup.
func (m *Manager) Inventory() []ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ToolInfo, 0, len(m.tools))
	for name, tool := range m.tools {
		out = append(out, ToolInfo{Name: name, Server: tool.server, ReadOnly: tool.readOnly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AddForTest registers a tool with no session behind it, so a caller deciding what an agent may
// reach can be tested without standing up a server. Exported for tests in other packages, which
// is where the capability rules live; calling it is not supported.
func (m *Manager) AddForTest(server, tool string, readOnly bool) string {
	full := toolName(server, tool)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools[full] = remoteTool{
		server:   server,
		name:     tool,
		readOnly: readOnly,
		def: anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        full,
			Description: anthropic.String("test tool"),
		}},
	}
	return full
}

// Manager owns every connected server for the process.
type Manager struct {
	notify func(string)

	mu       sync.RWMutex
	tools    map[string]remoteTool
	sessions []*mcp.ClientSession

	connectOnce sync.Once
	connected   chan struct{}
}

func New(notify func(string)) *Manager {
	return &Manager{
		notify:    notify,
		tools:     map[string]remoteTool{},
		connected: make(chan struct{}),
	}
}

func (m *Manager) say(format string, args ...any) {
	if m.notify != nil {
		m.notify(fmt.Sprintf(format, args...))
	}
}

// Connect spawns the user's servers and reaches the network, so it waits to be asked rather than
// happening on import. Only the application asks; anything else that pulls this package in — a
// test above all — gets no servers.
func (m *Manager) Connect(ctx context.Context) {
	m.connectOnce.Do(func() {
		go func() {
			defer close(m.connected)
			var wg sync.WaitGroup
			allowlist := LoadToolAllowlist()
			for name, config := range LoadServers() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					m.connectServer(ctx, name, config, allowlist)
				}()
			}
			wg.Wait()
		}()
	})
}

// Ready blocks until every server has connected or the startup budget runs out. Resolved
// immediately when Connect was never called, so a caller that never asked for servers is not
// made to wait for any.
func (m *Manager) Ready(ctx context.Context) {
	select {
	case <-m.connected:
	case <-time.After(startupWait):
	case <-ctx.Done():
	}
}

func (m *Manager) connectServer(ctx context.Context, name string, config ServerConfig, allowlist map[string][]string) {
	session, err := m.dial(ctx, name, config)
	if err != nil {
		m.say("mcp: %s failed: %v", name, err)
		return
	}

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		m.say("mcp: %s failed to list tools: %v", name, err)
		_ = session.Close()
		return
	}

	m.mu.Lock()
	m.sessions = append(m.sessions, session)
	kept := 0
	for _, tool := range listed.Tools {
		if !allowed(allowlist, name, tool.Name) {
			continue
		}
		kept++
		full := toolName(name, tool.Name)
		m.tools[full] = remoteTool{
			server:   name,
			name:     tool.Name,
			session:  session,
			def:      toolDefinition(full, tool),
			readOnly: readOnlyHint(tool),
		}
	}
	m.mu.Unlock()

	if kept < len(listed.Tools) {
		m.say("mcp: %s connected (%d of %d tools)", name, kept, len(listed.Tools))
		return
	}
	m.say("mcp: %s connected (%d tools)", name, kept)
}

// readOnlyHint is the server's claim that a tool does not modify anything, false unless it says
// so explicitly. Both ways of saying nothing — no annotations at all, or annotations that omit the
// field — have to come out false, because the only safe reading of silence is "may write".
func readOnlyHint(tool *mcp.Tool) bool {
	return tool != nil && tool.Annotations != nil && tool.Annotations.ReadOnlyHint
}

// toolDefinition converts an MCP tool into the Anthropic shape. The input schema passes through
// as raw JSON: it is already a JSON Schema and re-modelling it would only lose fields.
func toolDefinition(name string, tool *mcp.Tool) anthropic.ToolUnionParam {
	properties := map[string]any{}
	var required []string
	if tool.InputSchema != nil {
		if raw, err := json.Marshal(tool.InputSchema); err == nil {
			var schema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			}
			if json.Unmarshal(raw, &schema) == nil {
				if schema.Properties != nil {
					properties = schema.Properties
				}
				required = schema.Required
			}
		}
	}
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        name,
		Description: anthropic.String(tool.Description),
		InputSchema: anthropic.ToolInputSchemaParam{Properties: properties, Required: required},
	}}
}

func (m *Manager) dial(ctx context.Context, name string, config ServerConfig) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "alux", Version: "0.1.0"}, nil)
	transport, err := m.transport(name, config)
	if err != nil {
		return nil, err
	}
	return client.Connect(ctx, transport, nil)
}

func (m *Manager) transport(name string, config ServerConfig) (mcp.Transport, error) {
	if !config.remote() {
		args := make([]string, len(config.Args))
		for i, a := range config.Args {
			args[i] = expand(a)
		}
		cmd := exec.Command(expand(config.Command), args...)
		cmd.Dir = config.Cwd
		cmd.Env = os.Environ()
		for k, v := range expandAll(config.Env) {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		// Discarded rather than inherited: a server writing to stderr would otherwise paint
		// over the alternate screen.
		cmd.Stderr = nil
		return &mcp.CommandTransport{Command: cmd}, nil
	}

	handler, err := m.authHandler(name, expand(config.URL))
	if err != nil {
		return nil, err
	}
	httpClient := headerClient(expandAll(config.Headers))

	if config.Type == "sse" {
		return &mcp.SSEClientTransport{Endpoint: expand(config.URL), HTTPClient: httpClient}, nil
	}
	return &mcp.StreamableClientTransport{
		Endpoint:     expand(config.URL),
		HTTPClient:   httpClient,
		OAuthHandler: handler,
	}, nil
}

func headerClient(headers map[string]string) *http.Client {
	return &http.Client{Transport: headerTransport{headers: headers}, CheckRedirect: web.SameHostRedirects}
}

type headerTransport struct {
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// Tools is the definitions to send with a request, sorted so the prompt prefix is stable and
// stays cacheable.
func (m *Manager) Tools() []anthropic.ToolUnionParam {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]anthropic.ToolUnionParam, 0, len(m.tools))
	for _, tool := range m.tools {
		out = append(out, tool.def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OfTool.Name < out[j].OfTool.Name })
	return out
}

// MainTools is Tools with defer_loading set on the named servers' tools. Only an agent that also
// carries a tool-search tool may be given it: a deferred tool is otherwise unreachable.
func (m *Manager) MainTools(deferred []string) []anthropic.ToolUnionParam {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]anthropic.ToolUnionParam, 0, len(m.tools))
	for _, tool := range m.tools {
		def := tool.def
		if slices.Contains(deferred, tool.server) {
			param := *def.OfTool
			param.DeferLoading = anthropic.Bool(true)
			def = anthropic.ToolUnionParam{OfTool: &param}
		}
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OfTool.Name < out[j].OfTool.Name })
	return out
}

// Describe renders a call for the transcript.
func (m *Manager) Describe(name string, input json.RawMessage) string {
	m.mu.RLock()
	tool, ok := m.tools[name]
	m.mu.RUnlock()
	if !ok {
		return name + " " + string(input)
	}
	return tool.server + "." + tool.name + " " + string(input)
}

// Result is a tool call's output in the two forms the caller needs: blocks for the model, text
// for the transcript.
type Result struct {
	Blocks  []anthropic.ContentBlockParamUnion
	Display string
	IsError bool
}

// Call runs a tool. Cancelling the context both fails the call here and tells the server to stop
// working on it, so a slow tool is not left running.
func (m *Manager) Call(ctx context.Context, name string, input json.RawMessage) Result {
	m.mu.RLock()
	tool, ok := m.tools[name]
	m.mu.RUnlock()
	if !ok {
		return Result{
			Blocks:  []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("Unknown MCP tool " + name + ".")},
			Display: "Unknown MCP tool " + name + ".",
			IsError: true,
		}
	}

	var args map[string]any
	_ = json.Unmarshal(input, &args)

	result, err := tool.session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: args})
	if err != nil {
		return Result{
			Blocks:  []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(err.Error())},
			Display: err.Error(),
			IsError: true,
		}
	}

	blocks, display := convert(result)
	return Result{Blocks: blocks, Display: display, IsError: result.IsError}
}

// convert turns MCP content into Anthropic blocks, sharing one character budget across them so a
// single enormous result cannot swallow the context window.
func convert(result *mcp.CallToolResult) ([]anthropic.ContentBlockParamUnion, string) {
	budget := maxResultChars
	var blocks []anthropic.ContentBlockParamUnion
	var shown []string

	for _, content := range result.Content {
		switch c := content.(type) {
		case *mcp.TextContent:
			text := c.Text
			if len(text) > budget {
				text = text[:max(0, budget)] + "\n[truncated]"
			}
			budget -= len(text)
			blocks = append(blocks, anthropic.NewTextBlock(text))
			shown = append(shown, text)
		case *mcp.ImageContent:
			blocks = append(blocks, anthropic.NewImageBlockBase64(c.MIMEType, base64.StdEncoding.EncodeToString(c.Data)))
			shown = append(shown, "[image]")
		default:
			raw, _ := json.Marshal(content)
			text := string(raw)
			if len(text) > budget {
				text = text[:max(0, budget)] + "\n[truncated]"
			}
			budget -= len(text)
			blocks = append(blocks, anthropic.NewTextBlock(text))
			shown = append(shown, text)
		}
	}

	if len(blocks) == 0 && result.StructuredContent != nil {
		raw, _ := json.Marshal(result.StructuredContent)
		blocks = append(blocks, anthropic.NewTextBlock(string(raw)))
		shown = append(shown, string(raw))
	}
	if len(blocks) == 0 {
		blocks = append(blocks, anthropic.NewTextBlock("(no output)"))
		shown = append(shown, "(no output)")
	}
	return blocks, strings.Join(shown, "\n")
}

// Close shuts every session down. Open connections keep the process alive, so this is what lets
// the program exit once the UI is gone.
func (m *Manager) Close() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = nil
	m.mu.Unlock()
	for _, session := range sessions {
		_ = session.Close()
	}
}

func (m *Manager) authDir() string { return filepath.Join(configDir(), "mcp-auth") }
