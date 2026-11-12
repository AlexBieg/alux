// Package reviewgraph builds a file-level impact graph of a GitHub PR in a jj workspace at its head:
// sem diff names the changed entities and one ripgrep pass finds the files that mention them.
// Edges match bare names, so a mention in a string or comment, or of a namesake, also counts.
package reviewgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/alexbieg/alux/internal/paths"
)

type Graph struct {
	Repo       string
	Number     int
	Head       string
	Workspace  string
	Files      []File
	Edges      []Edge
	Unexamined []string
	TooCommon  []string
	Diff       string
	// Sources is each changed file's text at Head, keyed by path; deleted, binary and oversized files are absent.
	Sources map[string]string
}

type File struct {
	Path     string
	Changed  bool
	Status   string
	Entities []string
}

type Edge struct {
	From, To string
	Via      []string
}

const outputCap = 4096

// maxFiles is how many files may mention a name before it counts as too common to be a reference.
const maxFiles = 50

// codeExts keeps sem and ripgrep off data files, where sem is slow and a mention is no reference.
var codeExts = []string{".go", ".py", ".pyi", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".rb", ".rs", ".java", ".kt", ".swift", ".php", ".cs", ".c", ".h", ".cc", ".cpp", ".hpp"}

func Build(ctx context.Context, repo string, number int) (Graph, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Graph{}, fmt.Errorf("repo %q is not owner/name", repo)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Graph{}, err
	}
	checkout, err := checkoutFor(ctx, home, owner, name)
	if err != nil {
		return Graph{}, err
	}
	n := strconv.Itoa(number)
	out, err := run(ctx, checkout, nil, "gh", "pr", "view", n, "--repo", repo, "--json", "headRefOid,baseRefOid")
	if err != nil {
		return Graph{}, err
	}
	var pr struct {
		Head string `json:"headRefOid"`
		Base string `json:"baseRefOid"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil || pr.Head == "" || pr.Base == "" {
		return Graph{}, fmt.Errorf("gh pr view %s --repo %s gave %q", n, repo, out)
	}
	// jj git import skips a commit only FETCH_HEAD names, so fetch to a remote ref.
	ref := "refs/remotes/origin/alux-review/pr-" + n
	if _, err := run(ctx, checkout, nil, "git", "fetch", "origin", "+refs/pull/"+n+"/head:"+ref, pr.Base); err != nil {
		return Graph{}, err
	}
	base, err := run(ctx, checkout, nil, "git", "merge-base", pr.Base, pr.Head)
	if err != nil {
		return Graph{}, err
	}
	if _, err := jj(ctx, checkout, "--ignore-working-copy", "git", "import"); err != nil {
		return Graph{}, err
	}
	nameStatus, err := run(ctx, checkout, nil, "git", "diff", "--name-status", "-M", base, pr.Head)
	if err != nil {
		return Graph{}, err
	}
	dir := filepath.Join(home, paths.Workspaces, filepath.Base(checkout), "review")
	unlock, err := lockWorkspace(dir)
	if err != nil {
		return Graph{}, err
	}
	defer unlock()
	if err := ensureWorkspace(ctx, checkout, dir, "review", pr.Head); err != nil {
		return Graph{}, err
	}
	semArgs := append([]string{"diff", base + ".." + pr.Head, "--format", "json", "--file-exts"}, codeExts...)
	semOut, semErrOut, err := output(ctx, dir, []string{"SEM_CLOUD=0"}, "", "sem", semArgs...)
	if err != nil {
		return Graph{}, fmt.Errorf("sem diff in %s: %w\n%s", dir, err, tail(string(semErrOut), outputCap))
	}
	files, follow, unexamined, err := entities(parseNameStatus(nameStatus), semOut)
	if err != nil {
		return Graph{}, fmt.Errorf("sem diff in %s: %w", dir, err)
	}
	rgOut, err := mentions(ctx, dir, follow)
	if err != nil {
		return Graph{}, err
	}
	files, edges, tooCommon, err := impacts(files, follow, rgOut)
	if err != nil {
		return Graph{}, fmt.Errorf("rg in %s: %w", dir, err)
	}
	diff, err := run(ctx, checkout, nil, "git", "diff", "--no-color", "-M", base, pr.Head)
	if err != nil {
		return Graph{}, err
	}
	sources, err := readSources(ctx, checkout, pr.Head, files)
	if err != nil {
		return Graph{}, err
	}
	return Graph{Repo: repo, Number: number, Head: pr.Head, Workspace: dir, Files: files, Edges: edges, Unexamined: unexamined, TooCommon: tooCommon, Diff: diff, Sources: sources}, nil
}

// maxSource is the largest file kept in Graph.Sources.
const maxSource = 1 << 20

// readSources reads the changed files at head in one git cat-file pass.
func readSources(ctx context.Context, checkout, head string, files []File) (map[string]string, error) {
	var ask []string
	for _, f := range files {
		if f.Changed && f.Status != "D" {
			ask = append(ask, f.Path)
		}
	}
	if len(ask) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, p := range ask {
		in.WriteString(head + ":" + p + "\n")
	}
	out, stderr, err := output(ctx, checkout, nil, in.String(), "git", "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("git cat-file --batch in %s: %w\n%s", checkout, err, tail(string(stderr), outputCap))
	}
	return parseBatch(ask, out), nil
}

// parseBatch pairs git cat-file --batch output with the paths asked for, in order.
func parseBatch(paths []string, out []byte) map[string]string {
	sources := map[string]string{}
	for _, p := range paths {
		header, rest, ok := bytes.Cut(out, []byte("\n"))
		if !ok {
			break
		}
		out = rest
		f := strings.Fields(string(header))
		if len(f) != 3 {
			continue
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size+1 > len(out) {
			break
		}
		body := out[:size]
		out = out[size+1:]
		if f[1] == "blob" && size <= maxSource && !bytes.Contains(body, []byte{0}) {
			sources[p] = string(body)
		}
	}
	return sources
}

// mentions runs one ripgrep pass over dir's source files for every followed name, as JSON lines.
func mentions(ctx context.Context, dir string, follow map[string][]string) ([]byte, error) {
	if len(follow) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(follow))
	for n := range follow {
		names = append(names, n)
	}
	slices.Sort(names)
	args := []string{"--json", "--word-regexp", "--fixed-strings", "--no-require-git", "--file", "-"}
	for _, ext := range codeExts {
		args = append(args, "--glob", "*"+ext)
	}
	stdout, stderr, err := output(ctx, dir, nil, strings.Join(names, "\n")+"\n", "rg", append(args, ".")...)
	// rg exits 1 when nothing matched.
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rg in %s: %w\n%s", dir, err, tail(string(stderr), outputCap))
	}
	return stdout, nil
}

func checkoutFor(ctx context.Context, home, owner, name string) (string, error) {
	file := filepath.Join(home, paths.Config, "settings.json")
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var settings struct {
		Workspaces map[string]json.RawMessage `json:"workspaces"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return "", fmt.Errorf("%s: %w", file, err)
		}
	}
	keys := make([]string, 0, len(settings.Workspaces))
	for key := range settings.Workspaces {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		dir := key
		if strings.HasPrefix(dir, "~/") {
			dir = filepath.Join(home, dir[2:])
		}
		origin, err := run(ctx, dir, nil, "git", "remote", "get-url", "origin")
		if err != nil || !originMatches(origin, owner, name) {
			continue
		}
		if info, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
			return "", fmt.Errorf("%s (workspaces[%q] in %s) is not a colocated jj repo with a .git directory; point that key at the colocated checkout of %s/%s", dir, key, file, owner, name)
		}
		return dir, nil
	}
	return "", fmt.Errorf(`no checkout of github.com/%s/%s is configured; add its colocated jj checkout under "workspaces" in %s`, owner, name, file)
}

