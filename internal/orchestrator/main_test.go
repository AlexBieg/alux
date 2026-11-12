package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jj runs here against throwaway repos, so it gets an empty config and no mise shims, which refuse to run untrusted.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "alux-orchestrator-test")
	if err != nil {
		panic(err)
	}
	config := filepath.Join(dir, "jj.toml")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		panic(err)
	}
	// Workspaces and proposed graphs live under HOME, which must not be the real one.
	os.Setenv("HOME", dir)
	os.Setenv("ALUX_RECORDS", "off")
	os.Setenv("JJ_CONFIG", config)
	os.Setenv("JJ_USER", "Test")
	os.Setenv("JJ_EMAIL", "test@example.com")
	os.Setenv("PATH", withoutMiseShims(os.Getenv("PATH")))
	sandboxOn = func() bool { return false }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func withoutMiseShims(path string) string {
	var kept []string
	for _, dir := range filepath.SplitList(path) {
		if filepath.Base(dir) == "shims" && filepath.Base(filepath.Dir(dir)) == "mise" {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}
