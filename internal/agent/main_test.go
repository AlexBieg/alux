package agent

import (
	"os"
	"testing"
)

// The built-in tool set asks vault.Root() whether a vault is configured, and that reads
// ~/.config/alux/settings.json and caches the answer in a sync.Once for the whole process.
// Left alone, these tests assemble a different tool set on a machine with a vault than on one
// without, decided by whichever test asks first. HOME is redirected before any test runs so the
// answer is always "none".
//
// t.Setenv cannot close this: the first Root() may happen inside the first test, by which point
// the once has already fired against the real HOME.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "alux-agent-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	// Record search would otherwise call whatever Ollama this machine runs.
	os.Setenv("ALUX_RECORDS_EMBED", "off")
	// Every agent loads rules; tests that want some point ALUX_RECORDS at a store of their own.
	os.Setenv("ALUX_RECORDS", "off")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
