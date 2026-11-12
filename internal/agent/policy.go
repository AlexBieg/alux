package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/tools"
)

// Capability gating. A Policy decides, for one tool call, whether it runs — and it is consulted
// in runTool, the single place every tool call of every agent passes through, so one check covers
// the main agent, every subagent, the handler table and the fallback alike.
//
// Two properties are the whole design, and both exist because the alternative was a deny-list:
//
//   - It is an allow-list. The zero Verdict is Deny, so a Rules with nothing in it refuses
//     everything rather than permitting everything. An empty rule set left somewhere by accident
//     is then a loud failure instead of a silent hole.
//   - It fails closed on anything it cannot read. A bash command carrying command substitution,
//     eval, a redirection to a file or an unbalanced quote is not matched against a list of forbidden
//     shapes — it is refused for being undecomposable. That is the difference between this and a
//     pattern blocklist, which `eval "$CMD"` walks straight through.
//
// What this is not: a sandbox. An allow-list bounds what the agent directs, not what the code it
// runs may do: `go test` runs arbitrary code and a Makefile target can push. Containing that is
// internal/sandbox's job, which runs shell commands under Seatbelt when the agent has one.

// Verdict is what a Policy decides. Deny is zero so that an unset verdict, and an unset Rules,
// refuse; and the constants ascend by permissiveness, which is what lets restrictiveness across
// several commands in one line be combined with min.
type Verdict int

const (
	VerdictDeny Verdict = iota
	VerdictAsk
	VerdictAllow
)

func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictAsk:
		return "ask"
	default:
		return "deny"
	}
}

// Class is which rule list an intent is matched against. It is not derivable from the tool name
// alone: an unrecognised name reaches bash through the fallback, so what gates it is decided from
// the dispatch table and not from the call.
type Class int

const (
	ClassTool Class = iota
	ClassBash
	ClassEdit
)

// The two tool names the policy has to understand structurally, because for these the name does
// not say what the call will do.
const (
	bashTool   = "bash"
	editorTool = "str_replace_based_edit_tool"
	// backgroundTool starts a shell command that outlives the call, so it is a shell command to gate.
	backgroundTool = "run_background"
	// pollTool reruns a shell command on an interval, so it is gated as that command.
	pollTool = "poll"
)

// shellTool reports a tool whose command argument is a shell command to gate.
func shellTool(name string) bool {
	return name == bashTool || name == backgroundTool || name == pollTool
}

// Intent is one tool call decomposed far enough to decide on. It carries the label and body too,
// so a verdict of Ask can show the user the same thing the handler would have.
type Intent struct {
	Tool  string
	Class Class

	// Commands is every command the line will run, each as its words with quotes and escapes
	// resolved. Set for ClassBash only.
	Commands [][]string
	// Opaque says why the command could not be decomposed, and is empty when it could. A
	// non-empty Opaque is a refusal on its own: the policy will not guess at a line it cannot
	// read.
	Opaque string
	// Sandboxed says the command will run under the agent's sandbox. Set for ClassBash only.
	Sandboxed bool
	// Dir is the shell's live directory, which relative paths resolve against.
	Dir string
	// Root is where the agent's shell started, and the fence for cd when the rules set no BashRoot.
	Root string

	// Path and Writes describe an editor call. A view is a read, and reads are not gated here.
	Path   string
	Writes bool

	Label string
	Body  string
}

// Rule is one entry in an allow-list. Pattern matches a prefix in every class — the words of a
// command, the leading directories of a path, the start of a tool name — and across all three the
// longest matching pattern wins, so a specific rule beats a general one however they are ordered.
type Rule struct {
	Pattern string
	Verdict Verdict
	// Reason is shown to the agent when this rule refuses, and to the user when it asks. A
	// refusal the agent cannot understand gets retried.
	Reason string
}

// Rules is a capability set: what an agent may do, by class. Default decides anything no rule
// matches, and is Deny unless it is set otherwise.
type Rules struct {
	Bash  []Rule
	Edit  []Rule
	Tools []Rule

	// BashRoot is the tree a command may move within, besides the temp directories. Empty means
	// the directory the shell starts in.
	BashRoot string

	Default Verdict
	// DefaultReason explains Default when it refuses. Empty gets a generic sentence.
	DefaultReason string
	// AllowSandboxed makes Allow the default for a bash command that runs in the sandbox.
	AllowSandboxed bool
}

// AllowTools builds the common case: a subagent whose named tools are simply its tools, with no
// distinctions to draw inside any of them.
func AllowTools(names ...string) []Rule {
	out := make([]Rule, 0, len(names))
	for _, name := range names {
		out = append(out, Rule{Pattern: name, Verdict: VerdictAllow})
	}
	return out
}

