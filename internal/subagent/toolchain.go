package subagent

import (
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
)

// Which commands the coder may run depends on the repository it was started in, so its bash
// allow-list is composed at construction from the toolchains its marker files show. A monorepo
// holds several, so detection walks a bounded depth and the bundles it finds are unioned: a Go
// service and a web app in one tree license both sets everywhere in it. Precision is not what
// bounds this agent — the verbs and the write scope are — so a `cargo test` that is allowed in
// the Go half of a mixed repo costs nothing but a failed command.

// toolchain is one set of commands, licensed by any one of the marker files naming it.
type toolchain struct {
	markers []string
	bash    []agent.Rule
}

const (
	networkReason = "installing a dependency reaches the network; report what the change needs and let the user add it"
	runReason     = "running a program the agent just wrote walks round the allow-list in one step; build it and test it instead"
)

// Verbs that run code chosen at runtime — `go run`, `cargo run`, `npx`, a bare `python` — are
// absent by design, and named only where a refusal would otherwise read as an oversight.
var toolchains = []toolchain{
	{
		markers: []string{"go.mod", "go.work"},
		bash: []agent.Rule{
			allow("go build"), allow("go test"), allow("go vet"), allow("go fmt"), allow("gofmt"),
			deny("go run", runReason),
			deny("go get", networkReason), deny("go mod tidy", networkReason),
		},
	},
	{
		markers: []string{"Cargo.toml"},
		bash: []agent.Rule{
			allow("cargo build"), allow("cargo test"), allow("cargo check"),
			allow("cargo clippy"), allow("cargo fmt"),
			deny("cargo run", runReason),
			deny("cargo add", networkReason), deny("cargo install", networkReason),
			deny("cargo publish", "a subagent does not publish"),
		},
	},
	{
		markers: []string{"pyproject.toml", "setup.py", "requirements.txt", "tox.ini"},
		bash: []agent.Rule{
			allow("pytest"), allow("python -m pytest"), allow("python3 -m pytest"),
			allow("ruff check"), allow("mypy"), allow("pyright"),
			allow("ruff format"), allow("black"),
			deny("pip install", networkReason), deny("pip3 install", networkReason),
			deny("uv pip install", networkReason), deny("poetry add", networkReason),
		},
	},
	{
		markers: []string{"Gemfile"},
		bash: []agent.Rule{
			allow("rspec"), allow("bundle exec rspec"),
			allow("rubocop"), allow("bundle exec rubocop"),
			allow("bundle exec rake test"),
			deny("bundle install", networkReason), deny("gem install", networkReason),
		},
	},
	{
		markers: []string{"build.gradle", "build.gradle.kts", "settings.gradle", "gradlew"},
		bash: []agent.Rule{
			allow("./gradlew build"), allow("./gradlew test"), allow("./gradlew check"),
			allow("gradle build"), allow("gradle test"), allow("gradle check"),
		},
	},
	{
		markers: []string{"pom.xml"},
		bash: []agent.Rule{
			allow("mvn compile"), allow("mvn test"), allow("mvn verify"),
			deny("mvn deploy", "a subagent does not deploy"),
		},
	},
	// Named targets only. A bare `make` would make the repository's own Makefile the allow-list,
	// which is the boundary this gate exists to hold.
	{
		markers: []string{"Makefile", "makefile", "GNUmakefile"},
		bash: []agent.Rule{
			allow("make build"), allow("make test"), allow("make lint"), allow("make check"),
			allow("make fmt"), allow("make format"), allow("make typecheck"), allow("make vet"),
		},
	},
	{
		markers: []string{"justfile", "Justfile", ".justfile"},
		bash: []agent.Rule{
			allow("just build"), allow("just test"), allow("just lint"),
			allow("just check"), allow("just fmt"),
		},
	},
}

// jsScripts are the script names worth guessing. A script this misses falls to the default and is
// refused, which is a miss rather than a hole.
var jsScripts = []string{
	"build", "check",
	"test", "test:ci",
	"lint", "lint:check", "lint:ci",
	"typecheck", "typecheck:ci", "check:types",
	"format", "fmt", "format:fix", "format:check", "format:ci",
}

// jsWriters are script spellings refused by name, each naming the spelling to use instead: a
// refusal the agent cannot act on is one it retries in another spelling.
var jsWriters = map[string]string{
	"lint:fix": "a fixing lint rewrites files beyond this change; run `lint:check` and fix what it reports",
}

// jsTools are the locally installed binaries worth allowing. Prefix matching admits any flags, so
// the gate bounds what the agent directs, not what the tool can be told to do.
var jsTools = []string{
	"tsc", "eslint", "oxlint", "biome check", "jest", "vitest",
	"prettier", "oxfmt", "biome format",
}

// bashRules is the coder's allow-list for one repository: inspecting its own work, plus whatever
// the detected toolchains license.
func bashRules(root string) []agent.Rule {
	found := scan(root)
	rules := vcsRules()
	for _, t := range toolchains {
		if detected(found, t.markers) {
			rules = append(rules, t.bash...)
		}
	}
	return append(rules, jsRules(jsManager(found))...)
}

