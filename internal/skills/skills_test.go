package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/deep-research.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDeepResearchLoads(t *testing.T) {
	s, err := Load("testdata/deep-research.yaml")
	if err != nil {
		t.Fatalf("the reference skill must stay valid: %v", err)
	}
	leads := s.Nodes["leads"]
	if leads.Loop == nil || leads.Loop.Times.Ref != "hops" {
		t.Errorf("leads loops %+v, want times from input hops", leads.Loop)
	}
	if got := leads.Evidence[0].LeadsFollowed.Min.Ref; got != "leads_per_hop" {
		t.Errorf("leads min ref = %q", got)
	}
	if got := s.Nodes["read"].Evidence[0].Call; len(got.Tool) != 1 || got.Tool[0] != "web_fetch" || got.Min.N != 4 || got.Distinct != "domain" {
		t.Errorf("read's first rule = %+v", got)
	}
	if !s.Nodes["glean"].Optional || s.Nodes["glean"].When.Available != "mcp__glean__search" {
		t.Errorf("glean = %+v", s.Nodes["glean"])
	}
}

func TestStartsDefaultsToBoth(t *testing.T) {
	s, err := Parse("deep-research", []byte(strings.Replace(fixture(t), "starts: [user, model]\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Starts, ",") != "user,model" {
		t.Errorf("starts = %v", s.Starts)
	}
}

func TestBrokenSkillsSayWhy(t *testing.T) {
	cases := []struct {
		name, old, new, want string
	}{
		{"no schema key", "schema: 2\n", "", "no schema: key"},
		{"unknown format", "schema: 2\n", "schema: 3\n", "schema: 3 is not a format alux reads; it reads 2"},
		{"misspelled key", "    say: Search the vault before the web.\n", "    sya: Search the vault before the web.\n", `nodes.vault: unexpected additional properties ["sya"]`},
		{"unknown rule", "fetches_opened: { min: 4 }", "pages_read: { min: 4 }", `nodes.read.evidence[1]: unexpected additional properties ["pages_read"]`},
		{"bad start", "starts: [user, model]", "starts: [agent]", "agent"},
		{"order names a missing node", "[vault, glean, sweep]", "[vault, gleen, sweep]", "no node named gleen"},
		{"order cycle", "[vault, glean, sweep]", "[sweep, glean, vault]", "order has a cycle"},
		{"undefined template", "${RUN}/report.md answering", "${OUT}/report.md answering", "${OUT} is not an input"},
		{"var chains a var", "${now_utc}\n", "${now_utc}/${RUN}\n", "${RUN} is a var"},
		{"count from a string input", `times: "${hops}"`, `times: "${question}"`, "question is a string input"},
		{"count from an unknown input", `times: "${hops}"`, `times: "${rounds}"`, "no input named rounds"},
		{"gate on an undeclared tool", "when: { available: mcp__glean__search }", "when: { available: mcp__slack__search }", "not listed under requires.optional_tools"},
		{"optional tool without a gate", "    when: { available: mcp__glean__search }\n", "", "waits forever"},
		{"evidence from an undeclared tool", "call: { tool: vault_note }", "call: { tool: linear_create }", "tool linear_create is not listed"},
		{"an undeclared tool in a tool list", "call: { tool: vault_note }", "call: { tool: [vault_note, linear_create] }", "tool linear_create is not listed"},
		{"a condition reading an undeclared input", "distinct: query }", `distinct: query, when: 'call.args.query.contains(inputs.nope)' }`, "nodes.sweep.evidence[0].call.when: inputs.nope is not an input"},
		{"a template ref in a condition", "distinct: query }", `distinct: query, when: 'call.args.query == "${question}"' }`, "never ${...}"},
		{"a scenario field in a checklist", "distinct: query }", `distinct: query, when: 'call.args.query == scenario.id' }`, "scenario.id is never set in a checklist"},
		{"a scenario ref in a checklist", "${RUN}/report.md answering", "${scenario.id}/report.md answering", "${scenario.id} is not an input"},
		{"an input read by a computed key", "distinct: query }", `distinct: query, when: 'inputs[call.args.query] == "x"' }`, "inputs is read only as inputs.<name>"},
		{"a condition that is not a bool", "distinct: query }", `distinct: query, when: 'call.args.query' }`, "not a bool"},
		{"bad regexp", `pattern: "^Q: "`, `pattern: "^Q: ("`, "artifact_lines.pattern"},
		{"default of the wrong type", "default: 2", "default: two", "inputs.hops.default"},
		{"leads exclusion on an int input", "not_in_input: question", "not_in_input: hops", "hops is an int input"},
		{"count below one", "min: 3, distinct", "min: 0, distinct", "nodes.sweep.evidence[0].call.min: minimum"},
		{"two rule types in one item", "- fetches_opened: { min: 4 }", "- { fetches_opened: { min: 4 }, call: { tool: web_fetch } }", "evidence"},
		{"an empty tool list", "call: { tool: vault_note }", "call: { tool: [] }", "nodes.file.evidence[0]"},
		{"gate mixes available and input", "when: { available: mcp__glean__search }", "when: { available: mcp__glean__search, input: hops, equals: 2 }", "available stands alone"},
		{"input gate with neither equals nor in", "when: { available: mcp__glean__search }", "when: { input: hops }", "needs exactly one of equals or in"},
		{"input gate that can never match", "when: { available: mcp__glean__search }", "when: { input: hops, equals: two }", "can never match"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := fixture(t)
			if !strings.Contains(src, c.old) {
				t.Fatalf("fixture no longer contains %q", c.old)
			}
			_, err := Parse("deep-research", []byte(strings.Replace(src, c.old, c.new, 1)))
			if err == nil {
				t.Fatalf("want an error mentioning %q, got a valid skill", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestASchemaOneFileIsRefusedWithOneLine(t *testing.T) {
	_, err := Parse("deep-research", []byte(strings.Replace(fixture(t), "schema: 2\n", "schema: 1\n", 1)))
	var problems Problems
	want := "schema 1 is no longer supported: set schema: 2; step, dispatch and expand nodes are now flow stages (docs/skill-flow.md)"
	if !errors.As(err, &problems) || len(problems) != 1 || problems[0] != want {
		t.Errorf("a schema 1 file gave %v, want the one problem %q", err, want)
	}
}

func TestFileNameMustMatchSkill(t *testing.T) {
	_, err := Parse("research", []byte(fixture(t)))
	if err == nil || !strings.Contains(err.Error(), "does not match the file name research.yaml") {
		t.Errorf("err = %v", err)
	}
}

func TestEverySemanticProblemIsReportedAtOnce(t *testing.T) {
	src := strings.Replace(fixture(t), "[vault, glean, sweep]", "[vault, gleen, sweep]", 1)
	src = strings.Replace(src, "${RUN}/report.md answering", "${OUT}/report.md answering", 1)
	_, err := Parse("deep-research", []byte(src))
	var problems Problems
	if !errors.As(err, &problems) || len(problems) != 2 {
		t.Errorf("want both problems, got %v", err)
	}
}

func TestReportListsValidAndInvalidSkills(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("deep-research.yaml", fixture(t))
	write("broken.yaml", "schema: 2\nskill: broken\n")
	write("notes.md", "not a skill")

	report, err := Report(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**deep-research** v1: valid", "**broken**: invalid"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "notes") {
		t.Errorf("a .md file is not a skill:\n%s", report)
	}
	if strings.Index(report, "broken") > strings.Index(report, "deep-research") {
		t.Errorf("skills are not sorted:\n%s", report)
	}
}

func TestReportWithNoSkillsFolder(t *testing.T) {
	report, err := Report(filepath.Join(t.TempDir(), "skills"))
	if err != nil || !strings.HasPrefix(report, "no skills in") {
		t.Errorf("report = %q, err = %v", report, err)
	}
}

func TestValidateFailsWhenAnyFileIsInvalid(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("schema: 2\nskill: broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.yaml")

	var out strings.Builder
	ok, err := Validate(&out, []string{"testdata/deep-research.yaml", broken, missing}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("want a failure with a broken and a missing file:\n%s", out.String())
	}
	for _, want := range []string{"testdata/deep-research.yaml: ok", broken + ": invalid\n  - ", missing + ": invalid\n  - "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestValidateDefaultsToTheSkillsFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deep-research.yaml"), []byte(fixture(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	ok, err := Validate(&out, nil, dir)
	if err != nil || !ok || !strings.Contains(out.String(), "deep-research.yaml: ok") {
		t.Errorf("ok = %v, err = %v, output:\n%s", ok, err, out.String())
	}
}

func TestBuiltInsAreAllValidWithNoUserSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := List(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("entries = %d, err = %v", len(entries), err)
	}
	for _, e := range entries {
		if e.Err != nil {
			t.Errorf("built-in %s: %v", e.Name, e.Err)
		}
	}
}

func TestTheInternalKnowledgeStepIsGatedOnTheConfiguredTool(t *testing.T) {
	for _, c := range []struct{ settings, want string }{
		{`{"knowledge_tool": "mcp__wiki__search"}`, "mcp__wiki__search"},
		{`{}`, "knowledge_tool"},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, err := Dir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "settings.json"), []byte(c.settings), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Load(filepath.Join(dir, "deep-research.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Nodes["internal"]; got.When == nil || got.When.Available != c.want || got.Evidence[0].Call.Tool[0] != c.want {
			t.Errorf("settings %s: internal = %+v, want it gated on and calling %s", c.settings, got, c.want)
		}
	}
}

func TestUserSkillReplacesTheBuiltInOfTheSameName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "deep-research.yaml")
	if err := os.WriteFile(mine, []byte(strings.Replace(fixture(t), "version: 1", "version: 7", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []Entry
	for _, e := range entries {
		if e.Name == "deep-research" {
			found = append(found, e)
		}
	}
	if len(found) != 1 || found[0].Path != mine || found[0].Skill.Version != 7 {
		t.Fatalf("deep-research entries = %+v, want only the user's version 7", found)
	}
	s, err := Load(filepath.Join(dir, "pr-ship.yaml"))
	if err != nil || s.Name != "pr-ship" {
		t.Errorf("a built-in missing from the user folder did not load: %v", err)
	}
}
