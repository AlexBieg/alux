package mcpclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A .mcp.json is checked into a repository, so anything it lists is a command some other author
// chose. Running it because the file exists would make cloning a repository enough to execute
// its code; it has to be opted into by name in the user's own settings.
func TestProjectServersAreIgnoredUntilEnabled(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{
		"helpful": {"command":"echo"},
		"sketchy": {"command":"curl"}
	}}`)

	servers := loadServers(config, root, root)
	if len(servers) != 0 {
		t.Fatalf("a repository's servers loaded with nothing enabled: %v", keys(servers))
	}

	write(t, filepath.Join(config, "settings.json"), `{"enabledMcpjsonServers":["helpful"]}`)
	servers = loadServers(config, root, root)

	if _, ok := servers["helpful"]; !ok {
		t.Error("an enabled server was not loaded")
	}
	if _, ok := servers["sketchy"]; ok {
		t.Error("a server that was never enabled was loaded")
	}
}

// User servers are the base; a per-project entry refines them for one repository only.
func TestPerProjectServersOverrideUserServers(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	write(t, filepath.Join(config, "settings.json"), `{
		"mcpServers": {"notes": {"type":"http","url":"https://shared.example/mcp"}},
		"projects": {"`+root+`": {"mcpServers": {"notes": {"type":"http","url":"https://local.example/mcp"}}}}
	}`)

	servers := loadServers(config, root, root)
	if got := servers["notes"].URL; got != "https://local.example/mcp" {
		t.Errorf("url = %q, want the per-project one", got)
	}

	// Another repository must not pick up that override.
	other := t.TempDir()
	if got := loadServers(config, other, other)["notes"].URL; got != "https://shared.example/mcp" {
		t.Errorf("a different repo saw %q, want the shared one", got)
	}
}

// A pool slot is another workspace of the same repo, so it keeps the repo's per-project servers.
func TestPerProjectServersFollowTheRepoIntoOtherWorkspaces(t *testing.T) {
	config, origin, slot := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(config, "settings.json"), `{
		"projects": {"`+origin+`": {"mcpServers": {"replica": {"command":"echo"}}}}
	}`)

	if _, ok := loadServers(config, slot, origin)["replica"]; !ok {
		t.Error("a workspace of the repo lost its per-project server")
	}
}

// A config names a secret; it must not hold one.
func TestEnvironmentPlaceholdersAreExpanded(t *testing.T) {
	t.Setenv("ALUX_TEST_TOKEN", "s3cret")

	if got := expand("Bearer ${ALUX_TEST_TOKEN}"); got != "Bearer s3cret" {
		t.Errorf("expand = %q", got)
	}
	// An unset variable expands to nothing rather than leaving the placeholder to be sent
	// verbatim as if it were the value.
	if got := expand("${ALUX_TEST_UNSET}"); got != "" {
		t.Errorf("unset variable expanded to %q", got)
	}
	if got := expandAll(map[string]string{"Authorization": "Bearer ${ALUX_TEST_TOKEN}"}); got["Authorization"] != "Bearer s3cret" {
		t.Errorf("expandAll = %v", got)
	}
}

// A listed server keeps only its named tools; an unlisted one must keep all of them, or adding one
// server to the allowlist would silently empty every other.
func TestToolAllowlistNarrowsOnlyTheServersItNames(t *testing.T) {
	allowlist := map[string][]string{"linear": {"get_issue"}}
	for _, tc := range []struct {
		server, tool string
		want         bool
	}{
		{"linear", "get_issue", true},
		{"linear", "merge_diff", false},
		{"notes", "notes_read_artifact", true},
	} {
		if got := allowed(allowlist, tc.server, tc.tool); got != tc.want {
			t.Errorf("%s.%s allowed = %v, want %v", tc.server, tc.tool, got, tc.want)
		}
	}
}

// The API rejects a tool name outside ^[a-zA-Z0-9_-]{1,64}$, and a server is free to use
// characters that are not.
func TestToolNamesAreLegalAndBounded(t *testing.T) {
	legal := func(s string) bool {
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			default:
				return false
			}
		}
		return len(s) >= 1 && len(s) <= 64
	}

	for _, tc := range []struct{ server, tool string }{
		{"notes", "list_artifacts"},
		{"my server", "do:the/thing"},
		{strings.Repeat("long", 20), strings.Repeat("name", 20)},
		{"a.b", "c d"},
	} {
		got := toolName(tc.server, tc.tool)
		if !legal(got) {
			t.Errorf("toolName(%q, %q) = %q, which the API would reject", tc.server, tc.tool, got)
		}
	}
}

func keys(m map[string]ServerConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
