package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/project"
	"github.com/anthropics/anthropic-sdk-go"
)

// The coder is the first subagent that changes things. Everything that made the read-only agents
// safe by construction is gone here — it edits files and runs commands — so what bounds it is its
// policy, and the policy is the reason this definition can exist at all.
//
// What it may do: write anywhere in the repository, build, test and inspect its own work.
// What it may not: reach the network, rewrite history, or write outside the repo. Pushing is
// denied by name, one rule per way of spelling it.

// Coder builds the editing agent, or nil when there is no repository to scope it to. A write
// scope that fell back to the working directory would be a scope in name only: run alux
// from the wrong place and it covers the home directory.
//
// mcp may be nil, and the agent then simply has no MCP tools — which is what every subagent had
// before this, since the inner loop sets no fallback of its own.
func Coder(mcp *mcpclient.Manager) *Definition {
	root := project.Root()
	if root == "" || root == string(filepath.Separator) {
		return nil
	}
	return coderDef(root, mcp)
}

// coderDef is Coder with the root supplied, so a test can scope one at a temporary directory.
func coderDef(root string, mcp *mcpclient.Manager) *Definition {
	return scopedCoder(root, project.Cwd(), mcp)
}

// scopedCoder is the coder confined to root and told it starts in cwd.
func scopedCoder(root, cwd string, mcp *mcpclient.Manager) *Definition {
	rules := CoderRules(root)
	return &Definition{
		At:   func(dir string) *Definition { return scopedCoder(dir, dir, mcp) },
		Name: "coder",
		Description: "writes code. Give it a self-contained change and it edits the files, runs the build and the tests, and " +
			"reports what it did — none of the reading, editing or test output costs you context. It works only inside this " +
			"repository, and it cannot commit, push, or reach the network. Best for a change you can state completely: " +
			"'add X to Y', 'make this test pass', 'thread this argument through'. Do it yourself when the change needs what " +
			"you are holding in your head, or when you want to see the diff rather than a description of it.",
		System:   func() string { return coderPrompt(root, cwd, mcp, rules, true) },
		Seeded:   func() string { return coderPrompt(root, cwd, mcp, rules, false) },
		Seedable: true,
		// Rebuilt per run, which is what lets a server that connected after startup show up: the
		// tool list is the rules applied to whatever is connected at dispatch time.
		Tools:    func() []anthropic.ToolUnionParam { return coderTools(mcp, rules) },
		Handlers: coderHandlers(),
		// Strict, not the main agent's: an unmatched name becomes an error rather than a shell
		// command. This is the only route to an MCP tool, whose name is unknown at construction.
		Fallback: agent.McpHandler(mcp),
		Explore:  true,
		Layered:  true,
		Policy:   rules,
		// Edits, builds and tests; recording work is the user's call, and nothing leaves the repository.
		Commands: &guardian.Criteria{Agent: "coder", Allowed: []guardian.Category{guardian.Read, guardian.Build, guardian.Edit}, Threshold: guardian.BuildThreshold},
		// Detached by default, which is also the decision to let it approve its own writes: a run
		// whose turn has ended has nobody to ask, so the dispatch is the approval and the policy
		// is the bound. Every write is in the run's trail. A caller that wants to watch the edits
		// land, or needs the change before its next step, says so with background: false.
		Background: true,
		// No lock: several coders run at once. Two of them editing one file would lose each
		// other's writes, but that is a per-file problem and serialising whole agents is a
		// whole-agent answer to it. The edit tool locks the file it is changing instead, so
		// independent edits proceed and a genuine collision fails loudly on its anchor. See
		// fileLocks in internal/tools/edit.go for what that does and does not cover.
		//
		// Every call's label, like research: a report about code is only as trustworthy as the
		// commands behind it, and the box is where a test that was never run becomes visible.
		Trail: func(_ agent.ToolKind, label, _ string) string { return label },
	}
}

