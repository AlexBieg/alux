package subagent

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/alexbieg/alux/internal/project"
	"github.com/anthropics/anthropic-sdk-go"
)

// The navigator is the half of a pair that writes nothing. It plans and then reviews in one
// conversation, and throughout it can read and nothing else, through the same handlers and shell
// rules the research agent uses.
//
// That is also why it cannot deadlock the pair. The hazard the subagent package keeps coming back
// to is two runs reaching for the session's single approval slot; an agent whose rules never ask
// raises no approvals, so an exchange taken from inside the driver's own loop contends with nothing.

// askTool is the driver's name for asking its navigator something.
const askTool = "ask_navigator"

const (
	navigatorQuestions = 6
	navigatorSteers    = 4
	planSteps          = 18
	reviewSteps        = 4
)

var navigatorPrompt = `Once the driver starts, this conversation carries on: alux shows you what the driver has done at checkpoints, and the driver may ask you questions.

At a checkpoint you see what happened since your last look: the driver's own words, the files it edited, and every command it ran with the tail of its output. You are not shown the edits themselves. Read the files you need to judge the change, and skip what you have already read and that has not changed. At the end of the driver's turn you are also shown the working copy's diff since the run began, cut at 30 KB. You have ` + strconv.Itoa(reviewSteps) + ` turns for each message, so read what matters most first. You cannot build or run tests: the command output is your only evidence of what passes.

Stay silent unless something is genuinely wrong. When nothing is, reply with exactly ` + none + ` and nothing else. Silence is the normal reply: do not manufacture an objection to justify having been asked.

When something is wrong, reply with one steer: the most important problem, stated plainly enough to act on. Attack the approach before the details — the wrong abstraction matters and a misnamed variable does not. Name the case it has not handled, the assumption it made without noticing, the place it left the plan without a reason. Do not soften a real problem into a suggestion; if the work should be thrown away, say so in those words.

You are not reviewing style, naming or formatting unless they are load-bearing. You are not writing code: the shape of a fix is the most you give, and the driver decides.

When the driver asks you something, answer it squarely. When it pushes back on the plan or a steer with evidence from the repository, check the file if you can and concede when it is right. When it asks before giving up on a task it believes cannot be done, do not default to encouragement — a task can rest on something untrue of the repository or need a decision nobody has made. If it should stop, say stop; if you see a way through, say what it is.`

func planningPrompt(root string) string {
	return strings.Join([]string{
		"You are the navigator in a pair. Another agent, the driver, writes the code. You plan the change before the driver starts, then review its work in this same conversation while it makes it. Repository root: " + root,
		"",
		"You can read files with the text editor's view command and find them with glob and grep. You have no shell, and you cannot change anything. Search before you read, and say nothing while you work.",
		"",
		"Each reply you make is one turn, however many tool calls it holds, so put every call you can already name into the same reply: several views, or a grep and the views it points to. One call per turn will run you out of turns before you have read enough to plan.",
		"",
		"You have " + strconv.Itoa(planSteps) + " turns to plan. Your last message then is the plan, and it is the only thing of yours the driver will see. Make it concrete:",
		"- the files to change, and what changes in each",
		"- the approach, and the alternative you rejected if it was close",
		"- the risks: what could break, and what the task assumes that may not be true of this repository",
		"- what to test, and how",
		"",
		"Name files and functions as they are in the repository, not as you guess they are. If the task cannot be done as stated, say so and why instead of writing a plan. No preamble.",
	}, "\n")
}

// Navigator is the reviewer, built fresh per pair definition.
func Navigator() *Definition {
	return &Definition{
		Name:        "navigator",
		Description: "plans the driver's work in a pair, then reviews it at checkpoints.",
		System:      func() string { return navigatorPrompt },
		// Reviews run on the planner's tools; this is the budget for each message.
		Tools:    func() []anthropic.ToolUnionParam { return nil },
		MaxSteps: reviewSteps,
		Commands: &guardian.Criteria{Agent: "navigator", Allowed: []guardian.Category{guardian.Read}, Threshold: guardian.ReadOnlyThreshold},
	}
}

// Planner is the navigator's read-only tools and planning prompt, which its reviews carry on with.
func Planner(root string) *Definition {
	return &Definition{
		Name:     "navigator",
		System:   func() string { return planningPrompt(root) },
		Tools:    plannerTools,
		Handlers: plannerHandlers(),
		Policy:   readOnlyShell("navigator"),
		MaxSteps: planSteps,
		// It reads the repository to plan, and nothing more.
		Commands: &guardian.Criteria{Agent: "navigator", Allowed: []guardian.Category{guardian.Read}, Threshold: guardian.ReadOnlyThreshold},
	}
}

// plannerTools has no shell, so every read is a view, which is what seeds the driver.
func plannerTools() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{
		{OfTextEditor20250728: &anthropic.ToolTextEditor20250728Param{Name: editorName}},
		{OfTool: agent.GlobTool()},
		{OfTool: agent.GrepTool()},
	}
}

