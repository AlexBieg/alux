package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/paths"
)

// Prepare readies a fresh workspace from the main checkout's ignored state, so it builds and tests without reinstalling.
type Prepare struct {
	// Clone lists globs, relative to the repo, of ignored paths copied copy-on-write from the main checkout.
	Clone []string `json:"clone"`
	// Rewrite lists globs, relative to the workspace, of cloned files whose main-checkout paths are pointed at the workspace.
	Rewrite []string `json:"rewrite"`
	// Run lists shell commands run in the workspace afterwards, in order.
	Run []string `json:"run"`
	// Coders gives every dispatched coder its own prepared workspace instead of the user's working copy.
	Coders bool `json:"coders"`
	// Pool gives every session a workspace of its own from a pool, instead of the default workspace.
	Pool bool `json:"pool"`
	// Refresh lists shell commands run in a pool workspace at handout when its lockfiles changed since its installs.
	Refresh []string `json:"refresh"`
	// Flow is how a session's work leaves its workspace: FlowLand, the default, or FlowPR.
	Flow string `json:"flow"`
	// Trunk is the revision FlowPR work is measured against; empty is DefaultTrunk.
	Trunk string `json:"trunk"`
}

const (
	// FlowLand lands finished work under the default workspace's @.
	FlowLand = "land"
	// FlowPR ships finished work through pull requests, so nothing lands in the default workspace.
	FlowPR       = "pr"
	DefaultTrunk = "main@origin"
)

// ErrPRFlow is landing refused in a repo whose work ships through pull requests.
var ErrPRFlow = errors.New(`this repo ships work through pull requests ("flow": "pr" under "workspaces" in ~/.config/alux/settings.json), so nothing lands in the default workspace`)

// PRFlow says the repo's work ships through pull requests rather than landing in the default workspace.
func (p *Prepare) PRFlow() bool { return p != nil && p.Flow == FlowPR }

// TrunkRev is the revision pull-request work is measured against.
func (p *Prepare) TrunkRev() string {
	if p == nil || p.Trunk == "" {
		return DefaultTrunk
	}
	return p.Trunk
}

// TrunkBranch is the branch name in a plain trunk such as main@origin, empty for a trunk that is any other revset.
func (p *Prepare) TrunkBranch() string {
	t := p.TrunkRev()
	if i := strings.LastIndex(t, "@"); i > 0 && !strings.ContainsAny(t, " ()|&~:") {
		return t[:i]
	}
	return ""
}

func (p *Prepare) validate() error {
	switch p.Flow {
	case "", FlowLand, FlowPR:
	default:
		return fmt.Errorf(`"flow" is %q; it must be %q or %q`, p.Flow, FlowLand, FlowPR)
	}
	if p.Trunk != "" && p.Flow != FlowPR {
		return fmt.Errorf(`"trunk" is set but only "flow": %q uses it`, FlowPR)
	}
	return nil
}

// preparedMark lives under the workspace's .jj, which jj never tracks and which goes when the workspace does.
const preparedMark = ".jj/alux-prepared"

// cloneJobs bounds the copies run at once; a big node_modules is the long pole either way.
const cloneJobs = 4

// LoadPrepare reads settings.json's {"workspaces": {"<repo path>": Prepare}} for repo; none configured is nil.
func LoadPrepare(repo string) (*Prepare, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	return LoadPrepareIn(home, repo)
}

