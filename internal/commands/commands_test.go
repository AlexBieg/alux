package commands

import (
	"fmt"
	"strings"
	"testing"
)

// run drives one command and returns everything it showed. Show and ShowBlock are captured
// unless the caller supplied its own, which is how the two can be told apart.
func run(t *testing.T, name, args string, ctx Context) string {
	t.Helper()
	command, ok := Find(name)
	if !ok {
		t.Fatalf("no command named %q", name)
	}
	var shown []string
	capture := func(text string) { shown = append(shown, text) }
	if ctx.Show == nil {
		ctx.Show = capture
	}
	if ctx.ShowBlock == nil {
		ctx.ShowBlock = capture
	}
	command.Run(ctx, args)
	return strings.Join(shown, "\n")
}

func TestSkillsRoutesItsVerbs(t *testing.T) {
	var ran [2]string
	ended, listed := false, false
	ctx := func() Context {
		return Context{
			Skills:   func() (string, error) { listed = true; return "list", nil },
			RunSkill: func(name, text string) { ran = [2]string{name, text} },
			EndSkill: func() { ended = true },
		}
	}
	run(t, "skills", "run deep-research  how do harnesses check research? ", ctx())
	if ran != [2]string{"deep-research", "how do harnesses check research?"} {
		t.Errorf("run got %q", ran)
	}
	run(t, "skills", "end", ctx())
	if !ended {
		t.Error("end did not close the run")
	}
	if got := run(t, "skills", "strat deep-research", ctx()); listed || !strings.Contains(got, "unknown argument strat") {
		t.Errorf("a typo'd verb must be refused, not listed: %q", got)
	}
	if got := run(t, "skills", "run", ctx()); !strings.Contains(got, "/skills run <name>") {
		t.Errorf("run without a name = %q", got)
	}
}

// A mistyped subcommand must not be read as a bare status request: "/vault rebiuld" reporting
// the index unchanged looks exactly like a rebuild that silently did nothing.
func TestVaultRefusesAnUnknownArgument(t *testing.T) {
	for _, arg := range []string{"rebiuld", "alias", "rebuild aliases"} {
		called := false
		got := run(t, "vault", arg, Context{
			Vault: func(string) (string, error) { called = true; return "report", nil },
		})
		if called {
			t.Errorf("%q reached the vault report instead of being refused", arg)
		}
		for _, want := range []string{arg, "/vault rebuild", "/vault aliases"} {
			if !strings.Contains(got, want) {
				t.Errorf("refusal should name the bad argument and the real ones, got: %s", got)
			}
		}
	}
}

// The argument decides whether the index is rewritten and whether money is spent, so it has to
// be read exactly — and surrounding whitespace is the user's typing, not their intent.
func TestVaultPassesTheAction(t *testing.T) {
	for _, tc := range []struct {
		args string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"rebuild", "rebuild"},
		{"  rebuild  ", "rebuild"},
		{"aliases", "aliases"},
		{"  aliases  ", "aliases"},
	} {
		var got string
		seen := false
		run(t, "vault", tc.args, Context{
			Vault: func(action string) (string, error) { got, seen = action, true; return "report", nil },
		})
		if !seen {
			t.Fatalf("/vault %q never reached the report", tc.args)
		}
		if got != tc.want {
			t.Errorf("/vault %q passed action %q, want %q", tc.args, got, tc.want)
		}
	}
}

// Without a vault the command says so plainly rather than reporting on an index that is not
// there. Nil is how the session declines to offer the feature.
func TestVaultWithoutAVaultSaysSo(t *testing.T) {
	got := run(t, "vault", "", Context{})
	if !strings.Contains(got, "no vault") {
		t.Errorf("expected a plain explanation, got: %s", got)
	}
}

// An index that cannot be opened is the case the command exists for, so the reason has to reach
// the user rather than being swallowed into an empty report.
func TestVaultShowsWhyTheIndexFailed(t *testing.T) {
	got := run(t, "vault", "", Context{
		Vault: func(string) (string, error) { return "", fmt.Errorf("disk is on fire") },
	})
	if !strings.Contains(got, "disk is on fire") {
		t.Errorf("the underlying error did not reach the user, got: %s", got)
	}
}

// The report is laid out in aligned columns, so it must go to ShowBlock — Show rewraps it as
// prose and the alignment is lost.
func TestVaultReportGoesToTheBlockRenderer(t *testing.T) {
	var block, prose string
	run(t, "vault", "", Context{
		Vault:     func(string) (string, error) { return "  vault   /somewhere", nil },
		ShowBlock: func(text string) { block = text },
		Show:      func(text string) { prose = text },
	})
	if block == "" {
		t.Error("the report did not go to ShowBlock and will be reflowed")
	}
	if prose != "" {
		t.Errorf("the report also went to Show: %q", prose)
	}
}

