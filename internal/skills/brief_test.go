package skills

import (
	"strings"
	"testing"
)

func TestBriefGivesEveryStepAtOnceWithItsChecks(t *testing.T) {
	s, err := Load("testdata/deep-research.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := BindText(s, "how should a harness check research?", started, home)
	if err != nil {
		t.Fatal(err)
	}
	brief := Brief(r, []string{"vault_search", "vault_note", "web_search", "web_fetch"})
	last := -1
	for _, want := range []string{
		"question = how should a harness check research?",
		"1. **scope**: Break how should a harness check research? into sub-questions in " + runDir + "/scope.md",
		"**vault** (optional)",
		"**glean**: skipped",
		"at least 3 successful web_search calls with distinct queries",
		"Repeat this step 2 times.",
		"at least 6 leads in " + runDir + "/leads.md",
		runDir + "/report.md cites at least 1 URL(s), and every URL it cites was fetched",
		"**file** (optional)",
	} {
		i := strings.Index(brief, want)
		if i < 0 {
			t.Errorf("brief lacks %q:\n%s", want, brief)
			continue
		}
		if i < last {
			t.Errorf("%q is out of order", want)
		}
		last = i
	}
}

func TestBindTextFillsTheOneFreeInput(t *testing.T) {
	s, err := Load("testdata/deep-research.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindText(s, "", started, home); err == nil || !strings.Contains(err.Error(), "input question is required") {
		t.Errorf("empty text: %v", err)
	}
	s.Inputs["topic"] = Input{Type: "string"}
	if _, err := BindText(s, "q", started, home); err == nil || !strings.Contains(err.Error(), "2 required text inputs") {
		t.Errorf("two free inputs: %v", err)
	}
}

func TestSummaryListsEachNodeAndWhy(t *testing.T) {
	w := honest()
	w.drop(func(e Event) bool { return field(e.Input, "command") != "view" })
	res := check(t, w, nil)
	got := Summary("deep-research", "failed: no pages", res)
	for _, want := range []string{"**deep-research** failed: no pages, not complete", "- **read**: pending", "0 of 4 fetched pages were opened", "- **glean** (optional): skipped"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	pending := Unmet(res)
	if !strings.Contains(pending, "- read: 0 of 4 fetched pages were opened") || strings.Contains(pending, "glean") || strings.Contains(pending, "- scope") {
		t.Errorf("Pending should name only required pending nodes:\n%s", pending)
	}
}
