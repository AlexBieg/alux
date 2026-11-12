package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/clip"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

// The tools alux always has, whatever MCP servers happen to be configured. The text editor is a
// server-side type; bash is defined here because the server-side schema cannot carry sandbox.
func BuiltinTools() []anthropic.ToolUnionParam {
	tools := []anthropic.ToolUnionParam{
		{OfTool: BashTool()},
		{OfTextEditor20250728: &anthropic.ToolTextEditor20250728Param{Name: "str_replace_based_edit_tool"}},
		{OfTool: &globTool},
		{OfTool: &grepTool},
		{OfTool: &askTool},
		{OfTool: &declineSuggestionTool},
		{OfTool: &composeTool},
	}
	tools = append(tools, VaultTools()...)
	tools = append(tools, RecordTools()...)
	tools = append(tools, RecordReadTools()...)
	tools = append(tools, MigrationTools()...)
	tools = append(tools, CalendarTools()...)
	tools = append(tools, BrowserTools()...)
	return append(tools, WebTools()...)
}

// SandboxArg is the bash and run_background argument that asks to run a command outside the sandbox.
var SandboxArg = map[string]any{
	"type": "boolean",
	"description": "Defaults to true: the command runs in a sandbox that can write temp and tool caches, and the project " +
		"unless the session is read-only or its change has no description; it reaches only localhost, except in auto mode, " +
		"where it has the network. Set false for pushing, gh writes, installs, and anything needing the network or writes " +
		"outside the project; false runs it once the command screen passes it, and otherwise asks the person, " +
		"or is refused when nobody can be asked.",
}

var bashSchema = anthropic.ToolParam{
	Name: bashTool,
	Description: anthropic.String("Run a command in your bash shell and get back its combined output and exit status. " +
		"The shell persists between calls: a directory change, a variable, a function or an alias carries over. A command " +
		"that runs past its time limit is stopped, and the shell with it."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"command": map[string]any{"type": "string", "description": "The command to run."},
			"restart": map[string]any{"type": "boolean", "description": "Start a new shell at the working directory with a clean environment, instead of running a command."},
			"sandbox": SandboxArg,
			"head":    headArg,
			"tail":    tailArg,
		},
		Required:    []string{"command"},
		ExtraFields: map[string]any{"additionalProperties": false},
	},
}

var (
	headArg = map[string]any{"type": "integer", "description": fmt.Sprintf("Lines kept from the start of long output, "+
		"default %d. Past head plus tail lines, or about 30,000 characters, the middle is cut and the whole output is "+
		"saved to a file the notice names, with the line numbers that were cut, so there is no need to pipe into head "+
		"or tail.", clip.DefaultHead)}
	tailArg = map[string]any{"type": "integer", "description": fmt.Sprintf("Lines kept from the end of long output, "+
		"default %d.", clip.DefaultTail)}
)

// Lines is the head and tail a bash call asked for, defaults filled in.
func Lines(head, tail *int) (int, int) {
	h, t := clip.DefaultHead, clip.DefaultTail
	if head != nil {
		h = max(*head, 0)
	}
	if tail != nil {
		t = max(*tail, 0)
	}
	return h, t
}

// BashTool is the shell every agent with one is offered.
func BashTool() *anthropic.ToolParam { return &bashSchema }

// GlobTool and GrepTool are the search tool schemas, exported so a read-only subagent can be
// given the same two tools the main agent has rather than a second description of them that
// drifts.
func GlobTool() *anthropic.ToolParam { return &globTool }
func GrepTool() *anthropic.ToolParam { return &grepTool }

// DeclineSuggestionTool is listed by every agent with a shell, since any of them can be suggested a tool.
func DeclineSuggestionTool() *anthropic.ToolParam { return &declineSuggestionTool }

// BuiltinHandlers is the dispatch table for those tools. bash is deliberately absent: it is the
// fallback, so an unrecognised name from a model that has been told about the bash tool still
// runs rather than erroring.
func BuiltinHandlers() map[string]Handler {
	handlers := map[string]Handler{
		"str_replace_based_edit_tool": runEdit,
		"ask_user":                    runAsk,
		"glob":                        RunSearch,
		"grep":                        RunSearch,
	}
	for name, handler := range VaultHandlers() {
		handlers[name] = handler
	}
	for name, handler := range RecordHandlers() {
		handlers[name] = handler
	}
	for name, handler := range RecordReadHandlers() {
		handlers[name] = handler
	}
	for name, handler := range MigrationHandlers() {
		handlers[name] = handler
	}
	for name, handler := range CalendarHandlers() {
		handlers[name] = handler
	}
	for name, handler := range WebHandlers() {
		handlers[name] = handler
	}
	for name, handler := range BrowserHandlers() {
		handlers[name] = handler
	}
	return handlers
}

