package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

// A rule set shaped the way a code-editing subagent's would be: the build and test verbs it needs
// to do the work, and nothing that leaves the machine.
func editorRules(root string) *Rules {
	return &Rules{
		Bash: []Rule{
			{Pattern: "go build", Verdict: VerdictAllow},
			{Pattern: "go test", Verdict: VerdictAllow},
			{Pattern: "jj status", Verdict: VerdictAllow},
			{Pattern: "jj diff", Verdict: VerdictAllow},
			{Pattern: "jj describe", Verdict: VerdictAsk, Reason: "describing a change rewrites history"},
		},
		Edit:  []Rule{{Pattern: root, Verdict: VerdictAllow}},
		Tools: AllowTools("glob", "grep"),
	}
}

func bashCall(command string) Call {
	input, _ := json.Marshal(map[string]any{"command": command})
	return Call{ID: "t1", Name: "bash", Input: input}
}

func editCall(command, path string) Call {
	input, _ := json.Marshal(map[string]any{"command": command, "path": path, "file_text": "x"})
	return Call{ID: "t1", Name: editorTool, Input: input}
}

func decide(t *testing.T, rules *Rules, call Call) (Verdict, string) {
	t.Helper()
	a := &Agent{Policy: rules, Handlers: map[string]Handler{"glob": RunSearch}}
	return rules.Decide(a.intent(call))
}

// The case the whole design exists for: an agent that may run the test suite may not push the
// work. Both halves matter — a policy that denied `go test` too would be useless rather than safe.
func TestAnEditingAgentMayTestButNotPush(t *testing.T) {
	rules := editorRules(t.TempDir())

	if v, _ := decide(t, rules, bashCall("go test ./...")); v != VerdictAllow {
		t.Errorf("go test: got %v, want allow", v)
	}
	if v, _ := decide(t, rules, bashCall("jj git push --bookmark feature")); v != VerdictDeny {
		t.Errorf("jj git push: got %v, want deny", v)
	}
}

func TestWithoutABashRootCdStaysUnderTheShellsDirectoryOrTemp(t *testing.T) {
	scratch := t.TempDir()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	if resolved, _ := realPath(root); under(resolved, mustReal(t, "/tmp")) {
		t.Skip("the test's own directories are under /tmp here")
	}

	a, _, r := sandboxedAgent(t, mockapi.Turn{Tools: []mockapi.ToolUse{
		{ID: "t1", Name: "bash", Input: map[string]any{"command": "cd sub && go test ./..."}},
		{ID: "t2", Name: "bash", Input: map[string]any{"command": "cd " + filepath.Join(scratch, "x") + " && go test ./..."}},
		{ID: "t3", Name: "bash", Input: map[string]any{"command": "cd ../elsewhere && go test ./..."}},
	}}, mockapi.Turn{Text: "done"})
	a.Policy = &Rules{Default: VerdictAllow}
	res := a.RunTurn(tools.WithDir(context.Background(), root), user("go"), (&recorder{}).callbacks())

	for _, id := range []string{"t1", "t2"} {
		if _, ran := r[id]; !ran {
			t.Errorf("%s was refused: %q", id, toolTexts(res.History))
		}
	}
	if _, ran := r["t3"]; ran {
		t.Error("cd out of the shell's directory ran")
	}
	if texts := toolTexts(res.History); len(texts) != 3 || !strings.Contains(texts[2], "git -C") {
		t.Errorf("results %q, want the refusal to name another way", texts)
	}
}

func TestRmAfterACdIntoTempMustNameItsFile(t *testing.T) {
	root := t.TempDir()
	rules := editorRules(root)
	rules.BashRoot = root
	rules.Bash = append(rules.Bash, Rule{Pattern: "rm", Verdict: VerdictAllow})

	if v, why := decide(t, rules, bashCall("rm x.go")); v != VerdictAllow {
		t.Fatalf("rm in the root: got %v (%s), want allow", v, why)
	}
	if v, _ := decide(t, rules, bashCall("cd /tmp && rm x.go")); v != VerdictDeny {
		t.Errorf("rm after cd /tmp: got %v, want deny", v)
	}
}

