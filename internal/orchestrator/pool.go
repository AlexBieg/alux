package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/prs"
)

// PoolBase is the commit a slot starts from: the newest described ancestor of the default workspace's @, so undescribed edits stay out.
const PoolBase = `latest(::default@ & ~description(exact:""))`

// depsStamp records the lockfiles a slot's installs match, under its .jj like the prepared mark.
const depsStamp = ".jj/alux-deps"

// baseMark records the commit the slot's current session started from.
const baseMark = ".jj/alux-base"

// lockfiles are the tracked files whose change means a slot's installs are out of date.
var lockfiles = []string{"go.sum", "pnpm-lock.yaml", "package-lock.json", "yarn.lock", "uv.lock", "pixi.lock", "poetry.lock",
	"Cargo.lock", "Gemfile.lock", "mise.lock"}

var slotName = regexp.MustCompile(`^pool-(\d+)$`)

// Pool hands each session of one repo a prepared jj workspace of its own and keeps it for the next session afterwards.
type Pool struct {
	// Origin is the default workspace's root; Root holds the slots, one directory each beside its lock file.
	Origin, Root string
	Prepare      *Prepare
	// Log reports slow steps, such as preparing a new slot.
	Log func(string)
	// Merged looks up merged PRs by head branch for the PR flow's cleanup; nil leaves merged work alone.
	Merged func(ctx context.Context, slug string, heads []string) (map[string][]prs.MergedPR, error)
}

// Slot is the workspace one session holds; the lock goes with the process, however it ends.
type Slot struct {
	Name, Dir string
	lock      *os.File
}

// PoolFor is the pool of the repo whose default workspace is dir, or nil when dir is another workspace or the repo has no pool.
func PoolFor(ctx context.Context, dir string) (*Pool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	return PoolForIn(ctx, home, dir)
}

// PoolForIn is PoolFor for the home directory home.
func PoolForIn(ctx context.Context, home, dir string) (*Pool, error) {
	root, err := jjStdout(ctx, dir, "--ignore-working-copy", "root")
	if err != nil {
		return nil, nil
	}
	origin, err := jjStdout(ctx, dir, "--ignore-working-copy", "workspace", "root", "--name", "default")
	if err != nil || !samePath(root, origin) {
		return nil, nil
	}
	if origin, err = filepath.EvalSymlinks(origin); err != nil {
		return nil, err
	}
	prep, err := LoadPrepareIn(home, origin)
	if err != nil || prep == nil || !prep.Pool {
		return nil, err
	}
	return &Pool{Origin: origin, Root: filepath.Join(home, paths.Workspaces, filepath.Base(origin)), Prepare: prep}, nil
}

func (p *Pool) log(msg string) {
	if p.Log != nil {
		p.Log(msg)
	}
}

// Claim hands out a free prepared slot, moved onto the current base, or prepares a new one when none is free.
func (p *Pool) Claim(ctx context.Context) (*Slot, error) {
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		return nil, err
	}
	s, err := p.take(ctx, true)
	if err != nil || s == nil {
		return s, err
	}
	if err := p.begin(ctx, s); err != nil {
		s.unlock()
		return nil, err
	}
	return s, nil
}

// Restart ends the slot's session and begins the next one in it, as quitting and relaunching would, without giving up the lock.
func (p *Pool) Restart(ctx context.Context, s *Slot) error {
	if s == nil || s.lock == nil {
		return fmt.Errorf("no workspace is held")
	}
	return errors.Join(p.end(ctx, s), p.begin(ctx, s))
}

// begin readies a held slot for a new session.
func (p *Pool) begin(ctx context.Context, s *Slot) error {
	if err := p.refresh(ctx, s); err != nil {
		return fmt.Errorf("refreshing workspace %s: %w", s.Name, err)
	}
	return nil
}

// end records the session's described work for /land and drops a change it left empty, carrying on past a failed step.
func (p *Pool) end(ctx context.Context, s *Slot) error {
	var errs []error
	if err := p.record(ctx, s); err != nil {
		errs = append(errs, fmt.Errorf("%s: recording its work for /land failed: %w", s.Name, err))
	}
	if _, err := jj(ctx, s.Dir, "abandon", "@ & empty()"); err != nil {
		errs = append(errs, fmt.Errorf("%s kept its empty change: %w", s.Name, err))
	}
	return errors.Join(errs...)
}

// Spare prepares a free slot ahead of the next session, unless one is free already.
func (p *Pool) Spare(ctx context.Context) error {
	s, err := p.take(ctx, false)
	if s != nil {
		s.unlock()
	}
	return err
}

// take locks a free ready slot, or prepares one at the lowest free number. For a spare, an existing free slot counts as done.
func (p *Pool) take(ctx context.Context, claim bool) (*Slot, error) {
	for _, n := range p.numbers() {
		s, ok, err := p.lock(n)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if p.ready(ctx, s) {
			return s, nil
		}
		s.unlock()
	}
	for n := 1; ; n++ {
		s, ok, err := p.lock(n)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if p.ready(ctx, s) {
			return s, nil
		}
		if claim {
			p.log(fmt.Sprintf("alux: preparing workspace %s for this session; a new one is slow, and kept for later sessions", s.Name))
		}
		if err := p.create(ctx, s); err != nil {
			s.unlock()
			return nil, fmt.Errorf("preparing workspace %s: %w", s.Name, err)
		}
		return s, nil
	}
}