// Decide is the verdict for one intent, with the reason the deciding rule carried.
func (r *Rules) Decide(in Intent) (Verdict, string) {
	switch in.Class {
	case ClassBash:
		return r.decideBash(in)
	case ClassEdit:
		return r.decideEdit(in)
	default:
		verdict, reason := r.fallback()
		return matchName(r.Tools, in.Tool, verdict, reason)
	}
}

func (r *Rules) fallback() (Verdict, string) {
	reason := r.DefaultReason
	if reason == "" {
		reason = "this agent has no rule permitting it"
	}
	return r.Default, reason
}

// A line runs several commands, so its verdict is the most restrictive of theirs: `make test &&
// jj git push` is a push whatever the first half is.
func (r *Rules) decideBash(in Intent) (Verdict, string) {
	if in.Opaque != "" {
		return VerdictDeny, in.Opaque
	}
	if len(in.Commands) == 0 {
		return r.fallback()
	}

	defVerdict, defReason := r.fallback()
	if r.AllowSandboxed && in.Sandboxed {
		defVerdict, defReason = VerdictAllow, ""
	}
	verdict, reason := VerdictAllow, ""
	fence := r.BashRoot
	if fence == "" {
		fence = in.Root
	}
	if fence == "" {
		fence = in.Dir
	}
	// Once the line moves into a temp directory, rm and chmod stop resolving relative names.
	away := false
	dir := in.Dir
	if dir == "" {
		dir = r.BashRoot
	}
	for _, words := range in.Commands {
		v, why := match(r.Bash, words, defVerdict, defReason)
		// Two verbs answer from their arguments instead of from a pattern, and both override
		// whatever the rules said: the word alone does not carry what the command will do.
		switch words[0] {
		case "cd":
			var to string
			var left bool
			v, why, to, left = r.decideCd(words[1:], dir, fence)
			away = away || left
			if to != "" {
				dir = to
			}
		case "rm":
			v, why = r.decideRm(words[1:], dir, away)
		case "chmod":
			if v == VerdictAllow {
				v, why = r.decideChmod(words[1:], dir, away)
			}
		}
		if v < verdict {
			verdict, reason = v, why
		}
	}
	if verdict == VerdictAllow {
		return verdict, ""
	}
	return verdict, reason
}

// decideCd answers a `cd` from its argument rather than from a pattern, because the argument is
// what decides where the rest of the line runs. A relative target resolves from dir, the shell's
// live directory; the fence is where the shell started, or BashRoot when the rules set one.
//
// to is the resolved directory the rest of the line runs in, and left reports that it is a temp
// directory outside the fence, after which rm and chmod refuse relative names for the rest of the line.
func (r *Rules) decideCd(args []string, dir, fence string) (verdict Verdict, why, to string, left bool) {
	outside := "cd moves only within " + fence + " or into a temp directory; " + elsewhere
	if fence == "" {
		outside = "cd moves only into a temp directory; " + elsewhere
	}

	var dirs []string
	for _, arg := range args {
		switch {
		case arg == "-L" || arg == "-P" || arg == "-e" || arg == "-@":
		case arg == "-":
			return VerdictDeny, "cd - returns to a directory chosen earlier, which cannot be checked here; name the directory", "", false
		case strings.HasPrefix(arg, "~"), strings.ContainsAny(arg, "$*?["):
			return VerdictDeny, "the shell expands that directory, so it cannot be checked in advance; write the path out in full", "", false
		default:
			dirs = append(dirs, arg)
		}
	}
	if len(dirs) != 1 {
		return VerdictDeny, "cd takes exactly one directory here", "", false
	}

	target := dirs[0]
	if !filepath.IsAbs(target) {
		if dir == "" {
			return VerdictDeny, "this agent has no directory to resolve a relative path against; name it absolutely", "", false
		}
		target = filepath.Join(dir, target)
	}
	resolved, err := realPath(target)
	if err != nil {
		return VerdictDeny, "that directory could not be resolved: " + err.Error(), "", false
	}
	if fence != "" {
		root, err := realPath(fence)
		if err != nil {
			return VerdictDeny, "this agent's directory could not be resolved: " + err.Error(), "", false
		}
		if under(resolved, root) {
			return VerdictAllow, "", resolved, false
		}
	}
	for _, tmp := range tempDirs() {
		if under(resolved, tmp) {
			return VerdictAllow, "", resolved, true
		}
	}
	return VerdictDeny, outside, "", false
}

const elsewhere = "for anywhere else, give the path to the command itself (git -C, jj -R, an absolute path) or to view, grep and glob"