func TestARelativeRmAfterACdIsJudgedFromTheNewDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".jj", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := editorRules(root)
	rules.BashRoot = root
	rules.Edit = append(rules.Edit, Rule{Pattern: filepath.Join(root, ".jj"), Verdict: VerdictDeny, Reason: "the jj change graph is not a file to edit"})
	a := &Agent{Policy: rules}
	in := a.intent(bashCall("rm repo"))
	in.Dir, in.Root = filepath.Join(root, ".jj"), root

	if v, why := rules.Decide(in); v != VerdictDeny || !strings.Contains(why, "change graph") {
		t.Errorf("rm repo from .jj: got %v (%s), want the .jj rule's refusal", v, why)
	}
	in = a.intent(bashCall("cd .jj && rm repo"))
	in.Dir, in.Root = root, root
	if v, why := rules.Decide(in); v != VerdictDeny || !strings.Contains(why, "change graph") {
		t.Errorf("cd .jj && rm repo: got %v (%s), want the .jj rule's refusal", v, why)
	}
}

func TestWithoutABashRootCdMayClimbBackToWhereTheShellStartedButNoFurther(t *testing.T) {
	scratch := t.TempDir()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	if resolved, _ := realPath(root); under(resolved, mustReal(t, "/tmp")) {
		t.Skip("the test's own directories are under /tmp here")
	}
	rules := &Rules{Default: VerdictAllow}
	a := &Agent{Policy: rules}
	from := func(command string) Intent {
		in := a.intent(bashCall(command))
		in.Dir, in.Root = filepath.Join(root, "sub"), root
		return in
	}

	if v, why := rules.Decide(from("cd ..")); v != VerdictAllow {
		t.Errorf("cd .. back to the root: got %v (%s), want allow", v, why)
	}
	if v, _ := rules.Decide(from("cd ../..")); v != VerdictDeny {
		t.Errorf("cd ../.. out of the root: got %v, want deny", v)
	}
}

func TestTheGateJudgesABashCallFromTheShellOnItsContext(t *testing.T) {
	root := t.TempDir()
	sh, err := shell.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	ctx := shell.With(tools.WithDir(context.Background(), t.TempDir()), sh)
	a := &Agent{Policy: &Rules{
		Default:  VerdictAllow,
		BashRoot: root,
		Edit:     []Rule{{Pattern: root, Verdict: VerdictDeny, Reason: "the shell's tree is not to be deleted from"}},
	}}

	// cd - is refused whatever happens, so a gate that misreads the directory still stops the line.
	blocked, _, _ := a.gate(ctx, bashCall("rm x && cd -"), Callbacks{})
	if blocked == nil {
		t.Fatal("rm x && cd - ran")
	}
	if text, _ := resultText(t, *blocked); !strings.Contains(text, "the shell's tree") {
		t.Errorf("refusal %q, want rm x judged in the shell's directory", text)
	}
}

