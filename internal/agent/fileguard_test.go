package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/clip"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/web"
)

func tempHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileTool(t *testing.T, h Handler, name string, input map[string]any) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(input)
	cb := Callbacks{Approve: func(context.Context, string, string) bool { return true }}
	return resultText(t, h(context.Background(), Call{ID: "c", Name: name, Input: raw}, cb))
}

func edit(t *testing.T, input map[string]any) (string, bool) {
	return fileTool(t, runEdit, editorTool, input)
}

func TestACreateUnderSshIsRefused(t *testing.T) {
	home := tempHome(t)
	key := filepath.Join(home, ".ssh", "authorized_keys")
	if out, isErr := edit(t, map[string]any{"command": "create", "path": key, "file_text": "ssh-ed25519 AAAA"}); !isErr {
		t.Fatalf("a create in ~/.ssh succeeded: %s", out)
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("the refused create left a file behind: %v", err)
	}
}

func TestAJobMayWriteUnderJobsOutButNotItsStamps(t *testing.T) {
	home := tempHome(t)
	digest := filepath.Join(home, paths.JobsOut, "research-digest", "digest.md")
	stamp := filepath.Join(home, paths.State, "jobs", "research-digest.last")
	if out, isErr := edit(t, map[string]any{"command": "create", "path": digest, "file_text": "# This week"}); isErr {
		t.Errorf("creating a job's output gave %q", out)
	}
	if out, isErr := edit(t, map[string]any{"command": "create", "path": stamp, "file_text": "2020-01-01T00:00:00Z"}); !isErr {
		t.Errorf("a create over a job stamp succeeded: %s", out)
	}
}

func TestTheSettingsFileIsWrittenOnlyOnTheUsersRequiredYes(t *testing.T) {
	home := tempHome(t)
	settings := filepath.Join(home, paths.Config, "settings.json")
	other := filepath.Join(home, paths.Config, "AGENTS.md")
	put(t, settings, `{"a": 1}`)
	put(t, other, "x")
	approve := func(context.Context, string, string) bool { return true }
	run := func(path string, cb Callbacks) (string, bool) {
		raw, _ := json.Marshal(map[string]any{"command": "str_replace", "path": path, "old_str": "1", "new_str": "2"})
		return resultText(t, runEdit(context.Background(), Call{ID: "c", Name: editorTool, Input: raw}, cb))
	}
	read := func() string { b, _ := os.ReadFile(settings); return string(b) }

	if out, isErr := run(settings, Callbacks{Approve: approve}); !isErr || read() != `{"a": 1}` {
		t.Fatalf("with no Require the edit gave %q and left %s", out, read())
	}
	no := func(context.Context, string, string, string) bool { return false }
	if out, isErr := run(settings, Callbacks{Approve: approve, Require: no}); !isErr || read() != `{"a": 1}` {
		t.Fatalf("a declined Require gave %q and left %s", out, read())
	}
	yes := func(context.Context, string, string, string) bool { return true }
	if out, isErr := run(settings, Callbacks{Approve: approve, Require: yes}); isErr || read() != `{"a": 2}` {
		t.Fatalf("an approved Require gave %q and left %s", out, read())
	}
	if out, isErr := run(other, Callbacks{Approve: approve, Require: yes}); !isErr || !strings.Contains(out, "no agent may write") {
		t.Fatalf("another file under Config gave %q", out)
	}
}

