package skills

import "testing"

func noticeRun(t *testing.T, body string) *Run {
	t.Helper()
	s, err := Parse("", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Bind(s, nil, started, home)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestANodeWhoseOnlyPendingPredecessorIsOptionalIsNotBehind(t *testing.T) {
	r := noticeRun(t, `schema: 2
skill: tidy
version: 1
when: testing notices
requires: { tools: [glob, grep, str_replace_based_edit_tool] }
nodes:
  tidy:
    say: Tidy up first if you like.
    optional: true
    evidence:
      - call: { tool: glob }
  notes:
    say: Write the notes.
    evidence:
      - artifact_lines: { path: /tmp/notes.md, pattern: "^- ", min: 1 }
  write:
    say: Write it.
    evidence:
      - call: { tool: grep }
order: [[tidy, write], [notes, write]]
`)
	w := &world{files: map[string]string{"/tmp/notes.md": "- one\n"}}
	w.log = []Event{
		call(editorTool, map[string]any{"command": "create", "path": "/tmp/notes.md", "file_text": "- one\n"}),
		call("grep", nil),
		call(editorTool, map[string]any{"command": "view", "path": "/tmp/notes.md"}),
	}
	if got := NodesBehind(r, w.log, w.env()); got != nil {
		t.Errorf("an optional predecessor never done must be looked through, got %+v", got)
	}
}

func TestViewingAnEarlierNodesFileAfterALaterNodeIsDoneLeavesNothingBehind(t *testing.T) {
	r := noticeRun(t, `schema: 2
skill: notes
version: 1
when: testing notices
requires: { tools: [grep, str_replace_based_edit_tool] }
nodes:
  notes:
    say: Write the notes.
    evidence:
      - artifact_lines: { path: /tmp/notes.md, pattern: "^- ", min: 1 }
  write:
    say: Write it.
    evidence:
      - call: { tool: grep }
order: [[notes, write]]
`)
	w := &world{files: map[string]string{"/tmp/notes.md": "- one\n"}}
	w.log = []Event{
		call(editorTool, map[string]any{"command": "create", "path": "/tmp/notes.md", "file_text": "- one\n"}),
		call("grep", nil),
		call(editorTool, map[string]any{"command": "view", "path": "/tmp/notes.md"}),
	}
	if got := NodesBehind(r, w.log, w.env()); got != nil {
		t.Errorf("a view that moves notes' done time past write's is not a pending predecessor, got %+v", got)
	}
}
