package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
)

func edit(id string, input map[string]any) mockapi.Turn {
	return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: "str_replace_based_edit_tool", Input: input}}}
}

func TestOnlyEditsThatChangedAFileAreListedOnceEachMarkedByHowItFirstChanged(t *testing.T) {
	dir := t.TempDir()
	made, existing, untouched := filepath.Join(dir, "made.txt"), filepath.Join(dir, "existing.txt"), filepath.Join(dir, "untouched.txt")
	if err := os.WriteFile(existing, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestSession(t,
		edit("t1", map[string]any{"command": "create", "path": made, "file_text": "one\n"}),
		edit("t2", map[string]any{"command": "str_replace", "path": untouched, "old_str": "absent", "new_str": "x"}),
		edit("t3", map[string]any{"command": "view", "path": made}),
		edit("t4", map[string]any{"command": "str_replace", "path": made, "old_str": "one", "new_str": "two"}),
		edit("t5", map[string]any{"command": "str_replace", "path": existing, "old_str": "old", "new_str": "new"}),
		mockapi.Turn{Text: "all done"},
	)
	s.Submit("go")
	snap := settled(t, s, "all done")

	want := []File{{Path: made, Created: true}, {Path: existing}}
	if len(snap.Files) != len(want) || snap.Files[0] != want[0] || snap.Files[1] != want[1] {
		t.Fatalf("files = %+v, want %+v\n%s", snap.Files, want, transcript(snap))
	}
	s.NewSession()
	if got := s.Snapshot().Files; len(got) != 0 {
		t.Errorf("a new session kept %+v", got)
	}
}

func TestAFilesDiffRunsFromBeforeTheSessionsFirstEditToNow(t *testing.T) {
	dir := t.TempDir()
	existing, made := filepath.Join(dir, "existing.txt"), filepath.Join(dir, "made.txt")
	if err := os.WriteFile(existing, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestSession(t,
		edit("t1", map[string]any{"command": "str_replace", "path": existing, "old_str": "alpha", "new_str": "ALPHA"}),
		edit("t2", map[string]any{"command": "str_replace", "path": existing, "old_str": "beta", "new_str": "BETA"}),
		edit("t3", map[string]any{"command": "create", "path": made, "file_text": "one\n"}),
		mockapi.Turn{Text: "all done"},
	)
	s.Submit("go")
	settled(t, s, "all done")

	patch, err := s.FileDiff(existing)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-alpha\n", "-beta\n", "+ALPHA\n", "+BETA\n"} {
		if !strings.Contains(patch, want) {
			t.Errorf("diff of the edited file lacks %q:\n%s", want, patch)
		}
	}
	patch, err = s.FileDiff(made)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(patch, "@@ -0,0 +1 @@\n+one\n") {
		t.Errorf("diff of the created file is not all additions:\n%s", patch)
	}
}
