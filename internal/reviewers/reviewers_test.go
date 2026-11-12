package reviewers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/skills"
)

const corpus = `
cap: 2
personas:
  - id: test-skeptic
    name: Test skeptic
    always: true
    lens: do the tests assert behaviour
  - id: bug-finder
    name: Bug finder
    always: true
    unless_only: ["**/*.md", "docs/**"]
    lens: break it
  - id: database-engineer
    name: Database engineer
    paths: ["db/migrations/**", "**/models/**"]
    lens: locks
  - id: auth-access
    name: Auth
    paths: ["**/controllers/**"]
    lens: who can call it
  - id: query-performance
    name: Query performance
    paths: ["**/controllers/**", "**/repositories/**"]
    lens: N+1s
  - id: maintainer
    name: Maintainer
    lens: naming
`

func load(t *testing.T) *Corpus {
	t.Helper()
	c, err := Parse([]byte(corpus))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ids(ps []Persona) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return strings.Join(out, ",")
}

func TestMissingUserPersonasFallBackToTheBuiltInFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := skills.Dir()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(c.Select([]string{"main.go"}).Always); got != "test-skeptic,bug-finder" {
		t.Errorf("built-in always personas = %s", got)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "personas.yaml")); !os.IsNotExist(err) {
		t.Errorf("a missing file outside the user's folder: err = %v, want not-exist", err)
	}
}

func TestAlwaysPersonasSitOutsideTheCap(t *testing.T) {
	s := load(t).Select([]string{
		"db/migrations/20261014-add.py",
		"api/modules/orders/controllers/order_controller.py",
	})
	if got := ids(s.Always); got != "test-skeptic,bug-finder" {
		t.Errorf("always = %s", got)
	}
	if got := ids(s.Matched); got != "database-engineer,auth-access" {
		t.Errorf("matched = %s; the cap of 2 should cut query-performance, the later match", got)
	}
	if s.Open != 0 {
		t.Errorf("open = %d, want 0 with the cap full", s.Open)
	}
	in := s.Inputs("acme/web#7")
	if in["reviewers"] != "test-skeptic, bug-finder, database-engineer, auth-access" || in["reviewer_count"] != 4 || in["open_slots"] != 0 {
		t.Errorf("inputs = %v", in)
	}
}

func TestDocsOnlyDiffSkipsTheBugFinderButNotTheTestSkeptic(t *testing.T) {
	c := load(t)
	s := c.Select([]string{"README.md", "docs/setup.md"})
	if got := ids(s.Always); got != "test-skeptic" {
		t.Errorf("docs-only always = %s, want test-skeptic alone", got)
	}
	if s.Open != 2 {
		t.Errorf("open = %d, want the whole cap", s.Open)
	}
	if got := ids(c.Select([]string{"README.md", "api/app/main.py"}).Always); got != "test-skeptic,bug-finder" {
		t.Errorf("one code file among docs must keep the bug finder, got %s", got)
	}
	if got := ids(c.Select(nil).Always); got != "test-skeptic,bug-finder" {
		t.Errorf("an unknown file list must not skip the bug finder, got %s", got)
	}
}

func TestPersonasWithoutPathsOnlyRunWhenAdded(t *testing.T) {
	s := load(t).Select([]string{"pkg/core/helpers/strings.py"})
	if len(s.Matched) != 0 || s.Open != 2 {
		t.Errorf("matched %s with %d open; maintainer has no paths and must not match", ids(s.Matched), s.Open)
	}
}

func TestParseRejectsABrokenFile(t *testing.T) {
	for name, text := range map[string]string{
		"duplicate id":  "cap: 1\npersonas:\n  - {id: a, name: A, lens: x}\n  - {id: a, name: B, lens: y}\n",
		"unknown field": "cap: 1\npersonas:\n  - {id: a, name: A, lens: x, path: [web/**]}\n",
		"bad glob":      "cap: 1\npersonas:\n  - {id: a, name: A, lens: x, paths: [\"web/[\"]}\n",
		"no cap":        "personas:\n  - {id: a, name: A, lens: x}\n",
	} {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}
