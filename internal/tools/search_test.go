package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/project"
)

var bg = context.Background()

// fd only compares a glob against the whole path under --full-path, and the path it compares is
// absolute — so a model writing `src/*.go` gets nothing unless the pattern is rooted first. Both
// styles are common in model output, so both have to work.
func TestGlobPatternsWithASeparatorAreRooted(t *testing.T) {
	t.Parallel()
	if got, want := globArgs("*.go"), []string{"--glob", "--", "*.go"}; !slices.Equal(got, want) {
		t.Errorf("bare pattern = %v, want %v (name matching, no --full-path)", got, want)
	}
	for _, pattern := range []string{"src/*.go", "./src/*.go", "/src/*.go"} {
		got := globArgs(pattern)
		if !slices.Contains(got, "--full-path") {
			t.Errorf("%q = %v, want --full-path", pattern, got)
		}
		if !strings.HasPrefix(got[len(got)-1], "**/") {
			t.Errorf("%q = %v, want the pattern rooted with **/", pattern, got)
		}
	}
	if got := globArgs("**/x.go"); got[len(got)-1] != "**/x.go" {
		t.Errorf("already-rooted pattern was re-rooted: %v", got)
	}
}

// The two binaries disagree about exit 1: rg means "no matches", fd means "I refused your
// pattern". Treating them the same swallows a rejected pattern as an empty result, which reads to
// the model as "there is nothing there" and is the worst possible wrong answer.
func TestExitCodeOneMeansDifferentThingsPerTool(t *testing.T) {
	t.Parallel()
	// Assembled at runtime: a literal here would be found in this very file.
	absent := strings.Join([]string{"zzz", "definitely", "absent"}, "-")
	out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: absent})
	if isErr {
		t.Errorf("rg finding nothing was reported as an error: %q", out)
	}
	if out != "No matches." {
		t.Errorf("rg empty result = %q", out)
	}

	out, isErr = RunSearch(bg, "grep", SearchInput{Pattern: "("})
	if !isErr {
		t.Errorf("rg rejecting a bad regex was reported as success: %q", out)
	}

	out, isErr = RunSearch(bg, "glob", SearchInput{Pattern: "*.go", Path: "../../does-not-exist-here"})
	if !isErr {
		t.Errorf("fd complaining was reported as success: %q", out)
	}
}

func TestGrepFlagsShapeTheOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "before\nNEEDLE one\nafter\nfar\n")
	write("b.txt", "needle two\nneedle three\n")
	write("c.py", "needle four\n")

	grep := func(in SearchInput) string {
		t.Helper()
		in.Path = dir
		out, isErr := RunSearch(bg, "grep", in)
		if isErr {
			t.Fatalf("%+v errored: %q", in, out)
		}
		return out
	}

	if out := grep(SearchInput{Pattern: "NEEDLE", Before: 1, After: 1}); !strings.Contains(out, "before") || !strings.Contains(out, "after") || strings.Contains(out, "far") {
		t.Errorf("-B1 -A1 should show one line either side and no more: %q", out)
	}
	if out := grep(SearchInput{Pattern: "NEEDLE", Context: 1}); !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("-C1 should show one line either side: %q", out)
	}
	if out := grep(SearchInput{Pattern: "needle"}); strings.Contains(out, "one") {
		t.Errorf("matching is case-sensitive without -i: %q", out)
	}
	if out := grep(SearchInput{Pattern: "needle", IgnoreCase: true}); !strings.Contains(out, "one") {
		t.Errorf("-i should match NEEDLE: %q", out)
	}
	if out := grep(SearchInput{Pattern: "needle", OutputMode: ModeCount}); !strings.Contains(out, "b.txt:2") {
		t.Errorf("count mode should report two matching lines in b.txt: %q", out)
	}
	out := grep(SearchInput{Pattern: "needle", OutputMode: ModeFiles})
	if !strings.Contains(out, "b.txt") || strings.Contains(out, "two") {
		t.Errorf("files_with_matches should list paths without lines: %q", out)
	}
	if out := grep(SearchInput{Pattern: "needle", Type: "py"}); !strings.Contains(out, "c.py") || strings.Contains(out, "b.txt") {
		t.Errorf("type py should search only Python files: %q", out)
	}
	if out := grep(SearchInput{Pattern: "needle t", OnlyMatch: true}); !strings.Contains(out, ":needle t") || strings.Contains(out, "two") {
		t.Errorf("-o should show only the matched part: %q", out)
	}
}