func TestASkillFileIsWrittenOnlyOnTheUsersRequiredYes(t *testing.T) {
	home := tempHome(t)
	config := filepath.Join(home, paths.Config)
	skill := filepath.Join(config, "skills", "pr-review.yaml")
	put(t, skill, "version: 1")
	put(t, filepath.Join(config, "auth.json"), "1")
	if err := os.Symlink(filepath.Join(config, "auth.json"), filepath.Join(config, "skills", "escape.yaml")); err != nil {
		t.Fatal(err)
	}
	approve := func(context.Context, string, string) bool { return true }
	var asked []string
	yes := func(_ context.Context, _, _, reason string) bool { asked = append(asked, reason); return true }
	no := func(context.Context, string, string, string) bool { return false }
	run := func(path string, cb Callbacks) (string, bool) {
		raw, _ := json.Marshal(map[string]any{"command": "str_replace", "path": path, "old_str": "1", "new_str": "2"})
		return resultText(t, runEdit(context.Background(), Call{ID: "c", Name: editorTool, Input: raw}, cb))
	}
	read := func() string { b, _ := os.ReadFile(skill); return string(b) }

	if out, isErr := run(skill, Callbacks{Approve: approve}); !isErr || read() != "version: 1" {
		t.Fatalf("with no Require the edit gave %q and left %q", out, read())
	}
	if out, isErr := run(skill, Callbacks{Approve: approve, Require: no}); !isErr || read() != "version: 1" {
		t.Fatalf("a declined Require gave %q and left %q", out, read())
	}
	if out, isErr := run(skill, Callbacks{Approve: approve, Require: yes}); isErr || read() != "version: 2" {
		t.Fatalf("an approved Require gave %q and left %q", out, read())
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "an alux skill") {
		t.Errorf("the prompt should name a skill edit, asked %q", asked)
	}
	created, _ := json.Marshal(map[string]any{"command": "create", "path": filepath.Join(config, "skills", "new.yaml"), "file_text": "x"})
	if out, isErr := resultText(t, runEdit(context.Background(), Call{ID: "c", Name: editorTool, Input: created}, Callbacks{Approve: approve, Require: yes})); isErr {
		t.Errorf("creating a new skill on a yes gave %q", out)
	}
	for _, path := range []string{filepath.Join(config, "auth.json"), filepath.Join(config, "skills.yaml"), filepath.Join(config, "skills", "escape.yaml")} {
		if out, isErr := run(path, Callbacks{Approve: approve, Require: yes}); !isErr || !strings.Contains(out, "no agent may write") {
			t.Errorf("%s outside the skills dir gave %q", path, out)
		}
	}
}

func TestViewingAKeyUnderSshIsRefusedInAnyCasing(t *testing.T) {
	home := tempHome(t)
	put(t, filepath.Join(home, ".ssh", "id_ed25519"), "PRIVATE KEY")
	for _, path := range []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".SSH", "id_ed25519")} {
		if out, isErr := edit(t, map[string]any{"command": "view", "path": path}); !isErr || strings.Contains(out, "PRIVATE KEY") {
			t.Errorf("viewing %s gave %q", path, out)
		}
		if out, isErr := fileTool(t, RunView, editorTool, map[string]any{"command": "view", "path": path}); !isErr {
			t.Errorf("a read-only agent viewing %s gave %q", path, out)
		}
	}
}

func TestCoderLayersAreUsableWhileShareIsDenied(t *testing.T) {
	home := tempHome(t)
	share := filepath.Join(home, paths.Share)
	put(t, filepath.Join(share, "records.db"), "secret records")
	ws := filepath.Join(home, paths.Workspaces, "repo", "layers", "task-1")
	put(t, filepath.Join(ws, "main.go"), "package main\n")

	if out, isErr := edit(t, map[string]any{"command": "view", "path": filepath.Join(ws, "main.go")}); isErr {
		t.Errorf("viewing a coder workspace file gave %q", out)
	}
	if out, isErr := edit(t, map[string]any{"command": "create", "path": filepath.Join(ws, "new.go"), "file_text": "package main\n"}); isErr {
		t.Errorf("creating in a coder workspace gave %q", out)
	}
	if out, isErr := edit(t, map[string]any{"command": "view", "path": filepath.Join(share, "records.db")}); !isErr {
		t.Errorf("viewing records.db gave %q", out)
	}
	link := filepath.Join(ws, "records")
	if err := os.Symlink(filepath.Join(share, "records.db"), link); err != nil {
		t.Fatal(err)
	}
	if out, isErr := edit(t, map[string]any{"command": "view", "path": link}); !isErr {
		t.Errorf("viewing records.db through a link in a workspace gave %q", out)
	}
}