func (p *Pool) numbers() []int {
	entries, _ := os.ReadDir(p.Root)
	var out []int
	for _, e := range entries {
		if m := slotName.FindStringSubmatch(e.Name()); m != nil && e.IsDir() {
			n, _ := strconv.Atoi(m[1])
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func (p *Pool) lock(n int) (*Slot, bool, error) {
	name := fmt.Sprintf("pool-%d", n)
	f, err := os.OpenFile(filepath.Join(p.Root, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Slot{Name: name, Dir: filepath.Join(p.Root, name), lock: f}, true, nil
}

func (s *Slot) unlock() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}

// ready is a slot prepared to the end and registered in this repo under its own name.
func (p *Pool) ready(ctx context.Context, s *Slot) bool {
	if !prepared(s.Dir) {
		return false
	}
	root, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "workspace", "root", "--name", s.Name)
	return err == nil && samePath(root, s.Dir)
}

// create clears what a failed preparation left, adds the workspace at the base and prepares it from the default workspace.
func (p *Pool) create(ctx context.Context, s *Slot) error {
	if registered, err := (JJWorkspaces{Repo: p.Origin}).registered(ctx, s.Name); err != nil {
		return err
	} else if registered {
		if _, err := jj(ctx, p.Origin, "workspace", "forget", s.Name); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(s.Dir); err != nil {
		return err
	}
	if err := (JJWorkspaces{Repo: p.Origin}).prune(ctx); err != nil {
		return err
	}
	base, err := p.base(ctx, p.Origin)
	if err != nil {
		return err
	}
	if _, err := jj(ctx, p.Origin, "workspace", "add", "--name", s.Name, "-r", base, s.Dir); err != nil {
		return err
	}
	stamp, err := lockHash(ctx, p.Origin)
	if err != nil {
		return err
	}
	if err := writeStamp(filepath.Join(s.Dir, depsStamp), stamp); err != nil {
		return err
	}
	if p.Prepare != nil {
		return p.Prepare.prepare(ctx, p.Origin, s.Dir)
	}
	return writeStamp(filepath.Join(s.Dir, preparedMark), "")
}

func (p *Pool) base(ctx context.Context, dir string) (string, error) {
	commits, err := JJ{}.Resolve(ctx, dir, PoolBase)
	if err != nil {
		return "", err
	}
	if len(commits) == 1 {
		return commits[0], nil
	}
	commits, err = JJ{}.Resolve(ctx, dir, "trunk()")
	if err != nil || len(commits) != 1 {
		return "", fmt.Errorf("no described commit under the default workspace's @, and no trunk() to start from instead")
	}
	return commits[0], nil
}

// refresh moves the slot onto the current base, drops the change a session left empty, and reinstalls when lockfiles changed.
func (p *Pool) refresh(ctx context.Context, s *Slot) error {
	p.log(fmt.Sprintf("alux: moving %s onto the current base", s.Name))
	if _, err := jjStdout(ctx, s.Dir, "log", "-r", "@", "--no-graph", "-T", "commit_id"); err != nil {
		if !strings.Contains(err.Error(), "stale") {
			return err
		}
		if _, err := jj(ctx, s.Dir, "workspace", "update-stale"); err != nil {
			return err
		}
	}
	base, err := p.base(ctx, s.Dir)
	if err != nil {
		return err
	}
	at, err := jjStdout(ctx, s.Dir, "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T",
		`change_id ++ " " ++ parents.map(|c| c.commit_id()).join(",") ++ " " ++ if(empty, "empty", "work") ++ " " ++ if(description, "described", "bare")`)
	if err != nil {
		return err
	}
	f := strings.Fields(at)
	if len(f) != 4 {
		return fmt.Errorf("reading the slot's @ gave %q", at)
	}
	change, parents, empty, bare := f[0], f[1], f[2] == "empty", f[3] == "bare"
	if err := p.record(ctx, s); err != nil {
		return err
	}
	if parents != base || !empty || !bare {
		if _, err := jj(ctx, s.Dir, "new", base); err != nil {
			return err
		}
		if empty {
			if _, err := jj(ctx, s.Dir, "abandon", "change_id("+change+") & empty()"); err != nil {
				return err
			}
		}
	}
	if err := writeStamp(filepath.Join(s.Dir, baseMark), base); err != nil {
		return err
	}
	return p.reinstall(ctx, s)
}

func (p *Pool) reinstall(ctx context.Context, s *Slot) error {
	stamp := filepath.Join(s.Dir, depsStamp)
	want, err := lockHash(ctx, s.Dir)
	if err != nil {
		return err
	}
	if have, _ := os.ReadFile(stamp); string(have) == want {
		return nil
	}
	if p.Prepare != nil && len(p.Prepare.Refresh) > 0 {
		p.log(fmt.Sprintf("alux: lockfiles changed since %s was installed; reinstalling", s.Name))
		for _, command := range p.Prepare.Refresh {
			p.log("alux:   $ " + command)
			argv, err := shellArgv(s.Dir, command, true)
			if err != nil {
				return fmt.Errorf("%q: %w", command, err)
			}
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir = s.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%q failed: %w\n%s", command, err, tail(string(out), outputCap))
			}
		}
	}
	return writeStamp(stamp, want)
}

// writeStamp renames a temp file over path, so a symlink a session put there is replaced, not followed.
func writeStamp(path, body string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// lockHash fingerprints the tracked lockfiles in dir as they are on disk.
func lockHash(ctx context.Context, dir string) (string, error) {
	out, err := jjStdout(ctx, dir, "file", "list")
	if err != nil {
		return "", err
	}
	var files []string
	for _, f := range lines(out) {
		if slices.Contains(lockfiles, filepath.Base(f)) {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", f, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Release ends the slot's session and gives the slot back.
func (p *Pool) Release(ctx context.Context, s *Slot) {
	if s == nil || s.lock == nil {
		return
	}
	if err := p.end(ctx, s); err != nil {
		fmt.Fprintf(os.Stderr, "alux: %v\n", err)
	}
	s.unlock()
}
