package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The insert_text/new_str mix-up is the one bug in this tool that no type checker can catch: the
// input arrives as JSON, str_replace uses new_str, and guessing new_str for insert writes an empty
// line and reports success. The empty-text guard is what turns that into a visible failure.
func TestInsertRefusesEmptyTextRatherThanWritingABlankLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "a.txt", "one\ntwo\n")
	line := 1

	res := RunEdit(EditInput{Command: "insert", Path: path, InsertLine: &line, NewStr: "wrong field"})
	if !res.IsError {
		t.Fatalf("insert with no insert_text succeeded: %+v", res)
	}
	after, _ := os.ReadFile(path)
	if string(after) != "one\ntwo\n" {
		t.Errorf("file was modified by a failed insert: %q", after)
	}
}

func TestInsertPlacesTextAtTheLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "a.txt", "one\nthree\n")
	line := 1

	res := RunEdit(EditInput{Command: "insert", Path: path, InsertLine: &line, InsertText: "two"})
	if res.IsError {
		t.Fatalf("insert failed: %s", res.Output)
	}
	after, _ := os.ReadFile(path)
	if string(after) != "one\ntwo\nthree\n" {
		t.Errorf("file = %q", after)
	}
}

// A non-unique match must fail rather than pick one. This is the protection that stands in for
// read-before-edit staleness tracking, which is unenforceable here because bash can change files
// behind the tool's back.
func TestReplaceRefusesAmbiguousAndAbsentMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "a.txt", "x\nx\n")

	res := RunEdit(EditInput{Command: "str_replace", Path: path, OldStr: "x", NewStr: "y"})
	if !res.IsError || !strings.Contains(res.Output, "more than once") {
		t.Errorf("ambiguous replace = %+v, want a uniqueness error", res)
	}
	if after, _ := os.ReadFile(path); string(after) != "x\nx\n" {
		t.Errorf("file was modified by a refused replace: %q", after)
	}

	res = RunEdit(EditInput{Command: "str_replace", Path: path, OldStr: "nope", NewStr: "y"})
	if !res.IsError || !strings.Contains(res.Output, "not found") {
		t.Errorf("absent replace = %+v, want a not-found error", res)
	}
}

// The two sides differ on purpose: the reader wants the diff, and the model wants the file as it
// now reads, so that "did my edit land where I meant it to" needs no second view call.
func TestWriteGivesTheModelTheNewStateAndTheReaderTheDiff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")

	res := RunEdit(EditInput{Command: "str_replace", Path: path, OldStr: "three", NewStr: "THREE"})
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "3\tTHREE") {
		t.Errorf("model output does not carry the changed line in place: %q", res.Output)
	}
	if !strings.Contains(res.Output, "1\tone") || !strings.Contains(res.Output, "4\tfour") {
		t.Errorf("model output does not carry the surrounding lines: %q", res.Output)
	}
	if !strings.Contains(res.Display, "- three") || !strings.Contains(res.Display, "+ THREE") {
		t.Errorf("transcript body does not show the change: %q", res.Display)
	}
}

// The prompt has to be able to show the change before it is made, from the input alone: reading
// the file to build a diff would race the edit it is describing.
func TestPreviewShowsTheChangeBeforeItIsMade(t *testing.T) {
	t.Parallel()
	got := PreviewEdit(EditInput{Command: "str_replace", Path: "a.txt", OldStr: "old line", NewStr: "new line"})
	if !strings.Contains(got, "- old line") || !strings.Contains(got, "+ new line") {
		t.Errorf("str_replace preview = %q", got)
	}
	if got := PreviewEdit(EditInput{Command: "create", Path: "a.txt", FileText: "hello\n"}); got != "+ hello" {
		t.Errorf("create preview = %q", got)
	}
	if got := PreviewEdit(EditInput{Command: "view", Path: "a.txt"}); got != "" {
		t.Errorf("view has no change to preview, got %q", got)
	}
}

// The label is how the path reaches the renderer, which needs the extension to pick a lexer, so
// the two functions have to agree about the format.
func TestEditPathReadsBackWhatDescribeEditWrote(t *testing.T) {
	t.Parallel()
	line := 12
	for _, in := range []EditInput{
		{Command: "str_replace", Path: "a/b.go"},
		{Command: "create", Path: "a/b.go"},
		{Command: "insert", Path: "a/b.go", InsertLine: &line},
		{Command: "view", Path: "a/b.go", ViewRange: []int{1, 20}},
	} {
		if got := EditPath(DescribeEdit(in)); got != "a/b.go" {
			t.Errorf("%s: EditPath(%q) = %q", in.Command, DescribeEdit(in), got)
		}
	}
	if got := EditPath("$ rm -rf /"); got != "" {
		t.Errorf("a shell command is not an edit, got %q", got)
	}
}

