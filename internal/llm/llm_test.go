package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// modelsStub serves one model's metadata and records the path it was asked for.
func modelsStub(t *testing.T, status int, body string) (anthropic.Client, *string) {
	t.Helper()
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	// Retries are the SDK's business, not this test's, and waiting them out costs a second.
	return anthropic.NewClient(
		option.WithBaseURL(server.URL), option.WithAPIKey("test"), option.WithMaxRetries(0),
	), &asked
}

// The window is the gauge's denominator, and it differs fivefold across the lineup.
func TestContextWindowIsReadFromTheModelsEndpoint(t *testing.T) {
	client, asked := modelsStub(t, http.StatusOK,
		`{"id":"claude-opus-5","type":"model","display_name":"Claude Opus 5","max_input_tokens":1000000,"max_tokens":128000}`)

	got := fetchContextWindow(context.Background(), client, "claude-opus-5")

	if got != 1_000_000 {
		t.Errorf("window = %d, want 1000000", got)
	}
	// Asking about the wrong model would mis-calibrate the gauge silently.
	if *asked != "/v1/models/claude-opus-5" {
		t.Errorf("asked for %q, want /v1/models/claude-opus-5", *asked)
	}
}

// Offline, or an unknown model, must leave the gauge conservative rather than zero the denominator.
func TestAFailedLookupLeavesTheFallbackWindow(t *testing.T) {
	client, _ := modelsStub(t, http.StatusInternalServerError, `{"error":{"message":"nope"}}`)

	if got := fetchContextWindow(context.Background(), client, "claude-opus-5"); got != 0 {
		t.Errorf("window = %d, want 0 so the caller keeps its fallback", got)
	}
	if got := ContextWindow(); got != DefaultContextWindow {
		t.Errorf("ContextWindow() = %d, want the %d fallback", got, DefaultContextWindow)
	}
}

// A stub says nothing about the field being populated in production, which would leave the gauge
// on its fallback forever. Metadata is not billed. Skipped unless ALUX_REAL_API is set.
func TestRealAPIReportsAWindowForTheDefaultModel(t *testing.T) {
	if os.Getenv("ALUX_REAL_API") == "" {
		t.Skip("set ALUX_REAL_API=1 to ask the real API")
	}

	got := fetchContextWindow(context.Background(), Client(), DefaultModel)

	if got < DefaultContextWindow {
		t.Errorf("%s reports a %d-token window, want at least the %d fallback",
			DefaultModel, got, DefaultContextWindow)
	}
	t.Logf("%s reports a %d-token window", DefaultModel, got)
}

// writeAuthFile puts an auth file in a throwaway home and points HOME at it. os.UserHomeDir
// reads HOME on unix, so this reaches the same path production does.
func writeAuthFile(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, AuthFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
}

// The file is the whole point of this path: a key kept on disk has to reach the client without
// ANTHROPIC_API_KEY being exported anywhere.
func TestTheAuthFileSuppliesTheKeyWhenTheEnvironmentIsEmpty(t *testing.T) {
	writeAuthFile(t, `{"anthropic":{"type":"api_key","key":"sk-ant-from-file"}}`)
	t.Setenv("ANTHROPIC_API_KEY", "")

	if got := apiKey(); got != "sk-ant-from-file" {
		t.Errorf("apiKey() = %q, want the key from the file", got)
	}
}

// `ANTHROPIC_API_KEY=... alux` in front of one command is how a one-off or rotated key gets
// used. A file that won would make that impossible, so the precedence is load-bearing.
func TestTheEnvironmentKeyBeatsTheAuthFile(t *testing.T) {
	writeAuthFile(t, `{"anthropic":{"type":"api_key","key":"sk-ant-from-file"}}`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-env")

	if got := apiKey(); got != "sk-ant-from-env" {
		t.Errorf("apiKey() = %q, want the environment to win", got)
	}
}

// Pi writes type "oauth" for a subscription login. That token needs a bearer header and a Claude
// Code masquerade alux does not do, so sending it as an API key would only buy an opaque
// 401 — refusing it keeps the failure legible.
func TestAnOAuthCredentialInTheAuthFileIsRefused(t *testing.T) {
	writeAuthFile(t, `{"anthropic":{"type":"oauth","key":"sk-ant-oat01-nope"}}`)
	t.Setenv("ANTHROPIC_API_KEY", "")

	if got := apiKey(); got != "" {
		t.Errorf("apiKey() = %q, want nothing for a credential type alux cannot use", got)
	}
}

// The override exists for a model the endpoint reports wrongly, so it must beat discovery too.
func TestTheEnvOverrideBeatsTheDiscoveredWindow(t *testing.T) {
	discovered.Store(1_000_000)
	t.Cleanup(func() { discovered.Store(0) })

	if got := ContextWindow(); got != 1_000_000 {
		t.Fatalf("ContextWindow() = %d, want the discovered 1000000", got)
	}

	t.Setenv("ALUX_CONTEXT_WINDOW", "500000")

	if got := ContextWindow(); got != 500_000 {
		t.Errorf("ContextWindow() = %d, want the overridden 500000", got)
	}
}
