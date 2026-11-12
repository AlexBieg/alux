package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/vault"
)

func readIfExists(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// ContextFiles are the project instruction files that were folded into the system prompt, shown
// in the transcript at startup so it is never a mystery what the agent was told.
var ContextFiles []string

// loadProjectInstructions walks from the repository root down to the working directory, so a
// nested AGENTS.md refines the one above it. It drops the sections named in exclude and returns
// the names found in none of the files.
func loadProjectInstructions(cwd, root string, exclude []string) (files []struct{ Path, Content string }, missing []string) {
	var dirs []string
	for dir := cwd; ; dir = filepath.Dir(dir) {
		dirs = append([]string{dir}, dirs...)
		if dir == root || dir == filepath.Dir(dir) {
			break
		}
	}

	// Keyed on content so a CLAUDE.md symlinked or copied from AGENTS.md is only included once.
	seen := map[string]bool{}
	found := map[string]bool{}
	for _, dir := range dirs {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			path := filepath.Join(dir, name)
			content := strings.TrimSpace(readIfExists(path))
			if content == "" || seen[content] {
				continue
			}
			seen[content] = true
			content = strings.TrimSpace(excludeSections(content, exclude, found))
			if content == "" {
				continue
			}
			files = append(files, struct{ Path, Content string }{path, content})
		}
	}
	for _, name := range exclude {
		if !found[headingKey(name)] {
			missing = append(missing, name)
		}
	}
	return files, missing
}

func headingKey(text string) string { return strings.ToLower(strings.TrimSpace(text)) }

// excludeSections drops each #, ## or ### section whose heading is in names, subsections included,
// and marks in found the heading keys it saw.
func excludeSections(content string, names []string, found map[string]bool) string {
	if len(names) == 0 {
		return content
	}
	listed := map[string]bool{}
	for _, name := range names {
		listed[headingKey(name)] = true
	}
	var out []string
	dropping, fence := 0, ""
	for _, line := range strings.Split(content, "\n") {
		if fence != "" {
			if closesFence(line, fence) {
				fence = ""
			}
		} else if level, text := markdownHeading(line); level > 0 {
			if dropping > 0 && level <= dropping {
				dropping = 0
			}
			if key := headingKey(text); dropping == 0 && level <= 3 && listed[key] {
				found[key] = true
				dropping = level
			}
		} else {
			fence = opensFence(line)
		}
		if dropping == 0 {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// markdownHeading returns an ATX heading's level and text, or 0 for any other line.
func markdownHeading(line string) (int, string) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0, ""
	}
	level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
	rest := trimmed[level:]
	if level == 0 || level > 6 || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return 0, ""
	}
	rest = strings.TrimSpace(rest)
	if closing := strings.TrimRight(rest, "#"); closing == "" || strings.HasSuffix(closing, " ") || strings.HasSuffix(closing, "\t") {
		rest = strings.TrimSpace(closing)
	}
	return level, rest
}

func opensFence(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return ""
	}
	for _, c := range []string{"`", "~"} {
		if marker := trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, c))]; len(marker) >= 3 {
			if c == "`" && strings.Contains(trimmed[len(marker):], "`") {
				return ""
			}
			return marker
		}
	}
	return ""
}

func closesFence(line, fence string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == ""
}

