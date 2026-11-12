package sandbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// confined sets TMPDIR inside a fresh directory, so the always-writable temp directory is not the test's own.
func confined(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, dir := range []string{root, outside, filepath.Join(base, "tmp")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	return root, outside
}

var nested = sync.OnceValue(func() bool {
	out, err := exec.Command(exe, "-p", "(version 1)\n(allow default)", "true").CombinedOutput()
	return err != nil && strings.Contains(string(out), "sandbox_apply: Operation not permitted")
})

func run(t *testing.T, p Profile, command string) (string, error) {
	t.Helper()
	if nested() {
		t.Skip("already inside a sandbox, and Seatbelt profiles cannot nest")
	}
	w, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := w.Command(command)
	cmd.Dir = p.Root
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(20*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	err = cmd.Wait()
	return out.String(), err
}

func TestAWriteInsideTheRootRunsAndOneOutsideIsDenied(t *testing.T) {
	root, outside := confined(t)
	if out, err := run(t, Profile{Root: root, Write: true}, "echo hi > inside"); err != nil {
		t.Fatalf("a write inside the root failed: %v %s", err, out)
	}
	out, err := run(t, Profile{Root: root, Write: true}, fmt.Sprintf("echo hi > %q", filepath.Join(outside, "f")))
	if err == nil || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("a write outside the root was not denied: %v %q", err, out)
	}
}

func TestTheReadOnlyProfileDeniesARepoWrite(t *testing.T) {
	root, _ := confined(t)
	out, err := run(t, Profile{Root: root}, "echo hi > inside")
	if err == nil || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("a read-only profile let a repo write through: %v %q", err, out)
	}
}

func TestAReadOnlyProfileOnASecondWorkspaceDeniesFilesButLetsJJDescribe(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj is not on PATH")
	}
	base, _ := confined(t)
	base = filepath.Dir(base)
	jj := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("jj", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("jj %v: %v\n%s", args, err, out)
		}
	}
	jj(base, "git", "init", "repo")
	slot := filepath.Join(base, "slot")
	jj(filepath.Join(base, "repo"), "workspace", "add", slot)

	if out, err := run(t, Profile{Root: slot}, "echo hi > file.txt"); err == nil || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("a read-only profile let a write into the workspace through: %v %q", err, out)
	}
	if out, err := run(t, Profile{Root: slot}, `jj describe -m "session work"`); err != nil {
		t.Fatalf("jj describe under a read-only profile failed: %v\n%s", err, out)
	}
	cmd := exec.Command("jj", "log", "-r", "@", "--no-graph", "-T", "description")
	cmd.Dir = slot
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "session work" {
		t.Errorf("after describing under the profile @ reads %q (%v)", out, err)
	}
}

func TestMandatoryDeniesHoldInsideAWritableRoot(t *testing.T) {
	root, _ := confined(t)
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"echo x > .git/hooks/pre-commit",
		"echo x >> .git/config",
		"rm .git/config",
		"mv .git/HEAD .git/config",
	} {
		if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := run(t, Profile{Root: root, Write: true}, command); err == nil {
			t.Errorf("%s: ran inside a writable root: %q", command, out)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, ".git", "config")); string(data) != "[core]\n" {
		t.Errorf(".git/config changed to %q", data)
	}
	if out, err := run(t, Profile{Root: root, Write: true}, "echo x > .git/ORIG_HEAD"); err != nil {
		t.Errorf("an ordinary write to the git dir failed: %v %q", err, out)
	}
}

// pool is a jj repo and a second workspace whose .jj/repo and .git are pointer files into it.
func pool(t *testing.T) (repo, slot string) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj is not on PATH")
	}
	root, _ := confined(t)
	base := filepath.Dir(root)
	jj := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("jj", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("jj %v: %v\n%s", args, err, out)
		}
	}
	jj(base, "git", "init", "repo")
	repo, slot = filepath.Join(base, "repo"), filepath.Join(base, "slot")
	jj(repo, "workspace", "add", slot)
	for _, file := range []string{filepath.Join(slot, ".jj", "repo"), filepath.Join(slot, ".git")} {
		if info, err := os.Stat(file); err != nil || info.IsDir() {
			t.Fatalf("%s is not a pointer file (%v)", file, err)
		}
	}
	return repo, slot
}

func guardedPaths(repo, slot string) []string {
	store := filepath.Join(repo, ".jj", "repo")
	git := filepath.Join(repo, ".git")
	return []string{
		filepath.Join(slot, ".jj", "repo"),
		filepath.Join(slot, ".git"),
		filepath.Join(slot, ".jj", "workspace-config.toml"),
		filepath.Join(slot, ".jj", "workspace-config-id"),
		filepath.Join(store, "config.toml"),
		filepath.Join(store, "config", "x.toml"),
		filepath.Join(store, "config-id"),
		filepath.Join(store, "store", "git_target"),
		filepath.Join(git, "config"),
		filepath.Join(git, "hooks", "pre-commit"),
		filepath.Join(git, "info", "attributes"),
		filepath.Join(git, "commondir"),
		filepath.Join(git, "config.worktree"),
		filepath.Join(git, "worktrees", "slot", "commondir"),
		filepath.Join(git, "worktrees", "slot", "config.worktree"),
	}
}