// A file ending in a newline does not have a last empty line. Reporting one sends the model an
// insert_line and a view_range that the file does not have.
func TestViewDoesNotCountTheTrailingNewlineAsALine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "a.txt", "one\ntwo\nthree\n")

	res := RunEdit(EditInput{Command: "view", Path: path})
	if res.IsError {
		t.Fatal(res.Output)
	}
	if lines := strings.Count(res.Output, "\n") + 1; lines != 3 {
		t.Errorf("view of a 3-line file returned %d lines: %q", lines, res.Output)
	}
}

// Insert has to accept exactly the positions view reports, or appending to a file means guessing
// which end of the off-by-one the tool is on.
func TestInsertRangeMatchesTheLineCountViewReports(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "a.txt", "one\ntwo\n")
	end, past := 2, 3

	if res := RunEdit(EditInput{Command: "insert", Path: path, InsertLine: &end, InsertText: "three"}); res.IsError {
		t.Fatalf("insert at the last line failed: %s", res.Output)
	}
	if after, _ := os.ReadFile(path); string(after) != "one\ntwo\nthree\n" {
		t.Errorf("file = %q", after)
	}
	if res := RunEdit(EditInput{Command: "insert", Path: path, InsertLine: &past, InsertText: "x"}); res.IsError {
		t.Fatalf("insert at the new last line failed: %s", res.Output)
	}
	past = 5
	if res := RunEdit(EditInput{Command: "insert", Path: path, InsertLine: &past, InsertText: "x"}); !res.IsError {
		t.Error("insert past the end of the file succeeded")
	}
}

// A clipped view has to say where to resume, and lines have to survive whole: half a line cannot
// be used to build an old_str.
func TestClippedViewNamesTheResumePointAndKeepsLinesWhole(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	line := strings.Repeat("x", 500)
	var content strings.Builder
	for range 200 {
		content.WriteString(line + "\n")
	}
	path := write(t, dir, "big.txt", content.String())

	res := RunEdit(EditInput{Command: "view", Path: path})
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "of 200") {
		t.Errorf("clipped view does not say how much it held back: %q", tail(res.Output))
	}
	if !strings.Contains(res.Output, "view_range") {
		t.Errorf("clipped view does not say how to continue: %q", tail(res.Output))
	}
	for _, out := range strings.Split(res.Output, "\n") {
		if num, text, ok := strings.Cut(out, "\t"); ok && strings.TrimSpace(num) != "" && text != line {
			t.Fatalf("line %s came back cut: %d chars", strings.TrimSpace(num), len(text))
		}
	}
}

// A minified file is one enormous line, and returning it whole to keep it matchable would cost
// the context window instead.
func TestAnOverlongLineComesBackCutAndSaysSo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "bundle.js", strings.Repeat("y", 90_000)+"\n")

	res := RunEdit(EditInput{Command: "view", Path: path})
	if res.IsError {
		t.Fatal(res.Output)
	}
	if len(res.Output) > editMaxChars+200 {
		t.Errorf("view of a one-line file returned %d characters", len(res.Output))
	}
	if !strings.Contains(res.Output, "came back cut") {
		t.Errorf("view does not say the line was cut: %q", tail(res.Output))
	}
}

// The tool's own description promises two levels, so one level sends the caller to bash for it.
func TestDirectoryViewReachesTwoLevelsAndSkipsNoise(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, p := range []string{"sub/deeper", "node_modules/pkg", ".git"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, dir, "a.txt", "")
	write(t, dir, "sub/y.txt", "")
	write(t, dir, "sub/deeper/x.txt", "")

	res := RunEdit(EditInput{Command: "view", Path: dir})
	if res.IsError {
		t.Fatal(res.Output)
	}
	for _, want := range []string{"a.txt", "sub/", "sub/y.txt", "sub/deeper/"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("listing is missing %q:\n%s", want, res.Output)
		}
	}
	for _, unwanted := range []string{"sub/deeper/x.txt", "node_modules", ".git"} {
		if strings.Contains(res.Output, unwanted) {
			t.Errorf("listing reaches %q:\n%s", unwanted, res.Output)
		}
	}
}