// tempDirs is every scratch root, resolved: /tmp and $TMPDIR differ on macOS.
func tempDirs() []string {
	var out []string
	for _, dir := range []string{os.TempDir(), "/tmp"} {
		if resolved, err := realPath(dir); err == nil {
			out = append(out, resolved)
		}
	}
	return out
}

// Viewing is not gated: it is the capability a read-only agent is defined by, and RunView already
// refuses everything else for the agents that should not write at all.
func (r *Rules) decideEdit(in Intent) (Verdict, string) {
	if !in.Writes {
		return VerdictAllow, ""
	}
	target, err := realPath(in.Path)
	if err != nil {
		return VerdictDeny, "the path it writes to could not be resolved: " + err.Error()
	}
	return r.decideWritePath(target, "this agent may not write there")
}

// decideWritePath is the Edit fence over a path already resolved by its caller: the longest
// matching rule wins, the same way match works over command words. Shared with decideRm, which
// resolves its paths differently and must not be made to pretend otherwise.
func (r *Rules) decideWritePath(target, refusal string) (Verdict, string) {
	verdict, reason := r.fallback()
	best := -1
	for _, rule := range r.Edit {
		root, err := realPath(rule.Pattern)
		if err != nil {
			continue
		}
		if !under(target, root) || len(root) <= best {
			continue
		}
		best, verdict, reason = len(root), rule.Verdict, rule.Reason
	}
	if verdict != VerdictAllow && reason == "" {
		reason = refusal
	}
	return resolve(verdict, reason)
}

// decideRm answers an `rm` from its arguments, for the reason decideCd does: the verb carries no
// information about what is destroyed, so a pattern rule allowing `rm` is a rule allowing `rm -rf
// ~`. Like cd, it overrides the pattern rules entirely — there is no spelling of an rm rule that
// would be safe to write.
//
// The fence is the Edit rules and not BashRoot, because deleting a file is the most complete way
// of writing to it. That is what keeps `rm -rf .jj` refused by the rule that already refuses
// editing the change graph, and it is why moving where an agent may write moves both gates at
// once rather than leaving this one behind to be noticed later.
func (r *Rules) decideRm(args []string, dir string, away bool) (Verdict, string) {
	var paths []string
	for _, arg := range args {
		switch {
		case arg == "--":
		case arg == "--recursive", arg == "-R":
			return VerdictDeny, recursiveRm
		// A short cluster is read for what it contains, not matched whole: -rf, -fr and -vrf are
		// the same request, and listing spellings is how one gets missed.
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg, "rR"):
			return VerdictDeny, recursiveRm
		case strings.HasPrefix(arg, "-") && arg != "-":
			// Every other flag only changes how rm reports itself — -f on what is missing, -i on
			// what to ask, -v on what to print — and none of them widens what is destroyed.
		case strings.HasPrefix(arg, "~"), strings.ContainsAny(arg, "$*?["):
			return VerdictDeny, "the shell expands that before anything here can check what it would delete; name the file"
		default:
			paths = append(paths, arg)
		}
	}
	if len(paths) == 0 {
		return VerdictDeny, "rm with no path to delete"
	}

	verdict, reason := VerdictAllow, ""
	for _, path := range paths {
		// Relative names resolve from the shell's live directory; the fence is the Edit rules,
		// wherever the shell has moved. Without a BashRoot a relative name is refused outright.
		if !filepath.IsAbs(path) {
			if r.BashRoot == "" || away {
				return VerdictDeny, unrooted
			}
			path = filepath.Join(dir, path)
		}
		// Resolved parent plus unresolved basename, which is what deletion actually acts on: rm
		// removes a symlink rather than following it, so resolving the last element would check
		// the wrong file — and would admit deleting a link that sits outside the fence and happens
		// to point inside it.
		parent, err := realPath(filepath.Dir(path))
		if err != nil {
			return VerdictDeny, "the path it would delete could not be resolved: " + err.Error()
		}
		// Every path has to pass, and the verdict is the most restrictive of theirs — the same
		// rule decideBash applies across a line, for the same reason: `rm mine.go theirs.go` is a
		// deletion of theirs.go whatever else is on the line.
		v, why := r.decideWritePath(filepath.Join(parent, filepath.Base(path)), "this agent may not delete there")
		if v < verdict {
			verdict, reason = v, why
		}
	}
	return resolve(verdict, reason)
}

// chmodMode is a plain mode: three octal digits or rwxX bits, never setuid, setgid or sticky.
var chmodMode = regexp.MustCompile(`^(0?[0-7]{3}|[ugoa]*[-+=][rwxX]*(,[ugoa]*[-+=][rwxX]*)*)$`)