// Inspection and refusal are not detected, because what the coder may not do does not depend on
// the repository, and both read sets are harmless wherever they are unavailable.
func vcsRules() []agent.Rule {
	return []agent.Rule{
		allow("jj status"), allow("jj diff"), allow("jj log"), allow("jj show"),
		allow("git status"), allow("git diff"), allow("git log"), allow("git show"),
		allow("chmod"), allow("jj file chmod"),
		allow("tail"), allow("head"), allow("grep"), allow("wc"),
		// Whole verbs, so an option before the subcommand cannot walk round the denies below.
		deny("jj", "a subagent runs only jj status, diff, log, show and file chmod"),
		deny("git", "a subagent runs only git status, diff, log and show"),

		// Every spelling of "send this somewhere" gets its own rule: a shorter rule they could all
		// hang off would be evadable.
		deny("jj git push", "a subagent does not push; report the change and let the user push it"),
		deny("jj git fetch", "a subagent does not touch the remote"),
		deny("git push", "a subagent does not push; report the change and let the user push it"),
		deny("git fetch", "a subagent does not touch the remote"),
		deny("gh pr create", "a subagent does not open pull requests"),
		deny("gh pr merge", "a subagent does not merge"),

		// Rewriting history is the user's call about where a change boundary goes, and that is not a
		// decision to delegate to something working from one paragraph of instruction.
		deny("jj commit", "the user decides when work is committed and where the boundaries go"),
		deny("jj describe", "the user writes the change description"),
		deny("jj new", "the user decides when a new change starts"),
		deny("jj abandon", "abandoning a change rebases descendants that are not visible from here"),
		deny("git commit", "the user decides when work is committed and where the boundaries go"),
	}
}

// jsRules licenses one package manager's scripts, under both spellings, plus the test and lint
// binaries a repository installs locally. Empty for an empty manager.
func jsRules(manager string) []agent.Rule {
	if manager == "" {
		return nil
	}
	rules := []agent.Rule{
		deny("npx", "npx fetches and runs a package chosen at runtime"),
		deny(manager+" install", networkReason),
		deny(manager+" add", networkReason),
		deny(manager+" publish", "a subagent does not publish"),
	}
	if manager == "npm" {
		rules = append(rules, deny("npm i", networkReason))
	}
	// Directly and through the manager, which is how a locally installed binary is reached when it
	// is not on PATH. `<mgr> exec` alone is never allowed: the binary would be chosen at runtime.
	for _, tool := range jsTools {
		rules = append(rules, allow(tool), allow(manager+" exec "+tool))
	}
	for _, script := range jsScripts {
		rules = append(rules, allow(manager+" run "+script), allow(manager+" "+script))
	}
	// Sorted so the rule order, and therefore the prompt's summary, does not shift between runs.
	for _, script := range slices.Sorted(maps.Keys(jsWriters)) {
		rules = append(rules, deny(manager+" run "+script, jsWriters[script]), deny(manager+" "+script, jsWriters[script]))
	}
	return rules
}

// jsManager is the package manager this tree's lockfile names, the shallowest winning — a
// workspace's lockfile sits at its root and the packages under it have none of their own. npm is
// the fallback, because a package.json with no lockfile still has scripts.
func jsManager(found map[string]int) string {
	best, manager := 0, ""
	for _, lock := range []struct{ file, manager string }{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"bun.lock", "bun"},
		{"package-lock.json", "npm"},
	} {
		depth, ok := found[lock.file]
		if ok && (manager == "" || depth < best) {
			best, manager = depth, lock.manager
		}
	}
	if manager == "" && detected(found, []string{"package.json"}) {
		return "npm"
	}
	return manager
}

func detected(found map[string]int, markers []string) bool {
	for _, marker := range markers {
		if _, ok := found[marker]; ok {
			return true
		}
	}
	return false
}

// scanDepth is how many directory levels below the root detection descends. Three reaches the
// shapes that actually occur — packages/web, services/api/pyproject.toml — without walking a
// whole tree at every startup. A package deeper than this is missed, and its commands are then
// refused rather than silently permitted.
const scanDepth = 3

// scanLimit caps directories visited, so a repository with a pathological tree cannot turn
// startup into a filesystem crawl. Reaching it abandons that subtree and keeps what was found.
const scanLimit = 4096

// scan is every marker filename in the tree, mapped to the shallowest depth it was found at.
// Depth is kept because which package manager wins depends on it.
func scan(root string) map[string]int {
	found := map[string]int{}
	visited := 0
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		// An unreadable directory is skipped rather than fatal: detection is a best effort, and a
		// permission error somewhere deep should not cost the coder its whole allow-list.
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fs.SkipDir
		}
		depth := 0
		if rel != "." {
			depth = len(strings.Split(rel, string(filepath.Separator)))
		}

		if !entry.IsDir() {
			if prev, ok := found[entry.Name()]; !ok || depth < prev {
				found[entry.Name()] = depth
			}
			return nil
		}
		if path == root {
			return nil
		}
		visited++
		if visited > scanLimit || depth > scanDepth || skipDir(entry.Name()) {
			return fs.SkipDir
		}
		return nil
	})
	return found
}

// skipDir keeps detection out of trees that hold other people's marker files. A dependency's own
// Cargo.toml is not evidence about this repository, and node_modules alone can be larger than
// everything detection is looking for.
func skipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "target", "dist", "build", "__pycache__", "testdata":
		return true
	}
	return false
}

func allow(pattern string) agent.Rule {
	return agent.Rule{Pattern: pattern, Verdict: agent.VerdictAllow}
}

func deny(pattern, reason string) agent.Rule {
	return agent.Rule{Pattern: pattern, Verdict: agent.VerdictDeny, Reason: reason}
}
