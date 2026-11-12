package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// preparedRepo is a main checkout that ignores deps/ and .env and holds installs that name its own path, and a fresh workspace of it.
func preparedRepo(t *testing.T) (repo, dir string) {
	t.Helper()
	repo = mainRepo(t)
	write(t, repo, ".gitignore", "deps/\n.env\nran.log\n")
	write(t, repo, "tracked.txt", "tracked\n")
	must(t, repo, "commit", "-m", "ignore installs")
	if err := os.MkdirAll(filepath.Join(repo, "deps", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "deps/bin/tool", "#!"+repo+"/deps/bin/python\nNODE_PATH=\""+repo+"\"\nsibling="+repo+".abc-1/x\n")
	if err := os.Chmod(filepath.Join(repo, "deps/bin/tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "deps/bin/blob", "\x00"+repo+"/x")
	write(t, repo, ".env", "KEY=value\n")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, "T-1")
	must(t, repo, "workspace", "add", "--name", "T-1", "-r", "@-", dir)
	return repo, dir
}

func TestPreparePreparesAWorkspaceFromTheMainCheckout(t *testing.T) {
	t.Parallel()
	repo, dir := preparedRepo(t)
	p := &Prepare{
		Clone:   []string{"deps", ".env", "missing/*"},
		Rewrite: []string{"deps/bin/*"},
		Run:     []string{"echo ran >> ran.log"},
	}
	if err := p.prepare(ctx, repo, dir); err != nil {
		t.Fatal(err)
	}
	tool, err := os.ReadFile(filepath.Join(dir, "deps/bin/tool"))
	if err != nil {
		t.Fatal(err)
	}
	want := "#!" + dir + "/deps/bin/python\nNODE_PATH=\"" + dir + "\"\nsibling=" + repo + ".abc-1/x\n"
	if string(tool) != want {
		t.Errorf("rewritten tool is\n%s\nwant\n%s", tool, want)
	}
	if info, err := os.Stat(filepath.Join(dir, "deps/bin/tool")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("rewritten tool lost its mode: %v %v", info, err)
	}
	if blob, _ := os.ReadFile(filepath.Join(dir, "deps/bin/blob")); string(blob) != "\x00"+repo+"/x" {
		t.Errorf("binary file was rewritten to %q", blob)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, ".env")); string(env) != "KEY=value\n" {
		t.Errorf(".env is %q, want the main checkout's", env)
	}
	if main, _ := os.ReadFile(filepath.Join(repo, "deps/bin/tool")); !strings.Contains(string(main), "#!"+repo+"/deps") {
		t.Errorf("the main checkout's tool changed: %q", main)
	}
	if diff := must(t, dir, "diff", "--name-only"); diff != "" {
		t.Errorf("preparing left %q in the change", diff)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "ran.log")); string(log) != "ran\n" {
		t.Errorf("run commands ran %q, want once", log)
	}
	if !prepared(dir) {
		t.Error("the prepared workspace was not marked")
	}
}

func TestPrepareRefusesToCloneOverTrackedFiles(t *testing.T) {
	t.Parallel()
	repo, dir := preparedRepo(t)
	p := &Prepare{Clone: []string{"tracked.txt"}}
	if err := p.prepare(ctx, repo, dir); err == nil || !strings.Contains(err.Error(), "tracked.txt") {
		t.Fatalf("prepare gave %v, want a refusal naming tracked.txt", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "tracked.txt")); string(got) != "tracked\n" {
		t.Errorf("tracked.txt is %q, want it untouched", got)
	}
	if prepared(dir) {
		t.Error("a refused workspace was marked prepared")
	}
}

func TestPrepareUndoesAPreparationThatLeftFilesJJWouldCommitAndKeepsTheChangesWork(t *testing.T) {
	t.Parallel()
	repo, dir := preparedRepo(t)
	p := &Prepare{Run: []string{"echo oops > stray.txt"}}
	if err := p.prepare(ctx, repo, dir); err == nil || !strings.Contains(err.Error(), "stray.txt") {
		t.Fatalf("prepare gave %v, want a failure naming stray.txt", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stray.txt")); !os.IsNotExist(err) {
		t.Errorf("stray.txt is still on disk: %v", err)
	}

	write(t, dir, "work.txt", "agent work\n")
	if err := p.prepare(ctx, repo, dir); err == nil {
		t.Fatal("the retried preparation passed")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "work.txt")); string(got) != "agent work\n" {
		t.Errorf("work.txt is %q after the failed retry, want it kept", got)
	}

	p.Run = nil
	if err := p.prepare(ctx, repo, dir); err != nil {
		t.Fatalf("prepare once the run command was fixed: %v", err)
	}
	if !prepared(dir) {
		t.Error("the fixed preparation did not mark the workspace")
	}
}