// LoadPrepareIn is LoadPrepare for the home directory home.
func LoadPrepareIn(home, repo string) (*Prepare, error) {
	file := filepath.Join(home, paths.Config, "settings.json")
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var settings struct {
		Workspaces map[string]Prepare `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for key, p := range settings.Workspaces {
		if strings.HasPrefix(key, "~/") {
			key = filepath.Join(home, key[2:])
		}
		if samePath(key, repo) {
			if err := p.validate(); err != nil {
				return nil, fmt.Errorf("%s: workspaces[%q]: %w", file, key, err)
			}
			return &p, nil
		}
	}
	return nil, nil
}

func prepared(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, preparedMark))
	return err == nil
}

// prepare clones, rewrites and runs p's steps from src into the workspace dst, then marks it done; a failure leaves it unmarked so the next Ensure tries again.
func (p *Prepare) prepare(ctx context.Context, src, dst string) error {
	rels, err := expand(src, p.Clone)
	if err != nil {
		return err
	}
	if err := refuseTracked(ctx, dst, rels); err != nil {
		return err
	}
	before, err := changedFiles(ctx, dst)
	if err != nil {
		return err
	}
	if err := cloneAll(ctx, src, dst, rels); err != nil {
		return err
	}
	if err := p.rewrite(src, dst); err != nil {
		return err
	}
	for _, command := range p.Run {
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Dir = dst
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("preparing %s: %q failed: %w\n%s", dst, command, err, tail(string(out), outputCap))
		}
	}
	if err := refuseLeftovers(ctx, dst, before); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, preparedMark), nil, 0o600)
}

func expand(root string, globs []string) ([]string, error) {
	seen := map[string]bool{}
	var rels []string
	for _, g := range globs {
		if filepath.IsAbs(g) || strings.HasPrefix(filepath.Clean(g), "..") {
			return nil, fmt.Errorf("workspace glob %q must be relative to the repo and stay inside it", g)
		}
		matches, err := filepath.Glob(filepath.Join(root, g))
		if err != nil {
			return nil, fmt.Errorf("workspace glob %q: %w", g, err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(root, m)
			if err != nil {
				return nil, err
			}
			if !seen[rel] {
				seen[rel] = true
				rels = append(rels, rel)
			}
		}
	}
	sort.Strings(rels)
	return rels, nil
}

// refuseTracked stops a clone that would delete or shadow files jj tracks, which is what a misconfigured glob such as "web" would do.
func refuseTracked(ctx context.Context, dst string, rels []string) error {
	if len(rels) == 0 {
		return nil
	}
	args := []string{"--ignore-working-copy", "file", "list", "-r", "@", "--"}
	for _, rel := range rels {
		args = append(args, fmt.Sprintf("cwd:%q", filepath.ToSlash(rel)))
	}
	out, err := jjStdout(ctx, dst, args...)
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("not preparing %s: its clone list covers files jj tracks, starting with %s; clone only ignored paths", dst, strings.SplitN(out, "\n", 2)[0])
	}
	return nil
}

func cloneAll(ctx context.Context, src, dst string, rels []string) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		jobs = make(chan struct{}, cloneJobs)
	)
	for _, rel := range rels {
		wg.Add(1)
		jobs <- struct{}{}
		go func() {
			defer func() { <-jobs; wg.Done() }()
			if err := clone(ctx, filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// clone replaces to with a copy-on-write copy of from, so a retried preparation never nests one copy inside another.
func clone(ctx context.Context, from, to string) error {
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return cloneTree(ctx, from, to)
}

func copyTree(ctx context.Context, from, to string, flags ...string) error {
	args := append(flags, from, to)
	if out, err := exec.CommandContext(ctx, "cp", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cloning %s: %w\n%s", from, err, tail(string(out), outputCap))
	}
	return nil
}

// rewrite points every mention of the main checkout in the matched text files at the workspace; binary files and symlinks are left alone.
func (p *Prepare) rewrite(src, dst string) error {
	rels, err := expand(dst, p.Rewrite)
	if err != nil {
		return err
	}
	roots := []*regexp.Regexp{rootPattern(src)}
	if real, err := filepath.EvalSymlinks(src); err == nil && real != src {
		roots = append(roots, rootPattern(real))
	}
	to := []byte(strings.ReplaceAll(dst, "$", "$$") + "${1}")
	for _, rel := range rels {
		file := filepath.Join(dst, rel)
		info, err := os.Lstat(file)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		out := data
		for _, root := range roots {
			out = root.ReplaceAll(out, to)
		}
		if bytes.Equal(out, data) {
			continue
		}
		if err := replaceFile(file, out, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// rootPattern matches root only as the whole path or a parent of one, so a sibling such as root+".abc-1" is left alone.
func rootPattern(root string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(root) + `($|[/"':\s])`)
}

func replaceFile(file string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".alux-rewrite-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

func changedFiles(ctx context.Context, dst string) (map[string]bool, error) {
	out, err := jjStdout(ctx, dst, "diff", "--name-only")
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, f := range strings.Split(out, "\n") {
		if f != "" {
			files[f] = true
		}
	}
	return files, nil
}

// refuseLeftovers fails a preparation that left files jj would commit, and restores just those, so work already in the change survives.
func refuseLeftovers(ctx context.Context, dst string, before map[string]bool) error {
	after, err := changedFiles(ctx, dst)
	if err != nil {
		return err
	}
	var left []string
	for f := range after {
		if !before[f] {
			left = append(left, f)
		}
	}
	if len(left) == 0 {
		return nil
	}
	sort.Strings(left)
	args := []string{"restore", "--"}
	for _, f := range left {
		args = append(args, fmt.Sprintf("root:%q", f))
	}
	_, restoreErr := jj(ctx, dst, args...)
	return errors.Join(fmt.Errorf("preparing %s left files jj would commit, starting with %s; clone only ignored paths and have run commands write only ignored ones", dst, left[0]), restoreErr)
}
