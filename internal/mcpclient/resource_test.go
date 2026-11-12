package mcpclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A protected MCP server: the same shape as a remote server in ~/.config/alux/settings.json.
// It answers an unauthenticated request with a 401 carrying WWW-Authenticate, which is the trigger
// the client SDK needs to start the authorization flow.
type fakeResource struct {
	*httptest.Server

	Unauthorized atomic.Int32 // 401s served
	Calls        atomic.Int32 // successful tool calls

	// Scopes is what the server advertises, mutable so a test can add one the way a real server
	// does between releases — which is what makes a saved client registration stale.
	scopeMu sync.Mutex
	scopes  []string
}

func (r *fakeResource) Scopes() []string {
	r.scopeMu.Lock()
	defer r.scopeMu.Unlock()
	return append([]string(nil), r.scopes...)
}

// AddScope grows the advertised set, standing in for a server release that adds a capability.
func (r *fakeResource) AddScope(scope string) {
	r.scopeMu.Lock()
	defer r.scopeMu.Unlock()
	r.scopes = append(r.scopes, scope)
}

type echoInput struct {
	Text string `json:"text" jsonschema:"the text to echo"`
}

type echoOutput struct {
	Text string `json:"text" jsonschema:"the echoed text"`
}

func newFakeResource(as *fakeAS) *fakeResource {
	res := &fakeResource{scopes: []string{"mcp", "mcp:write"}}

	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo the input back."},
		func(ctx context.Context, req *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
			res.Calls.Add(1)
			return nil, echoOutput{Text: in.Text}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              res.URL + "/mcp",
			"authorization_servers": []string{as.URL},
			"scopes_supported":      res.Scopes(),
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || !as.ValidAccess(token) {
			res.Unauthorized.Add(1)
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, res.URL))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})

	res.Server = httptest.NewServer(mux)
	return res
}
