package reviewgraph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const semDiff = `{"summary":{"total":6},"changes":[
{"entityName":"Parse","entityType":"function","filePath":"a.go","changeType":"modified"},
{"entityName":"Parse","entityType":"function","filePath":"a.go","changeType":"moved"},
{"entityName":"id","entityType":"property","filePath":"a.go","changeType":"added"},
{"entityName":"module-level","entityType":"orphan","filePath":"a.go","changeType":"modified"},
{"entityName":"keeps an empty sentence","entityType":"test","filePath":"a_test.go","changeType":"added"},
{"entityName":"module-level","entityType":"orphan","filePath":"q.sql","changeType":"added"},
{"entityName":"Other","entityType":"function","filePath":"elsewhere.go","changeType":"added"}
],"binaryChanges":[]}`

func TestEntitiesFollowsNamedCodeAndCountsOrphanOnlyFilesAsUnexamined(t *testing.T) {
	changed := []File{{Path: "a.go", Changed: true, Status: "M"}, {Path: "a_test.go", Changed: true, Status: "A"}, {Path: "q.sql", Changed: true, Status: "A"}}
	files, follow, unexamined, err := entities(changed, []byte(semDiff))
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []File{
		{Path: "a.go", Changed: true, Status: "M", Entities: []string{"Parse", "id"}},
		{Path: "a_test.go", Changed: true, Status: "A", Entities: []string{"keeps an empty sentence"}},
		{Path: "q.sql", Changed: true, Status: "A"},
	}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Errorf("files:\n got %+v\nwant %+v", files, wantFiles)
	}
	if want := map[string][]string{"Parse": {"a.go"}}; !reflect.DeepEqual(follow, want) {
		t.Errorf("follow = %v, want %v: properties, tests and names outside the diff are not followed", follow, want)
	}
	if want := []string{"q.sql"}; !reflect.DeepEqual(unexamined, want) {
		t.Errorf("unexamined = %q, want %q", unexamined, want)
	}
}

func rgMatch(path string, names ...string) string {
	var subs []string
	for _, n := range names {
		subs = append(subs, fmt.Sprintf(`{"match":{"text":%q},"start":0,"end":%d}`, n, len(n)))
	}
	return fmt.Sprintf(`{"type":"match","data":{"path":{"text":%q},"lines":{"text":"x\n"},"line_number":1,"submatches":[%s]}}`, path, strings.Join(subs, ","))
}

func TestImpactsDrawsAnEdgeToEveryOtherFileMentioningAName(t *testing.T) {
	changed := []File{{Path: "a.go", Changed: true, Status: "M"}, {Path: "b.go", Changed: true, Status: "M"}}
	follow := map[string][]string{"Parse": {"a.go"}, "Lex": {"a.go"}, "Load": {"b.go"}, "Config": {"b.go"}}
	lines := []string{
		`{"type":"begin","data":{"path":{"text":"./a.go"}}}`,
		rgMatch("./a.go", "Parse", "Load"),
		rgMatch("./b.go", "Parse"),
		rgMatch("./z.go", "Lex", "Parse"),
		rgMatch("./z.go", "Lex"),
	}
	for i := range maxFiles + 1 {
		lines = append(lines, rgMatch(fmt.Sprintf("./many/%d.go", i), "Config"))
	}
	files, edges, tooCommon, err := impacts(changed, follow, []byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if want := append(changed, File{Path: "z.go"}); !reflect.DeepEqual(files, want) {
		t.Errorf("files:\n got %+v\nwant %+v", files, want)
	}
	wantEdges := []Edge{
		{From: "a.go", To: "b.go", Via: []string{"Parse"}},
		{From: "a.go", To: "z.go", Via: []string{"Lex", "Parse"}},
		{From: "b.go", To: "a.go", Via: []string{"Load"}},
	}
	if !reflect.DeepEqual(edges, wantEdges) {
		t.Errorf("edges:\n got %+v\nwant %+v", edges, wantEdges)
	}
	if want := []string{"Config"}; !reflect.DeepEqual(tooCommon, want) {
		t.Errorf("tooCommon = %q, want %q", tooCommon, want)
	}
}

func TestImpactsRejectsMalformedRgLine(t *testing.T) {
	if _, _, _, err := impacts(nil, nil, []byte("{\"type\":")); err == nil {
		t.Fatal("want an error for a line that is not a JSON object")
	}
}

func TestParseNameStatusTakesNewPathOfRenames(t *testing.T) {
	got := parseNameStatus("M\tkeep.go\nR087\told.go\tnew.go\nA\tadd.go\nD\tgone.go")
	want := []File{
		{Path: "keep.go", Changed: true, Status: "M"},
		{Path: "new.go", Changed: true, Status: "R"},
		{Path: "add.go", Changed: true, Status: "A"},
		{Path: "gone.go", Changed: true, Status: "D"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestOriginMatches(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"git@github.com:acme/app.git", true},
		{"git@github.com:acme/app", true},
		{"https://github.com/acme/app.git", true},
		{"https://github.com/Acme/App/", true},
		{"ssh://git@github.com/acme/app.git", true},
		{"https://github.com/acme/app-web.git", false},
		{"git@github.com:other/app.git", false},
		{"https://gitlab.com/acme/app.git", false},
	} {
		if got := originMatches(tc.url, "acme", "app"); got != tc.want {
			t.Errorf("originMatches(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestReadSourcesKeepsTextAtHeadAndSkipsDeletedMissingAndBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		out, err := run(context.Background(), dir, []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}, "git", args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git("init", "-q")
	for name, body := range map[string]string{"a.go": "line1\nline2\n", "empty.go": "", "bin.dat": "x\x00y"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "c")
	head := git("rev-parse", "HEAD")
	files := []File{
		{Path: "a.go", Changed: true, Status: "M"},
		{Path: "gone.go", Changed: true, Status: "D"},
		{Path: "missing.go", Changed: true, Status: "A"},
		{Path: "bin.dat", Changed: true, Status: "A"},
		{Path: "empty.go", Changed: true, Status: "A"},
		{Path: "impacted.go"},
	}
	got, err := readSources(context.Background(), dir, head, files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a.go": "line1\nline2\n", "empty.go": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