// CoderRules is the coder's capability set, exported so it can be read in a test and printed by
// /agents without reaching inside the definition.
//
// Written as an allow-list of whole commands rather than of verbs. `jj` is not allowed with `jj
// git push` denied on top, because an option written between the two words evades the narrower
// rule — the shape Rules.Validate now rejects outright.
func CoderRules(root string) *agent.Rules {
	return &agent.Rules{
		Bash: bashRules(root),
		// Seatbelt bounds sandboxed writes and network; the allow-list binds outside it.
		AllowSandboxed: true,
		// A monorepo's build command runs in its own package, so the coder has to be able to move within the repo.
		BashRoot: root,
		Edit: []agent.Rule{
			{Pattern: root, Verdict: agent.VerdictAllow},
			// Inside the scope and nothing a file write should ever touch: the change graph is
			// jj's own state, and a stray write there damages what no edit could repair.
			{Pattern: filepath.Join(root, ".jj"), Verdict: agent.VerdictDeny, Reason: "the jj change graph is not a file to edit"},
			{Pattern: filepath.Join(root, ".git"), Verdict: agent.VerdictDeny, Reason: "the git directory is not a file to edit"},
		},
		Tools:         append(agent.AllowTools("glob", "grep"), readOnlyMCP()...),
		DefaultReason: "the coder may only edit this repository, build it, test it and inspect its own work",
	}
}

// readOnlyMCP is the MCP tools the coder may reach: lookups on servers that hold context about
// the work, and nothing that changes anything.
//
// Enumerated per server, by name, and that is the whole design. The MCP spec has a readOnlyHint
// annotation that would seem to answer this for free, and alux does read it — but the spec
// is explicit that annotations "are not guaranteed to provide a faithful description of tool
// behavior" and that clients "should never make tool use decisions based on" them. A server
// volunteering that it is safe is not evidence. So names authorize, and the hint only audits:
// agent.AuditMcp reports anything these rules admit that its own server says is not read-only.
//
// The cost of enumerating is that a server adding a read tool outside these prefixes is invisible
// until someone adds it to "coder_mcp_readonly" in settings.json. That is the right way round: a
// new tool arriving unreachable is a missing feature, and one arriving reachable is a hole.
func readOnlyMCP() []agent.Rule {
	var prefixes []string
	for server, verbs := range loadCoderMCP() {
		for _, verb := range verbs {
			// An empty prefix would admit the whole server.
			if server == "" || verb == "" {
				continue
			}
			prefixes = append(prefixes, agent.McpPrefix+server+"__"+verb)
		}
	}
	// Sorted so the rule order, and therefore /agents' summary, is stable across runs. Map
	// iteration above is deliberate for grouping, not for output.
	slices.Sort(prefixes)
	return agent.AllowTools(prefixes...)
}

