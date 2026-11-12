// Package sandbox runs agent shell commands under macOS Seatbelt.
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/paths"
)

var exe = "/usr/bin/sandbox-exec"

// Enabled reads settings.json's {"sandbox": {"enabled": bool}}; anything but an explicit false is on.
func Enabled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return true
	}
	var settings struct {
		Sandbox *struct {
			Enabled *bool `json:"enabled"`
		} `json:"sandbox"`
	}
	if json.Unmarshal(data, &settings) != nil || settings.Sandbox == nil || settings.Sandbox.Enabled == nil {
		return true
	}
	return *settings.Sandbox.Enabled
}

// Profile is what one command may do: write under Root when Write is set, reach the network when Network is.
type Profile struct {
	Root    string
	Write   bool
	Network bool
	// ReadOnly is why Root is not writable, told to the agent when a write is refused.
	ReadOnly string
}

// Wrap is the argv prefix that runs a command under a built profile.
type Wrap struct {
	Args []string
	// Text is the generated profile, for reading; Args carries it too.
	Text string
	// ReadOnly is the profile's, or "" when its root is writable.
	ReadOnly string
}

// Command is bash -c command under w.
func (w Wrap) Command(command string) *exec.Cmd { return w.bash("-c", command) }

// bashPath is named in full so a PATH an agent exported cannot choose the shell.
const bashPath = "/bin/bash"

func (w Wrap) bash(args ...string) *exec.Cmd {
	argv := append(append(append([]string{}, w.Args[1:]...), bashPath), args...)
	return exec.Command(w.Args[0], argv...)
}

// Same reports whether two wraps run under one profile; nil is no sandbox.
func Same(a, b *Wrap) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.Equal(a.Args, b.Args)
}

type key struct{}

// With marks the shell commands run under ctx as sandboxed by w.
func With(ctx context.Context, w Wrap) context.Context { return context.WithValue(ctx, key{}, w) }

// From is the Wrap With put on ctx, if any.
func From(ctx context.Context) (Wrap, bool) {
	w, ok := ctx.Value(key{}).(Wrap)
	return w, ok
}

// Cmd is bash -c command, under ctx's Wrap when it carries one, and whether it is sandboxed.
func Cmd(ctx context.Context, command string) (*exec.Cmd, bool) {
	if w, ok := From(ctx); ok {
		return Shell(command, &w), true
	}
	return Shell(command, nil), false
}

// Shell is bash -c command, under w when it is non-nil, in the environment every agent shell command gets.
func Shell(command string, w *Wrap) *exec.Cmd { return Bash(w, "-c", command) }

// Bash is /bin/bash with args, under w when it is non-nil, in the base environment.
func Bash(w *Wrap, args ...string) *exec.Cmd {
	cmd := exec.Command(bashPath, args...)
	if w != nil {
		cmd = w.bash(args...)
	}
	cmd.Env = Env()
	return cmd
}

// Env is the environment every agent shell starts from.
func Env() []string {
	// With no terminal, jj waits forever on an editor it opens for a missing description.
	return append(os.Environ(), "JJ_EDITOR=false")
}

// Hint adds a line to a sandboxed command's output when it reads like the sandbox stopped it.
func Hint(out string) string { return (&Wrap{}).Hint(out) }

// Hint is the package Hint, naming why the root was read-only when a write was refused under w.
func (w *Wrap) Hint(out string) string {
	if w == nil {
		return out
	}
	if w.ReadOnly != "" && strings.Contains(out, "Operation not permitted") {
		return out + "\n[this ran in a sandbox that could not write the workspace: " + w.ReadOnly + "]"
	}
	if strings.Contains(out, "Operation not permitted") || strings.Contains(out, "Could not resolve host") {
		return out + "\n[this ran in the sandbox, which may have blocked it; sandbox: false asks to run it outside]"
	}
	return out
}

var devices = []string{"/dev/null", "/dev/zero", "/dev/tty", "/dev/stdout", "/dev/stderr", "/dev/dtracehelper"}

var denyOps = "file-write-create file-write-data file-write-unlink file-write-mode file-write-xattr file-write-flags " +
	"file-write-owner file-write-setugid file-write-acl file-link"

// Protected is what no agent may write under the home directory, relative to it.
var Protected = []string{".zshrc", ".zprofile", ".bashrc", ".bash_profile", ".profile", ".gitconfig", ".ssh",
	paths.Config, paths.Share, paths.State, paths.Cache}

// Build resolves p's paths and generates its profile. Every path reaches the profile as a -D parameter.
func Build(p Profile) (Wrap, error) {
	if runtime.GOOS != "darwin" {
		return Wrap{}, fmt.Errorf("the sandbox needs macOS sandbox-exec and this is %s; set sandbox.enabled to false in "+
			"~/.config/alux/settings.json to run commands unsandboxed", runtime.GOOS)
	}
	if _, err := os.Stat(exe); err != nil {
		return Wrap{}, fmt.Errorf("the sandbox needs %s: %w", exe, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Wrap{}, err
	}
	root, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		return Wrap{}, fmt.Errorf("the sandbox root: %w", err)
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return Wrap{}, fmt.Errorf("the temporary directory: %w", err)
	}
	writable := []string{"/private/tmp", tmp}
	for _, dir := range caches(home) {
		writable = append(writable, loose(dir))
	}
	if p.Write {
		writable = append(writable, root)
	}
	stores, gitDirs, denied := vcs(root)
	writable = append(writable, stores...)

	for _, dir := range append([]string{filepath.Join(root, ".git")}, gitDirs...) {
		for _, name := range []string{"hooks", "config", "config.worktree", "info", "commondir"} {
			denied = append(denied, filepath.Join(dir, name))
		}
		worktrees, _ := filepath.Glob(filepath.Join(dir, "worktrees", "*"))
		for _, worktree := range worktrees {
			denied = append(denied, filepath.Join(worktree, "config.worktree"), filepath.Join(worktree, "commondir"))
		}
	}
	for _, name := range Protected {
		denied = append(denied, filepath.Join(home, name))
	}
	var resolved []string
	for _, path := range denied {
		resolved = append(resolved, filepath.Join(loose(filepath.Dir(path)), filepath.Base(path)))
		if real, err := filepath.EvalSymlinks(path); err == nil {
			resolved = append(resolved, real)
		}
	}

	text, params := profile(dedupe(writable), dedupe(resolved), p.Network)
	args := []string{exe, "-p", text}
	for _, kv := range params {
		args = append(args, "-D", kv)
	}
	w := Wrap{Args: args, Text: text}
	if !p.Write {
		w.ReadOnly = p.ReadOnly
	}
	return w, nil
}

