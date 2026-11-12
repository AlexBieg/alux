package agent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
)

func pinGhAt(t *testing.T, path string) {
	t.Helper()
	ghPinOnce, ghLookup = sync.Once{}, func() string { return path }
	t.Cleanup(func() { ghPinOnce, ghLookup = sync.Once{}, func() string { return pinGh(os.Getenv("PATH")) } })
}

func pollTurn(command string, sandboxed bool) mockapi.Turn {
	input := map[string]any{"command": command}
	if !sandboxed {
		input["sandbox"] = false
	}
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: pollTool, Input: input}}}
}

func TestOnlyAnExactGhReadPollRunsOutsideTheSandboxUnscreened(t *testing.T) {
	for _, tc := range []struct {
		command   string
		sandboxed bool
		pin       string
		free      bool
	}{
		{"gh pr checks 12 --repo o/r", false, "/opt/gh", true},
		{"gh pr checks 12 --repo o/r --required", false, "/opt/gh", true},
		{"gh run view 99 --log-failed --repo acme/app", false, "/opt/gh", true},
		{"gh pr checks 12 --repo o/r", true, "/opt/gh", false},
		{"gh pr checks 12 --repo o/r", false, "", false},
		{"gh pr checks 12 --repo o/r;id", false, "/opt/gh", false},
		{"gh pr checks 12 --repo o/r $(id)", false, "/opt/gh", false},
		{"gh pr checks $(id) --repo o/r", false, "/opt/gh", false},
		{"GH_HOST=x gh pr checks 12 --repo o/r", false, "/opt/gh", false},
		{"/tmp/gh pr checks 12 --repo o/r", false, "/opt/gh", false},
		{"gh pr checks 12 --repo o/r --web", false, "/opt/gh", false},
		{"gh pr checks 12", false, "/opt/gh", false},
		{"gh run view 99 --log-failed", false, "/opt/gh", false},
		{"gh pr checks 12 --repo o/r\nid", false, "/opt/gh", false},
	} {
		pinGhAt(t, tc.pin)
		a, f, r := sandboxedAgent(t, pollTurn(tc.command, tc.sandboxed), mockapi.Turn{Text: "done"})
		a.Handlers[pollTool] = r.handler
		f.verdict = stopped
		lastResult(t, a, &recorder{})
		free := r["t1"] == "unwrapped" && len(f.asked) == 0
		if free != tc.free || !tc.free && r["t1"] == "unwrapped" {
			t.Errorf("%q sandboxed=%v pin=%q: ran under %q, screened %v; want free=%v",
				tc.command, tc.sandboxed, tc.pin, r["t1"], f.asked, tc.free)
		}
	}
}

func TestTheGhPinSkipsMise(t *testing.T) {
	mise, plain := filepath.Join(t.TempDir(), "mise", "shims"), t.TempDir()
	for _, dir := range []string{mise, plain} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := pinGh(mise+string(os.PathListSeparator)+plain), filepath.Join(plain, "gh"); got != want {
		t.Errorf("pinned %q, want %q", got, want)
	}
	if got := pinGh(mise); got != "" {
		t.Errorf("pinned %q with only mise on the path, want none", got)
	}
}
