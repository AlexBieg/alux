// Package mcpclient connects the configured MCP servers and exposes their tools to the agent.
package mcpclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/project"
)

// ServerConfig is one entry from the settings files. A url means a remote server; otherwise it
// is a command to spawn.
type ServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (c ServerConfig) remote() bool { return c.URL != "" }

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, paths.Config)
}

func readJSON(path string, into any) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, into)
}

type settingsFile struct {
	MCPServers            map[string]ServerConfig `json:"mcpServers"`
	EnabledMcpjsonServers []string                `json:"enabledMcpjsonServers"`
	Projects              map[string]struct {
		MCPServers map[string]ServerConfig `json:"mcpServers"`
	} `json:"projects"`
}

// LoadServers layers configuration the same way Claude Code does: user servers first, then the
// project's own .mcp.json but only the names explicitly enabled, then per-project local servers.
// The .mcp.json gate matters — that file is checked into repositories, and running whatever a
// repository asks for is not a default worth having.
func LoadServers() map[string]ServerConfig {
	return loadServers(configDir(), project.Root(), project.Home())
}

// loadServers takes its directories so the layering can be tested without a real home or repo; home keys the per-project entry, so every workspace of a repo shares it.
func loadServers(config, root, home string) map[string]ServerConfig {
	var settings settingsFile
	readJSON(filepath.Join(config, "settings.json"), &settings)

	var projectFile struct {
		MCPServers map[string]ServerConfig `json:"mcpServers"`
	}
	readJSON(filepath.Join(root, ".mcp.json"), &projectFile)

	enabled := map[string]bool{}
	for _, name := range settings.EnabledMcpjsonServers {
		enabled[name] = true
	}

	out := map[string]ServerConfig{}
	for name, server := range settings.MCPServers {
		out[name] = server
	}
	for name, server := range projectFile.MCPServers {
		if enabled[name] {
			out[name] = server
		}
	}
	for name, server := range settings.Projects[home].MCPServers {
		out[name] = server
	}
	return out
}

// LoadToolAllowlist reads {"mcp_tools": {"<server>": ["<tool>", ...]}}; a server not named keeps every tool.
func LoadToolAllowlist() map[string][]string {
	var settings struct {
		MCPTools map[string][]string `json:"mcp_tools"`
	}
	readJSON(filepath.Join(configDir(), "settings.json"), &settings)
	return settings.MCPTools
}

// LoadDeferred reads {"mcp_defer": ["<server>", ...]}, the servers whose tools the main agent loads only through tool search.
func LoadDeferred() []string {
	var settings struct {
		MCPDefer []string `json:"mcp_defer"`
	}
	readJSON(filepath.Join(configDir(), "settings.json"), &settings)
	return settings.MCPDefer
}

func allowed(allowlist map[string][]string, server, tool string) bool {
	names, listed := allowlist[server]
	return !listed || slices.Contains(names, tool)
}

var expansion = regexp.MustCompile(`\$\{(\w+)\}`)

// expand substitutes ${VAR} from the environment, so a config can name a secret without holding
// one.
func expand(value string) string {
	return expansion.ReplaceAllStringFunc(value, func(match string) string {
		return os.Getenv(strings.Trim(match, "${}"))
	})
}

func expandAll(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = expand(v)
	}
	return out
}

var unsafeToolChars = regexp.MustCompile(`[^\w-]`)

// toolName namespaces a server's tool. Names must match ^[a-zA-Z0-9_-]{1,64}$.
//
// Sanitising can in principle collide — a server offering both "a.b" and "a b" would map both
// to the same name and lose one. Not defended against: it needs a server to offer two tools
// differing only in punctuation.
func toolName(server, tool string) string {
	name := unsafeToolChars.ReplaceAllString("mcp__"+server+"__"+tool, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}