func TestHeadLimitClipsWithoutBlamingTheSearch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(strings.Repeat("hit\n", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: "hit", Path: dir, HeadLimit: 3})
	if isErr {
		t.Fatalf("grep errored: %q", out)
	}
	if got := strings.Count(out, "hit"); got != 3 {
		t.Errorf("head_limit 3 returned %d hits: %q", got, out)
	}
	if !strings.Contains(out, "[10 results, showing the first 3]") || strings.Contains(out, "narrow") {
		t.Errorf("a requested limit should be reported as such, not as a search to narrow: %q", out)
	}
}

// At -C3 the 200-line cap covers about 28 matches, so a notice counting lines as results misleads the model.
func TestContextIsCappedByWholeGroups(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var b strings.Builder
	for i := range 100 {
		fmt.Fprintf(&b, "hit %d\nfiller\nfiller\nfiller\nfiller\nfiller\nfiller\nfiller\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: "hit", Path: dir, Context: 3})
	if isErr {
		t.Fatalf("grep errored: %q", out)
	}
	shown := strings.Count(out, ":hit ")
	if !strings.Contains(out, fmt.Sprintf("[100 groups, showing the first %d — narrow the search]", shown)) {
		t.Errorf("the notice should count groups and match what is shown (%d): %q", shown, out[max(0, len(out)-120):])
	}
	body := out[:strings.LastIndex(out, "\n[")]
	if groups := strings.Split(body, "\n--\n"); len(groups) != shown || strings.Count(groups[len(groups)-1], "\n") != 6 {
		t.Errorf("want %d whole groups of seven lines, the last one uncut: %d groups, last %q", shown, len(groups), groups[len(groups)-1])
	}

	out, _ = RunSearch(bg, "grep", SearchInput{Pattern: "hit", Path: dir, Context: 1, HeadLimit: 2})
	if strings.Count(out, ":hit ") != 2 || !strings.Contains(out, "[100 groups, showing the first 2]") {
		t.Errorf("head_limit with context should count groups: %q", out)
	}
}

func TestMultilineAndFilesWithoutMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("guarded.go", "func A() {\n\tauthorize()\n}\n")
	write("open.go", "func B() {\n\treturn\n}\n")

	if out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: `func B\(\) \{\n\treturn`, Path: dir}); !isErr {
		t.Errorf("a pattern with \\n should fail without multiline, got %q", out)
	}
	out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: `func B\(\) \{\n\treturn`, Path: dir, Multiline: true})
	if isErr || !strings.Contains(out, "open.go:1:") || !strings.Contains(out, "open.go:2:") {
		t.Errorf("multiline should match across the two lines: isErr=%v %q", isErr, out)
	}

	out, isErr = RunSearch(bg, "grep", SearchInput{Pattern: "authorize", Path: dir, OutputMode: ModeFilesWithout})
	if isErr || !strings.Contains(out, "open.go") || strings.Contains(out, "guarded.go") {
		t.Errorf("files_without_match should list only open.go: isErr=%v %q", isErr, out)
	}
	if out, _ := RunSearch(bg, "grep", SearchInput{Pattern: "func", Path: dir, OutputMode: ModeFilesWithout}); out != "Every file searched has a match." {
		t.Errorf("no file lacking a match should say so, not report no matches: %q", out)
	}
}

func TestBadGrepArgumentsAreRefused(t *testing.T) {
	t.Parallel()
	for _, in := range []SearchInput{
		{Pattern: "x", OutputMode: "lines"},
		{Pattern: "x", After: -1},
		{Pattern: "x", HeadLimit: -5},
	} {
		if out, isErr := RunSearch(bg, "grep", in); !isErr {
			t.Errorf("%+v should be refused, got %q", in, out)
		}
	}
}

func TestEmptyPatternIsRefused(t *testing.T) {
	t.Parallel()
	if _, isErr := RunSearch(bg, "grep", SearchInput{}); !isErr {
		t.Error("an empty pattern should be refused rather than matching everything")
	}
}