// Completion and the unknown-command list both read the table, so a command that answers to a
// name nobody can type is invisible.
func TestVaultIsDiscoverable(t *testing.T) {
	if _, matches := Complete("/vau"); len(matches) != 1 || matches[0].Value != "/vault" {
		t.Errorf("/vau should complete to exactly the vault command, got %v", matches)
	}
	command, ok := Find("vault")
	if !ok {
		t.Fatal("vault is not in the command table")
	}
	if !strings.Contains(command.Description, "rebuild") {
		t.Errorf("the description should mention the rebuild argument, got %q", command.Description)
	}
}

func values(list []Suggestion) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.Value
	}
	return out
}

func fakeSources(t *testing.T) {
	t.Helper()
	s, l := skillNames, LandCandidates
	t.Cleanup(func() { skillNames, LandCandidates = s, l })
	skillNames = func() []Suggestion { return words("deep-research", "", "record-cleanup", "") }
	LandCandidates = func() []Suggestion { return words("wxpyzons", "pool fix", "lptryswt", "land completion") }
}

func TestCompleteOffersEachArgumentLevel(t *testing.T) {
	fakeSources(t)
	for _, tc := range []struct {
		input string
		start int
		want  string
	}{
		{"/skills ", 8, "run show end"},
		{"/skills run research", 12, "deep-research"},
		{"/skills run deep-research some text", 0, ""},
		{"/vault re", 7, "rebuild"},
		{"/land ", 6, "wxpyzons lptryswt"},
		{"/land lp", 6, "lptryswt"},
		{"/land wxpyzons ", 0, ""},
		{"/clear ", 0, ""},
		{"hello /skills ", 0, ""},
	} {
		start, got := Complete(tc.input)
		if strings.Join(values(got), " ") != tc.want || len(got) > 0 && start != tc.start {
			t.Errorf("Complete(%q) = %d %v, want %d %q", tc.input, start, values(got), tc.start, tc.want)
		}
	}
}

// Tab takes the first row, so an exact name must outrank a longer one it prefixes, and a prefix
// must hide substring matches that would otherwise crowd it.
func TestCompleteRanksExactThenPrefixThenSubstring(t *testing.T) {
	got := rank(words("runner", "", "run", "", "rerun", ""), [][]string{{"runner"}, {"run"}, {"rerun"}}, "run")
	if v := values(got); strings.Join(v, " ") != "run runner" {
		t.Errorf("rank = %v, want run before runner and rerun hidden", v)
	}
	got = rank(words("deep-research", ""), [][]string{{"deep-research"}}, "RESEARCH")
	if len(got) != 1 {
		t.Errorf("a substring should match, case-insensitively, when nothing has the prefix: %v", values(got))
	}
}

func TestCompleteOmitsUnconfiguredCommandsButTheyStillRun(t *testing.T) {
	old := Unconfigured
	t.Cleanup(func() { Unconfigured = old })
	Unconfigured = map[string]bool{"vault": true, "prs": true, "land": true, "resume": true}

	_, got := Complete("/")
	for _, s := range got {
		for _, hidden := range []string{"/vault", "/prs", "/land", "/resume"} {
			if s.Value == hidden {
				t.Errorf("Complete offered %s, which is not configured", hidden)
			}
		}
	}
	if _, got := Complete("/sk"); len(got) != 1 || got[0].Value != "/skills" {
		t.Errorf("a configured command went missing: %v", values(got))
	}
	if out := run(t, "vault", "", Context{}); !strings.Contains(out, "no vault") {
		t.Errorf("/vault typed in full did not run: %q", out)
	}
}

// An alias that was being typed is what tab should insert, not the canonical name.
func TestCompleteKeepsTheAliasBeingTyped(t *testing.T) {
	if _, got := Complete("/ne"); len(got) != 1 || got[0].Value != "/new" {
		t.Errorf("/ne completed to %v, want /new", values(got))
	}
}

// /prs answers in markdown, which only Show renders and wraps; ShowBlock would print it verbatim.
func TestPRsIsShownAsMarkdown(t *testing.T) {
	var prose, block []string
	command, _ := Find("prs")
	command.Run(Context{
		Show:      func(s string) { prose = append(prose, s) },
		ShowBlock: func(s string) { block = append(block, s) },
		PRs:       func(arg string) (string, error) { return "### acme/web\n\n| PR |\n| --- |\n| #1 |", nil },
	}, "")
	if len(block) != 0 || len(prose) != 1 {
		t.Fatalf("Show got %d, ShowBlock got %d; want the report through Show", len(prose), len(block))
	}
}
