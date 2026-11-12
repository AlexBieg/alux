// Package llm owns the Anthropic client, so the coding loop and the vault filing subagent share
// one configuration and tests can point both at a local mock.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const DefaultModel = "claude-opus-5-5"

// DefaultContextWindow is the smallest window in the lineup, so an undiscovered model
// under-promises rather than overshooting.
const DefaultContextWindow = 200_000

var (
	once   sync.Once
	client anthropic.Client
	// What the API reported for this model, zero until it answers.
	discovered atomic.Int64
)

// Client is created lazily so that importing this package never reaches for an API key.
func Client() anthropic.Client {
	once.Do(func() {
		var opts []option.RequestOption
		if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
			opts = append(opts, option.WithBaseURL(base))
		}
		// Left unset when nothing resolved, so a missing credential fails inside the SDK the
		// way it always has rather than as an empty key.
		if key := apiKey(); key != "" {
			opts = append(opts, option.WithAPIKey(key))
		}
		client = anthropic.NewClient(opts...)
	})
	return client
}

// AuthFile is where a key lives when it is not in the environment.
const AuthFile = paths.Config + "/auth.json"

// authFile mirrors the shape Pi uses in ~/.pi/agent/auth.json: one entry per provider, each
// naming its credential type beside the secret, so a second provider can be added later without
// reshaping the file.
type authFile struct {
	Anthropic struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	} `json:"anthropic"`
}

// KeyError is nil when a key resolves, else what to do about it.
func KeyError() error {
	if apiKey() != "" {
		return nil
	}
	return fmt.Errorf("no Anthropic API key found. Either export ANTHROPIC_API_KEY, or write ~/%s containing {\"anthropic\": {\"type\": \"api_key\", \"key\": \"sk-ant-...\"}}", AuthFile)
}

// APIKey is the key alux itself would use, for tools that call the API without the SDK client.
func APIKey() string { return apiKey() }

// apiKey resolves the credential: the environment first, then the auth file. The environment has
// to win — `ANTHROPIC_API_KEY=... alux` in front of one command is how a one-off key gets
// used, and a file that quietly overrode it would make that impossible.
func apiKey() string {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key
	}
	return fileAPIKey()
}

// fileAPIKey is the key from the auth file, or "" when the file is absent, unreadable, or holds a
// credential alux cannot use. Every failure is silent: an absent file is the normal case
// for someone who keeps the key in their environment.
func fileAPIKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, AuthFile))
	if err != nil {
		return ""
	}
	var auth authFile
	if json.Unmarshal(data, &auth) != nil {
		return ""
	}
	// Pi writes "api_key" here, and "oauth" for a Claude subscription login whose token needs a
	// bearer header and a Claude Code masquerade alux deliberately does not perform.
	// Refusing an unknown type beats sending an oauth token as an API key and reading a 401.
	if auth.Anthropic.Type != "api_key" {
		return ""
	}
	return strings.TrimSpace(auth.Anthropic.Key)
}

// AuthFileWarning is "" unless the auth file is readable by group or other.
func AuthFileWarning() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	info, err := os.Stat(filepath.Join(home, AuthFile))
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("~/%s is readable by other users; run: chmod 600 ~/%s", AuthFile, AuthFile)
}

func Model() string {
	if m := os.Getenv("ALUX_MODEL"); m != "" {
		return m
	}
	return DefaultModel
}

// ContextWindow is how many tokens the model holds: the env override first, then whatever the
// API reported for this model, then the fallback.
func ContextWindow() int64 {
	if v := os.Getenv("ALUX_CONTEXT_WINDOW"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	if n := discovered.Load(); n > 0 {
		return n
	}
	return DefaultContextWindow
}

// DiscoverContextWindow caches what the configured model really holds; windows span a factor of
// five across the lineup, so a constant mis-calibrates the gauge badly. Call it in a goroutine and
// never await it: ContextWindow is read per request, so a late value lands on the next turn.
func DiscoverContextWindow(ctx context.Context) {
	if n := fetchContextWindow(ctx, Client(), Model()); n > 0 {
		discovered.Store(n)
	}
}

// Split out so a test can drive it without the package's cached client.
func fetchContextWindow(ctx context.Context, client anthropic.Client, model string) int64 {
	info, err := client.Models.Get(ctx, model, anthropic.ModelGetParams{})
	if err != nil || info == nil {
		return 0
	}
	return info.MaxInputTokens
}

// VaultModel lets filing run on a cheaper model than the coding loop without touching
// ALUX_MODEL.
func VaultModel() string {
	if m := os.Getenv("ALUX_VAULT_MODEL"); m != "" {
		return m
	}
	return Model()
}

// DefaultAliasModel is the cheapest fast model in the lineup, which is what writing vault search
// aliases asks for: a few thousand independent calls, each reading one 600-byte section and
// answering with four short phrases, with nothing to reason about.
const DefaultAliasModel = "claude-haiku-4-5"

// AliasModel does not fall back to Model() the way VaultModel does. Filing is a handful of calls
// a day and the coding model is a defensible default for it; aliasing is one call per section of
// the whole vault, so inheriting an opus-class default would turn a tuning slip into a bill.
func AliasModel() string {
	if m := os.Getenv("ALUX_ALIAS_MODEL"); m != "" {
		return m
	}
	return DefaultAliasModel
}
