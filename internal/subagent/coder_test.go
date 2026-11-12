package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/sandbox"
)

// coderVerdict asks the coder's own rules about one call, through the same intent decomposition
// the loop uses. Going through a real Agent matters: the classification of a tool name depends on
// the handler table, so testing Rules directly would not exercise what actually happens.
func coderVerdict(t *testing.T, root string, call agent.Call) (agent.Verdict, string) {
	t.Helper()
	def := coderDef(root, nil)
	a := &agent.Agent{Policy: def.Policy, Handlers: def.Handlers}
	return a.Decide(call)
}

func coderBash(t *testing.T, root, command string) (agent.Verdict, string) {
	t.Helper()
	in, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return coderVerdict(t, root, agent.Call{ID: "t1", Name: "bash", Input: in})
}

// repoWith is a root carrying the marker files a toolchain is detected from. A marker may name a
// path, so a monorepo's layout can be written out directly.
func repoWith(t *testing.T, markers ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, marker := range markers {
		path := filepath.Join(root, marker)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func coderWrite(t *testing.T, root, path string) (agent.Verdict, string) {
	t.Helper()
	in, err := json.Marshal(map[string]any{"command": "create", "path": path, "file_text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	return coderVerdict(t, root, agent.Call{ID: "t1", Name: "str_replace_based_edit_tool", Input: in})
}

// The whole point of the agent, stated as a test: it can do the work and it cannot send the work
// anywhere. Both halves fail loudly — a coder that cannot test is useless, and one that can push
// is the thing this was built to prevent.
func TestTheCoderCanBuildAndTestButNotPublish(t *testing.T) {
	root := repoWith(t, "go.mod")

	for _, command := range []string{
		"go build ./...",
		"go test ./...",
		"go test -run TestFoo ./internal/agent",
		"go vet ./...",
		"gofmt -w internal/agent/policy.go",
		"jj status",
		"jj diff --stat",
		"jj log -r @",
		"jj show @",
		"go test ./... 2>&1 | tail -30",
		"go build ./... 2>&1 | grep -v warning | head -20",
	} {
		if v, reason := coderBash(t, root, command); v != agent.VerdictAllow {
			t.Errorf("%q: got %v (%s), want allow", command, v, reason)
		}
	}

	for _, command := range []string{
		"jj git push",
		"jj git push --bookmark feature",
		"jj git fetch",
		"git push origin main",
		"gh pr create --fill",
		"gh pr merge 123",
		"jj commit -m wip",
		"jj describe -m wip",
		"jj new",
		"jj abandon xyz",
		"git commit -am wip",
		"go test ./... 2>&1 | tail -30 > out.log",
		// Never mentioned by any rule, and refused by the default rather than by being thought of.
		"curl https://example.com",
		"npm install",
		"rm -rf /",
		"ssh host",
		"go run ./cmd/anything",
	} {
		v, reason := coderBash(t, root, command)
		if v != agent.VerdictDeny {
			t.Errorf("%q: got %v, want deny", command, v)
			continue
		}
		if reason == "" {
			t.Errorf("%q: refused without saying why", command)
		}
	}
}

// The commands are the repository's, not alux's. A coder that can only run `go test` is
// useless in four repositories out of five, and one that carries every language's verbs into every
// repository has an allow-list that no longer says anything.
func TestTheCodersCommandsFollowTheRepositorysToolchain(t *testing.T) {
	for _, tc := range []struct {
		markers        []string
		allowed, wrong string
	}{
		{[]string{"go.mod"}, "go test ./...", "cargo test"},
		{[]string{"Cargo.toml"}, "cargo test", "go test ./..."},
		{[]string{"pyproject.toml"}, "python -m pytest tests", "cargo test"},
		{[]string{"Gemfile"}, "bundle exec rspec", "go build ./..."},
		{[]string{"pom.xml"}, "mvn test", "gradle test"},
		{[]string{"Makefile"}, "make lint", "make deploy"},
		{[]string{"package.json", "pnpm-lock.yaml"}, "pnpm run build", "npm run build"},
		{[]string{"package.json"}, "npm test", "pnpm test"},
	} {
		root := repoWith(t, tc.markers...)
		if v, reason := coderBash(t, root, tc.allowed); v != agent.VerdictAllow {
			t.Errorf("%v: %q got %v (%s), want allow", tc.markers, tc.allowed, v, reason)
		}
		if v, _ := coderBash(t, root, tc.wrong); v != agent.VerdictDeny {
			t.Errorf("%v: %q got %v, want deny", tc.markers, tc.wrong, v)
		}
	}
}

// In the sandbox the coder runs whatever the repository needs, and the named refusals still hold.
// Asking to leave the sandbox puts the command back under the allow-list.
func TestASandboxedCoderRunsAnythingNotRefused(t *testing.T) {
	root := repoWith(t, "Makefile", "package.json")
	def := coderDef(root, nil)
	a := &agent.Agent{Policy: def.Policy, Handlers: def.Handlers,
		Sandbox: func(context.Context, [][]string) (sandbox.Wrap, error) { return sandbox.Wrap{}, nil }}
	decide := func(command string, sandboxed bool) (agent.Verdict, string) {
		input := map[string]any{"command": command}
		if !sandboxed {
			input["sandbox"] = false
		}
		in, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return a.Decide(agent.Call{ID: "t1", Name: "bash", Input: in})
	}

	for _, command := range []string{
		"make db-migrate",
		"cd api && make lint-check",
		"python manage.py db check",
		"./fixtures/load.sh",
	} {
		if v, reason := decide(command, true); v != agent.VerdictAllow {
			t.Errorf("%q in the sandbox: got %v (%s), want allow", command, v, reason)
		}
		if v, _ := decide(command, false); v != agent.VerdictDeny {
			t.Errorf("%q outside the sandbox: got %v, want deny", command, v)
		}
	}
	for _, command := range []string{
		"jj commit -m wip",
		"jj --ignore-working-copy describe -m wip",
		"git -C . commit -m wip",
		"jj git push",
		"npm install",
		"eval make test",
	} {
		if v, _ := decide(command, true); v != agent.VerdictDeny {
			t.Errorf("%q in the sandbox: got %v, want deny", command, v)
		}
	}
}

// A monorepo is the case detection exists for: several toolchains in one tree, none of them at
// the root, and the coder needs all of their commands.
func TestAMonorepoLicensesEveryToolchainItHolds(t *testing.T) {
	root := repoWith(t,
		"go.work",
		"services/api/go.mod",
		"services/worker/pyproject.toml",
		"pnpm-lock.yaml",
		"packages/web/package.json",
		"packages/native/android/build.gradle",
	)

	for _, command := range []string{
		"go build ./...",
		"cd services/worker && pytest",
		"cd packages/web && pnpm run build",
		"cd packages/native/android && ./gradlew test",
	} {
		if v, reason := coderBash(t, root, command); v != agent.VerdictAllow {
			t.Errorf("%q: got %v (%s), want allow", command, v, reason)
		}
	}

	// The workspace's own lockfile decides the manager, not a nested package.json with none.
	if v, _ := coderBash(t, root, "cd packages/web && npm run build"); v != agent.VerdictDeny {
		t.Errorf("npm was licensed in a pnpm workspace: %v", v)
	}
}

// Detection reads the repository, and a dependency's marker files are not the repository. A
// vendored Cargo.toml licensing `cargo` would be a toolchain nobody in the repo is using.
func TestDetectionIgnoresDependencyTreesAndDeepPackages(t *testing.T) {
	root := repoWith(t,
		"go.mod",
		"node_modules/some-dep/Cargo.toml",
		"a/b/c/d/Gemfile",
	)
	for _, command := range []string{"cargo test", "bundle exec rspec"} {
		if v, _ := coderBash(t, root, command); v != agent.VerdictDeny {
			t.Errorf("%q was licensed from outside the repository's own tree: %v", command, v)
		}
	}
}

// Moving within the repository is how a monorepo's packages are reached. Moving out of it is the boundary.
func TestTheCoderMovesWithinTheRepoButNotOutOfIt(t *testing.T) {
	root := repoWith(t, "go.mod", "services/api/go.mod")
	pinTemp(t)

	for _, command := range []string{"cd services/api && go test ./...", "cd /tmp && go build ./..."} {
		if v, reason := coderBash(t, root, command); v != agent.VerdictAllow {
			t.Errorf("%q was refused: %v (%s)", command, v, reason)
		}
	}
	for _, command := range []string{
		"cd .. && go test ./...",
		"cd ../../etc && go vet ./...",
		"cd ~ && go test ./...",
		"cd $HOME && go test ./...",
		"cd - && go test ./...",
		"cd && go test ./...",
	} {
		v, reason := coderBash(t, root, command)
		if v != agent.VerdictDeny {
			t.Errorf("%q: got %v, want deny", command, v)
			continue
		}
		if reason == "" {
			t.Errorf("%q: refused without saying why", command)
		}
	}
}

// A symlink inside the repo pointing out of it is the way a contained-looking path escapes, and
// the same resolution that protects the write scope has to protect this one.
func TestAMoveThroughASymlinkOutOfTheRepoIsRefused(t *testing.T) {
	root := repoWith(t, "go.mod")
	outside := t.TempDir()
	pinTemp(t)
	if err := os.Symlink(outside, filepath.Join(root, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if v, _ := coderBash(t, root, "cd elsewhere && go test ./..."); v != agent.VerdictDeny {
		t.Errorf("a move through a symlink out of the repo was allowed: %v", v)
	}
}

// pinTemp points $TMPDIR at a fresh directory, so the test's own directories stop counting as temp.
func pinTemp(t *testing.T) {
	t.Helper()
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	if resolved, _ := filepath.EvalSymlinks(scratch); strings.HasPrefix(resolved, "/private/tmp/") || strings.HasPrefix(resolved, "/tmp/") {
		t.Skip("the test's own directories are under /tmp here")
	}
}

// A coder that cannot lint or format hands back work that fails CI and costs the context it was saving.
func TestTheCoderCanCheckAndFormatItsWork(t *testing.T) {
	js := repoWith(t, "package.json", "pnpm-lock.yaml")
	py := repoWith(t, "pyproject.toml")

	for _, tc := range []struct{ root, command string }{
		{js, "pnpm lint"},
		{js, "pnpm run lint:check"},
		{js, "pnpm typecheck"},
		{js, "pnpm run check:types"},
		{js, "pnpm format:ci"},
		{js, "pnpm run test:ci"},
		// Locally installed binaries are not on PATH, so the manager is how they are reached.
		{js, "pnpm exec oxlint ."},
		{js, "pnpm exec tsc -p tsconfig.json --noEmit"},
		{js, "pnpm exec prettier --check ."},
		{py, "ruff check ."},
		{py, "black --check ."},
		{js, "pnpm format"},
		{js, "pnpm run format"},
		{js, "pnpm exec prettier --write src/a.ts"},
		{js, "pnpm exec oxfmt src/a.ts"},
		{py, "black src/a.py"},
		{py, "ruff format src/a.py"},
	} {
		if v, reason := coderBash(t, tc.root, tc.command); v != agent.VerdictAllow {
			t.Errorf("%q: got %v (%s), want allow", tc.command, v, reason)
		}
	}

	// The refusal has to name the spelling that works, or the agent retries blind.
	v, reason := coderBash(t, js, "pnpm lint:fix")
	if v != agent.VerdictDeny || !strings.Contains(reason, "lint:check") {
		t.Errorf("pnpm lint:fix: got %v (%s), want deny naming lint:check", v, reason)
	}
}

// Detection widens what the coder may run, and these are the two things it must never widen to:
// installing a dependency reaches the network, and running a program the agent just wrote is how
// an allow-list gets walked round in one step.
func TestADetectedToolchainBuysNoNetworkAndNoArbitraryRun(t *testing.T) {
	for _, tc := range []struct {
		markers []string
		refused []string
	}{
		{[]string{"go.mod"}, []string{"go run ./cmd/x", "go get example.com/x", "go mod tidy"}},
		{[]string{"Cargo.toml"}, []string{"cargo run", "cargo add serde", "cargo install x"}},
		{[]string{"pyproject.toml"}, []string{"pip install requests", "python main.py"}},
		{[]string{"package.json"}, []string{"npm install", "npm i left-pad", "npx tsc", "node main.js"}},
	} {
		root := repoWith(t, tc.markers...)
		for _, command := range tc.refused {
			v, reason := coderBash(t, root, command)
			if v != agent.VerdictDeny {
				t.Errorf("%v: %q got %v, want deny", tc.markers, command, v)
				continue
			}
			if reason == "" {
				t.Errorf("%v: %q refused without saying why", tc.markers, command)
			}
		}
	}
}

// A refusal the agent cannot understand gets retried in a different spelling. Each denial the
// coder is likely to actually attempt says what to do instead.
func TestTheCodersRefusalsExplainThemselves(t *testing.T) {
	root := t.TempDir()
	for command, want := range map[string]string{
		"jj git push":    "report the change",
		"jj commit -m x": "the user decides",
		"gh pr create":   "pull request",
	} {
		_, reason := coderBash(t, root, command)
		if !strings.Contains(reason, want) {
			t.Errorf("%q: reason %q does not contain %q", command, reason, want)
		}
	}
}

// The scope is the repository, and two directories inside it are still off limits: the change
// graph is not a file, and a stray write there damages what no edit could repair.
func TestTheCoderWritesInTheRepoButNotItsChangeGraph(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".jj", ".git", "internal"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if v, reason := coderWrite(t, root, filepath.Join(root, "internal", "x.go")); v != agent.VerdictAllow {
		t.Errorf("a write in the repo was refused: %v (%s)", v, reason)
	}
	for _, path := range []string{
		filepath.Join(root, ".jj", "repo", "op_heads"),
		filepath.Join(root, ".git", "config"),
		"/etc/hosts",
		filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"),
		filepath.Join(root, "..", "sibling.go"),
	} {
		if v, _ := coderWrite(t, root, path); v != agent.VerdictDeny {
			t.Errorf("a write to %s was allowed", path)
		}
	}
}

// Reading is not what the scope protects, and a coder that cannot read outside the repo cannot
// look at a dependency to see how it is meant to be used.
func TestTheCoderMayReadOutsideItsWriteScope(t *testing.T) {
	root := t.TempDir()
	in, err := json.Marshal(map[string]any{"command": "view", "path": "/etc/hosts"})
	if err != nil {
		t.Fatal(err)
	}
	if v, reason := coderVerdict(t, root, agent.Call{ID: "t1", Name: "str_replace_based_edit_tool", Input: in}); v != agent.VerdictAllow {
		t.Errorf("a read outside the repo was refused: %v (%s)", v, reason)
	}
}

// The coder's rules must survive the check that rejects an evadable narrowing — allow `jj`, deny
// `jj git push`, which an interleaved option walks straight through.
//
// A bundle is only as valid as the company it keeps: a deny in one that extends an allow in
// another is the same evadable narrowing. No single repository detects them all, so this loads
// them all — and every package manager, which detection picks exactly one of.
func TestTheCodersRulesAreValid(t *testing.T) {
	var markers []string
	for _, tc := range toolchains {
		markers = append(markers, tc.markers...)
	}
	root := repoWith(t, append(markers, "package.json")...)

	if err := CoderRules(root).Validate(); err != nil {
		t.Fatalf("the coder's own rules are invalid: %v", err)
	}
	for _, manager := range []string{"npm", "pnpm", "yarn", "bun"} {
		rules := &agent.Rules{Bash: append(bashRules(root), jsRules(manager)...)}
		if err := rules.Validate(); err != nil {
			t.Errorf("the %s rules are invalid: %v", manager, err)
		}
	}
	if _, err := NewManager(NewRoster(nil), coderDef(root, nil)); err != nil {
		t.Fatalf("the coder was rejected at construction: %v", err)
	}
}

// Detached by default, and the consequence is deliberate: a detached run approves its own
// writes, so delegating a change no longer puts each edit in front of the user first. What
// bounds it is the policy, and what makes it visible is the run's trail.
//
// Asserted rather than left implicit because this is the posture decision, not an incidental
// field. Anyone flipping it back is choosing to make the coder block its caller again.
func TestTheCoderIsDetachedByDefault(t *testing.T) {
	if !coderDef(t.TempDir(), nil).Background {
		t.Error("the coder blocks its caller by default")
	}
}

// The coder's writes reach the user through RunEdit's own cb.Approve, which no policy rule
// governs — so Policy.CanAsk() is false and the detachable() check does not fire. That is the
// whole reason auto-approval had to be built: detaching the coder is allowed, and would
// otherwise have parked an approval on a turn that had ended.
func TestTheCoderMayBeDetached(t *testing.T) {
	if err := coderDef(t.TempDir(), nil).detachable(); err != nil {
		t.Errorf("the coder cannot be detached: %v", err)
	}
}

// Several coders at once is the point of dropping the slot, so the absence of a lock is the
// assertion. What keeps concurrent edits safe is per-file locking in the edit tool, which is
// tested where it lives: TestConcurrentEditsToOneFileDoNotLoseEachOther in internal/tools.
func TestCodersAreNotSerialisedWithEachOther(t *testing.T) {
	if coderDef(t.TempDir(), nil).Lock != nil {
		t.Error("the coder still takes a whole-agent lock, so only one runs at a time")
	}
	if pairedCoderDef(t.TempDir(), nil).Lock != nil {
		t.Error("the pair still takes a whole-agent lock")
	}
}

// The prompt is built from the rules rather than describing them separately, so the two cannot
// drift apart: a verb added to the allow-list appears in the prompt without anyone editing prose.
func TestTheCodersPromptIsBuiltFromItsRules(t *testing.T) {
	root := repoWith(t, "go.mod")
	prompt := coderDef(root, nil).System()
	for _, want := range []string{"go test", "jj diff", root} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not mention %q", want)
		}
	}
	// And it says the refusals are refusals. An agent that thinks a denial is a prompt it could
	// get approved spends its budget rephrasing.
	for _, want := range []string{"refusals, not prompts", "nobody to ask"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not tell the agent %q", want)
		}
	}
}

func TestOnlyASeededDriversPromptDropsReadBeforeYouWrite(t *testing.T) {
	root := repoWith(t, "go.mod")
	const line = "Read before you write."
	pair := pairedCoderDef(root, nil)
	for name, prompt := range map[string]string{"coder": coderDef(root, nil).System(), "unseeded pair": pair.System()} {
		if !strings.Contains(prompt, line) {
			t.Errorf("the %s prompt lost %q", name, line)
		}
	}
	seeded := pair.Peer.Seeded()
	if strings.Contains(seeded, line) {
		t.Errorf("a seeded driver is still told %q", line)
	}
	for _, want := range []string{"Then verify.", "You are driving a pair.", root} {
		if !strings.Contains(seeded, want) {
			t.Errorf("the seeded prompt lost %q", want)
		}
	}
}

// coderMCPSettings points HOME at a settings.json holding this coder_mcp_readonly block.
func coderMCPSettings(t *testing.T, block string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, paths.Config)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"coder_mcp_readonly": ` + block + `}`)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

const lookupServers = `{"notes": ["notes_list", "notes_read"], "linear": ["list_", "get_", "search_"],
	"glean": ["search", "read_"], "context7": ["resolve-library-id", "get-library-docs"]}`

func offeredNames(def *Definition) []string {
	var offered []string
	for _, tool := range def.Tools() {
		if tool.OfTool != nil {
			offered = append(offered, tool.OfTool.Name)
		}
	}
	return offered
}

// Unconfigured, the coder reaches no MCP tool, however read-only its server says it is; an empty
// prefix would admit a whole server, so it admits nothing.
func TestTheCoderReachesOnlyConfiguredMcpPrefixes(t *testing.T) {
	mcp := mcpclient.New(nil)
	mcp.AddForTest("linear", "list_issues", true)
	mcp.AddForTest("notion", "update_page", false)

	if offered := offeredNames(coderDef(t.TempDir(), mcp)); slices.ContainsFunc(offered, func(n string) bool { return strings.HasPrefix(n, "mcp__") }) {
		t.Errorf("an MCP tool was offered with no coder_mcp_readonly: %v", offered)
	}
	coderMCPSettings(t, `{"notion": [""], "linear": ["get_"]}`)
	if offered := offeredNames(coderDef(t.TempDir(), mcp)); slices.ContainsFunc(offered, func(n string) bool { return strings.HasPrefix(n, "mcp__") }) {
		t.Errorf("a tool outside the configured prefixes was offered: %v", offered)
	}
	coderMCPSettings(t, `{"linear": ["list_"]}`)
	if offered := offeredNames(coderDef(t.TempDir(), mcp)); !slices.Contains(offered, "mcp__linear__list_issues") {
		t.Errorf("a configured prefix was not offered: %v", offered)
	}
}

// The coder reaches read-only MCP tools and nothing else on the same server. Both halves are the
// point: a coder with no Linear cannot read the ticket it is implementing, and one with all of
// Linear can close it.
func TestTheCoderGetsReadOnlyMcpToolsOnly(t *testing.T) {
	coderMCPSettings(t, lookupServers)
	mcp := mcpclient.New(nil)
	mcp.AddForTest("linear", "list_issues", true)
	mcp.AddForTest("linear", "get_issue", true)
	mcp.AddForTest("linear", "update_issue", false)
	mcp.AddForTest("notes", "notes_read_artifact", true)
	mcp.AddForTest("notes", "notes_update_artifact", false)

	def := coderDef(t.TempDir(), mcp)

	var offered []string
	for _, tool := range def.Tools() {
		if tool.OfTool != nil {
			offered = append(offered, tool.OfTool.Name)
		}
	}
	for _, want := range []string{"mcp__linear__list_issues", "mcp__linear__get_issue", "mcp__notes__notes_read_artifact"} {
		if !slices.Contains(offered, want) {
			t.Errorf("%s was not offered: %v", want, offered)
		}
	}
	for _, unwanted := range []string{"mcp__linear__update_issue", "mcp__notes__notes_update_artifact"} {
		if slices.Contains(offered, unwanted) {
			t.Errorf("%s was offered", unwanted)
		}
	}

	// And the rules agree with the list, so a tool withheld is also a tool refused — the two must
	// not be able to disagree.
	a := &agent.Agent{Policy: def.Policy, Handlers: def.Handlers, Fallback: def.Fallback}
	if v, _ := a.Decide(agent.Call{ID: "t1", Name: "mcp__linear__update_issue"}); v != agent.VerdictDeny {
		t.Errorf("update_issue decided %v, want deny", v)
	}
	if v, _ := a.Decide(agent.Call{ID: "t1", Name: "mcp__linear__list_issues"}); v != agent.VerdictAllow {
		t.Errorf("list_issues decided %v, want allow", v)
	}
}

// Giving a subagent MCP must not give it a shell. The main agent's fallback routes every
// unrecognised name to bash, so inheriting it would mean "give it Linear" quietly meant "give it
// bash under any name the model invents".
func TestTheCodersFallbackIsStrictNotBash(t *testing.T) {
	def := coderDef(t.TempDir(), mcpclient.New(nil))
	if def.Fallback == nil {
		t.Fatal("the coder has no fallback, so it can never reach an MCP tool")
	}

	input, err := json.Marshal(map[string]any{"command": "echo pwned"})
	if err != nil {
		t.Fatal(err)
	}
	result := def.Fallback(context.Background(), agent.Call{ID: "t1", Name: "not_a_tool", Input: input},
		agent.Callbacks{
			Approve:      func(context.Context, string, string) bool { return true },
			OnToolResult: func(string, string, string, agent.ToolKind) {},
		})
	if result.OfToolResult == nil || !result.OfToolResult.IsError.Or(false) {
		t.Error("an unknown tool name was not refused by the coder's fallback")
	}
}

// Nothing about the coder may require MCP. Every other subagent runs without it, and so does the
// coder when no servers are configured.
func TestTheCoderWorksWithoutMcp(t *testing.T) {
	def := coderDef(t.TempDir(), nil)
	if len(def.Tools()) != 6 {
		t.Errorf("tools = %d, want the six built-ins", len(def.Tools()))
	}
	if strings.Contains(def.System(), "look things up") {
		t.Error("the prompt offers lookups that do not exist")
	}
	if _, err := NewManager(NewRoster(nil), def); err != nil {
		t.Errorf("a coder without MCP was rejected: %v", err)
	}
}

// The prompt must not promise what the agent cannot do, nor stay silent about what it can. With
// servers connected it names them; the old wording claimed it could not reach the network at all,
// which stopped being true the moment it could call one.
func TestTheCodersPromptNamesItsLookupServers(t *testing.T) {
	coderMCPSettings(t, lookupServers)
	mcp := mcpclient.New(nil)
	mcp.AddForTest("linear", "list_issues", true)
	mcp.AddForTest("linear", "update_issue", false)

	prompt := coderDef(t.TempDir(), mcp).System()
	if !strings.Contains(prompt, "linear") {
		t.Error("the prompt does not say linear is available")
	}
	if strings.Contains(prompt, "reach the network") {
		t.Error("the prompt still claims it cannot reach the network")
	}
}

// The prompt's rule summary is scoped to the servers this repository actually has. The static
// allow-list names prefixes for linear, glean and context7 as well as notes, and printing all of
// them to an agent that was offered only notes's tools contradicts its own tool list — a coder that
// believes the summary spends a turn calling a tool that is not there. /agents keeps the full list,
// because "allowed but not connected" is the answer a person debugging a missing lookup needs.
func TestTheCodersPromptSummarisesOnlyConnectedServers(t *testing.T) {
	coderMCPSettings(t, lookupServers)
	mcp := mcpclient.New(nil)
	mcp.AddForTest("notes", "notes_read_artifact", true)

	def := coderDef(t.TempDir(), mcp)

	prompt := def.System()
	if !strings.Contains(prompt, "mcp__notes__notes_read") {
		t.Error("the prompt does not summarise the notes rules, which are connected")
	}
	for _, absent := range []string{"mcp__linear__", "mcp__glean__", "mcp__context7__"} {
		if strings.Contains(prompt, absent) {
			t.Errorf("the prompt summarises %s, which no connected server serves", absent)
		}
	}

	m, err := NewManager(NewRoster(nil), def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Catalogue(), "mcp__linear__") {
		t.Error("/agents dropped a configured rule; it reports what is allowed, not what is plugged in")
	}
}

// A withheld tool must not survive in the prompt's summary either. The rules allow the notes_read
// family by prefix, so a server offering only writes leaves that prefix matching nothing — and
// advertising it would promise a capability the tool list refuses.
func TestTheCodersPromptDropsRulesWithNoPermittedTool(t *testing.T) {
	coderMCPSettings(t, lookupServers)
	mcp := mcpclient.New(nil)
	mcp.AddForTest("notes", "notes_update_artifact", false)

	prompt := coderDef(t.TempDir(), mcp).System()
	if strings.Contains(prompt, "mcp__notes__") {
		t.Error("the prompt summarises notes rules, but every connected notes tool is refused")
	}
}

// Adding a server to the config wires it to the main agent and to no subagent at all, because the
// allow-list is enumerated by name. Nothing else notices: the main agent is ungated, so the
// session looks normal while every subagent silently lacks the lookup. This is the only thing that
// says so, and the server's own read-only claim must not buy it a pass — if the hint cannot grant
// reachability it cannot excuse the absence of it either.
func TestTheAuditReportsAServerNoSubagentReaches(t *testing.T) {
	coderMCPSettings(t, lookupServers)
	mcp := mcpclient.New(nil)
	mcp.AddForTest("notion", "search_pages", true)
	mcp.AddForTest("notion", "get_page", true)
	mcp.AddForTest("notes", "notes_read_artifact", true)

	m, err := NewManager(NewRoster(nil), Coder(mcp))
	if err != nil {
		t.Fatal(err)
	}

	found := m.AuditTools(mcp)
	if len(found) != 1 {
		t.Fatalf("audit found %d things, want only the unreached server: %v", len(found), found)
	}
	if !strings.Contains(found[0], "notion") || !strings.Contains(found[0], "2 tools") {
		t.Errorf("%q does not name the server and how much is out of reach", found[0])
	}
	// notes is wired up, so it must not appear — a warning that fires for the healthy case is one
	// nobody reads.
	if strings.Contains(found[0], "notes") {
		t.Errorf("%q complains about a server the coder can reach", found[0])
	}
}

// A server the rules reach in part is not a finding. The coder's list names read prefixes and
// deliberately leaves the writes out, so every wired server has unreachable tools by design —
// reporting those would mean the warning fires constantly and says nothing.
func TestTheAuditIsQuietAboutAPartlyReachedServer(t *testing.T) {
	coderMCPSettings(t, lookupServers)
	mcp := mcpclient.New(nil)
	mcp.AddForTest("notes", "notes_read_artifact", true)
	mcp.AddForTest("notes", "notes_update_artifact", false)

	m, err := NewManager(NewRoster(nil), Coder(mcp))
	if err != nil {
		t.Fatal(err)
	}
	if found := m.AuditTools(mcp); len(found) != 0 {
		t.Errorf("audit complained about a server that is wired up: %v", found)
	}
}

// Agents with no rules at all are not evidence that a server is unreachable — they have no MCP
// either way. With nothing gated the finding would describe the roster rather than the servers, so
// it is withheld.
func TestTheAuditDoesNotReportUnreachedServersWithoutAGatedAgent(t *testing.T) {
	mcp := mcpclient.New(nil)
	mcp.AddForTest("notion", "search_pages", true)

	m, err := NewManager(NewRoster(nil), echo(false))
	if err != nil {
		t.Fatal(err)
	}
	if found := m.AuditTools(mcp); len(found) != 0 {
		t.Errorf("audit reported %v, but no agent is gated for it to be missing from", found)
	}
}

// End to end through the real loop: the coder asks to push, and what comes back is the refusal
// rather than a shell running it. This is the one test that would catch the policy being wired to
// the definition but never reaching the agent that runs the tools.
func TestTheCoderIsRefusedThroughTheRealLoop(t *testing.T) {
	root := t.TempDir()
	def := coderDef(root, nil)
	def.Model = func() string { return "mock" }

	m, server := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "jj git push"}}}},
		mockapi.Turn{Text: "I could not push, as expected."},
	)

	// Waited for on purpose. The coder detaches by default now, and this test is about the policy
	// reaching the agent rather than about how the run is scheduled — a detached dispatch would
	// return a promise and assert against a request that had not been made yet.
	wait := false
	report, err := m.DispatchWith(context.Background(), "coder", "push the branch", "", &wait, agent.Callbacks{
		Approve:      func(context.Context, string, string) bool { return true },
		OnToolResult: func(string, string, string, agent.ToolKind) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report == "" {
		t.Fatal("no report came back")
	}

	last, err := json.Marshal(server.LastRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(last), "Refused") {
		t.Errorf("the refusal never reached the agent:\n%s", last)
	}
	// Approving it must not help: the refusal is structural, so auto mode cannot wave it through
	// and neither can a user who says yes.
	if strings.Contains(string(last), "[exit ") {
		t.Error("the command ran")
	}
}

// A refused command reports no result, so without this the run's box would show nothing of it.
func TestARefusedCommandIsNamedInTheTrail(t *testing.T) {
	root := t.TempDir()
	def := coderDef(root, nil)
	def.Model = func() string { return "mock" }
	m, _ := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "jj git push"}}}},
		mockapi.Turn{Text: "I could not push."},
	)

	wait := false
	if _, err := m.DispatchWith(context.Background(), "coder", "push the branch", "", &wait, agent.Callbacks{
		Approve: func(context.Context, string, string) bool { return true },
	}); err != nil {
		t.Fatal(err)
	}

	trail := m.Roster.Runs()[0].TrailText()
	if !strings.Contains(trail, "not run: $ jj git push — Refused:") {
		t.Errorf("the trail does not name the refused command:\n%s", trail)
	}
	if strings.Count(trail, "jj git push") != 1 {
		t.Errorf("the refused command is in the trail more than once:\n%s", trail)
	}
}

// Probing a behaviour by writing something throwaway is good practice, and an agent that cannot
// then remove it leaves the probe in the user's working copy where it reads as part of the
// change. Relative as well as absolute, because that is how a file in the shell's directory gets named.
func TestTheCoderDeletesItsOwnScratchFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"rm " + filepath.Join(root, "internal", "zz_scratch_test.go"),
		"rm internal/zz_scratch_test.go",
		"rm -f internal/zz_scratch_test.go",
		"rm -- internal/zz_scratch_test.go",
		"rm internal/one_test.go internal/two_test.go",
	} {
		if v, reason := coderBash(t, root, command); v != agent.VerdictAllow {
			t.Errorf("%q was refused: %v (%s)", command, v, reason)
		}
	}
}

// The central claim of the design: deletion is fenced by the Edit rules, not by BashRoot, because
// deleting a file is the most complete way of writing to it. So every path the coder may not write
// to is a path it may not delete — asserted against coderWrite rather than restated, so the two
// gates cannot drift apart without this failing.
func TestDeletingIsFencedExactlyWhereWritingIs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".jj", ".git", "internal"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, path := range []string{
		filepath.Join(root, ".jj", "repo", "op_heads"),
		filepath.Join(root, ".git", "config"),
		"/etc/hosts",
		filepath.Join(root, "..", "sibling.go"),
	} {
		write, _ := coderWrite(t, root, path)
		remove, _ := coderBash(t, root, "rm "+path)
		if write != agent.VerdictDeny {
			t.Fatalf("premise broken: writing %s is no longer refused, so this test proves nothing", path)
		}
		if remove != write {
			t.Errorf("writing %s is %v but deleting it is %v: the two fences have drifted", path, write, remove)
		}
	}
}

// A tree is the one deletion whose blast radius is not visible in the command: the fence can say
// the directory is writable and not that it holds a week of someone's work. Every spelling,
// because clustered flags are the same request and listing them one by one is how one gets missed.
func TestRecursiveDeletionIsRefusedInEverySpelling(t *testing.T) {
	root := t.TempDir()
	for _, flag := range []string{"-r", "-R", "-rf", "-fr", "-vrf", "-f -r", "--recursive"} {
		command := "rm " + flag + " " + filepath.Join(root, "internal")
		v, reason := coderBash(t, root, command)
		if v != agent.VerdictDeny {
			t.Errorf("%q was allowed", command)
			continue
		}
		// The refusal has to name what to do instead. A reason an agent cannot act on is one it
		// retries in another spelling, which is the whole argument for reading the cluster.
		if !strings.Contains(reason, "by name") {
			t.Errorf("%q was refused without naming the alternative: %s", command, reason)
		}
	}
}

// Three ways a deletion could read as contained and not be. The symlink is the reason the final
// path element is deliberately left unresolved: rm removes the link rather than following it, so
// a link outside the fence pointing inside it is a deletion outside the fence.
func TestDeletionCannotBeSmuggledPastTheFence(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	inside := filepath.Join(root, "kept.go")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "link.go")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}

	for name, command := range map[string]string{
		"a glob the shell expands before anything can check it": "rm " + filepath.Join(root, "*.go"),
		"a link outside the fence that points inside it":        "rm " + link,
		"one allowed path carrying one that is not":             "rm " + inside + " " + filepath.Join(outside, "theirs.go"),
	} {
		if v, _ := coderBash(t, root, command); v != agent.VerdictDeny {
			t.Errorf("%s was allowed: %q", name, command)
		}
	}
}

func TestTheCoderSetsModesOnlyInsideItsRepo(t *testing.T) {
	root := repoWith(t, "go.mod", "greet.sh")
	outside := repoWith(t, "theirs.sh")
	link := filepath.Join(root, "escape.sh")
	if err := os.Symlink(filepath.Join(outside, "theirs.sh"), link); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"chmod +x greet.sh",
		"chmod -x greet.sh",
		"chmod u+x,go-w greet.sh",
		"chmod 755 " + filepath.Join(root, "greet.sh"),
		"jj file chmod x greet.sh",
		"jj file chmod n greet.sh",
	} {
		if v, reason := coderBash(t, root, command); v != agent.VerdictAllow {
			t.Errorf("%q was refused: %v (%s)", command, v, reason)
		}
	}
	for _, command := range []string{
		"chmod +x /etc/passwd",
		"chmod +x ../theirs.sh",
		"chmod +x " + link,
		"chmod +x greet.sh /etc/passwd",
		"chmod +x " + filepath.Join(root, ".jj", "repo"),
		"chmod -R +x " + root,
		"chmod +x -R " + root,
		"chmod u+s greet.sh",
		"chmod 4755 greet.sh",
		"chmod --reference=/etc/passwd greet.sh",
		"chmod +x *.sh",
		"chmod +x",
		"jj --repository /tmp/x file chmod x greet.sh",
	} {
		if v, _ := coderBash(t, root, command); v != agent.VerdictDeny {
			t.Errorf("%q was %v, want deny", command, v)
		}
	}
	if v, _ := (&agent.Rules{Edit: []agent.Rule{{Pattern: root, Verdict: agent.VerdictAllow}}, BashRoot: root}).Decide(
		agent.Intent{Class: agent.ClassBash, Commands: [][]string{{"chmod", "+x", "greet.sh"}}}); v != agent.VerdictDeny {
		t.Errorf("chmod with no bash rule allowing it was %v, want deny", v)
	}
}