// loadCoderMCP reads {"coder_mcp_readonly": {"<server>": ["<prefix>", ...]}}; unusable is none.
func loadCoderMCP() map[string][]string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return nil
	}
	var settings struct {
		CoderMCP map[string][]string `json:"coder_mcp_readonly"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	return settings.CoderMCP
}

const formatChanged = "Run the repository's formatter on the files you changed, naming them, never on `.` or a whole " +
	"directory: a repository with formatting debt would hand back a diff full of files the change never touched."

func coderPrompt(root, cwd string, mcp *mcpclient.Manager, rules *agent.Rules, readFirst bool) string {
	parts := []string{
		"You are a coding subagent. Another agent, working in the user's terminal, has handed you a change it does not want to spend its own context making. You never talk to the user.",
		"Repository root: " + root,
		"Working directory: " + cwd,
		"",
	}
	if readFirst {
		parts = append(parts,
			"Read before you write. Search for the thing you are about to change, read what the search found, and match what is already there — this repository has conventions, and a change that ignores them is a change that gets reverted.",
			"")
	}
	rest := []string{
		"Then verify. A report that the code compiles when it was never built is worse than no report, because it is believed. Build it, run the tests that cover what you touched, and if you could not, say so in exactly those words.",
		"",
		"What you can do, and nothing else:",
		"  " + strings.Join(rules.OfferedSummary(connected(mcp)), "\n  "),
		"",
		readWithView,
		"",
		formatChanged,
		"",
		agent.SharedShell + " To build or test a package that lives elsewhere in the repository, `cd <dir>` once and run its commands from there.",
		"",
		"Delete what you made and no longer need. Writing something throwaway to find an answer is good practice — a test that prints a value, a script that probes a behaviour — but it is yours to remove when it has served its purpose: `rm <file>`, named, one at a time. Anything left behind lands in the user's working copy and reads as part of the change. You can delete a file you could have written; you cannot delete a directory tree, and you should not be trying to.",
		"",
		lookups(mcp, rules),
		"You cannot commit, push, open a pull request, or change anything outside this repository. Those are refusals, not prompts — nothing you can rephrase will get them approved, and there is nobody to ask. Leave the work in the working copy and report it; the user commits.",
		"",
		agent.BatchCalls,
		"",
		"Your final message is the whole report. Name every file you changed and what you changed in it, say what you ran and what it said, and state plainly anything you could not finish or could not verify. Do not describe the code back as prose it already states; describe what is different and why. No preamble, no offering to do more.",
	}
	return strings.Join(append(parts, rest...), "\n")
}

// connected is every MCP tool name on offer right now, which is what narrows the prompt's rule
// summary to this repository's servers. Nil without a manager, and the summary then names no MCP
// at all.
func connected(mcp *mcpclient.Manager) []string {
	if mcp == nil {
		return nil
	}
	var names []string
	for _, info := range mcp.Inventory() {
		names = append(names, info.Name)
	}
	return names
}

// lookups tells the agent what it may read from outside the repository, naming the servers rather
// than the tools — the tool list already carries the names, and what the prompt has to establish
// is that these are reads it is welcome to make and that none of them changes anything.
//
// Empty when nothing is connected, so an agent running without MCP is never told about tools it
// does not have.
func lookups(mcp *mcpclient.Manager, rules *agent.Rules) string {
	if mcp == nil {
		return ""
	}
	var servers []string
	for _, info := range mcp.Inventory() {
		if rules.Permits(info.Name) && !slices.Contains(servers, info.Server) {
			servers = append(servers, info.Server)
		}
	}
	if len(servers) == 0 {
		return ""
	}
	slices.Sort(servers)
	return "You can also look things up through " + strings.Join(servers, ", ") +
		". Only the read-only tools are available to you, so nothing you do there changes anything. Use them when the " +
		"change depends on context the repository does not hold — a ticket's actual requirement, how an API is meant to " +
		"be called. Do not go looking when the task is self-contained.\n"
}

// coderTools is the built-ins plus whichever connected MCP tools the rules permit. Offering only
// the permitted ones is deliberate: an agent shown a tool it may not call will call it, eat a
// refusal and spend a turn learning what the list could have told it.
func coderTools(mcp *mcpclient.Manager, rules *agent.Rules) []anthropic.ToolUnionParam {
	tools := []anthropic.ToolUnionParam{
		{OfTool: agent.BashTool()},
		{OfTextEditor20250728: &anthropic.ToolTextEditor20250728Param{Name: "str_replace_based_edit_tool"}},
		{OfTool: agent.GlobTool()},
		{OfTool: agent.GrepTool()},
		{OfTool: agent.DeclineSuggestionTool()},
		{OfTool: agent.ComposeTool()},
	}
	return append(tools, agent.McpTools(mcp, rules)...)
}

func coderHandlers() map[string]agent.Handler {
	return map[string]agent.Handler{
		"str_replace_based_edit_tool": agent.RunEdit,
		"bash":                        agent.RunBash,
		"glob":                        agent.RunSearch,
		"grep":                        agent.RunSearch,
	}
}
