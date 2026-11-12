package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

// repo is a fresh jj repo with workspaces ws1 and ws2 beside it, returned as absolute paths.
func repo(t *testing.T) (ws1, ws2 string) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs real jj")
	}
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj is not on PATH")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must(t, dir, "git", "init", "repo")
	main := filepath.Join(dir, "repo")
	ws1, ws2 = filepath.Join(dir, "ws1"), filepath.Join(dir, "ws2")
	must(t, main, "workspace", "add", ws1)
	must(t, main, "workspace", "add", ws2)
	return ws1, ws2
}

func must(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := jj(ctx, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mainRepo(t *testing.T) string {
	t.Helper()
	ws1, _ := repo(t)
	return filepath.Join(filepath.Dir(ws1), "repo")
}

func workspaceNames(t *testing.T, repo string) []string {
	t.Helper()
	return strings.Split(must(t, repo, "workspace", "list", "-T", `name ++ "\n"`), "\n")
}

func changeAt(t *testing.T, dir, rev string) string {
	t.Helper()
	return must(t, dir, "log", "-r", rev, "--no-graph", "-T", `change_id ++ "\n"`)
}

func TestChangeDiffIsTheChangesOwnGitDiff(t *testing.T) {
	t.Parallel()
	ws1, ws2 := repo(t)
	must(t, ws1, "new", "root()")
	w1 := changeAt(t, ws1, "@")
	write(t, ws1, "a.txt", "a\n")
	must(t, ws1, "status")
	must(t, ws2, "new", w1)
	w2 := changeAt(t, ws2, "@")
	write(t, ws2, "b.txt", "b\n")
	must(t, ws2, "status")
	got, err := ChangeDiff(ctx, ws1, w2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "diff --git a/b.txt b/b.txt") || !strings.Contains(got, "+b") || strings.Contains(got, "a.txt") {
		t.Errorf("diff of w2 is %q, want b.txt alone", got)
	}
}

func TestErrorOutputKeepsTheTail(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 2*outputCap) + "THE END"
	got := tail(long, outputCap)
	if !strings.HasSuffix(got, "THE END") || len(got) > outputCap+64 {
		t.Errorf("tail kept %d bytes ending %q, want at most about %d ending with THE END", len(got), got[len(got)-10:], outputCap)
	}
}