// decideChmod narrows a chmod a Bash rule allowed to one plain mode, no flags, on files inside the Edit fence.
func (r *Rules) decideChmod(args []string, dir string, away bool) (Verdict, string) {
	if len(args) == 0 || !chmodMode.MatchString(args[0]) {
		return VerdictDeny, "chmod takes one plain mode first — +x, -x, u+x, 644 — and no flags"
	}
	var paths []string
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "-"):
			return VerdictDeny, "chmod takes one plain mode first and then files; -R and other flags are refused"
		case strings.HasPrefix(arg, "~"), strings.ContainsAny(arg, "$*?["):
			return VerdictDeny, "the shell expands that before anything here can check what it would change; name the file"
		default:
			paths = append(paths, arg)
		}
	}
	if len(paths) == 0 {
		return VerdictDeny, "chmod with no file to change"
	}
	verdict, reason := VerdictAllow, ""
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			if r.BashRoot == "" || away {
				return VerdictDeny, unrooted
			}
			path = filepath.Join(dir, path)
		}
		// Fully resolved, unlike rm: chmod follows a symlink and changes the file it points at.
		target, err := realPath(path)
		if err != nil {
			return VerdictDeny, "the path it would change could not be resolved: " + err.Error()
		}
		v, why := r.decideWritePath(target, "this agent may not change modes there")
		if v < verdict {
			verdict, reason = v, why
		}
	}
	return resolve(verdict, reason)
}

const unrooted = "a relative path here cannot be checked against this agent's directory; name it absolutely"

// recursiveRm is refused rather than fenced. A tree is the one deletion whose blast radius is not
// visible in the command — the fence can say the directory is writable, and not that it holds a
// week of someone's work — and an agent cleaning up after itself never needs it.
const recursiveRm = "a subagent does not delete directory trees; remove the files it created by name"

// match finds the longest pattern that is a prefix of words and returns its verdict.
func match(rules []Rule, words []string, defVerdict Verdict, defReason string) (Verdict, string) {
	best, verdict, reason := 0, defVerdict, defReason
	for _, rule := range rules {
		pattern := strings.Fields(rule.Pattern)
		if len(pattern) == 0 || len(pattern) > len(words) || len(pattern) <= best {
			continue
		}
		if !equal(pattern, words[:len(pattern)]) {
			continue
		}
		best, verdict, reason = len(pattern), rule.Verdict, rule.Reason
	}
	return resolve(verdict, reason)
}

// matchName is the same longest-wins rule over a tool name, matched as a string prefix. Prefixes
// rather than exact names because MCP tools are namespaced and the namespace is the useful unit:
// `mcp__linear__` names a server, and a name is self-describing enough that `create_issue` and
// `list_issues` can be told apart without the policy knowing anything about Linear.
func matchName(rules []Rule, name string, defVerdict Verdict, defReason string) (Verdict, string) {
	best, verdict, reason := 0, defVerdict, defReason
	for _, rule := range rules {
		if !strings.HasPrefix(name, rule.Pattern) || len(rule.Pattern) <= best {
			continue
		}
		best, verdict, reason = len(rule.Pattern), rule.Verdict, rule.Reason
	}
	return resolve(verdict, reason)
}

// resolve drops the reason from an allow, where nothing needs explaining, and supplies one for a
// refusal that was configured without.
func resolve(verdict Verdict, reason string) (Verdict, string) {
	if verdict == VerdictAllow {
		return verdict, ""
	}
	if reason == "" {
		reason = "this agent has no rule permitting it"
	}
	return verdict, reason
}