var dirArg = map[string]any{
	"type": "string",
	"description": "Directory to search in, relative to the working directory or absolute. Defaults to the whole working " +
		"directory.",
}

var hiddenArg = map[string]any{
	"type": "boolean",
	"description": "Also search hidden and .gitignored paths. Off by default, which is the view git has — so set it when a " +
		"match you are confident exists comes back as No matches, or when the target is itself hidden or ignored.",
}

var globTool = anthropic.ToolParam{
	Name: "glob",
	Description: anthropic.String("Find files by name, using fd. Hidden files and .gitignored paths are skipped unless " +
		"hidden is set. Prefer this to running fd or find through bash: the output is capped and sorted."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Glob, e.g. *.go matches by file name anywhere, internal/**/*.go matches by path, Dockerfile* by prefix",
			},
			"path":   dirArg,
			"hidden": hiddenArg,
		},
		Required: []string{"pattern"},
	},
}

var grepTool = anthropic.ToolParam{
	Name: "grep",
	Description: anthropic.String("Search file contents by regular expression, using ripgrep. Hidden files and .gitignored " +
		"paths are skipped unless hidden is set; binaries always are. Prefer this to running rg or grep through bash: the output " +
		"is capped and long lines are trimmed. A pattern matches within one line unless multiline is set."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Rust regex to search for"},
			"path":    dirArg,
			"glob": map[string]any{
				"type": "string",
				"description": "Only search files whose path matches this glob, e.g. *.go. Prefix with ! to exclude instead, " +
					"e.g. !*_test.go",
			},
			"type": map[string]any{
				"type":        "string",
				"description": "Only search files of this ripgrep type, e.g. go, py, ts.",
			},
			"output_mode": map[string]any{
				"type": "string",
				"enum": []string{tools.ModeContent, tools.ModeFiles, tools.ModeFilesWithout, tools.ModeCount},
				"description": "content (the default) shows matching lines with their line numbers; files_with_matches shows " +
					"only the paths of matching files, for surveying where something appears; files_without_match shows the " +
					"paths of files with no match, e.g. handlers that never call a check; count shows how many lines match in each file.",
			},
			"-i": map[string]any{"type": "boolean", "description": "Ignore case."},
			"-A": map[string]any{"type": "integer", "description": "Lines to show after each match. content mode only."},
			"-B": map[string]any{"type": "integer", "description": "Lines to show before each match. content mode only."},
			"-C": map[string]any{"type": "integer", "description": "Lines to show before and after each match. content mode only."},
			"-o": map[string]any{"type": "boolean", "description": "Show only the matched part of each line."},
			"multiline": map[string]any{
				"type":        "boolean",
				"description": "Let the pattern span lines: . matches newlines and \\n can appear in the pattern. Slower over a large tree.",
			},
			"head_limit": map[string]any{
				"type": "integer",
				"description": "Show only the first N results, like piping to head. With -A/-B/-C a result is a group: a match, " +
					"or adjacent matches, with their context. With multiline and no context it is a line. " +
					"The output is capped at 200 lines and about 20,000 characters regardless; groups are kept whole, except a " +
					"first group longer than the cap on its own.",
			},
			"hidden": hiddenArg,
		},
		Required: []string{"pattern"},
	},
}

const DeclineSuggestion = "decline_suggestion"

var declineSuggestionTool = anthropic.ToolParam{
	Name: DeclineSuggestion,
	Description: anthropic.String("Decline a tool suggested in place of a blocked shell command. It puts the blocked command, " +
		"exactly as you first wrote it, to the user together with your reason, and runs it only if they approve. Use it only " +
		"with an id from a blocked call's result in this turn; suggestions lapse when the turn ends, and each id can be " +
		"declined once."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"id":     map[string]any{"type": "string", "description": "The suggestion's id, as the blocked call's result gave it, e.g. s1"},
			"reason": map[string]any{"type": "string", "description": "Why the suggested tool cannot do this work, for the user to read"},
		},
		Required: []string{"id", "reason"},
	},
}

var askContextArg = map[string]any{
	"type": "string",
	"description": "What the user needs to know to answer: what you found and why it forces this choice. Shown above the " +
		"options, reproduced verbatim in a monospace block, so it can also hold an ascii diagram, a code diff, a short table, " +
		"the two snippets being chosen between. Lines starting with + or - are coloured as a diff. Keep it short enough to " +
		"read on screen.",
}

var askOptionsArg = map[string]any{
	"type": "array",
	"description": "The answers to offer, in the order they should be read. Make each one a decision the user could act on, " +
		"not a restatement of the question. Omit entirely to ask an open question.",
	"items": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label":  map[string]any{"type": "string", "description": "The answer itself, short enough to read in one line."},
			"detail": map[string]any{"type": "string", "description": "One line on what this choice implies, shown under the label."},
		},
		"required": []string{"label"},
	},
}