func profile(writable, denied []string, network bool) (string, []string) {
	var b strings.Builder
	var params []string
	param := func(prefix string, i int, path string) string {
		name := fmt.Sprintf("%s%d", prefix, i)
		params = append(params, name+"="+path)
		return fmt.Sprintf("(subpath (param %q))", name)
	}
	b.WriteString("(version 1)\n(allow default)\n(deny appleevent-send)\n(deny file-write*)\n(allow file-write*")
	for _, dev := range devices {
		fmt.Fprintf(&b, "\n    (literal %q)", dev)
	}
	b.WriteString("\n    (subpath \"/dev/fd\")")
	for i, path := range writable {
		b.WriteString("\n    " + param("W", i, path))
	}
	b.WriteString(")\n")
	if !network {
		b.WriteString("(deny network*)\n(allow network-outbound (remote ip \"localhost:*\"))\n" +
			"(allow network-inbound network-bind (local ip \"localhost:*\"))\n")
	}
	b.WriteString("(deny " + denyOps)
	for i, path := range denied {
		b.WriteString("\n    " + param("D", i, path))
	}
	b.WriteString(")\n")
	return b.String(), params
}

// caches are the tool caches every profile may write: go's build and module caches, the npm, yarn and pnpm stores, uv's data.
func caches(home string) []string {
	goenv.Do(func() {
		out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line = strings.TrimSpace(line); filepath.IsAbs(line) {
					goDirs = append(goDirs, line)
				}
			}
		}
		if len(goDirs) == 0 {
			goDirs = []string{filepath.Join(home, "go", "pkg", "mod")}
		}
	})
	dirs := []string{
		filepath.Join(home, "Library", "Caches"),
		filepath.Join(home, ".cache"),
		filepath.Join(home, ".npm"),
		filepath.Join(home, ".yarn", "berry"),
		filepath.Join(home, "Library", "pnpm"),
		filepath.Join(home, ".local", "share", "pnpm"),
		filepath.Join(home, ".local", "share", "uv"),
	}
	return append(dirs, goDirs...)
}

var (
	goenv  sync.Once
	goDirs []string
)

// vcs is the jj and git stores root's commands write even to read, the git directories among them,
// and the config and pointer files in them that later unsandboxed jj or git runs would trust.
func vcs(root string) (stores, gitDirs, guarded []string) {
	add := func(path string) string {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return ""
		}
		stores = append(stores, real)
		return real
	}
	jj := filepath.Join(root, ".jj")
	if add(jj) != "" {
		guarded = append(guarded, filepath.Join(jj, "workspace-config.toml"), filepath.Join(jj, "workspace-config-id"))
		repo := filepath.Join(jj, "repo")
		if isFile(repo) {
			guarded = append(guarded, repo)
		}
		if target, ok := pointer(repo, jj, ""); ok {
			repo = target
		}
		if repo = add(repo); repo != "" {
			store := filepath.Join(repo, "store")
			guarded = append(guarded, filepath.Join(repo, "config.toml"), filepath.Join(repo, "config"),
				filepath.Join(repo, "config-id"), filepath.Join(store, "git_target"))
			if target, ok := pointer(filepath.Join(store, "git_target"), store, ""); ok {
				if dir := add(target); dir != "" {
					gitDirs = append(gitDirs, dir)
				}
			}
		}
	}
	dotgit := filepath.Join(root, ".git")
	if isFile(dotgit) {
		guarded = append(guarded, dotgit)
	}
	if target, ok := pointer(dotgit, root, "gitdir:"); ok {
		if dir := add(target); dir != "" {
			gitDirs = append(gitDirs, dir)
			if common, ok := pointer(filepath.Join(dir, "commondir"), dir, ""); ok {
				if dir := add(common); dir != "" {
					gitDirs = append(gitDirs, dir)
				}
			}
		}
	} else if dir := add(dotgit); dir != "" {
		gitDirs = append(gitDirs, dir)
	}
	return stores, gitDirs, guarded
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// pointer reads a file holding a path, relative to base unless absolute, after an optional prefix.
func pointer(file, base, prefix string) (string, bool) {
	info, err := os.Stat(file)
	if err != nil || info.IsDir() {
		return "", false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	path, ok := strings.CutPrefix(strings.TrimSpace(string(data)), prefix)
	path = strings.TrimSpace(path)
	if !ok || path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return path, true
}

// loose is path with symlinks resolved as far as it exists, since Seatbelt matches real paths.
func loose(path string) string {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	dir := filepath.Dir(path)
	if dir == path {
		return path
	}
	return filepath.Join(loose(dir), filepath.Base(path))
}

func dedupe(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