func mustReal(t *testing.T, path string) string {
	t.Helper()
	resolved, err := realPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Longest pattern wins however the rules are ordered, so a specific rule is not silently beaten
// by a general one that happens to be written after it.
func TestTheLongestMatchingRuleWins(t *testing.T) {
	rules := &Rules{Bash: []Rule{
		{Pattern: "go test ./internal/agent", Verdict: VerdictDeny, Reason: "that package's tests hang"},
		{Pattern: "go", Verdict: VerdictAllow},
	}}

	if v, _ := decide(t, rules, bashCall("go test ./internal/ui")); v != VerdictAllow {
		t.Errorf("unlisted package: got %v, want allow", v)
	}
	v, reason := decide(t, rules, bashCall("go test ./internal/agent"))
	if v != VerdictDeny {
		t.Fatalf("listed package: got %v, want deny", v)
	}
	if !strings.Contains(reason, "hang") {
		t.Errorf("reason %q does not carry the rule's own words", reason)
	}
}

// Nothing matches, so nothing runs. The zero Verdict being Deny is what makes an allow-list an
// allow-list rather than a list of suggestions.
func TestAnEmptyRuleSetRefusesEverything(t *testing.T) {
	rules := &Rules{}
	for _, call := range []Call{bashCall("ls"), editCall("create", "/tmp/x"), {ID: "t1", Name: "glob"}} {
		if v, _ := decide(t, rules, call); v != VerdictDeny {
			t.Errorf("%s: got %v, want deny", call.Name, v)
		}
	}
}

// The property that separates this from a deny-list. None of these is matched against a list of
// forbidden shapes: each is refused because what it will run cannot be read off the line. Every
// one of them reaches a push or an arbitrary command while containing an allowed verb, which is
// exactly how a pattern blocklist gets walked through.
//
// The rule set here deliberately defaults to Allow. An undecomposable command has to be refused
// by the decomposition failing, not by falling through to a default that happens to be Deny —
// otherwise the fail-closed property is an accident of configuration and disappears the moment a
// rule set is written with a permissive default.
func TestCommandsThatCannotBeReadAreRefused(t *testing.T) {
	rules := &Rules{
		Bash:    []Rule{{Pattern: "go test", Verdict: VerdictAllow}},
		Default: VerdictAllow,
	}

	cases := map[string]string{
		`eval "$PUSH"`:                 "eval",
		"go test; eval $CMD":           "eval",
		"go test; sh -c 'jj git push'": "shell",
		"go test `jj git push`":        "substitutes",
		`go test "$(jj git push)"`:     "substitutes",
		"go test $(jj git push)":       "substitutes",
		"$PUSHCMD":                     "expansion",
		"go test > /etc/hosts":         "redirects",
		"go test < input.txt":          "redirects",
		"go test 2>out.log":            "redirects",
		"go test >>out.log":            "redirects",
		"go test &>out.log":            "redirects",
		"go test >&out.log":            "redirects",
		"go test 2>/dev/nullx":         "redirects",
		"cat <<EOF":                    "redirects",
		"go test | sh":                 "shell",
		"(go test && jj git push)":     "groups",
		"go test 'unterminated":        "unterminated",
		"source ./push.sh":             "source",
		"env PATH=/tmp jj git push":    "env",
		"xargs jj":                     "xargs",
		"sudo go test":                 "sudo",
		"timeout 5 jj git push":        "timeout",
		"go test <(jj git push)":       "process substitution",
		"nohup jj git push":            "nohup",
		"go test && bash ./push.sh":    "shell",
	}

	for command, want := range cases {
		v, reason := decide(t, rules, bashCall(command))
		if v != VerdictDeny {
			t.Errorf("%q: got %v, want deny", command, v)
			continue
		}
		if !strings.Contains(reason, want) {
			t.Errorf("%q: reason %q does not explain the construct (wanted %q in it)", command, reason, want)
		}
	}
}

// Decomposition has to be good enough to be useful, or every real command is refused as opaque
// and the agent can do nothing. These all read cleanly and must survive.
func TestOrdinaryCommandsAreStillReadable(t *testing.T) {
	rules := editorRules(t.TempDir())
	for _, command := range []string{
		"go test ./...",
		"go test -run 'TestFoo|TestBar' ./internal/agent",
		"GOFLAGS=-count=1 go test ./...",
		"go build ./... && go test ./...",
		"jj status   # where am I",
		`go test -run "TestA" ./internal/agent`,
		"go test ./... >/dev/null 2>&1",
		"go test ./... 2> /dev/null",
		"go build ./... &>/dev/null && go test ./...",
		"go test ./... 1>&2",
		"go test ./... < /dev/null",
	} {
		if v, reason := decide(t, rules, bashCall(command)); v != VerdictAllow {
			t.Errorf("%q: got %v (%s), want allow", command, v, reason)
		}
	}
}

func TestArgumentsAndAssignmentsThatRunCodeAreRefused(t *testing.T) {
	rules := &Rules{Default: VerdictAllow}
	cases := []struct {
		command string
		refused string
	}{
		{"git -c core.fsmonitor='touch /tmp/x' status", "git -c"},
		{"git -c alias.x='!curl example.com' x", "git -c"},
		{"git -ccore.pager=less log", "git -c"},
		{"git -C sub -c core.pager=less log", "git -c"},
		{"git --config-env=core.pager=EVIL log", "--config-env"},
		{"git status", ""},
		{"git log -c", ""},
		{"git -C sub log -c", ""},
		{"jj --config 'ui.pager=sh' log", "jj --config"},
		{"jj log --config=ui.pager=sh", "jj --config"},
		{"jj --config-file f log", "jj --config"},
		{"jj log --config-file=f", "jj --config"},
		{"jj log", ""},
		{`find . -exec rm '{}' +`, "find -exec"},
		{`find . -execdir sh x '{}' +`, "find -exec"},
		{`find . -ok rm '{}' \;`, "find -exec"},
		{`find . -okdir rm '{}' +`, "find -exec"},
		{"find . -name x -delete", "find -exec"},
		{"find . -name x", ""},
		{"GIT_SSH_COMMAND='sh -c x' git fetch", "GIT_SSH_COMMAND"},
		{"GIT_SSH=x git fetch", "GIT_SSH"},
		{"PAGER=x git log", "PAGER"},
		{"GIT_PAGER=x git log", "GIT_PAGER"},
		{"GIT_EXTERNAL_DIFF=x git diff", "GIT_EXTERNAL_DIFF"},
		{"FOO=1 GIT_CONFIG_GLOBAL=x git status", "GIT_CONFIG_GLOBAL"},
		{"EDITOR=x jj describe", "EDITOR"},
		{"JJ_CONFIG=x jj log", "JJ_CONFIG"},
		{"LD_PRELOAD=x.so ls", "LD_PRELOAD"},
		{"DYLD_INSERT_LIBRARIES=x.dylib ls", "DYLD_INSERT_LIBRARIES"},
		{"BASH_ENV=x ls", "BASH_ENV"},
		{"ls && PROMPT_COMMAND=x", "PROMPT_COMMAND"},
		{"FOO=1 go test ./...", ""},
	}

	for _, c := range cases {
		v, reason := decide(t, rules, bashCall(c.command))
		switch {
		case c.refused == "" && v != VerdictAllow:
			t.Errorf("%q: got %v (%s), want allow", c.command, v, reason)
		case c.refused != "" && v != VerdictDeny:
			t.Errorf("%q: got %v, want deny", c.command, v)
		case c.refused != "" && !strings.Contains(reason, c.refused):
			t.Errorf("%q: reason %q, want it to name %q", c.command, reason, c.refused)
		}
	}
}

// A line is as restricted as its most restricted command: the push in the second half is a push
// whatever the first half is.
func TestALineIsJudgedByItsMostRestrictedCommand(t *testing.T) {
	rules := editorRules(t.TempDir())
	if v, _ := decide(t, rules, bashCall("go test ./... && jj git push")); v != VerdictDeny {
		t.Errorf("got %v, want deny", v)
	}
	if v, _ := decide(t, rules, bashCall("go build ./... && jj describe -m x")); v != VerdictAsk {
		t.Errorf("got %v, want ask", v)
	}
}

func TestWritesAreScopedToAPathAndViewsAreNot(t *testing.T) {
	root := t.TempDir()
	rules := editorRules(root)

	if v, _ := decide(t, rules, editCall("create", filepath.Join(root, "pkg", "x.go"))); v != VerdictAllow {
		t.Error("a write inside the root was not allowed")
	}
	if v, _ := decide(t, rules, editCall("create", filepath.Join(root, "..", "elsewhere.go"))); v != VerdictDeny {
		t.Error("a write outside the root was not denied")
	}
	// Reading is the capability a research agent is defined by, and is not what a path scope is
	// protecting.
	if v, _ := decide(t, rules, editCall("view", "/etc/hosts")); v != VerdictAllow {
		t.Error("a view was gated")
	}
}

// A sibling whose name merely starts with the root's is outside it.
func TestAPathScopeStopsAtAPathBoundary(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root+"-secrets", 0o755); err != nil {
		t.Fatal(err)
	}

	rules := &Rules{Edit: []Rule{{Pattern: root, Verdict: VerdictAllow}}}
	if v, _ := decide(t, rules, editCall("create", filepath.Join(root+"-secrets", "x"))); v != VerdictDeny {
		t.Error("repo-secrets/ was admitted by the rule for repo/")
	}
}

