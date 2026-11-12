package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A git diff from HEAD would hide every file the run created, and in a change that adds files
// that is most of the change.
func TestDiffInGitIncludesNewFilesAndOnlyChangesSinceTheMark(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("before.txt", "PRE-EXISTING\n")

	mark, ok := Snapshot(dir)
	if !ok {
		t.Fatal("a git repository was not snapshotted")
	}
	write("before.txt", "PRE-EXISTING\nEDITED-LINE\n")
	write("created.txt", "CREATED-LINE\n")

	diff, err := mark.Diff()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+EDITED-LINE", "created.txt", "+CREATED-LINE"} {
		if !strings.Contains(diff, want) {
			t.Errorf("the diff is missing %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "+PRE-EXISTING") {
		t.Errorf("the diff includes what was there before the mark:\n%s", diff)
	}
	if out, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); strings.Contains(string(out), "A ") {
		t.Errorf("snapshotting staged files in the user's own index:\n%s", out)
	}
}

func TestSnapshotOutsideARepositoryReportsFalse(t *testing.T) {
	if _, ok := Snapshot(t.TempDir()); ok {
		t.Error("a plain directory was snapshotted as a repository")
	}
}