func storePaths(repo string) []string {
	store := filepath.Join(repo, ".jj", "repo")
	git := filepath.Join(repo, ".git")
	return []string{
		filepath.Join(store, "store", "extra", "new"),
		filepath.Join(store, "op_store", "new"),
		filepath.Join(store, "op_heads", "heads", "new"),
		filepath.Join(store, "index", "new"),
		filepath.Join(git, "objects", "ab", "new"),
		filepath.Join(git, "objects", "info", "new"),
		filepath.Join(git, "refs", "heads", "new"),
		filepath.Join(git, "worktrees", "slot", "HEAD"),
	}
}

func under(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

func TestAPoolWorkspaceProfileDeniesConfigAndPointerFilesButNotTheStores(t *testing.T) {
	repo, slot := pool(t)
	w, err := Build(Profile{Root: slot, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	var denied, writable []string
	for i, arg := range w.Args {
		if arg != "-D" || i+1 == len(w.Args) {
			continue
		}
		name, path, _ := strings.Cut(w.Args[i+1], "=")
		if strings.HasPrefix(name, "D") {
			denied = append(denied, path)
		} else {
			writable = append(writable, path)
		}
	}
	matches := func(path string, dirs []string) bool {
		for _, dir := range dirs {
			if under(path, dir) {
				return true
			}
		}
		return false
	}
	for _, path := range guardedPaths(repo, slot) {
		if !matches(path, denied) {
			t.Errorf("%s is not denied", path)
		}
	}
	for _, path := range storePaths(repo) {
		if matches(path, denied) {
			t.Errorf("%s is denied", path)
		}
		if !matches(path, writable) {
			t.Errorf("%s is not writable", path)
		}
	}
}

func TestAPoolWorkspaceCannotWriteConfigOrPointerFilesButJJStillWorks(t *testing.T) {
	repo, slot := pool(t)
	for _, path := range guardedPaths(repo, slot) {
		command := fmt.Sprintf("mkdir -p %q; echo x >> %q", filepath.Dir(path), path)
		if out, err := run(t, Profile{Root: slot, Write: true}, command); err == nil {
			t.Errorf("a write to %s went through: %q", path, out)
		}
	}
	cmd := exec.Command("jj", "log", "-r", "@", "--no-graph", "-T", "commit_id")
	cmd.Dir = slot
	commit, err := cmd.Output()
	if err != nil {
		t.Fatalf("jj log: %v", err)
	}
	// jj refuses to import a ref under .git/refs that does not hold a commit id.
	for _, path := range storePaths(repo) {
		command := fmt.Sprintf("mkdir -p %q && echo %s > %q", filepath.Dir(path), commit, path)
		if out, err := run(t, Profile{Root: slot, Write: true}, command); err != nil {
			t.Errorf("a write to %s failed: %v %q", path, err, out)
		}
	}
	if out, err := run(t, Profile{Root: slot, Write: true}, `jj describe -m "session work" && jj new`); err != nil {
		t.Errorf("jj under the profile failed: %v\n%s", err, out)
	}
}

func TestLoopbackIsReachableAndTheNetworkIsNot(t *testing.T) {
	root, _ := confined(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("local-ok")) }))
	defer server.Close()
	if out, err := run(t, Profile{Root: root}, "curl -sS -m 5 "+server.URL); err != nil || !strings.Contains(out, "local-ok") {
		t.Errorf("loopback was not reachable: %v %q", err, out)
	}
	serve := "printf 'HTTP/1.0 200 OK\\r\\n\\r\\nserved-ok' | nc -l 127.0.0.1 47613 & sleep 0.5; curl -sS -m 5 http://127.0.0.1:47613; wait"
	if out, err := run(t, Profile{Root: root}, serve); err != nil || !strings.Contains(out, "served-ok") {
		t.Errorf("a server on loopback inside the sandbox did not answer: %v %q", err, out)
	}
	if out, err := run(t, Profile{Root: root}, "curl -sS -m 5 https://1.1.1.1"); err == nil {
		t.Errorf("a non-loopback connect went through: %q", out)
	}
	if out, err := run(t, Profile{Root: root, Network: true}, "curl -sS -m 5 -o /dev/null https://1.1.1.1"); err != nil {
		t.Logf("the network profile could not reach 1.1.1.1 either, so the denial above may be the network's: %v %q", err, out)
	}
}

func TestARefusedWriteSaysWhyTheWorkspaceWasReadOnly(t *testing.T) {
	root, _ := confined(t)
	refused := "bash: inside: Operation not permitted"
	w, err := Build(Profile{Root: root, ReadOnly: "the change had no description"})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Hint(refused); !strings.Contains(got, "the change had no description") || strings.Contains(got, "sandbox: false") {
		t.Fatalf("got %q", got)
	}
	writable, err := Build(Profile{Root: root, Write: true, ReadOnly: "the change had no description"})
	if err != nil {
		t.Fatal(err)
	}
	if got := writable.Hint(refused); strings.Contains(got, "no description") || !strings.Contains(got, "sandbox: false") {
		t.Fatalf("a writable profile gave the read-only reason: %q", got)
	}
}