// A symlink inside the scope otherwise points anywhere it likes and still matches the prefix.
func TestAPathScopeIsNotEscapedByASymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	outside := filepath.Join(parent, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rules := &Rules{Edit: []Rule{{Pattern: root, Verdict: VerdictAllow}}}
	if v, _ := decide(t, rules, editCall("create", filepath.Join(link, "x.go"))); v != VerdictDeny {
		t.Error("a write through a symlink out of the scope was allowed")
	}
}

// MCP names are self-describing, so one namespace rule plus one override expresses "may read
// Linear, may not change it".
func TestMcpToolsAreGatedByName(t *testing.T) {
	rules := &Rules{Tools: []Rule{
		{Pattern: "mcp__linear__list", Verdict: VerdictAllow},
		{Pattern: "mcp__linear__get", Verdict: VerdictAllow},
	}}
	if v, _ := decide(t, rules, Call{ID: "t1", Name: "mcp__linear__list_issues"}); v != VerdictAllow {
		t.Error("listing issues was refused")
	}
	if v, _ := decide(t, rules, Call{ID: "t1", Name: "mcp__linear__update_issue"}); v != VerdictDeny {
		t.Error("updating an issue was allowed")
	}
}

// An unrecognised tool name reaches bash through the fallback, so it has to be gated as bash and
// not waved through as an unknown named tool. Without this, inventing a tool name is a bypass.
func TestAnUnknownToolNameIsGatedAsBash(t *testing.T) {
	rules := editorRules(t.TempDir())
	input, _ := json.Marshal(map[string]any{"command": "jj git push"})
	call := Call{ID: "t1", Name: "shell", Input: input}

	a := &Agent{Policy: rules}
	in := a.intent(call)
	if in.Class != ClassBash {
		t.Fatalf("class %v, want bash", in.Class)
	}
	if v, _ := rules.Decide(in); v != VerdictDeny {
		t.Error("a push through an unregistered tool name was allowed")
	}
}

