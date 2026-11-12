package reviewstore

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

var patches = map[string]string{
	"src/a.go": "@@ -10,3 +10,4 @@\n keep\n+added\n keep\n keep\n",
	"big.json": "",
}

// filled gives f a non-empty problem, impact, evidence and fix.
func filled(f Finding) Finding {
	f.Problem, f.Impact, f.Evidence, f.Fix = "p", "i", "e", "f"
	return f
}

func TestValidateRefusesEachBadFindingAndNamesIt(t *testing.T) {
	blankImpact := filled(Finding{Severity: "blocking", Path: "src/a.go", StartLine: 11})
	blankImpact.Impact = "  "
	r := Review{Verdict: "lgtm", Findings: []Finding{
		filled(Finding{Severity: "blocking", Path: "src/a.go", StartLine: 11}),
		filled(Finding{Severity: "major", Path: "src/a.go"}),
		filled(Finding{Severity: "blocking", Path: "a.go", StartLine: 11}),
		filled(Finding{Severity: "blocking", Path: "src/a.go", StartLine: 12, EndLine: 11}),
		filled(Finding{Severity: "blocking", Path: "src/a.go", EndLine: 11}),
		blankImpact,
		{Severity: "blocking", Path: "src/a.go", StartLine: 11, Text: "a body from before the four fields"},
	}}
	problems := Validate(&r, patches)
	want := []string{"verdict \"lgtm\"", "finding 1", "finding 2", "finding 3", "finding 4", "finding 5 (src/a.go): impact empty",
		"finding 6 (src/a.go): problem, impact, evidence, fix empty"}
	if len(problems) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(problems), len(want), strings.Join(problems, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(problems[i], w) {
			t.Errorf("problem %d = %q, want it to start %q", i, problems[i], w)
		}
	}
}

func TestValidateMarksRangesOutsideTheDiffWithoutRefusingThem(t *testing.T) {
	r := Review{Verdict: "request changes", Findings: []Finding{
		filled(Finding{Severity: "blocking", Path: "src/a.go", StartLine: 13}),
		filled(Finding{Severity: "blocking", Path: "src/a.go", StartLine: 1, EndLine: 10}),
		filled(Finding{Severity: "non-blocking", Path: "src/a.go", StartLine: 50, EndLine: 60}),
		filled(Finding{Severity: "non-blocking", Path: "big.json", StartLine: 3}),
		filled(Finding{Severity: "non-blocking", Path: "big.json"}),
	}}
	if problems := Validate(&r, patches); len(problems) != 0 {
		t.Fatalf("want no problems, got %q", problems)
	}
	var off []bool
	for _, f := range r.Findings {
		off = append(off, f.OffDiff)
	}
	if want := []bool{false, false, true, true, false}; !reflect.DeepEqual(off, want) {
		t.Errorf("off-diff = %v, want %v", off, want)
	}
	if r.Findings[0].EndLine != 13 {
		t.Errorf("a one-line finding should end where it starts, ended %d", r.Findings[0].EndLine)
	}
}

func TestSaveReplacesAndLoadReadsBack(t *testing.T) {
	home := t.TempDir()
	if _, ok, err := Load(home, "Acme/Web", 7); ok || err != nil {
		t.Fatalf("an empty store should have no review, got ok=%v err=%v", ok, err)
	}
	for _, head := range []string{"aaa", "bbb"} {
		if err := Save(home, Review{Repo: "Acme/Web", Number: 7, Head: head, Verdict: "approve"}); err != nil {
			t.Fatal(err)
		}
	}
	r, ok, err := Load(home, "acme/web", 7)
	if err != nil || !ok || r.Head != "bbb" {
		t.Fatalf("want the second save back, got %+v ok=%v err=%v", r, ok, err)
	}
}

func TestAppendDismissalAddsOneParseableLinePerDismissalWithItsThread(t *testing.T) {
	home := t.TempDir()
	for _, text := range []string{"first", "second"} {
		d := Dismissal{Repo: "acme/web", Number: 7, Head: "aaa", Finding: Finding{Path: "a.go", Text: text,
			Thread: []Message{{Role: "user", Text: "why " + text}, {Role: "assistant", Text: "because"}}}}
		if err := AppendDismissal(home, d); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(DismissedLogPath(home))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), data)
	}
	for i, text := range []string{"first", "second"} {
		var d Dismissal
		if err := json.Unmarshal([]byte(lines[i]), &d); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if d.Finding.Text != text || len(d.Finding.Thread) != 2 || d.Finding.Thread[0].Text != "why "+text {
			t.Errorf("line %d = %+v, want finding %q with its thread", i, d, text)
		}
	}
}

func TestPathRefusesARepoThatEscapes(t *testing.T) {
	for _, repo := range []string{"acme", "../x/y", "acme/../../etc", "acme/a/b"} {
		if _, err := Path("/h", repo, 1); err == nil {
			t.Errorf("Path accepted %q", repo)
		}
	}
}