func TestSkillRunFilesAreWritableWhileTheRestOfStateIsDenied(t *testing.T) {
	home := tempHome(t)
	state := filepath.Join(home, paths.State)
	brief := filepath.Join(state, "skills", "dev-cycle", "20261027T190341Z", "brief.md")
	if out, isErr := edit(t, map[string]any{"command": "create", "path": brief, "file_text": "brief"}); isErr {
		t.Errorf("creating a skill run file gave %q", out)
	}
	if out, isErr := edit(t, map[string]any{"command": "create", "path": filepath.Join(state, "dispatches.jsonl"), "file_text": "{}"}); !isErr {
		t.Errorf("creating a file elsewhere in State gave %q", out)
	}
	link := filepath.Join(state, "skills", "escape")
	if err := os.MkdirAll(filepath.Join(state, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(state, "sessions"), link); err != nil {
		t.Fatal(err)
	}
	if out, isErr := edit(t, map[string]any{"command": "create", "path": filepath.Join(link, "x.md"), "file_text": "x"}); !isErr {
		t.Errorf("creating through a link out of the skills folder gave %q", out)
	}
}

func TestAWriteThroughADanglingLinkToAProtectedPathIsRefused(t *testing.T) {
	home := tempHome(t)
	ws := filepath.Join(home, paths.Workspaces, "repo", "layers", "task-1")
	put(t, filepath.Join(ws, "main.go"), "package main\n")
	if err := os.Symlink(filepath.Join(home, ".zprofile"), filepath.Join(ws, "profile")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(ws, "keys")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(ws, "profile"), filepath.Join(ws, "keys", "authorized_keys")} {
		if err := checkPath(context.Background(), path, true); err == nil || !strings.Contains(err.Error(), "no agent may write") {
			t.Errorf("a write to %s gave %v", path, err)
		}
	}
}

func TestAWriteThroughADanglingLinkWithinTheWorkspaceIsAllowed(t *testing.T) {
	home := tempHome(t)
	ws := filepath.Join(home, paths.Workspaces, "repo", "layers", "task-1")
	put(t, filepath.Join(ws, "main.go"), "package main\n")
	if err := os.Symlink("docs/new.md", filepath.Join(ws, "notes.md")); err != nil {
		t.Fatal(err)
	}
	if err := checkPath(context.Background(), filepath.Join(ws, "notes.md"), true); err != nil {
		t.Errorf("a write through a link to a missing file in the workspace gave %v", err)
	}
	if got, err := realPath(filepath.Join(ws, "notes.md")); err != nil || got != filepath.Join(ws, "docs", "new.md") {
		t.Errorf("the link resolved to %q, %v", got, err)
	}
}

func TestWebCacheAndSpillFilesStayViewable(t *testing.T) {
	home := tempHome(t)
	cached := filepath.Join(web.CacheDir(), "page.md")
	if !strings.HasPrefix(cached, home) {
		t.Fatalf("the web cache %s is not under the test home", cached)
	}
	put(t, cached, "cached page")
	spill := filepath.Join(clip.SpillDir, "fileguard-test.txt")
	put(t, spill, "spilled output")
	t.Cleanup(func() { os.Remove(spill) })

	for path, want := range map[string]string{cached: "cached page", spill: "spilled output"} {
		if out, isErr := edit(t, map[string]any{"command": "view", "path": path}); isErr || !strings.Contains(out, want) {
			t.Errorf("viewing %s gave %q", path, out)
		}
	}
}

func TestAHiddenGrepOverHomeIsRefused(t *testing.T) {
	home := tempHome(t)
	put(t, filepath.Join(home, ".ssh", "id_ed25519"), "PRIVATE KEY")
	out, isErr := fileTool(t, RunSearch, "grep", map[string]any{"pattern": "PRIVATE", "path": home, "hidden": true})
	if !isErr || strings.Contains(out, "PRIVATE KEY") || !strings.Contains(out, "would descend") {
		t.Fatalf("a hidden grep over home gave %q", out)
	}
}

func TestAWriteToTheOriginInOtherCasingIsRefused(t *testing.T) {
	origin, slot := slotRepo(t)
	prev := Guard
	Guard = &SlotGuard{Slot: slot, Origin: origin}
	t.Cleanup(func() { Guard = prev })

	path := filepath.Join(filepath.Dir(origin), strings.ToUpper(filepath.Base(origin)), "file.go")
	if out, isErr := edit(t, map[string]any{"command": "create", "path": path, "file_text": "package x\n"}); !isErr || !strings.Contains(out, "read-only") {
		t.Fatalf("a write to %s gave %q", path, out)
	}
	if _, err := os.Stat(filepath.Join(origin, "file.go")); !os.IsNotExist(err) {
		t.Fatalf("the refused write reached the origin: %v", err)
	}
}

func TestACoderEditUnderJJInOtherCasingIsRefused(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".jj", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Rules{Edit: []Rule{
		{Pattern: root, Verdict: VerdictAllow},
		{Pattern: filepath.Join(root, ".jj"), Verdict: VerdictDeny},
	}}
	if v, _ := r.Decide(Intent{Class: ClassEdit, Writes: true, Path: filepath.Join(root, ".JJ", "repo", "op_heads")}); v != VerdictDeny {
		t.Fatalf("an edit under .JJ was %v", v)
	}
}