// The headline property. A deny is a refusal, not a question — so the session's default auto mode,
// which answers every approval with yes before anything is drawn, cannot turn the gate off.
func TestADenyIsNotBypassedByAutoApproval(t *testing.T) {
	rules := editorRules(t.TempDir())
	ran := false
	a := &Agent{
		Policy: rules,
		Handlers: map[string]Handler{"bash": func(context.Context, Call, Callbacks) anthropic.ContentBlockParamUnion {
			ran = true
			return ok("t1", "")
		}},
	}

	// Callbacks as a session in auto mode supplies them: approve everything, without asking.
	asked := false
	cb := Callbacks{
		Approve:      func(context.Context, string, string) bool { asked = true; return true },
		Require:      func(context.Context, string, string, string) bool { asked = true; return true },
		OnToolResult: func(string, string, string, ToolKind) {},
	}

	result := a.runTool(context.Background(), bashCall("jj git push"), cb)
	if ran {
		t.Error("the handler ran despite a deny")
	}
	if asked {
		t.Error("a deny was put to the user, who could then approve it")
	}
	if result.OfToolResult == nil || !result.OfToolResult.IsError.Or(false) {
		t.Fatal("a deny did not answer the call with an error result")
	}
}

// An ask must reach the user even in auto mode, which is the other half of the same property:
// Require exists precisely because Approve is short-circuited.
func TestAnAskGoesToRequireNotApprove(t *testing.T) {
	rules := editorRules(t.TempDir())
	ran := false
	a := &Agent{
		Policy: rules,
		Handlers: map[string]Handler{"bash": func(context.Context, Call, Callbacks) anthropic.ContentBlockParamUnion {
			ran = true
			return ok("t1", "")
		}},
		Screen: &fakeScreen{verdict: passing},
	}

	var gotReason string
	required := false
	cb := Callbacks{
		Approve: func(context.Context, string, string) bool {
			t.Error("an ask went to Approve, which auto mode answers")
			return true
		},
		Require: func(_ context.Context, _, _, reason string) bool {
			required, gotReason = true, reason
			return true
		},
		OnToolResult: func(string, string, string, ToolKind) {},
	}

	a.runTool(context.Background(), bashCall("jj describe -m wip"), cb)
	if !required {
		t.Fatal("Require was never called")
	}
	if !ran {
		t.Error("the handler did not run after the ask was granted")
	}
	if !strings.Contains(gotReason, "rewrites history") {
		t.Errorf("reason %q does not carry the rule's own words", gotReason)
	}
}

