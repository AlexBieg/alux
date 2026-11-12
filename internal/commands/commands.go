// Package commands is the slash-command table. Commands share the session's prompt queue, so one
// typed while a turn is running waits for it rather than racing it.
package commands

import (
	"regexp"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/report"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/workers"
)

// Context is what a command is allowed to do to the session. Deliberately narrow: a command that
// needs more than this is probably a tool.
type Context struct {
	NewSession func()
	// Handoff runs a model turn, so it blocks like any turn.
	Handoff func(toClipboard bool)
	// Compact runs a model turn on the compacted history, so it blocks like any turn.
	Compact func()
	Show    func(text string)
	Name    func() string

	// ShowBlock shows text that is already laid out in columns, so the pane must not reflow it.
	// Nil falls back to Show, which is prose and may rewrap.
	ShowBlock func(text string)

	// Breakdown measures where the prompt's tokens sit. It calls the API, so it blocks.
	Breakdown func() (string, error)

	// ShowTable shows a table the pane draws at its width. Nil falls back to ShowBlock with its plain layout.
	ShowTable func(t report.Table)

	// Agents reports every subagent this session has dispatched. Nil when none are configured.
	Agents func() report.Table

	// Jobs reports each scheduled job and its last run. Nil when the session schedules none.
	Jobs func() report.Table

	// Vault reports on the knowledge-vault index, having first done whatever the action names:
	// "" reports, "rebuild" rebuilds, "aliases" writes search aliases. Nil when no vault is
	// configured. All three block — a rebuild walks every note, and an alias run is one model
	// call per section that has none.
	Vault func(action string) (string, error)

	// PRs is /prs with its argument; nil when PR tracking is not configured. It may call GitHub, so it blocks.
	PRs func(arg string) (string, error)

	// Workers lists this repo's headless workers. Nil when the session cannot see any.
	Workers func() []workers.Worker

	// Land is /land with its argument; nil outside a pool workspace. It runs jj, so it blocks.
	Land func(arg string) (string, error)
	// Resume is /resume with its argument; nil outside a pool workspace. It runs jj, so it blocks.
	Resume func(arg string) (string, error)

	// Skills validates every skill file in ~/.config/alux/skills and reports each one.
	Skills func() (string, error)

	// RunSkill opens a run of the named skill, the rest of the line filling its text input; EndSkill reports and closes it.
	RunSkill func(name, text string)
	EndSkill func()
	// ShowSkill draws the named skill's graph without opening a run.
	ShowSkill func(name string) (string, error)
}

type Command struct {
	Name        string
	Aliases     []string
	Description string
	Run         func(ctx Context, args string)

	// Args offers the candidates for the next argument, given the ones already typed in full.
	Args func(done []string) []Suggestion
}

// Suggestion is one completion candidate; Value is what replaces the word being typed.
type Suggestion struct {
	Value       string
	Label       string
	Description string
}

func words(pairs ...string) []Suggestion {
	var out []Suggestion
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Suggestion{Value: pairs[i], Label: pairs[i], Description: pairs[i+1]})
	}
	return out
}

// Overridden in tests so completion never reads the real state directories.
var (
	skillNames = func() []Suggestion {
		dir, err := skills.Dir()
		if err != nil {
			return nil
		}
		entries, _ := skills.List(dir)
		var out []Suggestion
		for _, e := range entries {
			if e.Err == nil {
				out = append(out, Suggestion{Value: e.Name, Label: e.Name, Description: "use when " + e.Skill.When})
			}
		}
		return out
	}
)

// LandCandidates offers /land the unlanded changes; nil outside a pool workspace.
var LandCandidates func() []Suggestion

// SendWorker hands /workers send's text to the worker running as pid; nil leaves the command unable to.
var SendWorker func(pid int, text string) error

// Unconfigured is left out of completion; typed in full, each still runs.
var Unconfigured = map[string]bool{}

