package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Finding files and searching their contents through bash works, but the output arrives unshaped:
// no cap on how much comes back, binaries and ignored paths included, and one accidental `rg .`
// costs a context window. These are thin wrappers over fd and rg whose job is the shaping.
const (
	searchMaxLines = 200
	searchMaxChars = 20_000
	// Long minified lines are worth nothing to the model and cost the same as real ones.
	searchMaxColumns = 200
)

// The grep fields follow Claude Code's Grep tool, flag names included, so a bash grep's flags carry across one to one.
type SearchInput struct {
	Pattern    string `json:"pattern,omitempty"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	Type       string `json:"type,omitempty"`
	OutputMode string `json:"output_mode,omitempty"`
	IgnoreCase bool   `json:"-i,omitempty"`
	Before     int    `json:"-B,omitempty"`
	After      int    `json:"-A,omitempty"`
	Context    int    `json:"-C,omitempty"`
	OnlyMatch  bool   `json:"-o,omitempty"`
	Multiline  bool   `json:"multiline,omitempty"`
	HeadLimit  int    `json:"head_limit,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"`
}

const (
	ModeContent      = "content"
	ModeFiles        = "files_with_matches"
	ModeFilesWithout = "files_without_match"
	ModeCount        = "count"
)

const (
	unitResults = "results"
	// rg merges adjacent matches into one group with their context, so groups are what can be counted.
	unitGroups = "groups"
	// A multiline match spans lines with nothing to mark where one match ends and the next begins.
	unitLines = "lines"
)

func contentMode(in SearchInput) bool { return in.OutputMode == "" || in.OutputMode == ModeContent }

func hasContext(in SearchInput) bool {
	return contentMode(in) && (in.Context > 0 || in.Before > 0 || in.After > 0)
}

func clipUnit(name string, in SearchInput) string {
	switch {
	case name != "grep":
		return unitResults
	case hasContext(in):
		return unitGroups
	case contentMode(in) && in.Multiline:
		return unitLines
	}
	return unitResults
}

func DescribeSearch(name string, in SearchInput) string {
	var where []string
	if in.Glob != "" {
		where = append(where, in.Glob)
	}
	if in.Type != "" {
		where = append(where, in.Type+" files")
	}
	if in.Path != "" {
		where = append(where, in.Path)
	}
	out := name + " " + in.Pattern
	if len(where) > 0 {
		out += " — " + strings.Join(where, " in ")
	}
	if flags := grepFlags(in); len(flags) > 0 {
		out += " " + strings.Join(flags, " ")
	}
	switch in.OutputMode {
	case ModeFiles:
		out += " (paths only)"
	case ModeFilesWithout:
		out += " (paths without a match)"
	case ModeCount:
		out += " (counts)"
	}
	if in.Hidden {
		out += " (hidden and ignored too)"
	}
	return out
}

func grepFlags(in SearchInput) []string {
	var flags []string
	if in.IgnoreCase {
		flags = append(flags, "-i")
	}
	if in.OnlyMatch {
		flags = append(flags, "-o")
	}
	if in.Multiline {
		flags = append(flags, "multiline")
	}
	if contentMode(in) {
		for _, f := range []struct {
			name string
			n    int
		}{{"-C", in.Context}, {"-B", in.Before}, {"-A", in.After}} {
			if f.n > 0 {
				flags = append(flags, fmt.Sprintf("%s%d", f.name, f.n))
			}
		}
	}
	if in.HeadLimit > 0 {
		flags = append(flags, fmt.Sprintf("head_limit=%d", in.HeadLimit))
	}
	return flags
}

func checkGrep(in SearchInput) error {
	switch in.OutputMode {
	case "", ModeContent, ModeFiles, ModeFilesWithout, ModeCount:
	default:
		return fmt.Errorf("output_mode %q is not one of content, files_with_matches, files_without_match or count", in.OutputMode)
	}
	if in.Before < 0 || in.After < 0 || in.Context < 0 || in.HeadLimit < 0 {
		return fmt.Errorf("-A, -B, -C and head_limit take a count of zero or more")
	}
	return nil
}

func grepArgs(in SearchInput) []string {
	// Without the preview an over-long line comes back as "[Omitted long matching line]", which
	// locates the match and withholds it.
	args := []string{"--line-number", "--no-heading", fmt.Sprintf("--max-columns=%d", searchMaxColumns), "--max-columns-preview"}
	switch in.OutputMode {
	case ModeFiles:
		args = append(args, "--files-with-matches")
	case ModeFilesWithout:
		args = append(args, "--files-without-match")
	case ModeCount:
		args = append(args, "--count")
	default:
		if in.Context > 0 {
			args = append(args, fmt.Sprintf("--context=%d", in.Context))
		}
		if in.Before > 0 {
			args = append(args, fmt.Sprintf("--before-context=%d", in.Before))
		}
		if in.After > 0 {
			args = append(args, fmt.Sprintf("--after-context=%d", in.After))
		}
	}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.OnlyMatch {
		args = append(args, "--only-matching")
	}
	if in.Multiline {
		args = append(args, "--multiline", "--multiline-dotall")
	}
	if in.Type != "" {
		args = append(args, "--type="+in.Type)
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	return append(args, "--regexp", in.Pattern)
}

// A path under the working directory is handed over relative, so both tools keep printing paths
// relative to it; anywhere else is passed through as given. Joining it to the working directory
// first would bury an absolute path under it, since filepath.Join does not reset on an absolute
// element.
func searchPath(dir, path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	// The working directory itself is better expressed by omitting the argument: passing "." puts
	// a ./ prefix on every printed path.
	if rel == "." {
		return ""
	}
	return rel
}

// fd matches a glob against the file name, and against the whole path only under --full-path,
// where the path it compares is absolute — so `src/*.ts` matches nothing unless it is rooted.
// Models write both styles, so a pattern carrying a separator gets rooted and switched to
// full-path matching, and a bare one keeps matching names.
func globArgs(pattern string) []string {
	if !strings.Contains(pattern, "/") {
		return []string{"--glob", "--", pattern}
	}
	rooted := pattern
	if !strings.HasPrefix(rooted, "**/") {
		rooted = "**/" + strings.TrimPrefix(strings.TrimPrefix(rooted, "."), "/")
	}
	return []string{"--glob", "--full-path", "--", rooted}
}