// An absolute path has to reach the directory it names. Joining it to the working directory
// instead points both tools at a path that cannot exist, and the error that comes back reads as
// the tool being broken rather than as an argument it declined.
func TestAnAbsolutePathSearchesWhereItPoints(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	needle := strings.Join([]string{"absolute", "path", "needle"}, "-")
	if err := os.WriteFile(filepath.Join(outside, "found.txt"), []byte(needle+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: needle, Path: outside})
	if isErr || !strings.Contains(out, needle) {
		t.Errorf("grep in an absolute path outside the working directory: isErr=%v out=%q", isErr, out)
	}

	out, isErr = RunSearch(bg, "glob", SearchInput{Pattern: "found.txt", Path: outside})
	if isErr || !strings.Contains(out, "found.txt") {
		t.Errorf("glob in an absolute path outside the working directory: isErr=%v out=%q", isErr, out)
	}

	// Inside the working directory it is rewritten, so hits stay comparable with every other
	// search rather than arriving with a machine-specific prefix.
	out, isErr = RunSearch(bg, "glob", SearchInput{Pattern: "search.go", Path: project.Cwd()})
	if isErr || out != "search.go" {
		t.Errorf("an absolute path at the working directory should print relative: isErr=%v out=%q", isErr, out)
	}
}

func TestASearchUnderWithDirRunsThere(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "only-here.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := RunSearch(WithDir(bg, dir), "glob", SearchInput{Pattern: "only-here.txt"})
	if isErr || out != "only-here.txt" {
		t.Errorf("glob under WithDir: isErr=%v out=%q, want only-here.txt relative to the dir", isErr, out)
	}
}

// Hidden and ignored paths are skipped by default, and that empty result is indistinguishable
// from the string being absent. The escape hatch is what makes "No matches." trustworthy.
func TestHiddenAndIgnoredPathsAreReachableOnRequest(t *testing.T) {
	t.Parallel()
	// Inside the repository, because rg only applies .gitignore rules within a git repo.
	dir, err := os.MkdirTemp(project.Cwd(), "searchprobe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	needle := strings.Join([]string{"out", "of", "sight"}, "-")
	write := func(name, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "ignored.txt\n")
	write("ignored.txt", needle+"\n")
	write(".hidden/deep.txt", needle+"\n")

	if out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: needle, Path: dir}); isErr || out != "No matches." {
		t.Errorf("hidden and ignored files should be invisible by default: isErr=%v out=%q", isErr, out)
	}

	out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: needle, Path: dir, Hidden: true})
	if isErr {
		t.Fatalf("hidden search errored: %q", out)
	}
	for _, want := range []string{"ignored.txt", "deep.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("hidden search missed %s: %q", want, out)
		}
	}
	if out, isErr := RunSearch(bg, "glob", SearchInput{Pattern: "deep.txt", Path: dir, Hidden: true}); isErr || !strings.Contains(out, "deep.txt") {
		t.Errorf("hidden glob missed deep.txt: isErr=%v out=%q", isErr, out)
	}
}

// A model-supplied pattern or path read as an option would let fd --exec or rg --pre run a command unasked.
func TestALeadingDashIsNeverAnOption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ran := func(what string) {
		t.Helper()
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("%s ran a command", what)
			os.Remove(marker)
		}
	}

	RunSearch(bg, "grep", SearchInput{Pattern: "package", Path: "--pre=" + script})
	ran("a grep path of --pre=")

	RunSearch(bg, "glob", SearchInput{Pattern: "*.go", Path: "--exec=" + script})
	ran("a glob path of --exec=")

	RunSearch(bg, "grep", SearchInput{Pattern: "package", Type: "--pre=" + script})
	ran("a grep type of --pre=")
	RunSearch(bg, "grep", SearchInput{Pattern: "package", Glob: "--pre=" + script})
	ran("a grep glob of --pre=")

	// An injected -x runs on fd's own hits, so plant one under the working directory with an old mtime.
	probe, err := os.MkdirTemp(project.Cwd(), "dashprobe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(probe) })
	stale := filepath.Join(probe, "stale.txt")
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.WriteFile(stale, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	RunSearch(bg, "glob", SearchInput{Pattern: "-xtouch"})
	if info, err := os.Stat(stale); err != nil || !info.ModTime().Equal(old) {
		t.Errorf("a glob pattern of -xtouch ran a command: %v", err)
	}
}

// A match on an over-long line is worth locating and worth reading. Omitting the line entirely
// gives the model a line number and no text, so the only way to see the match is to run the
// search again in bash.
func TestAMatchOnALongLineComesBackPreviewed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prefix := strings.Join([]string{"start", "of", "line"}, "-")
	line := prefix + strings.Repeat("x", searchMaxColumns*2) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	out, isErr := RunSearch(bg, "grep", SearchInput{Pattern: prefix, Path: dir})
	if isErr {
		t.Fatalf("grep errored: %q", out)
	}
	if !strings.Contains(out, prefix) {
		t.Errorf("the matching text was withheld: %q", out)
	}
	if len(out) > searchMaxColumns*2 {
		t.Errorf("the long line came back untrimmed: %d chars", len(out))
	}
}