// agentsExclude reads settings.json's {"agents_md_exclude": {"<repo path>": [headings]}} for repo.
func agentsExclude(repo string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return nil
	}
	var settings struct {
		Exclude map[string][]string `json:"agents_md_exclude"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	for key, names := range settings.Exclude {
		if strings.HasPrefix(key, "~/") {
			key = filepath.Join(home, key[2:])
		}
		if filepath.Clean(key) == filepath.Clean(repo) {
			return names
		}
	}
	return nil
}

// projectPrompt is the project-instructions part of the prompt for the repo named repo in settings,
// the files it came from, and a notice for each excluded heading no file has.
func projectPrompt(cwd, root, repo string) (prompt string, files, notices []string) {
	loaded, missing := loadProjectInstructions(cwd, root, agentsExclude(repo))
	var blocks []string
	for _, f := range loaded {
		files = append(files, f.Path)
		blocks = append(blocks, "<project-instructions path=\""+f.Path+"\">\n"+f.Content+"\n</project-instructions>")
	}
	for _, name := range missing {
		notices = append(notices, "AGENTS.md exclude: no section "+strconv.Quote(name)+" in "+repo+" — it may have been renamed")
	}
	return strings.Join(blocks, "\n\n"), files, notices
}

// ExcludeDrift is a startup notice for each heading in agents_md_exclude that no loaded file has.
func ExcludeDrift() []string {
	_, _, notices := projectPrompt(project.Cwd(), project.Root(), project.Home())
	return notices
}

// BatchCalls is in every agent's prompt; the examples are what get it applied, not the rule.
const BatchCalls = "Batch independent calls into one reply; they run together, and a turn is one reply however many tools it " +
	"calls. Several files or ranges are several view calls in that reply, several edits are several str_replace calls, " +
	"and a grep can go with the views it points to. When the steps must run in order and you already know each one, " +
	"such as edits then the build, or a jj describe then a jj log, send them as one compose call rather than a reply each."

// SystemPrompt assembles the standing instructions, including the vault paragraph when a vault
// is configured. extra is for anything a caller wants to add on top.
func SystemPrompt(extra string) string {
	vcsContext := "This is not a jj repo."
	if root := project.JJRoot(); root != "" {
		vcsContext = "This is a jj (Jujutsu) repo rooted at " + root + ". Use jj, not git, for version control. " +
			"jj commit, jj new and jj describe already print the change ids they leave @ and @- at, so read them there " +
			"rather than running jj log after."
	}
	if Guard != nil {
		vcsContext += " This session has a jj workspace of its own, " + Guard.Slot + ", one of a pool kept for the repo at " + Guard.Origin +
			", which is the user's and read-only to you. Before your first edit, describe this session's change with " +
			"`jj describe -m \"<what this session is doing>\"`: until it has a description the edit tool refuses and shell commands " +
			"cannot write in the workspace, except a call made only of jj commands. To start from an existing bookmark, run " +
			"`jj new <bookmark>` rather than describing the empty change you are leaving, then describe the new one. " +
			"Rewrite the description as the work becomes clearer. The user brings this work into " +
			"the default workspace with /land; call land_session only when they ask."
	}

	var instructions string
	instructions, ContextFiles, _ = projectPrompt(project.Cwd(), project.Root(), project.Home())

	home, _ := os.UserHomeDir()

	parts := append([]string{
		"You are a coding agent running in the user's terminal.",
		"Working directory: " + project.Cwd(),
		vcsContext,
	}, harnessInstructions...)
	parts = append(parts,
		"",
		strings.TrimSpace(readIfExists(filepath.Join(home, paths.Config, "AGENTS.md"))),
		"",
		instructions,
		"",
		vault.SystemNote(),
		"",
		RecordSystemNote(),
		"",
		WebSystemNote(),
		"",
		strings.TrimSpace(extra),
	)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// PromptVersion names the main agent's harness instructions and startup rules, leaving out what differs per session.
func PromptVersion(rules *RuleInjector) string {
	var ids []string
	if rules != nil {
		ids = rules.startupIDs()
	}
	return promptVersion(strings.Join(harnessInstructions, "\n"), ids)
}

