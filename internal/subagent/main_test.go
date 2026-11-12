package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Building the vault agent asks vault.Root() whether a vault is configured, and that reads
// ~/.config/alux/settings.json and caches the answer in a sync.Once for the whole process.
// Left alone, these tests behave one way on a machine with a vault and another on a machine
// without one, decided by whichever test happens to ask first — and a test pointed at a
// temporary vault could find itself resolving against the real one.
//
// t.Setenv cannot close this: the first Root() may happen inside the first test, by which point
// the once has already fired against the real HOME. It has to be set before any test runs.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "alux-subagent-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("ALUX_RECORDS_EMBED", "off")
	os.Setenv("ALUX_RECORDS", "off")
	os.Setenv("PATH", withoutMiseShims(os.Getenv("PATH")))
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// A mise shim refuses to run once HOME no longer holds the trust list for the user's config.
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
