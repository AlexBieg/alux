package mine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func turn(session string, conv int, skill string, calls ...string) Turn {
	return Turn{Session: session, Conversation: conv, Skill: skill, Calls: calls}
}

func find(fams []Family, pattern ...string) *Family {
	for i := range fams {
		if slices.Equal(fams[i].Pattern, pattern) {
			return &fams[i]
		}
	}
	return nil
}

func cohesiveAndChain() []Turn {
	var turns []Turn
	for i := range 5 {
		turns = append(turns, turn("rare"+strconv.Itoa(i), 0, "", "alpha", "beta", "gamma"))
	}
	for i := range 40 {
		turns = append(turns, turn("common"+strconv.Itoa(i), 0, "", "go_build", "go_test", "jj_commit"))
	}
	return turns
}

func TestACohesivePatternOutranksACommonChain(t *testing.T) {
	res := Mine(cohesiveAndChain())
	if len(res.Skills) < 2 {
		t.Fatalf("want both planted patterns, got %+v", res.Skills)
	}
	if got := res.Skills[0].Pattern; !slices.Equal(got, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("top skill candidate = %v, want the cohesive pattern; all: %+v", got, res.Skills)
	}
	if find(res.Skills, "go_build", "go_test", "jj_commit") == nil {
		t.Fatalf("common chain missing from skills: %+v", res.Skills)
	}
}

func TestALoopCollapsesToOneSymbolWhateverRotationItStartsOn(t *testing.T) {
	var turns []Turn
	for i := range 3 {
		turns = append(turns, turn("abc"+strconv.Itoa(i), 0, "", "a", "b", "c", "a", "b", "c", "a"))
		turns = append(turns, turn("bca"+strconv.Itoa(i), 0, "", "b", "c", "a", "b", "c", "a", "b"))
	}
	res := Mine(turns)
	f := find(res.Skills, "loop{a,b,c}")
	if f == nil || f.Support != 6 {
		t.Fatalf("want loop{a,b,c} in all 6 conversations, got %+v", res.Skills)
	}
}

func TestAPatternHoldingAFamilysTopPatternInOrderIsListedUnderIt(t *testing.T) {
	var turns []Turn
	for i := range 30 {
		turns = append(turns, turn("core"+strconv.Itoa(i), 0, "", "go_build", "go_vet", "go_test"))
	}
	for i := range 6 {
		turns = append(turns, turn("commit"+strconv.Itoa(i), 0, "", "go_build", "go_vet", "go_test", "jj_commit"))
		turns = append(turns, turn("fmt"+strconv.Itoa(i), 0, "", "go_build", "go_vet", "gofmt", "go_test"))
	}
	for i := range 60 {
		turns = append(turns, turn("other"+strconv.Itoa(i), 0, "", "noop"+strconv.Itoa(i)))
	}
	res := Mine(turns)
	if len(res.Skills) != 1 {
		t.Fatalf("want one family, got %+v", res.Skills)
	}
	f := res.Skills[0]
	want := [][]string{{"go_build", "go_vet", "go_test", "jj_commit"}, {"go_build", "go_vet", "gofmt", "go_test"}}
	if !slices.Equal(f.Pattern, []string{"go_build", "go_vet", "go_test"}) || len(f.Extensions) != 2 {
		t.Fatalf("want the core with two extensions, got %+v", f)
	}
	for _, w := range want {
		if find(f.Extensions, w...) == nil {
			t.Errorf("extension %v missing from %+v", w, f.Extensions)
		}
	}
}

func TestAnExtensionIsIndentedToItsEntrysContentColumn(t *testing.T) {
	var fams []Family
	for i := range 10 {
		fams = append(fams, Family{Pattern: []string{"p" + strconv.Itoa(i)}, Extensions: []Family{{Pattern: []string{"e" + strconv.Itoa(i)}}}})
	}
	out := string(render(Result{Skills: fams}, time.Now()))
	for _, want := range []string{"\n1. p0 ", "\n   - e0 ", "\n10. p9 ", "\n    - e9 "} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}

func TestAPatternRunUnderOneSkillIsCovered(t *testing.T) {
	var turns []Turn
	for i := range 5 {
		s := "review" + strconv.Itoa(i)
		turns = append(turns, turn(s, 0, "pr-review", "pr_detail", "pr_diff"))
		turns = append(turns, turn(s, 0, "", "task", "vault_search"))
	}
	for i := range 3 {
		turns = append(turns, turn("other"+strconv.Itoa(i), 0, "", "go_build", "go_test"))
	}
	res := Mine(turns)
	pattern := []string{"pr_detail", "pr_diff", "task", "vault_search"}
	f := find(res.Covered, pattern...)
	if f == nil || f.Skill != "pr-review" {
		t.Fatalf("want %v covered by pr-review, got %+v", pattern, res.Covered)
	}
	if find(res.Skills, pattern...) != nil {
		t.Fatalf("covered pattern also listed as a skill candidate: %+v", res.Skills)
	}
}

// Five copies only reach minimum support if the two in session s count separately.
func TestConversationsInOneSessionCountSeparately(t *testing.T) {
	turns := []Turn{
		turn("s", 0, "", "fetch", "parse", "store"),
		turn("s", 1, "", "fetch", "parse", "store"),
	}
	for _, s := range []string{"t", "u", "v"} {
		turns = append(turns, turn(s, 0, "", "fetch", "parse", "store"))
	}
	res := Mine(turns)
	f := find(res.Skills, "fetch", "parse", "store")
	if f == nil || f.Support != 5 {
		t.Fatalf("want support 5, got %+v", res.Skills)
	}
	if !slices.Equal(f.Examples, []string{"s#0", "s#1", "t#0"}) {
		t.Fatalf("examples = %v", f.Examples)
	}
}

func writeLog(t *testing.T, dir string, turns []Turn) {
	t.Helper()
	var b strings.Builder
	for _, tr := range turns {
		line, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteString("\n")
	}
	if err := os.WriteFile(SequencesFile(dir), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunWritesCandidatesUnlessAFreshLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, cohesiveAndChain())
	if err := Run(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "candidates.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Workflow candidates", "## Skill candidates", "1. alpha > beta > gamma — 5 conversations", "## Script candidates", "## Covered by existing skills"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("candidates.md lacks %q:\n%s", want, data)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lock left behind: %v", err)
	}

	locked := t.TempDir()
	writeLog(t, locked, cohesiveAndChain())
	if err := os.WriteFile(filepath.Join(locked, "mine.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(locked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(locked, "candidates.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Run wrote candidates.md despite a fresh lock: %v", err)
	}
}
