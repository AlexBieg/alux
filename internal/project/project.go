// Package project resolves the directories the agent works in, once, at startup.
package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var (
	once   sync.Once
	cwd    string
	jjDir  string
	root   string
	origin string
)

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func resolve() {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	cwd = wd

	jjDir = run("jj", "root")

	r := jjDir
	if r == "" {
		r = run("git", "rev-parse", "--show-toplevel")
	}
	if r == "" {
		r = cwd
	}
	if real, err := filepath.EvalSymlinks(r); err == nil {
		r = real
	}
	root = r

	origin = jjDir
	if jjDir != "" {
		if o := run("jj", "-R", jjDir, "--ignore-working-copy", "workspace", "root", "--name", "default"); o != "" {
			if real, err := filepath.EvalSymlinks(o); err == nil {
				origin = real
			}
		}
	}
}

// Cwd is the working directory, with symlinks resolved. Comparing an unresolved path against a
// resolved one is the macOS /tmp trap, so everything here is resolved up front.
func Cwd() string { once.Do(resolve); return cwd }

// JJRoot is the jj workspace root, or "" outside a jj repo.
func JJRoot() string { once.Do(resolve); return jjDir }

// Origin is the root of the repo's default workspace, which keys its settings, or JJRoot when it has none.
func Origin() string { once.Do(resolve); return origin }

// Home is the path that names this repo in settings and notes: Origin in a jj repo, Root otherwise.
func Home() string {
	if o := Origin(); o != "" {
		return o
	}
	return Root()
}

// Workspace is the jj workspace's name, read off its root since pool slots are named for their directories, or "" outside a jj repo.
func Workspace() string {
	switch once.Do(resolve); {
	case jjDir == "":
		return ""
	case jjDir == origin:
		return "default"
	default:
		return filepath.Base(jjDir)
	}
}

// Root is the repository root, falling back to the working directory.
func Root() string { once.Do(resolve); return root }

// RepoOf is the root of the jj or git workspace holding path and the root of its repo's main workspace, or "" for both outside any repo.
func RepoOf(path string) (root, origin string) {
	dir := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	} else if real, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		dir = filepath.Join(real, filepath.Base(dir))
	}
	for ; ; dir = filepath.Dir(dir) {
		if o, ok := jjOrigin(dir); ok {
			return dir, o
		}
		if o, ok := gitOrigin(dir); ok {
			return dir, o
		}
		if dir == filepath.Dir(dir) {
			return "", ""
		}
	}
}

// A secondary jj workspace's .jj/repo is a file naming the main workspace's .jj/repo.
func jjOrigin(dir string) (string, bool) {
	repo := filepath.Join(dir, ".jj", "repo")
	info, err := os.Stat(repo)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return dir, true
	}
	data, err := os.ReadFile(repo)
	if err != nil {
		return dir, true
	}
	target := strings.TrimSpace(string(data))
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, ".jj", target)
	}
	return realOr(filepath.Dir(filepath.Dir(target))), true
}

// A git worktree's .git is a file whose gitdir sits under the main checkout's .git.
func gitOrigin(dir string) (string, bool) {
	dotgit := filepath.Join(dir, ".git")
	info, err := os.Stat(dotgit)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return dir, true
	}
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return dir, true
	}
	target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	target = filepath.Clean(target)
	if i := strings.LastIndex(target, string(filepath.Separator)+".git"+string(filepath.Separator)); i > 0 {
		return realOr(target[:i]), true
	}
	return dir, true
}

func realOr(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// Short renders a path relative to the working directory when it is inside it.
func Short(path string) string {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(Cwd(), path)
	}
	abs = filepath.Clean(abs)
	if rel, err := filepath.Rel(Cwd(), abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}