func plannerHandlers() map[string]agent.Handler {
	handlers := readOnlyHandlers()
	delete(handlers, "bash")
	return handlers
}

// PairedCoder is the coder with a navigator beside it, or nil when there is no repository — the
// same bargain Coder makes, and for the same reason.
//
// It is a second definition rather than a flag on the first because the choice belongs to the
// main agent at dispatch time: most changes are better off going to the plain coder, and a pair
// that cannot be declined is a tax on every one of them.
func PairedCoder(mcp *mcpclient.Manager) *Definition {
	root := project.Root()
	if root == "" || root == string(filepath.Separator) {
		return nil
	}
	return pairedCoderDef(root, mcp)
}

// pairedCoderDef is PairedCoder with the root supplied, so a test can scope one at a temporary
// directory.
func pairedCoderDef(root string, mcp *mcpclient.Manager) *Definition {
	return scopedPair(root, project.Cwd(), mcp)
}

// scopedPair is the pair, driver and navigator both, confined to root and told it starts in cwd.
func scopedPair(root, cwd string, mcp *mcpclient.Manager) *Definition {
	// A fresh rule set rather than the coder's: CoderRules builds a new value per call, so adding
	// the ask tool here cannot reach the plain coder's policy.
	rules := CoderRules(root)
	rules.Tools = append(rules.Tools, agent.AllowTools(askTool)...)

	def := scopedCoder(root, cwd, mcp)
	def.Name = "pair"
	def.Description = "writes code with a reviewer beside it. Same as coder — it edits files, builds and tests inside this " +
		"repository, and cannot commit, push or reach the network — except that a second agent first reads the code and " +
		"plans the change, then reviews the driver's edits, commands and final diff as it goes and steers it when " +
		"something is wrong, and adds its own closing view to the report. Costs more than coder and takes longer. Worth " +
		"it for a change where going down the wrong path is expensive to undo: a refactor across several files, a design " +
		"you are unsure of. Use coder for a change you could state completely."
	def.Policy = rules
	def.Commands = &guardian.Criteria{Agent: "pair", Allowed: []guardian.Category{guardian.Read, guardian.Build, guardian.Edit}, Threshold: guardian.BuildThreshold}
	def.Tools = func() []anthropic.ToolUnionParam { return coderTools(mcp, rules) }
	def.At = func(dir string) *Definition { return scopedPair(dir, dir, mcp) }
	def.System = func() string { return coderPrompt(root, cwd, mcp, rules, true) + "\n\n" + drivingPrompt() }
	def.Seeded = nil
	def.Peer = &PeerSpec{
		Def:     Navigator(),
		Planner: Planner(root),
		Tool:    askTool,
		Seeded:  func() string { return coderPrompt(root, cwd, mcp, rules, false) + "\n\n" + drivingPrompt() },
		Description: "Ask the navigator — the agent that planned this change and reviews your work — about its plan or a " +
			"steer you do not understand, or push back on either with evidence. It can read the repository and sees " +
			"your commands with the tail of their output at checkpoints, but none of your other tool results, and it cannot " +
			"build or run tests, so quote output that bears on the question.",
		Rounds: navigatorQuestions,
		Steers: navigatorSteers,
		Root:   root,
	}
	return def
}

// drivingPrompt is appended to the coder's own prompt.
func drivingPrompt() string {
	return strings.Join([]string{
		"You are driving a pair. A navigator works with you: a second agent that read the repository to plan this change and reviews your work while you make it. It can read files but cannot change anything or run builds and tests.",
		"",
		"Its plan, if it made one, is in your task. Work from it. It was written from reading, not running, so where the repository contradicts it, go with the repository and push back with " + askTool + ", quoting what you found.",
		"",
		"At checkpoints and when you stop, alux tells the navigator which files you edited and shows it your commands and their output, and it reads the files itself; you do not need to report to it. When it sees something wrong it steers you, in a message marked as coming from the navigator. A steer about what the code does or how output renders is a hypothesis, not a finding: when you can check it, check it before you act on it. It is not your approver and it is often wrong; override it when you have the better case.",
		"",
		"Use " + askTool + " when the plan or a steer is unclear, or to dispute one. It costs no turn, and you have " + strconv.Itoa(navigatorQuestions) + " questions.",
		"",
		"If the task cannot be done as stated, stop: it may rest on something untrue of this repository, need a decision nobody has made, or be far larger than described. Before you stop, ask the navigator — what you were asked for, what you found, and why it cannot be done. If it agrees, stop and report what would have to be true for the task to be possible.",
		"",
		"Do not relay what the navigator told you. It reports to the dispatching agent separately, in its own words. Write your own account: what you did, and — where you went against its advice — a clause on why.",
	}, "\n")
}