func promptVersion(template string, ruleIDs []string) string {
	sum := sha256.Sum256([]byte(template + "\n" + strings.Join(ruleIDs, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// SharedShell is what every agent with bash is told about its shell.
const SharedShell = "Your bash calls share one shell: `cd`, variables, functions and aliases carry to the next call; `set` " +
	"options and traps do not. A call with `sandbox: false` runs in a fresh shell in the same directory and changes " +
	"nothing. If the shell is lost (`exit`, a `set -e` or `set -u` failure, a cancel, a command held past its time " +
	"limit), a new one starts with the directory and exported variables from before that call, and anything started " +
	"with `&` dies with it. `restart: true` starts over at the working directory. File tools still resolve relative " +
	"paths from the working directory."

var harnessInstructions = []string{
	"Use the text editor tool to read and change files, `glob` and `grep` to find them, and the bash tool for everything else. " + SharedShell,
	"A bash call holds the turn for two minutes, or for its `seconds` (at most ten) — give a test suite or build you know is long a larger `seconds` when you need its result next. A command still going at that point is held as a numbered process, still running — do not start it again: `background_proc` lets it run on, or `kill_shell` stops it when it looks stuck, which a test run silent for minutes usually is. A held process you do neither to is killed when your turn ends. Start anything with no natural end — a server, a watcher, a tail — with `run_background`. Backgrounding always takes a `check_in`, and optionally an `until` pattern such as a server's ready line: you are woken when the process exits, matches, or reaches its check-in, so there is nothing to wait on — carry on with other work, or end your turn if there is none. When asked to keep an eye on something's progress, add `repeat` and the check-in comes round every `check_in` until it ends. To wait on anything outside the session — CI, a review bot, a container, a URL — use `poll`. Never wait by sleeping in bash: a sleep loop is refused, and so is a command that opens with `sleep` while a background process is running. Do not wrap commands in `timeout`; a long one is held, not killed.",
	"Never modify a file through bash: no `sed -i`, no `python`/`perl` rewriting a file in place, no heredoc or `>` redirection over one, no `mv` of a rewritten copy over the original. Use the text editor tool, whatever the shape of the change. When a replacement occurs more than once, make one `str_replace` call per site with enough surrounding context to be unique, rather than scripting the whole thing. A half-applied script is indistinguishable from an edit that never ran, and the user sees nothing of what it changed. A formatter is the exception: run the repository's own (`gofmt -w`, `prettier --write`, `ruff format`) on the files you changed, naming them rather than `.`.",
	"Do not search through bash either: no `grep`, `rg`, `find` or `ls -R`. `glob` and `grep` cost less — their output is capped and sorted — and they accept an absolute `path`, so they reach other repos and the vault as readily as the working directory. `grep` takes `-A`/`-B`/`-C` for context lines, `-i`, `-o`, `multiline`, `head_limit`, and `output_mode` for paths with a match, paths without one, or counts per file. Go to bash only for what they cannot express: lines that do not match, or piping results onward. Say which it is when you do.",
	"Do not read files through bash either: no `cat`, `head`, `tail`, `sed -n '1,20p'` or `awk` to print a file. Use the text editor's `view`, which takes a `view_range` for part of a file and a directory path for a listing. Bash is for reading that `view` cannot express: a pipeline into `head`/`wc`, or output that is not a file at all.",
	BatchCalls,
	"The transcript draws diagrams from fences; use one when a flow, a call sequence or a set of relationships reads better drawn than described. Write every diagram as a ```mermaid fence: a flow as a `flowchart` (`TD`, or `LR` on its side) with labelled edges `A -->|label| B`, dashed `-.->`, no arrow `---`, and `<br/>` for a second line in a box; box shapes, where they help, are `A(step)` rounded, `A{question}` decision, `A([start or end])` terminal, `A[(store)]` database, `A[[call]]` subprocess and `A{{setup}}` preparation; a state machine as `stateDiagram-v2`, a call sequence as `sequenceDiagram`, entity relationships as `erDiagram`, and likewise `gantt`, `pie`, `xychart-beta`, `classDiagram` and `gitGraph`. Other fence languages, dot included, are shown as source. A diagram that cannot be drawn, or is too wide for the pane, is shown as its source instead. Only small diagrams draw, so keep to about ten nodes, three branches from any node, and labels of two or three words; a larger flow goes in several small diagrams, one per stage.",
}
