package prs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	diffInlineLimit = 60000
	filesPerPage    = 100
	// GitHub lists at most 3000 files for a pull request.
	maxFilePages = 30
)

// REST is a GitHub REST GET of path, relative to the API root.
type REST func(ctx context.Context, path string) ([]byte, error)

func GhREST(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("gh: %s", msg)
		}
		return nil, fmt.Errorf("gh: %w", err)
	}
	return out, nil
}

type DiffFile struct {
	Path, Previous       string
	Status               string
	Additions, Deletions int
	Patch                string
}

type Diff struct {
	Repo    string
	Number  int
	Title   string
	HeadSHA string
	// Since is the commit an incremental diff starts from; empty for the whole PR.
	Since        string
	ChangedFiles int
	Files        []DiffFile
}

type apiFile struct {
	Filename  string `json:"filename"`
	Previous  string `json:"previous_filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

func (d *Diff) add(files []apiFile) {
	for _, f := range files {
		d.Files = append(d.Files, DiffFile{
			Path: f.Filename, Previous: f.Previous, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch,
		})
	}
}

func fetchPR(ctx context.Context, rest REST, owner, name string, number int) (Diff, error) {
	body, err := rest(ctx, fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, number))
	if err != nil {
		return Diff{}, err
	}
	var pr struct {
		Title        string `json:"title"`
		ChangedFiles int    `json:"changed_files"`
		Head         struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return Diff{}, fmt.Errorf("prs: reading GitHub's answer: %w", err)
	}
	return Diff{Repo: owner + "/" + name, Number: number, Title: pr.Title, HeadSHA: pr.Head.SHA, ChangedFiles: pr.ChangedFiles}, nil
}

func FetchDiff(ctx context.Context, rest REST, owner, name string, number int) (Diff, error) {
	d, err := fetchPR(ctx, rest, owner, name, number)
	if err != nil {
		return Diff{}, err
	}
	for page := 1; page <= maxFilePages; page++ {
		body, err := rest(ctx, fmt.Sprintf("repos/%s/%s/pulls/%d/files?per_page=%d&page=%d", owner, name, number, filesPerPage, page))
		if err != nil {
			return Diff{}, err
		}
		var files []apiFile
		if err := json.Unmarshal(body, &files); err != nil {
			return Diff{}, fmt.Errorf("prs: reading GitHub's file list: %w", err)
		}
		d.add(files)
		if len(files) < filesPerPage {
			break
		}
	}
	return d, nil
}

// Statuses of FetchSince: GitHub's compare statuses, Gone for a commit GitHub no longer has, and TooLarge past its file cap.
const (
	CompareIdentical = "identical"
	CompareDiverged  = "diverged"
	CompareBehind    = "behind"
	CompareGone      = "gone"
	CompareTooLarge  = "too-large"
)

// GitHub's compare lists at most 300 files, all on its first page; paging it pages commits.
const compareFileCap = 300

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// FetchSince is what changed from the commit since to the PR's current head; it holds no files unless the status is ahead.
func FetchSince(ctx context.Context, rest REST, owner, name string, number int, since string) (Diff, string, error) {
	if !commitSHA.MatchString(since) {
		return Diff{}, "", fmt.Errorf("since %q is not a commit SHA", since)
	}
	d, err := fetchPR(ctx, rest, owner, name, number)
	if err != nil {
		return Diff{}, "", err
	}
	d.Since = since
	body, err := rest(ctx, fmt.Sprintf("repos/%s/%s/compare/%s...%s", owner, name, since, d.HeadSHA))
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "HTTP 422") {
			return d, CompareGone, nil
		}
		return Diff{}, "", err
	}
	var cmp struct {
		Status string    `json:"status"`
		Files  []apiFile `json:"files"`
	}
	if err := json.Unmarshal(body, &cmp); err != nil {
		return Diff{}, "", fmt.Errorf("prs: reading GitHub's comparison: %w", err)
	}
	switch {
	case cmp.Status == CompareIdentical || cmp.Status == CompareDiverged || cmp.Status == CompareBehind:
		return d, cmp.Status, nil
	case len(cmp.Files) >= compareFileCap:
		return d, CompareTooLarge, nil
	}
	d.add(cmp.Files)
	d.ChangedFiles = len(d.Files)
	return d, cmp.Status, nil
}

// unified is one file as a git-style diff, or "" when GitHub sent no patch.
func (f DiffFile) unified() string {
	if f.Patch == "" {
		return ""
	}
	old, cur := f.Path, f.Path
	if f.Previous != "" {
		old = f.Previous
	}
	from, to := "a/"+old, "b/"+cur
	switch f.Status {
	case "added":
		from = "/dev/null"
	case "removed":
		to = "/dev/null"
	}
	return fmt.Sprintf("diff --git a/%s b/%s\n--- %s\n+++ %s\n%s\n", old, cur, from, to, strings.TrimRight(f.Patch, "\n"))
}

func statusLetter(s string) string {
	switch s {
	case "added":
		return "A"
	case "removed":
		return "D"
	case "renamed":
		return "R"
	case "copied":
		return "C"
	}
	return "M"
}

// wantsPath is exact, or everything under a directory.
func wantsPath(path string, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, w := range wanted {
		w = strings.TrimPrefix(w, "./")
		if path == w || strings.HasPrefix(path, strings.TrimSuffix(w, "/")+"/") {
			return true
		}
	}
	return false
}

// WriteDiff saves the whole PR as one unified diff and returns its path and each file's first and last line in it.
func WriteDiff(dir string, d Diff) (string, map[string][2]int, error) {
	ranges := map[string][2]int{}
	var b strings.Builder
	line := 1
	for _, f := range d.Files {
		u := f.unified()
		if u == "" {
			continue
		}
		n := strings.Count(u, "\n")
		ranges[f.Path] = [2]int{line, line + n - 1}
		line += n
		b.WriteString(u)
	}
	sub := filepath.Join(dir, strings.ReplaceAll(d.Repo, "/", "__"))
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", nil, err
	}
	file := fmt.Sprintf("%d-%s.diff", d.Number, short(d.HeadSHA))
	if d.Since != "" {
		file = fmt.Sprintf("%d-%s..%s.diff", d.Number, short(d.Since), short(d.HeadSHA))
	}
	path := filepath.Join(sub, file)
	tmp, err := os.CreateTemp(sub, ".diff-*")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		return "", nil, err
	}
	return path, ranges, os.Rename(tmp.Name(), path)
}

// RenderDiff is the index of every file, then as many of the wanted files' patches as fit under limit, in PR order.
func RenderDiff(d Diff, path string, ranges map[string][2]int, wanted []string, limit int) string {
	var b strings.Builder
	adds, dels := 0, 0
	for _, f := range d.Files {
		adds, dels = adds+f.Additions, dels+f.Deletions
	}
	head := "head " + short(d.HeadSHA)
	if d.Since != "" {
		head = "changes since " + short(d.Since) + " · " + head
	}
	fmt.Fprintf(&b, "### #%d %s\n\n%s · %d files · +%d/−%d\n", d.Number, d.Title, head, len(d.Files), adds, dels)
	fmt.Fprintf(&b, "Whole diff: %s\n", path)
	if d.ChangedFiles > len(d.Files) {
		fmt.Fprintf(&b, "GitHub lists only %d of the %d changed files.\n", len(d.Files), d.ChangedFiles)
	}

	var unknown []string
	for _, w := range wanted {
		if !matchesAny(d.Files, w) {
			unknown = append(unknown, w)
		}
	}
	if len(unknown) > 0 {
		fmt.Fprintf(&b, "Not in this PR: %s\n", strings.Join(unknown, ", "))
	}

	var shown, left []DiffFile
	used := 0
	for _, f := range d.Files {
		u := f.unified()
		if u == "" || !wantsPath(f.Path, wanted) {
			continue
		}
		if used+len(u) > limit {
			left = append(left, f)
			continue
		}
		used += len(u)
		shown = append(shown, f)
	}

	b.WriteString("\n#### Files, with their lines in the whole diff\n\n")
	for _, f := range d.Files {
		where := "no patch from GitHub: binary or too large"
		if r, ok := ranges[f.Path]; ok {
			where = fmt.Sprintf("lines %d–%d", r[0], r[1])
		}
		name := "`" + f.Path + "`"
		if f.Previous != "" {
			name = "`" + f.Previous + "` → " + name
		}
		fmt.Fprintf(&b, "- %s %s +%d/−%d · %s\n", statusLetter(f.Status), name, f.Additions, f.Deletions, where)
	}

	if len(left) > 0 {
		names := make([]string, len(left))
		for i, f := range left {
			names[i] = "`" + f.Path + "`"
		}
		fmt.Fprintf(&b, "\nNot shown below, past the %dk-character cap: %s. View them in the whole diff at the lines above.\n",
			limit/1000, strings.Join(names, ", "))
	}
	if len(shown) == 0 {
		return strings.TrimRight(b.String(), "\n")
	}
	var body strings.Builder
	for _, f := range shown {
		body.WriteString(f.unified())
	}
	fence := strings.Repeat("`", max(3, longestRun(body.String(), '`')+1))
	b.WriteString("\n" + fence + "diff\n" + body.String() + fence)
	return b.String()
}

func matchesAny(files []DiffFile, w string) bool {
	for _, f := range files {
		if wantsPath(f.Path, []string{w}) {
			return true
		}
	}
	return false
}

func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}
