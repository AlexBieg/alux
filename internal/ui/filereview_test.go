package ui

import (
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/charmbracelet/x/ansi"
)

func TestAFindingBoxShowsProblemImpactEvidenceAndFixInOrderAndAnOldTextBodyAsIs(t *testing.T) {
	box := ansi.Strip(strings.Join(findingRows(reviewstore.Finding{Severity: "blocking", Class: "c", Path: "a.go",
		Problem: "PROBLEMTEXT", Impact: "IMPACTTEXT", Evidence: "EVIDENCETEXT", Fix: "FIXTEXT"}, findingBox{}, 60), "\n"))
	at := -1
	for _, want := range []string{"problem", "PROBLEMTEXT", "impact", "IMPACTTEXT", "evidence", "EVIDENCETEXT", "fix", "FIXTEXT"} {
		i := strings.Index(box[at+1:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order:\n%s", want, box)
		}
		at += 1 + i
	}
	old := ansi.Strip(strings.Join(findingRows(reviewstore.Finding{Severity: "blocking", Class: "c", Path: "a.go", Text: "OLDBODY"}, findingBox{}, 60), "\n"))
	if !strings.Contains(old, "OLDBODY") || strings.Contains(old, "problem") {
		t.Errorf("a finding stored with text alone should show it without labels:\n%s", old)
	}
}

func TestXLogsTheDismissedFindingWithItsThreadFromBothViews(t *testing.T) {
	g := sampleFileGraph
	g.Head = "headnow"
	stubFileGraph(t, g)
	stubReview(reviewstore.Review{Repo: "acme/web", Number: 2, Head: "headnow", Verdict: "request changes", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "RACE", Path: "src/load.go", StartLine: 9, EndLine: 9, Text: "FIRSTFINDING",
			Thread: []reviewstore.Message{{Role: "user", Text: "IN THE DIFF"}}},
		{Severity: "non-blocking", Class: "NAME", Path: "src/load.go", Text: "SECONDFINDING",
			Thread: []reviewstore.Message{{Role: "user", Text: "is this real?"}, {Role: "assistant", Text: "no"}}},
	}})
	var logged []reviewstore.Dismissal
	old := logDismissal
	logDismissal = func(d reviewstore.Dismissal) error { logged = append(logged, d); return nil }
	t.Cleanup(func() { logDismissal = old })
	m, _ := pickerModel(t)
	runCmd(m, typeLine(m, "/prs"))
	press(m, "down")
	runCmd(m, press(m, "g"))
	press(m, "down")

	press(m, "l")
	press(m, "down")
	press(m, "x")
	press(m, "h")
	press(m, "enter")
	press(m, "]")
	press(m, "x")

	if len(logged) != 2 {
		t.Fatalf("each dismissal should log one line, got %+v", logged)
	}
	d := logged[0]
	if d.Repo != "acme/web" || d.Number != 2 || d.Head != "headnow" || d.DismissedAt.IsZero() ||
		d.Finding.Text != "SECONDFINDING" || len(d.Finding.Thread) != 2 || d.Finding.Thread[1].Text != "no" {
		t.Errorf("x in the overview should log the PR, head, time and the finding with its thread; got %+v", d)
	}
	if d := logged[1]; d.Finding.Text != "FIRSTFINDING" || len(d.Finding.Thread) != 1 {
		t.Errorf("x in the diff view should log the finding with its thread; got %+v", d)
	}
}
