package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func jjIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, out)
	}
}

func TestASlotCountsItsWholeStackSinceDefaultOnceEach(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("no jj")
	}
	t.Setenv("JJ_CONFIG", "")
	t.Setenv("JJ_USER", "t")
	t.Setenv("JJ_EMAIL", "t@example.com")
	root := t.TempDir()
	repo, slot := filepath.Join(root, "repo"), filepath.Join(root, "slot")
	jjIn(t, root, "git", "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "landed.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jjIn(t, repo, "commit", "-m", "landed")
	jjIn(t, repo, "workspace", "add", "--name", "pool-1", slot)

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(slot, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "one\n")
	jjIn(t, slot, "commit", "-m", "first")
	write("a.txt", "one\ntwo\n")
	jjIn(t, slot, "status")

	s := readSlot(slot, Scope{Slot: "pool-1", Base: "default@"}, []string{"id", "", "0", "0", ""})
	if s == nil {
		t.Fatal("no status")
	}
	want := Counts{Added: 1, LinesAdded: 2}
	if s.Pending != want {
		t.Errorf("pending = %+v; want %+v (one new file over two changes, nothing from default)", s.Pending, want)
	}
}
