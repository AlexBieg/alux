package rulecel

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCmdMatchesWholeWordsAsWrittenAndNormalised(t *testing.T) {
	for _, c := range []struct {
		segment, prefix string
		want            bool
	}{
		{"jj git init", "jj git init", true},
		{"  jj git init --colocate", "jj git init", true},
		{"jj git initx", "jj git init", false},
		{"jj -R ../x git init", "jj git init", true},
		{"/opt/homebrew/bin/gh pr create --fill", "gh pr create", true},
		{"bash /abs/scripts/bootstrap.sh", "bootstrap.sh", true},
		{"echo jj git init", "jj git init", false},
		{"", "jj", false},
	} {
		if got := Cmd(c.segment, c.prefix); got != c.want {
			t.Errorf("Cmd(%q, %q) = %v, want %v", c.segment, c.prefix, got, c.want)
		}
	}
}

func TestHasFlagMatchesAFlagItsValueFormAndALongAbbreviation(t *testing.T) {
	for _, c := range []struct {
		segment, flag string
		want          bool
	}{
		{"jj git init --colocate", "--colocate", true},
		{"jj git init --colo", "--colocate", true},
		{"hw local start --no-sync-env-vars=true", "--no-sync-env-vars", true},
		{"jj git init", "--colocate", false},
		{"jj git init --", "--colocate", false},
		{"jj git init --colocated", "--colocate", false},
		{"jj git init -h", "-h", true},
		{"jj git init -hx", "-h", false},
	} {
		if got := HasFlag(c.segment, c.flag); got != c.want {
			t.Errorf("HasFlag(%q, %q) = %v, want %v", c.segment, c.flag, got, c.want)
		}
	}
}

func TestPositionalSkipsTheProgramItsSubcommandAndFlags(t *testing.T) {
	if got := Positional("git add -p a b"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Positional = %v, want [a b]", got)
	}
	if got := Positional("ls"); got == nil || len(got) != 0 {
		t.Errorf("Positional of a bare program = %#v, want an empty list", got)
	}
}

func TestAConditionReadsTheCallAndSession(t *testing.T) {
	c, err := Compile(`call.commands.exists(c, cmd(c, 'jj git init') && !hasFlag(c, '--colocate'))
		&& session.agent == '' && size(positional(call.commands[0])) == 2 && words(call.args.command) > 4`)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{Tool: "bash", Args: map[string]any{"command": "jj git init x"}, Commands: []string{"jj git init x"}}
	if got, err := c.Eval(call, Session{}); err != nil || got {
		t.Fatalf("got %v, %v; want false with no error, since the command has four words", got, err)
	}
	call.Args["command"] = "jj  git init  x --bare"
	call.Commands = []string{"jj git init x --bare"}
	if got, err := c.Eval(call, Session{}); err != nil || !got {
		t.Fatalf("got %v, %v; want true", got, err)
	}
	if got, err := c.Eval(call, Session{Agent: "coder"}); err != nil || got {
		t.Fatalf("for a subagent got %v, %v; want false", got, err)
	}
}

func TestBroadRootIsTheTopTwoLevelsAndHomeButNotDotDirsOrFiles(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, cwd string
		want      bool
	}{
		{"/", "", true},
		{"/tmp", "", true},
		{"/tmp/", "", true},
		{"/tmp/alux-e2e", "", false},
		{home, "", true},
		{filepath.Join(home, "Code"), "", true},
		{filepath.Join(home, "Code", "alux"), "", false},
		{filepath.Join(home, "Library"), "", true},
		{filepath.Join(home, ".config"), "", false},
		{filepath.Join(home, ".local"), "", false},
		{filepath.Join(home, ".local", "state"), "", true},
		{filepath.Join(home, ".local", "state", "alux"), "", false},
		{file, "", false},
		{"..", filepath.Join(home, "Code", "alux"), true},
		{"internal", filepath.Join(home, "Code", "alux"), false},
		{"..", "", false},
	} {
		if got := BroadRoot(c.path, c.cwd, home); got != c.want {
			t.Errorf("BroadRoot(%q, cwd %q) = %v, want %v", c.path, c.cwd, got, c.want)
		}
	}
}

func TestBroadRootInAConditionReadsTheSessionCwdAndHome(t *testing.T) {
	c, err := Compile(`broadRoot(call.args.path)`)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	call := Call{Tool: "grep", Args: map[string]any{"path": ".."}}
	if got, err := c.Eval(call, Session{Cwd: filepath.Join(home, "Code", "alux"), Home: home}); err != nil || !got {
		t.Fatalf("got %v, %v; want true for .. from a repo under ~/Code", got, err)
	}
	if got, err := c.Eval(call, Session{Cwd: filepath.Join(home, "Code", "alux", "internal"), Home: home}); err != nil || got {
		t.Fatalf("got %v, %v; want false for .. from inside a repo", got, err)
	}
}