func equal(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// under reports whether target is root or inside it, at a path boundary — so a rule for /foo/bar
// does not admit /foo/barn.
func under(target, root string) bool {
	root = strings.TrimSuffix(root, string(filepath.Separator))
	if strings.EqualFold(target, root) {
		return true
	}
	// Case-insensitive because APFS is: /Users/x/code and /Users/x/Code are one directory.
	return len(target) > len(root) && target[len(root)] == filepath.Separator && strings.EqualFold(target[:len(root)], root)
}

// realPath is the path a write will actually land on: absolute, cleaned, and with every symlink
// resolved, dangling ones included, since a create through a link to a missing file writes the
// link's target and a symlink otherwise points anywhere it likes and still matches the prefix.
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for dir, hops := abs, 0; ; {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if target, err := os.Readlink(dir); err == nil {
			if hops++; hops > maxLinkHops {
				return "", fmt.Errorf("%s: too many levels of symbolic links", path)
			}
			if !filepath.IsAbs(target) {
				parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
				if err != nil {
					return "", err
				}
				target = filepath.Join(parent, target)
			}
			dir = filepath.Clean(target)
			continue
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(dir, rest), nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// maxLinkHops caps a chain of dangling links, about as deep as a kernel follows in one lookup.
const maxLinkHops = 40

// Permits reports whether a named tool is worth offering at all — anything not denied outright.
// An Ask counts: the point of Ask is that the call may proceed with a yes, so withholding the
// tool would turn "check with me" into "no".
func (r *Rules) Permits(name string) bool {
	verdict, _ := matchName(r.Tools, name, r.Default, "")
	return verdict != VerdictDeny
}

// CanAsk reports whether any verdict this rule set can reach puts a call to the user. It is what
// lets a caller refuse, at construction, to pair a rule set that asks with an agent that has
// nobody to ask — a background run outliving the turn that started it.
func (r *Rules) CanAsk() bool {
	if r.Default == VerdictAsk {
		return true
	}
	for _, rules := range [][]Rule{r.Bash, r.Edit, r.Tools} {
		for _, rule := range rules {
			if rule.Verdict == VerdictAsk {
				return true
			}
		}
	}
	return false
}

// Summary is the rule set in one line per class, for /agents. It reads out the allow-list rather
// than describing it, because the useful question about an agent is the concrete one: what can
// this thing actually run.
func (r *Rules) Summary() []string {
	var out []string
	for _, class := range []struct {
		name  string
		rules []Rule
	}{{"tools", r.Tools}, {"bash", r.Bash}, {"writes", r.Edit}} {
		line := summarise(class.rules)
		if line == "" {
			continue
		}
		// Only alongside verbs: moving around is not a capability on its own.
		if class.name == "bash" && r.BashRoot != "" {
			line += "; cd anywhere under " + r.BashRoot + " or a temp directory"
		}
		out = append(out, class.name+": "+line)
	}
	if r.AllowSandboxed {
		out = append(out, "bash in the sandbox: any command no rule refuses")
	}
	if len(out) == 0 && r.Default == VerdictDeny {
		return []string{"nothing: every call is refused"}
	}
	if r.Default != VerdictDeny {
		out = append(out, "anything else: "+r.Default.String())
	}
	return out
}

// OfferedSummary is Summary with the MCP rules narrowed to tools that are actually connected and
// actually permitted — the capability an agent has right now, rather than the one it is configured
// for. available is the connected tool names, which is mcpclient.Manager's inventory.
//
// The two summaries exist because they answer different questions and have different readers.
// /agents asks what an agent is configured to permit, and is printed to a person who may be
// debugging why a lookup did not happen: "linear reads are allowed, no linear is connected" is the
// answer there, so Summary shows every rule. A prompt asks what this agent can do on this run, and
// is read by a model that will believe it — naming mcp__linear__list_ to an agent with no Linear
// buys a wasted turn and a refusal, the same way offering it the tool would. Same reasoning as
// McpTools filtering the tool list; this stops the prompt from contradicting it.
//
// Only MCP patterns are narrowed. A built-in is always there, so its absence from the inventory
// means nothing about whether it can be called.
func (r *Rules) OfferedSummary(available []string) []string {
	narrowed := *r
	narrowed.Tools = nil
	for _, rule := range r.Tools {
		if !strings.HasPrefix(rule.Pattern, McpPrefix) || r.offers(rule.Pattern, available) {
			narrowed.Tools = append(narrowed.Tools, rule)
		}
	}
	return narrowed.Summary()
}

// offers reports whether any connected tool is both named by this pattern and permitted. Permits
// as well as the prefix because a longer deny rule can shadow a shorter allow, and a pattern whose
// every tool is refused is one the prompt should not be advertising.
func (r *Rules) offers(pattern string, available []string) bool {
	for _, name := range available {
		if strings.HasPrefix(name, pattern) && r.Permits(name) {
			return true
		}
	}
	return false
}

func summarise(rules []Rule) string {
	var allowed, asks []string
	for _, rule := range rules {
		switch rule.Verdict {
		case VerdictAllow:
			allowed = append(allowed, rule.Pattern)
		case VerdictAsk:
			asks = append(asks, rule.Pattern)
		}
	}
	parts := strings.Join(allowed, ", ")
	if len(asks) > 0 {
		if parts != "" {
			parts += "; "
		}
		parts += "asks for " + strings.Join(asks, ", ")
	}
	return parts
}

// Validate rejects a rule set that does not mean what it appears to mean. It is called at
// construction so a misconfigured agent never starts, rather than at the first tool call.
//
// The shape it exists for: an Allow on a bare verb with a Deny on a longer form of it, such as
// allowing `jj` and denying `jj git push`. That reads as a safe override and is not one, because
// global options go between — `jj --repository /tmp/x git push` matches the Allow and never
// reaches the Deny. The fix is to allow the specific subcommands wanted and let everything else
// fall to the default, which is how an allow-list is supposed to be written anyway.
func (r *Rules) Validate() error {
	for class, rules := range map[string][]Rule{"bash": r.Bash, "edit": r.Edit, "tools": r.Tools} {
		for _, rule := range rules {
			if strings.TrimSpace(rule.Pattern) == "" {
				return fmt.Errorf("%s rule with an empty pattern: it would match every call", class)
			}
		}
	}
	for _, allowed := range r.Bash {
		if allowed.Verdict != VerdictAllow {
			continue
		}
		for _, narrower := range r.Bash {
			if narrower.Verdict == VerdictAllow {
				continue
			}
			if extends(narrower.Pattern, allowed.Pattern) {
				return fmt.Errorf("bash rule %q cannot narrow the allow on %q: options written between the two "+
					"words evade it. Allow the specific subcommands this agent needs and let everything else fall "+
					"to the default", narrower.Pattern, allowed.Pattern)
			}
		}
	}
	return nil
}

// extends reports whether long is a strictly longer word-prefix extension of short.
func extends(long, short string) bool {
	a, b := strings.Fields(long), strings.Fields(short)
	if len(a) <= len(b) || len(b) == 0 {
		return false
	}
	return equal(b, a[:len(b)])
}

// Decide is what this agent would do with one call, and why. Exported because "what may this
// agent actually do" is a question worth being able to ask from outside the loop — a test, and
// eventually a dry run — rather than only observable by making the call and seeing what happens.
//
// An agent with no policy allows everything, which is the main loop.
func (a *Agent) Decide(call Call) (Verdict, string) {
	if a.Policy == nil {
		return VerdictAllow, ""
	}
	return a.Policy.Decide(a.intent(call))
}

// intent decomposes one call. It is a method because the class of an unrecognised name depends on
// the dispatch table: McpFallback sends anything not namespaced mcp__ to bash, so a name nobody
// registered is a bash call and has to be gated as one.
func (a *Agent) intent(call Call) Intent {
	switch {
	case call.Name == editorTool:
		return editIntent(call)
	case a.Handlers[call.Name] != nil && !shellTool(call.Name), strings.HasPrefix(call.Name, McpPrefix):
		return Intent{Tool: call.Name, Class: ClassTool, Label: call.Name}
	default:
		in := bashIntent(call)
		in.Sandboxed = a.Sandbox != nil && !escalates(call)
		return in
	}
}

// Describe is the label and kind a call's box carries, read from its input alone, for a call that never ran.
func Describe(call Call) (string, ToolKind) {
	switch {
	case call.Name == editorTool:
		return editIntent(call).Label, KindEdit
	case shellTool(call.Name):
		return bashIntent(call).Label, KindBash
	}
	return call.Name, ""
}

func editIntent(call Call) Intent {
	var in tools.EditInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Intent{Tool: call.Name, Class: ClassEdit, Writes: true,
			Opaque: "its arguments could not be read: " + err.Error(), Label: call.Name}
	}
	return Intent{
		Tool:   call.Name,
		Class:  ClassEdit,
		Path:   in.Path,
		Writes: in.Command != "view",
		Label:  tools.DescribeEdit(in),
		Body:   tools.PreviewEdit(in),
	}
}

func bashIntent(call Call) Intent {
	var in struct {
		Command string `json:"command"`
		Restart bool   `json:"restart"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Intent{Tool: call.Name, Class: ClassBash,
			Opaque: "its arguments could not be read: " + err.Error(), Label: call.Name}
	}
	// A restart runs nothing, and RunBash answers it before reaching the shell at all.
	if in.Restart {
		return Intent{Tool: call.Name, Class: ClassBash, Label: "$ restart"}
	}
	commands, opaque := decompose(in.Command)
	return Intent{
		Tool:     call.Name,
		Class:    ClassBash,
		Commands: commands,
		Opaque:   opaque,
		Label:    "$ " + in.Command,
	}
}

// CommandRefusal is why alux refuses a bash command whatever an agent's policy says, or "" when it does not; it runs nothing.
func CommandRefusal(command string) string {
	input, _ := json.Marshal(map[string]string{"command": command})
	in := bashIntent(Call{Name: "bash", Input: input})
	if in.Opaque != "" {
		return in.Opaque
	}
	return postsToPullRequest(in, command)
}

// Indirection verbs run something chosen somewhere else. There is no prefix to match on, so they
// are refused for being unreadable rather than permitted on the strength of their own name.
var indirect = map[string]string{
	"eval":    "eval runs a command built at runtime, which cannot be checked in advance",
	"exec":    "exec replaces the shell with a command chosen at runtime",
	"source":  "source runs a script's contents, which are not visible here",
	".":       "sourcing a script runs contents that are not visible here",
	"sh":      "a nested shell runs a command this check cannot see",
	"bash":    "a nested shell runs a command this check cannot see",
	"zsh":     "a nested shell runs a command this check cannot see",
	"env":     "env runs a command chosen by its arguments",
	"xargs":   "xargs runs a command built from its input",
	"sudo":    "sudo escalates, and this agent has no rule permitting it",
	"nohup":   "nohup detaches a command from this check",
	"time":    "time wraps another command; run the command directly",
	"timeout": "timeout wraps another command; run the command directly",
	"command": "command bypasses shell lookup for a verb chosen at runtime",
	"builtin": "builtin bypasses shell lookup for a verb chosen at runtime",
	"watch":   "watch re-runs a command built from its arguments",
}

// codeOption is the arguments that make a read-looking verb run a command of the caller's choosing.
type codeOption struct {
	exact    []string
	prefixes []string
	// beforeSubcommand stops the scan at the first word that is not an option; valued options take the next word.
	beforeSubcommand bool
	valued           []string
	why              string
}

var codeOptions = map[string]codeOption{
	"git": {
		prefixes:         []string{"-c", "--config-env"},
		beforeSubcommand: true,
		valued:           []string{"-C", "--git-dir", "--work-tree", "--namespace"},
		why:              "git -c and --config-env set config that can run a command (fsmonitor, aliases, pagers); run git without config overrides",
	},
	"jj": {
		exact:    []string{"--config", "--config-file"},
		prefixes: []string{"--config=", "--config-file="},
		why:      "jj --config and --config-file set config that can run a command (pagers, editors, aliases); run jj without config overrides",
	},
	"find": {
		exact: []string{"-exec", "-execdir", "-ok", "-okdir", "-delete"},
		why:   "find -exec, -execdir, -ok, -okdir and -delete run or delete; list the files with find and act on them separately",
	},
}

func (o codeOption) refuses(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if slices.Contains(o.exact, arg) || hasAnyPrefix(arg, o.prefixes) {
			return true
		}
		if !o.beforeSubcommand {
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return false
		}
		if slices.Contains(o.valued, arg) {
			i++
		}
	}
	return false
}

func hasAnyPrefix(word string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(word, p) {
			return true
		}
	}
	return false
}

var codeVariables = map[string]bool{
	"GIT_SSH_COMMAND": true, "GIT_SSH": true, "GIT_PROXY_COMMAND": true, "GIT_EXTERNAL_DIFF": true,
	"GIT_PAGER": true, "PAGER": true, "GIT_EDITOR": true, "GIT_SEQUENCE_EDITOR": true, "EDITOR": true, "VISUAL": true,
	"GIT_ASKPASS": true, "SSH_ASKPASS": true, "LD_PRELOAD": true, "BASH_ENV": true, "ENV": true,
	"JJ_CONFIG": true, "JJ_EDITOR": true, "PROMPT_COMMAND": true,
}

var codeVariablePrefixes = []string{"GIT_CONFIG", "DYLD_"}

// codeAssignment is why a leading assignment makes the command run something of the caller's choosing, or "".
func codeAssignment(run []string) string {
	for _, word := range run {
		if !isAssignment(word) {
			return ""
		}
		name := word[:strings.IndexByte(word, '=')]
		if codeVariables[name] || hasAnyPrefix(name, codeVariablePrefixes) {
			return "setting " + name + " makes the command run a program of its choosing; run the command without that assignment"
		}
	}
	return ""
}

// decompose splits a command line into the commands it will run, each as its words. The second
// return is why it could not be done, and it is not an error — it is the refusal, phrased for the
// agent that will read it.
//
// Everything it does not understand makes it give up rather than guess. Substitution, grouping,
// redirection to a file and an unbalanced quote all end the walk, because each one means the words present
// are not the words that will run.
func decompose(command string) ([][]string, string) {
	words, opaque := lex(command)
	if opaque != "" {
		return nil, opaque
	}

	var out [][]string
	for _, run := range split(words) {
		if why := codeAssignment(run); why != "" {
			return nil, why
		}
		run = stripAssignments(run)
		if len(run) == 0 {
			continue
		}
		verb := run[0]
		if strings.ContainsAny(verb, "$*?[") {
			return nil, "the command to run is built by expansion (" + verb + "), so it cannot be checked in advance"
		}
		if why, ok := indirect[filepath.Base(verb)]; ok {
			return nil, why
		}
		if opt, ok := codeOptions[filepath.Base(verb)]; ok && opt.refuses(run[1:]) {
			return nil, opt.why
		}
		out = append(out, run)
	}
	return out, ""
}

// token is a word or a command separator. Separators are all that survive lexing: anything else
// operator-shaped has already stopped the walk.
type token struct {
	text string
	sep  bool
}

func lex(command string) ([]token, string) {
	var out []token
	var cur strings.Builder
	started := false

	flush := func() {
		if started {
			out = append(out, token{text: cur.String()})
			cur.Reset()
			started = false
		}
	}

	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\':
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				started = true
			}

		case c == '\'':
			// Single quotes expand nothing, so their contents are literally one word.
			end := i + 1
			for end < len(runes) && runes[end] != '\'' {
				end++
			}
			if end >= len(runes) {
				return nil, "the command has an unterminated quote, so what it runs is unclear"
			}
			cur.WriteString(string(runes[i+1 : end]))
			started = true
			i = end

		case c == '"':
			text, end, opaque := lexDouble(runes, i)
			if opaque != "" {
				return nil, opaque
			}
			cur.WriteString(text)
			started = true
			i = end

		case c == '`', c == '$' && i+1 < len(runes) && runes[i+1] == '(':
			return nil, "the command substitutes the output of another command, which cannot be checked in advance"

		case c == '<' && i+1 < len(runes) && runes[i+1] == '(':
			return nil, "the command uses process substitution, which cannot be checked in advance"

		case c == '>' || c == '<', c == '&' && i+1 < len(runes) && runes[i+1] == '>':
			end, ok := harmlessRedirect(runes, i)
			if !ok {
				return nil, "the command redirects to or from a file or uses a heredoc; write files with the editor tool instead (2>&1 and >/dev/null are fine)"
			}
			if c != '&' && started && allDigits(cur.String()) {
				cur.Reset()
				started = false
			}
			flush()
			i = end

		case c == '(' || c == ')' || c == '{' || c == '}':
			return nil, "the command groups other commands, and this check reads only a flat command line"

		case c == '#' && !started:
			for i < len(runes) && runes[i] != '\n' {
				i++
			}

		case c == '&' || c == '|' || c == ';' || c == '\n':
			flush()
			if i+1 < len(runes) && runes[i+1] == c {
				i++
			}
			out = append(out, token{text: string(c), sep: true})

		case c == ' ' || c == '\t' || c == '\r':
			flush()

		default:
			cur.WriteRune(c)
			started = true
		}
	}
	flush()
	return out, ""
}

// harmlessRedirect reads the redirection starting at runes[i] and returns the index of its last
// rune when it can touch no file: a duplication onto another descriptor, or /dev/null.
func harmlessRedirect(runes []rune, i int) (int, bool) {
	j := i
	both := runes[j] == '&'
	if both {
		j++
	}
	op := runes[j]
	j++
	if op == '>' && j < len(runes) && (runes[j] == '>' || runes[j] == '|') {
		j++
	}

	if !both && j < len(runes) && runes[j] == '&' {
		k := j + 1
		if k < len(runes) && runes[k] == '-' {
			k++
		} else {
			for k < len(runes) && runes[k] >= '0' && runes[k] <= '9' {
				k++
			}
		}
		if k == j+1 || k < len(runes) && !redirectBoundary(runes[k]) {
			return 0, false
		}
		return k - 1, true
	}

	for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t') {
		j++
	}
	const null = "/dev/null"
	k := j + len(null)
	if k <= len(runes) && string(runes[j:k]) == null && (k == len(runes) || redirectBoundary(runes[k])) {
		return k - 1, true
	}
	return 0, false
}

func redirectBoundary(c rune) bool {
	return strings.ContainsRune(" \t\r\n;&|", c)
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// lexDouble reads a double-quoted run. Variable expansion inside it is harmless in an argument,
// but substitution is not: it still runs a command.
func lexDouble(runes []rune, start int) (string, int, string) {
	var sb strings.Builder
	i := start + 1
	for i < len(runes) && runes[i] != '"' {
		switch {
		case runes[i] == '\\' && i+1 < len(runes):
			i++
			sb.WriteRune(runes[i])
		case runes[i] == '`', runes[i] == '$' && i+1 < len(runes) && runes[i+1] == '(':
			return "", 0, "the command substitutes the output of another command, which cannot be checked in advance"
		default:
			sb.WriteRune(runes[i])
		}
		i++
	}
	if i >= len(runes) {
		return "", 0, "the command has an unterminated quote, so what it runs is unclear"
	}
	return sb.String(), i, ""
}

func split(words []token) [][]string {
	var out [][]string
	var cur []string
	for _, w := range words {
		if w.sep {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, w.text)
	}
	return append(out, cur)
}

// stripAssignments drops the FOO=bar prefix a command may carry, so the verb is the first word
// that is actually a verb.
func stripAssignments(run []string) []string {
	for len(run) > 0 && isAssignment(run[0]) {
		run = run[1:]
	}
	return run
}

func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i, c := range word[:eq] {
		letter := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