func originMatches(raw, owner, name string) bool {
	raw = strings.TrimSpace(raw)
	var host, p string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		host, p = u.Hostname(), u.Path
	} else {
		rest := raw
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		host, p, _ = strings.Cut(rest, ":")
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	return strings.EqualFold(host, "github.com") && strings.EqualFold(p, owner+"/"+name)
}

// lockWorkspace holds dir's review workspace for one build, across alux processes.
func lockWorkspace(dir string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func ensureWorkspace(ctx context.Context, checkout, dir, name, head string) error {
	registered, err := registeredAt(ctx, checkout, name)
	if err != nil {
		return err
	}
	if registered != "" && samePath(registered, dir) {
		if root, err := jj(ctx, dir, "--ignore-working-copy", "workspace", "root"); err == nil && samePath(root, dir) {
			return moveTo(ctx, dir, head)
		}
	}
	if registered != "" {
		if _, err := jj(ctx, checkout, "workspace", "forget", name); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if _, err := run(ctx, checkout, nil, "git", "worktree", "prune"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	_, err = jj(ctx, checkout, "workspace", "add", "--name", name, "-r", head, dir)
	return err
}

// registeredAt is the root jj records for workspace name in checkout, or "" when it has none.
func registeredAt(ctx context.Context, checkout, name string) (string, error) {
	out, err := jj(ctx, checkout, "--ignore-working-copy", "workspace", "list", "-T", `name ++ "\n"`)
	if err != nil {
		return "", err
	}
	if !slices.Contains(strings.Split(out, "\n"), name) {
		return "", nil
	}
	root, err := jj(ctx, checkout, "--ignore-working-copy", "workspace", "root", "--name", name)
	if err != nil {
		return "", err
	}
	return root, nil
}

func moveTo(ctx context.Context, dir, head string) error {
	at, err := jj(ctx, dir, "log", "-r", "@", "--no-graph", "-T", `parents.map(|c| c.commit_id()).join(",") ++ " " ++ if(empty, "empty", "work")`)
	if err != nil {
		if !strings.Contains(err.Error(), "stale") {
			return err
		}
		if _, err := jj(ctx, dir, "workspace", "update-stale"); err != nil {
			return err
		}
	}
	if at == head+" empty" {
		return nil
	}
	_, err = jj(ctx, dir, "new", head)
	return err
}

func parseNameStatus(out string) []File {
	var files []File
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		status := f[0][:1]
		switch status {
		case "C":
			status = "A"
		case "T":
			status = "M"
		}
		files = append(files, File{Path: f[len(f)-1], Changed: true, Status: status})
	}
	return files
}

var identifier = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)

// unfollowed: properties are reached through an object, and nothing calls a test by name.
var unfollowed = map[string]bool{"orphan": true, "property": true, "test": true, "test_suite": true, "test_hook": true}

// entities attaches sem diff's changed entities to the changed files, and returns the names to follow,
// each with the files defining it, and the changed files sem read no entity in.
func entities(changed []File, semJSON []byte) ([]File, map[string][]string, []string, error) {
	var d struct {
		Changes []struct {
			Name string `json:"entityName"`
			Type string `json:"entityType"`
			File string `json:"filePath"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(semJSON, &d); err != nil {
		return nil, nil, nil, err
	}
	files := slices.Clone(changed)
	index := map[string]int{}
	for i, f := range files {
		index[f.Path] = i
	}
	follow := map[string][]string{}
	for _, c := range d.Changes {
		p := clean(c.File)
		at, ok := index[p]
		if !ok || c.Type == "orphan" {
			continue
		}
		if !slices.Contains(files[at].Entities, c.Name) {
			files[at].Entities = append(files[at].Entities, c.Name)
		}
		if !unfollowed[c.Type] && identifier.MatchString(c.Name) && !slices.Contains(follow[c.Name], p) {
			follow[c.Name] = append(follow[c.Name], p)
		}
	}
	var unexamined []string
	for _, f := range files {
		if len(f.Entities) == 0 {
			unexamined = append(unexamined, f.Path)
		}
	}
	return files, follow, unexamined, nil
}

// impacts turns ripgrep's JSON matches into edges from each defining file to every other file mentioning
// the name, and returns the names mentioned in more than maxFiles files, which draw no edge.
func impacts(changed []File, follow map[string][]string, rgJSONL []byte) ([]File, []Edge, []string, error) {
	hits := map[string]map[string]bool{}
	for i, line := range bytes.Split(rgJSONL, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec struct {
			Type string `json:"type"`
			Data struct {
				Path       struct{ Text string } `json:"path"`
				Submatches []struct {
					Match struct{ Text string } `json:"match"`
				} `json:"submatches"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, nil, nil, fmt.Errorf("line %d of rg's output: %w", i+1, err)
		}
		p := clean(rec.Data.Path.Text)
		if rec.Type != "match" || p == "" {
			continue
		}
		for _, s := range rec.Data.Submatches {
			if _, ok := follow[s.Match.Text]; !ok {
				continue
			}
			if hits[s.Match.Text] == nil {
				hits[s.Match.Text] = map[string]bool{}
			}
			hits[s.Match.Text][p] = true
		}
	}
	names := make([]string, 0, len(hits))
	for n := range hits {
		names = append(names, n)
	}
	slices.Sort(names)
	files := slices.Clone(changed)
	index := map[string]int{}
	for i, f := range files {
		index[f.Path] = i
	}
	type pair struct{ from, to string }
	via := map[pair][]string{}
	impacted := map[string]bool{}
	var tooCommon []string
	for _, n := range names {
		if len(hits[n]) > maxFiles {
			tooCommon = append(tooCommon, n)
			continue
		}
		for to := range hits[n] {
			for _, from := range follow[n] {
				if to == from {
					continue
				}
				k := pair{from, to}
				via[k] = append(via[k], n)
				if _, ok := index[to]; !ok {
					impacted[to] = true
				}
			}
		}
	}
	extra := make([]string, 0, len(impacted))
	for p := range impacted {
		extra = append(extra, p)
	}
	slices.Sort(extra)
	for _, p := range extra {
		files = append(files, File{Path: p})
	}
	edges := make([]Edge, 0, len(via))
	for k, v := range via {
		edges = append(edges, Edge{From: k.from, To: k.to, Via: v})
	}
	slices.SortFunc(edges, func(a, b Edge) int {
		if c := strings.Compare(a.From, b.From); c != 0 {
			return c
		}
		return strings.Compare(a.To, b.To)
	})
	return files, edges, tooCommon, nil
}

func clean(p string) string {
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func output(ctx context.Context, dir string, env []string, stdin, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	stdout, stderr, err := output(ctx, dir, env, "", name, args...)
	if err != nil {
		return "", fmt.Errorf("%s %s in %s: %w\n%s", name, strings.Join(args, " "), dir, err, tail(string(stderr)+string(stdout), outputCap))
	}
	return strings.TrimSpace(string(stdout)), nil
}

func jj(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, dir, nil, "jj", append([]string{"--no-pager", "--color=never"}, args...)...)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "[…" + strconv.Itoa(len(s)-n) + " bytes cut]\n" + s[len(s)-n:]
}