var table = []Command{
	{
		Name:        "clear",
		Aliases:     []string{"new"},
		Description: "start a new session",
		Run:         func(ctx Context, _ string) { ctx.NewSession() },
	},
	{
		Name:        "spawn",
		Description: "start a new session, in a workspace of its own where the repo has a pool, and switch this pane to it; this one keeps running",
		Run: func(ctx Context, _ string) {
			ctx.Show("only a pane attached to a served session can spawn one; run alux attach -new")
		},
	},
	{
		Name:        "kill",
		Description: "stop this session and switch this pane to another running one, if there is one",
		Run: func(ctx Context, _ string) {
			ctx.Show("only a pane attached to a served session can kill it; run alux attach -new")
		},
	},
	{
		Name:        "quit",
		Description: "stop this session and close this pane",
		Run: func(ctx Context, _ string) {
			ctx.Show("only a pane attached to a served session can quit it; run alux attach -new")
		},
	},
	{
		Name:        "detach",
		Description: "close this pane and leave the session running",
		Run: func(ctx Context, _ string) {
			ctx.Show("only a pane attached to a served session can detach from it; run alux attach -new")
		},
	},
	{
		Name:        "handoff",
		Description: "have the agent write a handoff note, start a new session, and put the note in the input to edit and send; /handoff copy copies it to the clipboard and keeps this session",
		Run: func(ctx Context, args string) {
			if ctx.Handoff == nil {
				ctx.Show("this session cannot hand off")
				return
			}
			arg := strings.TrimSpace(args)
			if arg != "" && arg != "copy" {
				ctx.Show("unknown argument " + arg + "; /handoff starts a new session with the note, /handoff copy copies it to the clipboard")
				return
			}
			ctx.Handoff(arg == "copy")
		},
		Args: func(done []string) []Suggestion {
			if len(done) > 0 {
				return nil
			}
			return words("copy", "copy the note to the clipboard and keep this session")
		},
	},
	{
		Name:        "compact",
		Description: "start a new session whose first message is this one's history, compacted without a model call",
		Run: func(ctx Context, _ string) {
			if ctx.Compact == nil {
				ctx.Show("this session cannot compact")
				return
			}
			ctx.Compact()
		},
	},
	{
		Name:        "context",
		Aliases:     []string{"ctx"},
		Description: "show where the prompt's tokens are going",
		Run: func(ctx Context, _ string) {
			if ctx.Breakdown == nil {
				ctx.Show("this session cannot measure its context")
				return
			}
			report, err := ctx.Breakdown()
			if err != nil {
				ctx.Show("could not measure the context: " + err.Error())
				return
			}
			if ctx.ShowBlock != nil {
				ctx.ShowBlock(report)
				return
			}
			ctx.Show(report)
		},
	},
	{
		Name:        "vault",
		Description: "show the knowledge-vault index; /vault rebuild rebuilds it, /vault aliases writes search aliases",
		Run: func(ctx Context, args string) {
			if ctx.Vault == nil {
				ctx.Show("no vault is configured for this session")
				return
			}
			// One argument from a known list, and anything else is refused rather than read as a
			// bare status: a typo'd "/vault rebiuld" that silently reported status would look
			// exactly like a rebuild that did nothing. It matters more with every action added,
			// because the actions now include one that spends money.
			arg := strings.TrimSpace(args)
			if arg != "" && arg != "rebuild" && arg != "aliases" {
				ctx.Show("unknown argument " + arg + "; /vault shows the index, /vault rebuild rebuilds it, " +
					"/vault aliases writes search aliases for the sections that have none")
				return
			}
			report, err := ctx.Vault(arg)
			if err != nil {
				ctx.Show("vault index: " + err.Error())
				return
			}
			if ctx.ShowBlock != nil {
				ctx.ShowBlock(report)
				return
			}
			ctx.Show(report)
		},
		Args: func(done []string) []Suggestion {
			if len(done) > 0 {
				return nil
			}
			return words("rebuild", "rebuild the index", "aliases", "write search aliases for sections that have none")
		},
	},
	{
		Name:        "prs",
		Description: "show your PR queue; /prs 123 shows one PR, /prs refresh fetches now",
		Run: func(ctx Context, args string) {
			if ctx.PRs == nil {
				ctx.Show(`PR tracking is off; add a "prs" block to ~/.config/alux/settings.json`)
				return
			}
			report, err := ctx.PRs(args)
			if err != nil {
				ctx.Show("prs: " + err.Error())
				return
			}
			// Markdown, so it goes through Show to be rendered and wrapped rather than shown verbatim.
			ctx.Show(report)
		},
		Args: func(done []string) []Suggestion {
			if len(done) > 0 {
				return nil
			}
			return words("refresh", "fetch the queue from GitHub now")
		},
	},
	{
		Name:        "workers",
		Description: "show this repo's headless workers; /workers KEY shows where one's transcript, log and channel are; /workers send KEY text sends a live one a line as a user message",
		Run: func(ctx Context, args string) {
			var list []workers.Worker
			if ctx.Workers != nil {
				list = ctx.Workers()
			}
			key := strings.TrimSpace(args)
			if verb, rest, _ := strings.Cut(key, " "); verb == "send" {
				to, text, _ := strings.Cut(strings.TrimSpace(rest), " ")
				sent, err := workers.Send(list, to, text, SendWorker)
				if err != nil {
					ctx.Show("workers send: " + err.Error())
					return
				}
				ctx.Show("sent to " + sent)
				return
			}
			if key == "" {
				showTable(ctx, workers.Table(list, time.Now()))
				return
			}
			detail, ok := workers.Detail(list, key)
			if !ok {
				detail = "no worker " + key + " in this repo"
			}
			if ctx.ShowBlock != nil {
				ctx.ShowBlock(detail)
				return
			}
			ctx.Show(detail)
		},
	},
	{
		Name:        "land",
		Description: "open the workspaces pane to pick a session's work to put under your working copy in the default workspace; /land <change> lands one directly",
		Run: func(ctx Context, args string) {
			if ctx.Land == nil {
				ctx.Show(`this session is not in a pool workspace; set "pool": true for the repo under "workspaces" in ~/.config/alux/settings.json`)
				return
			}
			report, err := ctx.Land(args)
			if err != nil {
				ctx.Show("land: " + err.Error())
				return
			}
			ctx.Show(report)
		},
		Args: func(done []string) []Suggestion {
			if len(done) > 0 || LandCandidates == nil {
				return nil
			}
			return LandCandidates()
		},
	},
	{
		Name:        "resume",
		Description: "carry on from an ended session's change in this session's workspace, in a new change on top of it; /land shows the workspaces to pick from",
		Run: func(ctx Context, args string) {
			switch {
			case ctx.Resume == nil:
				ctx.Show(`this session is not in a pool workspace; set "pool": true for the repo under "workspaces" in ~/.config/alux/settings.json`)
			case strings.TrimSpace(args) == "":
				ctx.Show("/resume <change> carries on from that change here; /land shows the workspaces and s resumes the selected one")
			default:
				report, err := ctx.Resume(args)
				if err != nil {
					ctx.Show("resume: " + err.Error())
					return
				}
				ctx.Show(report)
			}
		},
	},
	{
		Name:        "skills",
		Description: "list the skills in ~/.config/alux/skills; /skills run <name> [text] starts one, /skills show <name> draws its graph, /skills end reports and closes it",
		Run: func(ctx Context, args string) {
			if ctx.Skills == nil || ctx.RunSkill == nil || ctx.EndSkill == nil {
				ctx.Show("skills are not wired in this session")
				return
			}
			verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
			switch verb {
			case "":
			case "run":
				name, text, _ := strings.Cut(strings.TrimSpace(rest), " ")
				if name == "" {
					ctx.Show("/skills run <name> [text] starts a skill; /skills lists them")
					return
				}
				ctx.RunSkill(name, strings.TrimSpace(text))
				return
			case "end":
				ctx.EndSkill()
				return
			case "show":
				name := strings.TrimSpace(rest)
				if name == "" || ctx.ShowSkill == nil {
					ctx.Show("/skills show <name> draws a skill's graph; /skills lists them")
					return
				}
				graph, err := ctx.ShowSkill(name)
				if err != nil {
					ctx.Show("skills: " + err.Error())
					return
				}
				ctx.Show(graph)
				return
			default:
				ctx.Show("unknown argument " + verb + "; /skills lists them, /skills run <name> [text] starts one, /skills show <name> draws one, /skills end closes it")
				return
			}
			report, err := ctx.Skills()
			if err != nil {
				ctx.Show("skills: " + err.Error())
				return
			}
			ctx.Show(report)
		},
		Args: func(done []string) []Suggestion {
			switch {
			case len(done) == 0:
				return words("run", "start a skill", "show", "draw a skill's graph without running it", "end", "report on the open skill run and close it")
			case len(done) == 1 && (done[0] == "run" || done[0] == "show"):
				return skillNames()
			}
			return nil
		},
	},
	{
		Name:        "agents",
		Description: "show the subagents this session has run",
		Run: func(ctx Context, _ string) {
			if ctx.Agents == nil {
				ctx.Show("this session has no subagents")
				return
			}
			showTable(ctx, ctx.Agents())
		},
	},
	{
		Name:        "jobs",
		Description: "show the scheduled background jobs and their last runs",
		Run: func(ctx Context, _ string) {
			if ctx.Jobs == nil {
				ctx.Show("this session has no jobs")
				return
			}
			showTable(ctx, ctx.Jobs())
		},
	},
}