// A nil policy is the main loop: the policy adds nothing to what its screen decides.
func TestANilPolicyGatesNothing(t *testing.T) {
	ran := false
	a := &Agent{Handlers: map[string]Handler{"bash": func(context.Context, Call, Callbacks) anthropic.ContentBlockParamUnion {
		ran = true
		return ok("t1", "")
	}}, Screen: &fakeScreen{verdict: passing}}
	a.runTool(context.Background(), bashCall("jj git push"), Callbacks{
		Approve:      func(context.Context, string, string) bool { return true },
		OnToolResult: func(string, string, string, ToolKind) {},
	})
	if !ran {
		t.Error("a nil policy blocked a call")
	}
}

// The policy has to cover the fallback, because that is where bash and MCP actually live. A check
// installed only over the handler table would miss them entirely.
func TestThePolicyCoversTheFallbackPath(t *testing.T) {
	ran := false
	a := &Agent{
		Policy: &Rules{},
		Fallback: func(context.Context, Call, Callbacks) anthropic.ContentBlockParamUnion {
			ran = true
			return ok("t1", "")
		},
	}
	a.runTool(context.Background(), bashCall("ls"), Callbacks{OnToolResult: func(string, string, string, ToolKind) {}})
	if ran {
		t.Error("the fallback ran without being gated")
	}
}

// An allow on a bare verb with a deny on a longer form of it reads as a safe override and is not
// one: options written between the two words evade the deny. Refusing the shape at construction
// is the only place it can be caught, since by the time a command arrives it looks legitimate.
func TestValidateRejectsADenyThatOptionsCanEvade(t *testing.T) {
	rules := &Rules{Bash: []Rule{
		{Pattern: "jj", Verdict: VerdictAllow},
		{Pattern: "jj git push", Verdict: VerdictDeny},
	}}
	err := rules.Validate()
	if err == nil {
		t.Fatal("the shadowed deny was accepted")
	}
	if !strings.Contains(err.Error(), "jj git push") {
		t.Errorf("error %q does not name the offending rule", err)
	}

	// The evasion is real, which is why the shape cannot merely be documented against: with the
	// option in the middle, the words are [jj, --repository, /tmp/x, git, push], which the deny's
	// three-word pattern does not prefix — so the one-word allow is the longest match and the push
	// goes through. This asserts the hole exists, and Validate above is what keeps anyone from
	// configuring their way into it.
	if v, _ := decide(t, rules, bashCall("jj --repository /tmp/x git push")); v != VerdictAllow {
		t.Errorf("expected the interleaved option to evade the deny, got %v — if this now denies, "+
			"the matcher has changed and Validate's rejection may no longer be necessary", v)
	}

	// The shape it is pushing the author towards passes.
	fixed := &Rules{Bash: []Rule{{Pattern: "jj status", Verdict: VerdictAllow}, {Pattern: "jj diff", Verdict: VerdictAllow}}}
	if err := fixed.Validate(); err != nil {
		t.Errorf("explicit allows were rejected: %v", err)
	}
}

func TestValidateRejectsAnEmptyPattern(t *testing.T) {
	rules := &Rules{Tools: []Rule{{Pattern: "  ", Verdict: VerdictAllow}}}
	if rules.Validate() == nil {
		t.Error("a pattern matching every call was accepted")
	}
}

func TestASummaryOfAnAllowingDefaultDoesNotClaimEverythingIsRefused(t *testing.T) {
	rules := &Rules{Default: VerdictAllow, Edit: []Rule{{Pattern: "/", Verdict: VerdictDeny}}}
	got := strings.Join(rules.Summary(), "; ")
	if strings.Contains(got, "refused") || !strings.Contains(got, "anything else: allow") {
		t.Errorf("summary misstates the rules: %q", got)
	}
}

// A peer runs with no approval path, and its bash must still run rather than crash the pair.
func TestRunBashRunsWithNoApprovalPath(t *testing.T) {
	res := RunBash(context.Background(), Call{ID: "c", Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)}, Callbacks{})
	if res.OfToolResult == nil || res.OfToolResult.IsError.Or(false) {
		t.Fatalf("bash with no approval path did not run: %+v", res)
	}
}