func TestRevsTakesTargetsAndSkipsDestinations(t *testing.T) {
	for segment, want := range map[string][]string{
		"jj abandon qvsvoxko":                        {"qvsvoxko"},
		"jj abandon":                                 {"@"},
		"jj rebase -s abc -d trunk()":                {"abc"},
		"jj rebase --source=abc --destination xyz":   {"abc"},
		"jj rebase -r 'abc' -o xyz":                  {"abc"},
		"jj rebase -rabc -A xyz":                     {"abc"},
		"jj rebase -d trunk()":                       {"@"},
		"jj -R ../x abandon a b":                     {"a", "b"},
		"jj abandon a && jj log -r b":                {"a"},
		`jj abandon -r 'a+ & description(exact:"")'`: {`a+ & description(exact:"")`},
		"git rebase main":                            {},
		"jj rebase -d main 2>&1":                     {"@"},
		"jj abandon abc 2>/dev/null":                 {"abc"},
		"jj abandon abc > out.txt":                   {"abc"},
	} {
		if got := Revs(segment); !reflect.DeepEqual(got, want) {
			t.Errorf("Revs(%q) = %q, want %q", segment, got, want)
		}
	}
}

func TestDescendantsRunsOncePerRevisionAndListedReadsTheSession(t *testing.T) {
	c, err := Compile(`call.commands.exists(c, revs(c).exists(r, size(descendants(r)) > 0 && size(descendants(r)) < 9 && !listed(r)))`)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	session := Session{
		Listed: func(rev string) bool { return rev == "seen" },
		Descendants: func(string) ([]string, error) {
			runs++
			return []string{"child"}, nil
		},
	}
	call := Call{Tool: "bash", Commands: []string{"jj abandon x"}}
	if got, err := c.Eval(call, session); err != nil || !got || runs != 1 {
		t.Fatalf("got %v, %v after %d runs; want true after one", got, err, runs)
	}
	call.Commands = []string{"jj abandon seen"}
	if got, err := c.Eval(call, session); err != nil || got {
		t.Fatalf("for a listed revision got %v, %v; want false", got, err)
	}
	if _, err := c.Eval(call, Session{}); err == nil {
		t.Fatal("with no session facts the condition evaluated without error")
	}
}

func TestExplainNamesOnlyTheRevisionsAndRootsThatMadeTheConditionTrue(t *testing.T) {
	c, err := Compile(`call.commands.exists(c, revs(c).exists(r, size(descendants(r)) > 0 && !listed(r)))`)
	if err != nil {
		t.Fatal(err)
	}
	session := Session{
		Listed:      func(rev string) bool { return rev == "seen" },
		Descendants: func(string) ([]string, error) { return []string{"a", "b", "c"}, nil },
	}
	holds, ev, err := c.Explain(Call{Tool: "bash", Commands: []string{"jj abandon seen x"}}, session)
	want := []string{"3 changes sit on x and no earlier jj log in this conversation listed them"}
	if err != nil || !holds || !reflect.DeepEqual(ev, want) {
		t.Fatalf("got %v, %q, %v; want true, %q", holds, ev, err, want)
	}
	if holds, ev, _ := c.Explain(Call{Tool: "bash", Commands: []string{"jj abandon seen"}}, session); holds || ev != nil {
		t.Fatalf("a false condition gave %v with evidence %q", holds, ev)
	}

	broad, err := Compile(`broadRoot(call.args.path)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ev, _ := broad.Explain(Call{Tool: "grep", Args: map[string]any{"path": "/"}}, Session{}); !reflect.DeepEqual(ev, []string{"/ is a broad root"}) {
		t.Fatalf("broadRoot evidence %q", ev)
	}
}

func TestAMissingArgIsAnErrorNotFalse(t *testing.T) {
	c, err := Compile(`words(call.args.body) > 400`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Eval(Call{Tool: "pr_create"}, Session{}); err == nil {
		t.Fatal("a missing key evaluated without error")
	}
}

func TestCompileRefusesSyntaxErrorsUnknownNamesAndNonBools(t *testing.T) {
	for expr, want := range map[string]string{
		`call.tool ==`:          "Syntax error",
		`words(call.tool, 1)`:   "found no matching overload",
		`nope.tool == 'x'`:      "undeclared reference",
		`words(call.args.body)`: "not a bool",
	} {
		if _, err := Compile(expr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%q) = %v, want an error containing %q", expr, err, want)
		}
	}
}