var askAllowOtherArg = map[string]any{
	"type":        "boolean",
	"description": "Offer a free-text choice alongside the options. Defaults to true; set false only when the options genuinely cover the field.",
}

var askTool = anthropic.ToolParam{
	Name: "ask_user",
	Description: anthropic.String("Put a question — or a short series of them — to the user and wait for their answers. Use it when a " +
		"decision is genuinely theirs: a fork in the approach, a missing requirement, a destructive step worth confirming in words " +
		"rather than a yes/no, and generally when guessing would risk doing the wrong work. Do not use it for things you can find out " +
		"by reading the repo, and do not use it to narrate progress.\n\n" +
		"The user cannot see your thinking. The question, context and options must stand on their own: name the finding, file " +
		"or trade-off the choice rests on.\n\n" +
		"One decision is one question: pass question (plus context, options, allow_other). Several decisions are a series: pass " +
		"questions instead, and the user answers them one after another in a single uninterrupted flow before the turn resumes. " +
		"Prefer that to welding unrelated choices into one consolidated question — a series costs no more than a single question does, " +
		"and each answer can then be a real answer rather than a line in a table the user has to compose. At most 6 per call.\n\n" +
		"A later question can be gated on an earlier answer with when, so a branch the user does not take is never put to them; the " +
		"result says which questions were skipped and why. Gating matches option labels, so a question gated on a free-text answer is " +
		"skipped. When an answer changes what else needs asking in a way you could not have declared up front — the skipped branch now " +
		"matters, or their own words opened something new — call this tool again. Do not ask a question whose answer you already have."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"question":    map[string]any{"type": "string", "description": "The question, in one or two sentences. Use this or questions, not both."},
			"context":     askContextArg,
			"options":     askOptionsArg,
			"allow_other": askAllowOtherArg,
			"questions": map[string]any{
				"type": "array",
				"description": "A series of questions, asked in this order, one screen at a time. Use instead of question when the user " +
					"has several decisions to make; order them so an answer never depends on a question that comes later.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{
							"type": "string",
							"description": "Short identifier for this question, needed only so a later question's when can refer to it. " +
								"Defaults to its position: q1, q2, …",
						},
						"question":    map[string]any{"type": "string", "description": "The question, in one or two sentences."},
						"context":     askContextArg,
						"options":     askOptionsArg,
						"allow_other": askAllowOtherArg,
						"when": map[string]any{
							"type":        "object",
							"description": "Ask this question only if an earlier one was answered a particular way. Omit to always ask it.",
							"properties": map[string]any{
								"question": map[string]any{"type": "string", "description": "The id of an earlier question in this series."},
								"answered": map[string]any{
									"type":        "array",
									"description": "Ask this question if that one was answered with any of these option labels, spelled as you spelled them there.",
									"items":       map[string]any{"type": "string"},
								},
							},
							"required": []string{"question", "answered"},
						},
					},
					"required": []string{"question"},
				},
			},
		},
	},
}

// RunEdit is the editor with every command available. Exported so an editing subagent gets the
// same handler the main agent has rather than a second one that drifts from it; what an editing
// subagent may actually write is decided by its policy, not by a different handler.
func RunEdit(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	return runEdit(ctx, call, cb)
}

func runEdit(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in tools.EditInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	if err := checkPath(ctx, inDir(ctx, in.Path), in.Command != "view"); err != nil {
		return failed(call.ID, err)
	}
	label := tools.DescribeEdit(in)
	what, config := userConfig(inDir(ctx, in.Path))
	switch {
	case in.Command == "view":
	case config:
		if cb.Require == nil {
			return refusedResult(call.ID, "editing "+what+" needs the user's yes (and there is nobody to ask)")
		}
		if !cb.Require(ctx, label, tools.PreviewEdit(in), "it edits "+what+", which only the user may allow") {
			return declined(call.ID, "edit")
		}
	case !cb.Approve(ctx, label, tools.PreviewEdit(in)):
		return declined(call.ID, "edit")
	}
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}
	cb.toolStart(call.ID, label, KindEdit)
	in.Path = inDir(ctx, in.Path)
	res := tools.RunEdit(in)
	cb.toolResult(call.ID, label, res.Display, KindEdit)
	return editResultBlock(call.ID, in, res)
}

// inDir anchors a relative path at the directory the tools run in under ctx.
func inDir(ctx context.Context, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(tools.Dir(ctx), path)
}

func editResultBlock(id string, in tools.EditInput, res tools.EditResult) anthropic.ContentBlockParamUnion {
	if res.Image == nil {
		return anthropic.NewToolResultBlock(id, markWebContent(in, res.Output), res.IsError)
	}
	return imageResultBlock(id, res.Output, res.Image, res.IsError)
}