func tail(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestCreateMakesParentDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "a.txt")

	if res := RunEdit(EditInput{Command: "create", Path: path, FileText: "hi\n"}); res.IsError {
		t.Fatal(res.Output)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "hi\n" {
		t.Errorf("file = %q, err = %v", got, err)
	}
}

func TestCreateThroughASymlinkNeverWritesItsTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	existing := write(t, dir, "existing.txt", "kept\n")
	missing := filepath.Join(dir, "missing.txt")
	for target, link := range map[string]string{existing: "to-existing", missing: "to-missing"} {
		path := filepath.Join(dir, link)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if res := RunEdit(EditInput{Command: "create", Path: path, FileText: "written\n"}); !res.IsError {
			t.Errorf("a create through %s succeeded: %s", link, res.Output)
		}
	}
	if got, _ := os.ReadFile(existing); string(got) != "kept\n" {
		t.Errorf("the existing target now holds %q", got)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("the missing target was created: %v", err)
	}
}

func TestViewRangeIsBoundsChecked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "a.txt", "1\n2\n3\n")

	if res := RunEdit(EditInput{Command: "view", Path: path, ViewRange: []int{9, 10}}); !res.IsError {
		t.Errorf("out-of-range start succeeded: %+v", res)
	}
	if res := RunEdit(EditInput{Command: "view", Path: path, ViewRange: []int{3, 1}}); !res.IsError {
		t.Errorf("reversed range succeeded: %+v", res)
	}
	res := RunEdit(EditInput{Command: "view", Path: path, ViewRange: []int{2, -1}})
	if res.IsError || !strings.Contains(res.Output, "2\t2") {
		t.Errorf("open-ended range = %+v", res)
	}
}

// Several coders run at once now, so two edits to one file is ordinary rather than exotic. Each
// one reads the whole file, changes it in memory and writes it back, so without a per-file lock
// one of them is lost — and lost silently, with both callers told they succeeded.
//
// Many trials because the window is a few microseconds wide: one pass proves nothing, and the
// unlocked version failed 164 of 200 when this was measured.
func TestConcurrentEditsToOneFileDoNotLoseEachOther(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for trial := range 200 {
		path := write(t, dir, fmt.Sprintf("a%d.go", trial), "ALPHA\nfiller\nBETA\n")

		var wg sync.WaitGroup
		results := make([]EditResult, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			results[0] = RunEdit(EditInput{Command: "str_replace", Path: path, OldStr: "ALPHA", NewStr: "ALPHA-EDITED"})
		}()
		go func() {
			defer wg.Done()
			results[1] = RunEdit(EditInput{Command: "str_replace", Path: path, OldStr: "BETA", NewStr: "BETA-EDITED"})
		}()
		wg.Wait()

		// Neither edit touches the other's anchor, so both must succeed and both must survive.
		// A failure here would be wrong too, not merely safe: it would mean the lock turned
		// independent edits into a conflict.
		for i, res := range results {
			if res.IsError {
				t.Fatalf("trial %d: edit %d failed on an anchor nothing else touched: %s", trial, i, res.Output)
			}
		}
		final, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(final); !strings.Contains(got, "ALPHA-EDITED") || !strings.Contains(got, "BETA-EDITED") {
			t.Fatalf("trial %d: an edit was reported as written and is not in the file:\n%s", trial, got)
		}
	}
}

// The other half of the bargain: when two edits really do collide, the loser has to say so. This
// is what makes multiple coders safe rather than merely fast — str_replace matches on content,
// so an anchor another run has already consumed cannot be found and the call errors instead of
// overwriting work it never saw.
func TestTwoEditsToTheSameTextCannotBothSucceed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	collisions := 0
	for trial := range 100 {
		path := write(t, dir, fmt.Sprintf("b%d.go", trial), "TARGET\n")

		var wg sync.WaitGroup
		results := make([]EditResult, 2)
		wg.Add(2)
		for i, replacement := range []string{"FIRST", "SECOND"} {
			go func() {
				defer wg.Done()
				results[i] = RunEdit(EditInput{Command: "str_replace", Path: path, OldStr: "TARGET", NewStr: replacement})
			}()
		}
		wg.Wait()

		if !results[0].IsError && !results[1].IsError {
			t.Fatalf("trial %d: both edits claimed the same text and both reported success", trial)
		}
		collisions++
	}
	if collisions != 100 {
		t.Errorf("collisions = %d, want every trial to have one", collisions)
	}
}
