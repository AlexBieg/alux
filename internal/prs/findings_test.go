package prs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/reviewstore"
)

const findingsHead = "0123456789abcdef"

func findingsSetup(t *testing.T, submissions ...reviewstore.Submission) (*Poller, string) {
	t.Helper()
	home := t.TempDir()
	for _, s := range submissions {
		s.Repo, s.Number = "acme/web", 42
		if _, err := reviewstore.SaveSubmission(home, s); err != nil {
			t.Fatal(err)
		}
	}
	areas := filepath.Join(t.TempDir(), "areas.md")
	if err := os.WriteFile(areas, []byte("- bug-finder | a.go | nil maps\n- test-skeptic | a_test.go | asserts | added: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rest, _ := fakeREST(t, nil)
	return &Poller{conf: Config{Repos: []Repo{{Owner: "acme", Name: "web"}}}, rest: rest, home: home}, areas
}

func TestFindingsListsEveryAreasPersonasSubmissionAtTheHead(t *testing.T) {
	p, areas := findingsSetup(t,
		reviewstore.Submission{Head: findingsHead, Persona: "test-skeptic", Findings: []reviewstore.Finding{
			{Severity: "non-blocking", Class: "weak test", Path: "a_test.go", Problem: "It passes\nwhen broken.", Impact: "i", Evidence: "e", Fix: "f"},
		}},
		reviewstore.Submission{Head: findingsHead, Persona: "bug-finder", Findings: []reviewstore.Finding{
			{Severity: "blocking", Class: "nil map", Path: "a.go", StartLine: 3, EndLine: 5, Problem: "p", Impact: "i", Evidence: "a.go:4", Fix: "f"},
			{Severity: "non-blocking", Class: "typo", Path: "a.go", StartLine: 9, Problem: "p2", Impact: "i2", Evidence: "e2", Fix: "f2"},
		}},
		reviewstore.Submission{Head: findingsHead, Persona: "security", Findings: []reviewstore.Finding{
			{Severity: "blocking", Class: "stale", Path: "a.go", Problem: "p", Impact: "i", Evidence: "e", Fix: "f"},
		}},
	)
	out, err := p.Findings(context.Background(), "", 42, areas)
	if err != nil {
		t.Fatal(err)
	}
	want := "Findings submitted for acme/web#42 at head 0123456789ab by bug-finder, test-skeptic:\n" +
		"- bug-finder | blocking | nil map | a.go:3-5 | problem: p | impact: i | evidence: a.go:4 | fix: f\n" +
		"- bug-finder | non-blocking | typo | a.go:9 | problem: p2 | impact: i2 | evidence: e2 | fix: f2\n" +
		"- test-skeptic | non-blocking | weak test | a_test.go | problem: It passes when broken. | impact: i | evidence: e | fix: f"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestFindingsFailsNamingAPersonaWithNoSubmissionAtTheHead(t *testing.T) {
	p, areas := findingsSetup(t,
		reviewstore.Submission{Head: findingsHead, Persona: "bug-finder"},
		reviewstore.Submission{Head: "fedcba9876543210", Persona: "test-skeptic", Findings: []reviewstore.Finding{
			{Severity: "blocking", Class: "c", Path: "a.go", Problem: "p", Impact: "i", Evidence: "e", Fix: "f"},
		}},
	)
	_, err := p.Findings(context.Background(), "", 42, areas)
	if err == nil || !strings.HasSuffix(err.Error(), "at head 0123456789ab from: test-skeptic") {
		t.Errorf("err = %v", err)
	}
}