func entries(stdout, unit string) [][]string {
	var all [][]string
	var group []string
	for _, line := range strings.Split(stdout, "\n") {
		switch {
		case unit == unitGroups && line == "--":
			if len(group) > 0 {
				all = append(all, group)
				group = nil
			}
		case line == "":
		case unit == unitGroups:
			group = append(group, line)
		default:
			all = append(all, []string{line})
		}
	}
	if len(group) > 0 {
		all = append(all, group)
	}
	return all
}

// Caps keep whole entries so the notice counts what is shown; fd walks in parallel, so its hits are sorted.
func clipResults(stdout string, doSort bool, unit string, headLimit int) string {
	all := entries(stdout, unit)
	if doSort {
		sort.Slice(all, func(i, j int) bool { return all[i][0] < all[j][0] })
	}
	total := len(all)
	limit := total
	if headLimit > 0 && headLimit < total {
		limit = headLimit
	}
	size := func(e []string) int {
		n := 0
		for _, line := range e {
			n += len(line) + 1
		}
		return n
	}
	kept, lines, chars := 0, 0, 0
	for kept < limit && (kept == 0 || lines+len(all[kept]) <= searchMaxLines && chars+size(all[kept]) <= searchMaxChars) {
		lines += len(all[kept])
		chars += size(all[kept])
		kept++
	}
	shown := all[:kept]
	if kept == 1 && len(shown[0]) > searchMaxLines {
		shown = [][]string{shown[0][:searchMaxLines]}
	}
	note := ""
	switch {
	case kept < limit:
		note = fmt.Sprintf("\n[%d %s, showing the first %d — narrow the search]", total, unit, kept)
	case limit < total:
		note = fmt.Sprintf("\n[%d %s, showing the first %d]", total, unit, kept)
	}
	if len(shown) == 1 && len(shown[0]) < len(all[0]) {
		note += fmt.Sprintf("\n[the first group runs past %d lines, showing its first %d]", searchMaxLines, searchMaxLines)
	}
	sep := "\n"
	if unit == unitGroups {
		sep = "\n--\n"
	}
	parts := make([]string, len(shown))
	for i, e := range shown {
		parts[i] = strings.Join(e, "\n")
	}
	out := strings.Join(parts, sep)
	if len(out) > searchMaxChars {
		out = out[:searchMaxChars] + "\n[truncated]"
	}
	return out + note
}

func RunSearch(ctx context.Context, name string, in SearchInput) (output string, isError bool) {
	dir := Dir(ctx)
	if in.Pattern == "" {
		return "No pattern given.", true
	}
	var bin string
	var args []string
	if in.Hidden {
		args = append(args, "--hidden", "--no-ignore")
	}
	if name == "glob" {
		bin = "fd"
		args = append(append(args, "--type", "f"), globArgs(in.Pattern)...)
	} else {
		if err := checkGrep(in); err != nil {
			return err.Error(), true
		}
		bin = "rg"
		args = append(append(args, grepArgs(in)...), "--")
	}
	if where := searchPath(dir, in.Path); where != "" {
		args = append(args, where)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	// stdin is left nil because rg reads it instead of the working directory when it is a pipe
	// and no path was given, which silently turns every unscoped search into a search of nothing.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if cmd.ProcessState == nil {
		return fmt.Sprintf("%s: %v", bin, err), true
	}
	code := cmd.ProcessState.ExitCode()

	// The two disagree about exit codes: fd calls nothing-found success and any complaint 1, while
	// rg reserves 1 for nothing-found and fails with 2. Reading fd's 1 as "no matches" would
	// swallow a rejected pattern silently.
	failed := code != 0
	if bin == "rg" {
		failed = code > 1
	}
	if failed {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Sprintf("%s exited %d", bin, code)
		}
		return msg, true
	}
	if out := clipResults(stdout.String(), bin == "fd", clipUnit(name, in), in.HeadLimit); out != "" {
		return out, false
	}
	if in.OutputMode == ModeFilesWithout {
		return "Every file searched has a match.", false
	}
	return "No matches.", false
}