func showTable(ctx Context, t report.Table) {
	switch {
	case ctx.ShowTable != nil:
		ctx.ShowTable(t)
	case ctx.ShowBlock != nil:
		ctx.ShowBlock(t.Plain())
	default:
		ctx.Show(t.Plain())
	}
}

// All returns the command table.
func All() []Command { return table }

func names(c Command) []string {
	return append([]string{c.Name}, c.Aliases...)
}

// Label renders every name a command answers to, for the completion list.
func Label(c Command) string {
	out := names(c)
	for i, n := range out {
		out[i] = "/" + n
	}
	return strings.Join(out, ", ")
}

// A path like "/Users/someone/x" has a slash after its first word, so it does not parse as a
// command and is sent to the model as ordinary text.
var pattern = regexp.MustCompile(`^/([\w-]+)(?:\s+([\s\S]*))?$`)

// Parse splits a command line. ok is false when the text is an ordinary prompt.
func Parse(text string) (name, args string, ok bool) {
	match := pattern.FindStringSubmatch(text)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

func Find(name string) (Command, bool) {
	for _, c := range table {
		for _, n := range names(c) {
			if n == name {
				return c, true
			}
		}
	}
	return Command{}, false
}

// Complete returns the candidates for the last word typed, best first, and the offset that word starts at.
func Complete(input string) (start int, out []Suggestion) {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, "\n\t") {
		return 0, nil
	}
	start = strings.LastIndexByte(input, ' ') + 1
	partial := input[start:]
	if start == 0 {
		var cands []Suggestion
		var keys [][]string
		for _, c := range table {
			if Unconfigured[c.Name] {
				continue
			}
			cands = append(cands, Suggestion{Label: Label(c), Description: c.Description})
			keys = append(keys, names(c))
		}
		out = rank(cands, keys, partial[1:])
		for i := range out {
			out[i].Value = "/" + out[i].Value
		}
		return 0, out
	}
	fields := strings.Fields(input[:start])
	c, ok := Find(strings.TrimPrefix(fields[0], "/"))
	if !ok || c.Args == nil {
		return 0, nil
	}
	cands := c.Args(fields[1:])
	keys := make([][]string, len(cands))
	for i, s := range cands {
		keys[i] = []string{s.Value}
	}
	return start, rank(cands, keys, partial)
}

// rank orders exact, then prefix matches, falling back to substrings only when neither exists; the best-matching key becomes Value.
func rank(cands []Suggestion, keys [][]string, partial string) []Suggestion {
	p := strings.ToLower(partial)
	var exact, prefix, inside []Suggestion
	for i, s := range cands {
		best := 3
		for _, key := range keys[i] {
			k := strings.ToLower(key)
			tier := 3
			switch {
			case k == p:
				tier = 0
			case strings.HasPrefix(k, p):
				tier = 1
			case strings.Contains(k, p):
				tier = 2
			}
			if tier < best {
				best, s.Value = tier, key
			}
		}
		switch best {
		case 0:
			exact = append(exact, s)
		case 1:
			prefix = append(prefix, s)
		case 2:
			inside = append(inside, s)
		}
	}
	if len(exact)+len(prefix) > 0 {
		return append(exact, prefix...)
	}
	return inside
}
