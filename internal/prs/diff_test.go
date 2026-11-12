package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func fakeREST(t *testing.T, files []map[string]any) (REST, *[]string) {
	var asked []string
	return func(_ context.Context, path string) ([]byte, error) {
		asked = append(asked, path)
		if path == "repos/acme/web/pulls/42" {
			return []byte(`{"title": "Add the thing", "changed_files": ` + fmt.Sprint(len(files)) + `, "head": {"sha": "0123456789abcdef"}}`), nil
		}
		var page int
		if _, err := fmt.Sscanf(path, "repos/acme/web/pulls/42/files?per_page=100&page=%d", &page); err != nil {
			t.Fatalf("unexpected path %q", path)
		}
		lo, hi := min((page-1)*100, len(files)), min(page*100, len(files))
		return json.Marshal(files[lo:hi])
	}, &asked
}

func TestDiffSavesTheWholePRAndInlinesWhatFits(t *testing.T) {
	files := []map[string]any{
		{"filename": "a.go", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1 +1 @@\n-old\n+new"},
		{"filename": "big.lock", "status": "modified", "additions": 400, "deletions": 0, "patch": "@@ -0,0 +1,400 @@\n" + strings.Repeat("+x\n", 400)},
		{"filename": "logo.png", "status": "added", "additions": 0, "deletions": 0},
		{"filename": "pkg/new.go", "previous_filename": "pkg/old.go", "status": "renamed", "additions": 1, "deletions": 0, "patch": "@@ -1 +1,2 @@\n x\n+```"},
	}
	rest, _ := fakeREST(t, files)
	d, err := FetchDiff(context.Background(), rest, "acme", "web", 42)
	if err != nil {
		t.Fatal(err)
	}
	path, ranges, err := WriteDiff(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "acme__web/42-0123456789ab.diff") {
		t.Errorf("path = %s", path)
	}
	saved, _ := os.ReadFile(path)
	lines := strings.Split(string(saved), "\n")
	for file, header := range map[string]string{"a.go": "diff --git a/a.go b/a.go", "pkg/new.go": "diff --git a/pkg/old.go b/pkg/new.go"} {
		r := ranges[file]
		if lines[r[0]-1] != header {
			t.Errorf("%s starts at line %d with %q, want %q", file, r[0], lines[r[0]-1], header)
		}
		if next := lines[r[1]]; next != "" && !strings.HasPrefix(next, "diff --git") {
			t.Errorf("%s ends at line %d, but the next line is %q", file, r[1], next)
		}
	}
	if _, ok := ranges["logo.png"]; ok {
		t.Error("a file with no patch was given lines")
	}

	out := RenderDiff(d, path, ranges, nil, 1000)
	for _, want := range []string{
		"head 0123456789ab · 4 files · +402/−1",
		"- A `logo.png` +0/−0 · no patch from GitHub: binary or too large",
		"- R `pkg/old.go` → `pkg/new.go`",
		"Not shown below, past the 1k-character cap: `big.lock`.",
		"+new",
		"\n````diff\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "+x\n") {
		t.Error("the file past the cap was inlined anyway")
	}

	out = RenderDiff(d, path, ranges, []string{"pkg/", "nope.go"}, 1000)
	if strings.Contains(out, "+new") || !strings.Contains(out, "+```") {
		t.Errorf("paths did not narrow what was inlined:\n%s", out)
	}
	if !strings.Contains(out, "Not in this PR: nope.go") {
		t.Errorf("an unknown path went unreported:\n%s", out)
	}
}

func TestDiffReadsEveryPageOfFiles(t *testing.T) {
	var files []map[string]any
	for i := range 101 {
		files = append(files, map[string]any{"filename": fmt.Sprintf("f%03d.go", i), "status": "modified", "patch": "@@ -1 +1 @@\n-a\n+b"})
	}
	rest, asked := fakeREST(t, files)
	d, err := FetchDiff(context.Background(), rest, "acme", "web", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 101 || len(*asked) != 3 {
		t.Errorf("got %d files in %d requests, want 101 in 3", len(d.Files), len(*asked))
	}
}

const sinceSHA = "1111111111111111"

func compareREST(t *testing.T, status string, files []map[string]any, fail error) (REST, *[]string) {
	var asked []string
	return func(_ context.Context, path string) ([]byte, error) {
		asked = append(asked, path)
		if path == "repos/acme/web/pulls/42" {
			return []byte(`{"title": "Add the thing", "head": {"sha": "0123456789abcdef"}}`), nil
		}
		if path != "repos/acme/web/compare/"+sinceSHA+"...0123456789abcdef" {
			t.Fatalf("unexpected path %q", path)
		}
		if fail != nil {
			return nil, fail
		}
		return json.Marshal(map[string]any{"status": status, "files": files})
	}, &asked
}

func diffPoller(rest REST, dir string) *Poller {
	return &Poller{conf: Config{Repos: []Repo{{Owner: "acme", Name: "web"}}}, rest: rest, diffDir: dir}
}

func TestDiffSinceRendersOnlyWhatChangedAndSaysSo(t *testing.T) {
	rest, _ := compareREST(t, "ahead", []map[string]any{
		{"filename": "a.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1,2 @@\n x\n+later"},
	}, nil)
	dir := t.TempDir()
	out, err := diffPoller(rest, dir).Diff(context.Background(), "acme/web", 42, nil, sinceSHA)
	if err != nil {
		t.Fatal(err)
	}
	whole := regexp.MustCompile(`Whole diff: (\S+)`).FindStringSubmatch(out)
	if whole == nil || !strings.HasSuffix(whole[1], "acme__web/42-111111111111..0123456789ab.diff") {
		t.Fatalf("whole diff line = %v in:\n%s", whole, out)
	}
	if saved, _ := os.ReadFile(whole[1]); !strings.Contains(string(saved), "+later") {
		t.Errorf("saved file = %q", saved)
	}
	for _, want := range []string{"changes since 111111111111 · head 0123456789ab · 1 files · +1/−0", "+later"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDiffSinceAtGitHubsFileCapIsNoIncrementalDiff(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{299, "ahead"}, {300, CompareTooLarge}} {
		var files []map[string]any
		for i := range c.n {
			files = append(files, map[string]any{"filename": fmt.Sprintf("f%03d.go", i), "status": "modified", "patch": "@@ -1 +1 @@\n-a\n+b"})
		}
		rest, asked := compareREST(t, "ahead", files, nil)
		d, status, err := FetchSince(context.Background(), rest, "acme", "web", 42, sinceSHA)
		if err != nil || status != c.want || len(*asked) != 2 {
			t.Errorf("%d files: status %q, err %v in %d requests, want %q in 2", c.n, status, err, len(*asked), c.want)
		}
		if c.want == CompareTooLarge && len(d.Files) != 0 {
			t.Errorf("%d files: kept %d files from a list that may be cut short", c.n, len(d.Files))
		}
	}
}

func TestDiffSinceRefusesWhenThereIsNoIncrementalDiff(t *testing.T) {
	stored := "the stored head 111111111111 is not an ancestor of the PR head 0123456789ab, likely a rebase or force-push"
	for _, c := range []struct {
		name, status string
		fail         error
		want         string
	}{
		{"diverged", "diverged", nil, "No incremental diff: " + stored},
		{"behind", "behind", nil, "No incremental diff: " + stored},
		{"identical", "identical", nil, "No incremental diff: nothing changed since 111111111111."},
		{"old commit gone", "", errors.New("gh: No commit found for SHA (HTTP 404)"), "No incremental diff: GitHub cannot compare the stored head 111111111111"},
		{"unprocessable", "", errors.New("gh: Unprocessable Entity (HTTP 422)"), "No incremental diff: GitHub cannot compare the stored head 111111111111"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rest, _ := compareREST(t, c.status, []map[string]any{{"filename": "a.go", "patch": "@@ -1 +1 @@\n-a\n+b"}}, c.fail)
			dir := t.TempDir()
			out, err := diffPoller(rest, dir).Diff(context.Background(), "acme/web", 42, nil, sinceSHA)
			if err != nil || !strings.HasPrefix(out, c.want) {
				t.Fatalf("got %q, %v; want prefix %q", out, err, c.want)
			}
			if c.status != "identical" && !strings.Contains(out, "Review the whole PR diff instead") {
				t.Errorf("no pointer to the whole diff: %q", out)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a diff was saved: %v", entries)
			}
		})
	}
}

func TestDiffSinceOtherFailuresAreErrorsNotAWholeDiffHint(t *testing.T) {
	rest, _ := compareREST(t, "", nil, errors.New("gh: HTTP 502"))
	if out, err := diffPoller(rest, t.TempDir()).Diff(context.Background(), "acme/web", 42, nil, sinceSHA); err == nil {
		t.Errorf("a server error read as %q", out)
	}
	if _, err := diffPoller(rest, t.TempDir()).Diff(context.Background(), "acme/web", 42, nil, "../pulls"); err == nil {
		t.Error("a since that is not a commit SHA was sent to GitHub")
	}
}