func imageResultBlock(id, text string, img *tools.Image, isError bool) anthropic.ContentBlockParamUnion {
	image := anthropic.NewImageBlockBase64(img.MediaType, img.Data)
	return anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
		ToolUseID: id,
		Content: []anthropic.ToolResultBlockParamContentUnion{
			{OfText: &anthropic.TextBlockParam{Text: text}},
			{OfImage: image.OfImage},
		},
		IsError: anthropic.Bool(isError),
	}}
}

// RunView is the editor with everything but view refused. It is what a read-only subagent gets:
// the restriction is enforced here rather than by asking the model nicely, because "it cannot
// change anything" is the property that lets several of them run at once without an answer for
// where an approval prompt would go.
func RunView(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in tools.EditInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	if in.Command != "view" {
		return anthropic.NewToolResultBlock(call.ID,
			"This agent is read-only: only the view command is available. Report what needs changing instead of changing it.", true)
	}
	if err := checkPath(ctx, inDir(ctx, in.Path), false); err != nil {
		return failed(call.ID, err)
	}
	label := tools.DescribeEdit(in)
	cb.toolStart(call.ID, label, KindEdit)
	in.Path = inDir(ctx, in.Path)
	res := tools.RunEdit(in)
	cb.toolResult(call.ID, label, res.Display, KindEdit)
	return editResultBlock(call.ID, in, res)
}

// RunSearch is read-only, so it skips the approval prompt. Exported for the same reason RunView
// is: a read-only subagent runs the same search the main agent does.
func RunSearch(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in tools.SearchInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	if err := checkSearch(ctx, in.Path); err != nil {
		return failed(call.ID, err)
	}
	label := tools.DescribeSearch(call.Name, in)
	cb.toolStart(call.ID, label, KindSearch)
	output, isError := tools.RunSearch(ctx, call.Name, in)
	cb.toolResult(call.ID, label, output, KindSearch)
	return anthropic.NewToolResultBlock(call.ID, output, isError)
}

// Asking needs no approval prompt — a question is already the user being asked — and it ignores
// auto mode, so the turn parks here until the answers come back or the user cancels.
//
// A series is walked here rather than in the session: one question reaches the snapshot at a
// time, so the panel, the mirror pane and cancellation all keep working on the shape they already
// had, and the model is out of the loop until the last answer.
func runAsk(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	seq := ask.Parse(call.Input)
	exchanges, cancelled := ask.Run(seq, func(q ask.Question) *ask.Answer { return cb.Ask(ctx, q) })
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}
	// Backing out of the first question answers nothing, which is a declined call. Backing out
	// later is not: the answers already given are real, and discarding them would make the user
	// give them again.
	if len(exchanges) == 0 {
		return declined(call.ID, "question")
	}
	// No start announced: the question panel has been showing what this call is doing all along.
	cb.toolResult(call.ID, ask.Describe(exchanges), ask.Display(exchanges, cancelled), KindAsk)
	return ok(call.ID, ask.Format(exchanges, cancelled))
}

// RunBash is the fallback handler. Exported so main can install it after the MCP check.
func RunBash(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Command string `json:"command"`
		Restart bool   `json:"restart"`
		Head    *int   `json:"head"`
		Tail    *int   `json:"tail"`
		Sandbox *bool  `json:"sandbox"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	sh, live := shell.From(ctx)
	if in.Restart {
		if !live {
			return ok(call.ID, "There is no shell to restart; every call gets a fresh one.")
		}
		sh.Restart()
		return ok(call.ID, "Shell restarted at "+sh.Root()+".")
	}
	head, tail := Lines(in.Head, in.Tail)
	label := "$ " + in.Command
	// A peer has no approval path; its policy and guardian are the whole decision, as in runMcp.
	if cb.Approve != nil && !cb.Approve(ctx, label, "") {
		return declined(call.ID, "command")
	}
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}
	cb.toolStart(call.ID, label, KindBash)
	// Cancelling kills the command, so its partial output is still worth showing and sending back.
	var output, status string
	switch {
	case live && (in.Sandbox == nil || *in.Sandbox):
		output, status = tools.RunShell(ctx, sh, in.Command, head, tail)
	case live:
		// Outside the sandbox it runs fresh where the shell is, with nothing the shell exported.
		output, status = tools.RunBash(tools.WithDir(ctx, sh.Dir()), in.Command, head, tail)
	default:
		output, status = tools.RunBash(ctx, in.Command, head, tail)
	}
	SetExit(ctx, status)
	cb.toolResult(call.ID, label, output, KindBash)
	return ok(call.ID, output)
}
