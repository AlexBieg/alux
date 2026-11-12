package procs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/anthropics/anthropic-sdk-go"
)

// foregroundLimit is how long a command holds the turn, unless it asks for longer, before it is held for the agent to decide on.
var foregroundLimit = 120 * time.Second

const (
	// maxForeground caps a bash call's seconds, so a stuck command is looked at again at least this often.
	maxForeground = 10 * time.Minute
	minCheckIn    = 5 * time.Second
	maxCheckIn    = 6 * time.Hour

	lifetimeFinite  = "finite"
	lifetimeOngoing = "ongoing"
)

var checkInArg = map[string]any{"type": "integer", "description": "Required. Wake me after this many seconds if " +
	"it is still running (and nothing else has), with its new output and how long since it last printed: your " +
	"chance to spot a stuck command. Set it to about how long the work should take. 5 to 21600."}

var wakeUntilArg = map[string]any{"type": "string", "description": "Also wake me at the first output line this Go " +
	"regular expression matches, leaving the process running: a server's ready line, or a test's first failure. " +
	"Put error lines in the same pattern (\"Listening on|Traceback|EADDRINUSE\") and read which matched. Colour codes " +
	"are stripped, lines already printed but not yet read count, and (?i) ignores case."}

var repeatArg = map[string]any{"type": "boolean", "description": "Wake me every check_in rather than once, until " +
	"it exits: the way to keep an eye on long work and its progress every few minutes without re-arming. Each " +
	"check-in brings the output since the last one."}

func strict(name, description string, properties map[string]any, required []string) anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        name,
		Description: anthropic.String(description),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties:  properties,
			Required:    required,
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}
}

var idArg = map[string]any{
	"type":        "integer",
	"description": "The process id reported when it started, as in 'proc 3'.",
}

// Tools are the background-process tools. bash is not among them: it gains adoption through the handler.
func (r *Registry) Tools() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{
		strict("run_background",
			"Start a shell command that keeps running after this tool returns, and get back a process id instead of "+
				"output. Use it for anything with no natural end — a dev server, a file watcher, a tail — and for work you "+
				"want to carry on with while it runs.\n\n"+
				"Say which with lifetime. \"ongoing\" runs until something stops it: a dev server, a file watcher, a tail, "+
				"an emulator, a tunnel. \"finite\" ends on its own: a test run, a build, a migration. A finite command "+
				"keeps the Mac from idle-sleeping until it exits; an ongoing one never does.\n\n"+
				"It returns as soon as the command starts, so a command that fails immediately reports success here. "+
				"You are woken when it exits, when a line matches until, or when check_in passes, whichever comes "+
				"first, so there is nothing to wait on: carry on with other work, or end your turn if there is none. "+
				"Never sleep or grep its log to find out whether a server is up; give until its ready line. Give repeat "+
				"to be woken every check_in until it exits.\n\n"+
				"Prefer plain bash when you need the output before your next step, giving it seconds when the "+
				"command is known to be long. Do not wrap a command in timeout; a long one is held, not killed.",
			map[string]any{
				"command":  map[string]any{"type": "string", "description": "The command to run, as bash would."},
				"check_in": checkInArg,
				"until":    wakeUntilArg,
				"repeat":   repeatArg,
				"sandbox":  agent.SandboxArg,
				"lifetime": map[string]any{"type": "string", "enum": []string{lifetimeFinite, lifetimeOngoing},
					"description": "Required. finite: it ends on its own, like a test run, a build or a migration. " +
						"ongoing: it runs until stopped, like a dev server, a file watcher, a tail, an emulator or a tunnel."},
			},
			[]string{"command", "check_in", "lifetime"}),
		strict("bash_output",
			"Read what a background process has printed since you last read it. Output already returned is not repeated, "+
				"so this is how a long log is followed rather than re-read. Long output is cut to its first and last lines, "+
				"with the whole saved to a file the notice names; a poll's log says instead how much fell out of its buffer. Empty output means nothing new, which for a server is the normal state.",
			map[string]any{"id": idArg},
			[]string{"id"}),
		strict("kill_shell",
			"Stop a background process, and everything it started. Use it when a server is no longer needed, when a "+
				"command is stuck, or before starting a second copy of something that binds a port.",
			map[string]any{"id": idArg},
			[]string{"id"}),
		strict("poll",
			"Run a quick check command on an interval until its output says to stop, as a process like any other: the "+
				"way to wait on something outside the session — CI, a review bot, a deploy, a container, a URL coming up. "+
				"Never write a sleep loop or a sleep-then-check in bash for this. By default it returns a process id "+
				"with the first check's outcome and you are woken when the poll stops or check_in passes, which is then required; give repeat to "+
				"hear its log every check_in until it stops. With background "+
				"false it holds the turn until the poll stops, like a bash call: the way to do a short act-then-check "+
				"such as a rerun followed by a status read. Past two minutes it is held for background_proc or "+
				"kill_shell, exactly as bash is. Approved once, then rerun without asking.\n\n"+
				"Give until to stop at the first output that matches, or while to keep going while the output matches "+
				"(while=\"pending|queued|in_progress\" waits out a check). With neither, it stops at the first exit 0. "+
				"A non-zero exit is part of the answer, not a failure. Output saying the network could not be reached "+
				"(gh, curl or ssh failing to connect, or whatever unreachable matches) is never an answer: the poll "+
				"logs going offline once, retries at doubling waits up to 5 minutes, and logs coming back. When the "+
				"machine wakes from sleep it logs that and checks at once. Its output "+
				"is a timestamped log of each change in the check's output, not every run.",
			map[string]any{
				"command": map[string]any{"type": "string", "description": "The check, as bash would run it. It should " +
					"return within a minute; a run past that counts as a failure, and three in a row stop the poll."},
				"every": map[string]any{"type": "integer", "description": "Seconds between checks, at least 5. Defaults to 30."},
				"until": map[string]any{"type": "string", "description": "Stop at the first check whose output this Go regular expression matches."},
				"while": map[string]any{"type": "string", "description": "Stop at the first check whose output this Go regular expression does not match."},
				"unreachable": map[string]any{"type": "string", "description": "A Go regular expression for output " +
					"meaning the check could not reach the network, in place of the built-in test."},
				"seconds": map[string]any{"type": "integer", "description": "Give up after this long awake; time the " +
					"machine sleeps is not counted. Defaults to 1800, at most 21600."},
				"background": map[string]any{"type": "boolean", "description": "Defaults to true: return at once and " +
					"be woken when it stops. false waits for it here, up to two minutes before it is held."},
				"check_in": map[string]any{"type": "integer", "description": "Required in the background: wake me " +
					"after this many seconds if the poll is still going, with its log so far. 5 to 21600."},
				"repeat":  repeatArg,
				"sandbox": agent.SandboxArg,
			},
			[]string{"command"}),
		strict("background_proc",
			"Let a held bash command keep running after this turn, and say what should wake you about it: its exit "+
				"always does, and so does the first line matching until or check_in passing, whichever comes first. "+
				"Without this a held command is killed when the turn ends. Then carry on with other work, or end your "+
				"turn if there is none; do not sleep. Called on a process already in the background, it replaces "+
				"that process's wake — the way to keep going after a check-in, or to start or stop it repeating.",
			map[string]any{"id": idArg, "check_in": checkInArg, "until": wakeUntilArg, "repeat": repeatArg},
			[]string{"id", "check_in"}),
	}
}

// BashTool is the main agent's bash: the shared schema, plus seconds, which only a shell that holds rather than kills can honour.
func (r *Registry) BashTool() *anthropic.ToolParam {
	base := agent.BashTool()
	props := map[string]any{}
	for k, v := range base.InputSchema.Properties.(map[string]any) {
		props[k] = v
	}
	props["seconds"] = map[string]any{"type": "integer", "description": "How long to hold the turn for it, " +
		"default 120, at most 600. Give more for a test suite or build you know is long and whose result you need " +
		"next; past it the command is held, not killed."}
	t := *base
	t.Description = anthropic.String("Run a command in your bash shell and get back its combined output and exit " +
		"status. The shell persists between calls: a directory change, a variable, a function or an alias carries over. " +
		"A command still running after seconds is held, not killed, for you to background or kill; the next call then " +
		"gets a new shell.")
	t.InputSchema.Properties = props
	return &t
}

func (w Wake) describe() string {
	switch {
	case w.Repeat && w.Until != nil:
		return fmt.Sprintf("when it exits, at the first line matching /%s/, and every %s while it is still running", w.Until, short(w.CheckIn))
	case w.Repeat:
		return fmt.Sprintf("when it exits, and every %s while it is still running", short(w.CheckIn))
	case w.Until != nil:
		return fmt.Sprintf("when it exits, at the first line matching /%s/, or after %s, whichever comes first", w.Until, short(w.CheckIn))
	}
	return fmt.Sprintf("when it exits, or after %s if it is still running", short(w.CheckIn))
}

// wakeOf reads check_in, until and repeat, refusing a check_in out of range.
func (r *Registry) wakeOf(checkIn int, until string, repeat bool) (Wake, error) {
	d := time.Duration(checkIn) * time.Second
	if d < minCheckIn || d > maxCheckIn {
		return Wake{}, fmt.Errorf("check_in is required, in seconds from %d to %d: about how long the work should take",
			int(minCheckIn.Seconds()), int(maxCheckIn.Seconds()))
	}
	if r.checkInUnit != 0 {
		d = time.Duration(checkIn) * r.checkInUnit
	}
	re, err := pattern("until", until)
	return Wake{CheckIn: d, Until: re, Repeat: repeat}, err
}

// Handlers is the dispatch table for those tools, merged into the agent's by main.
func (r *Registry) Handlers() map[string]agent.Handler {
	return map[string]agent.Handler{
		"run_background":  r.runStart,
		"bash_output":     r.runOutput,
		"kill_shell":      r.runKill,
		"poll":            r.runPoll,
		"background_proc": r.runBackground,
	}
}

func toolStart(cb agent.Callbacks, id, label string, kind agent.ToolKind) {
	if cb.OnToolStart != nil {
		cb.OnToolStart(id, label, kind)
	}
}

func toolResult(cb agent.Callbacks, id, label, output string, kind agent.ToolKind) {
	if cb.OnToolResult != nil {
		cb.OnToolResult(id, label, output, kind)
	}
}

// withWake is a backgrounding call's head: what it runs, then what will wake the agent about it.
func withWake(label string, w Wake) string { return label + " · " + w.Head() }

// kindOf is the box a process's calls are drawn in, so a poll's follow-ups match the poll.
func kindOf(p *Proc) agent.ToolKind {
	if p.Poll {
		return agent.KindPoll
	}
	return agent.KindBash
}

func (r *Registry) runStart(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Command  string `json:"command"`
		CheckIn  int    `json:"check_in"`
		Until    string `json:"until"`
		Repeat   bool   `json:"repeat"`
		Lifetime string `json:"lifetime"`
		Sandbox  *bool  `json:"sandbox"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if strings.TrimSpace(in.Command) == "" {
		return anthropic.NewToolResultBlock(call.ID, "command is required", true)
	}
	wake, err := r.wakeOf(in.CheckIn, in.Until, in.Repeat)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if in.Lifetime != lifetimeFinite && in.Lifetime != lifetimeOngoing {
		return anthropic.NewToolResultBlock(call.ID, `lifetime is required: "finite" for a command that ends on its own, `+
			`"ongoing" for one that runs until it is stopped`, true)
	}

	label := "$ " + in.Command + " &"
	if cb.Approve != nil && !cb.Approve(ctx, label, "") {
		return anthropic.NewToolResultBlock(call.ID, "User declined this command.", true)
	}
	if ctx.Err() != nil {
		return anthropic.NewToolResultBlock(call.ID, "Interrupted by the user.", true)
	}

	// Announced before it starts, like any other tool, which is also what gives the box the call id
	// a process's dot is found by.
	head := withWake(label, wake)
	toolStart(cb, call.ID, head, agent.KindBash)

	p, err := StartAt(r.Context(), in.Command, wrapOf(ctx), whereOf(ctx, in.Sandbox))
	if err != nil {
		toolResult(cb, call.ID, head, err.Error(), agent.KindBash)
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if in.Lifetime == lifetimeFinite {
		r.keepAwake(p)
	}
	id := r.Adopt(p, call.ID)
	r.Arm(p, wake)

	out := fmt.Sprintf("Started as proc %d. You will be woken %s. Carry on with other work, or end your turn if "+
		"there is none.", id, wake.describe())
	toolResult(cb, call.ID, head, out, agent.KindBash)
	return anthropic.NewToolResultBlock(call.ID, out, false)
}

func (r *Registry) runOutput(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	p, bad := r.resolve(call)
	if bad != nil {
		return *bad
	}

	label := fmt.Sprintf("output of proc %d", p.ID)
	out := p.Take()
	status := p.Status()
	switch {
	case strings.TrimSpace(out) == "" && status == "running":
		out = fmt.Sprintf("Nothing new. proc %d is still running after %s.", p.ID, short(p.Elapsed()))
	case strings.TrimSpace(out) == "":
		out = fmt.Sprintf("Nothing new. proc %d has finished — %s.", p.ID, status)
	case status != "running":
		out += fmt.Sprintf("\n[proc %d has finished — %s]", p.ID, status)
	}
	out = p.hint(out)
	toolResult(cb, call.ID, label, out, kindOf(p))
	return anthropic.NewToolResultBlock(call.ID, out, false)
}

func (r *Registry) runKill(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	p, bad := r.resolve(call)
	if bad != nil {
		return *bad
	}

	label := fmt.Sprintf("kill proc %d", p.ID)
	if p.Exited() {
		out := fmt.Sprintf("proc %d had already finished — %s.", p.ID, p.Status())
		toolResult(cb, call.ID, label, out, kindOf(p))
		return anthropic.NewToolResultBlock(call.ID, out, false)
	}
	if cb.Approve != nil && !cb.Approve(ctx, label+" — "+p.Command, "") {
		return anthropic.NewToolResultBlock(call.ID, "User declined this kill.", true)
	}

	p.Kill()
	out := fmt.Sprintf("Killed proc %d (%s).", p.ID, p.Command)
	toolResult(cb, call.ID, label, out, kindOf(p))
	return anthropic.NewToolResultBlock(call.ID, out, false)
}

// pattern compiles an optional regular expression argument, naming the argument when it does not compile.
func pattern(arg, expr string) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid Go regular expression: %v", arg, err)
	}
	return re, nil
}

func (r *Registry) runPoll(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Command     string `json:"command"`
		Every       int    `json:"every"`
		Until       string `json:"until"`
		While       string `json:"while"`
		Unreachable string `json:"unreachable"`
		Seconds     int    `json:"seconds"`
		Background  *bool  `json:"background"`
		CheckIn     int    `json:"check_in"`
		Repeat      bool   `json:"repeat"`
		Sandbox     *bool  `json:"sandbox"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if strings.TrimSpace(in.Command) == "" {
		return anthropic.NewToolResultBlock(call.ID, "command is required", true)
	}
	foreground := in.Background != nil && !*in.Background
	var wake Wake
	if !foreground {
		var err error
		if wake, err = r.wakeOf(in.CheckIn, "", in.Repeat); err != nil {
			return anthropic.NewToolResultBlock(call.ID, err.Error()+"; or set background false to wait for it here", true)
		}
	}
	if in.Until != "" && in.While != "" {
		return anthropic.NewToolResultBlock(call.ID, "give until or while, not both", true)
	}
	s := Poll{Check: in.Command, Every: defaultPollEvery, For: defaultPollFor, At: whereOf(ctx, in.Sandbox)}
	if pinned, ok := agent.GhRead(in.Command); ok && oneOff(in.Sandbox) {
		s.Check = pinned
	}
	if in.Every != 0 {
		s.Every = time.Duration(in.Every) * time.Second
	}
	if s.Every < minPollEvery {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("every must be at least %d seconds", int(minPollEvery.Seconds())), true)
	}
	if in.Seconds != 0 {
		s.For = min(time.Duration(in.Seconds)*time.Second, maxPollFor)
	}
	var err error
	if s.Until, err = pattern("until", in.Until); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if s.While, err = pattern("while", in.While); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if s.Unreachable, err = pattern("unreachable", in.Unreachable); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}

	label := s.describe()
	if cb.Approve != nil && !cb.Approve(ctx, label, "") {
		return anthropic.NewToolResultBlock(call.ID, "User declined this command.", true)
	}
	if ctx.Err() != nil {
		return anthropic.NewToolResultBlock(call.ID, "Interrupted by the user.", true)
	}
	if foreground {
		toolStart(cb, call.ID, label, agent.KindPoll)
		p := StartPoll(r.Context(), s, wrapOf(ctx))
		out := r.holdTurn(ctx, call.ID, p, foregroundLimit)
		toolResult(cb, call.ID, label, out, agent.KindPoll)
		return anthropic.NewToolResultBlock(call.ID, out, false)
	}
	head := withWake(label, wake)
	toolStart(cb, call.ID, head, agent.KindPoll)
	p := StartPoll(r.Context(), s, wrapOf(ctx))
	r.keepAwake(p)
	id := r.Adopt(p, call.ID)
	r.Arm(p, wake)

	next := "or after " + short(wake.CheckIn) + " if it is still going"
	if wake.Repeat {
		next = "and every " + short(wake.CheckIn) + " while it is still going"
	}
	out := fmt.Sprintf("Polling as proc %d, for up to %s. You will be woken when it stops, %s. Carry on with other "+
		"work, or end your turn if there is none.", id, short(s.For), next)
	timer := time.NewTimer(checkTimeout + firstCheckMargin)
	defer timer.Stop()
	select {
	case <-p.checked:
		out += fmt.Sprintf("\n\nFirst check:\n%s\nNot met yet: %s.\nIf that is not what you meant to watch, "+
			"kill_shell(%d) and start a corrected poll.", strings.TrimSpace(p.Take()), p.unmet, id)
	case <-p.Done():
		out = p.hint(strings.TrimSpace(strings.TrimSpace(r.Claim(p)+p.Take()) +
			fmt.Sprintf("\n[proc %d finished — %s]", id, p.Status())))
	case <-ctx.Done():
	case <-timer.C:
	}
	toolResult(cb, call.ID, head, out, agent.KindPoll)
	return anthropic.NewToolResultBlock(call.ID, out, false)
}

const firstCheckMargin = 5 * time.Second

func (r *Registry) runBackground(_ context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	p, bad := r.resolve(call)
	if bad != nil {
		return *bad
	}
	var in struct {
		CheckIn int    `json:"check_in"`
		Until   string `json:"until"`
		Repeat  bool   `json:"repeat"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	wake, err := r.wakeOf(in.CheckIn, in.Until, in.Repeat)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	label := withWake(fmt.Sprintf("background proc %d", p.ID), wake)
	var out string
	if r.Background(p) && r.Arm(p, wake) {
		out = fmt.Sprintf("proc %d will keep running after this turn. You will be woken %s. Carry on with other work, "+
			"or end your turn if there is none.", p.ID, wake.describe())
	} else {
		out = strings.TrimSpace(r.Claim(p)+p.Take()) + fmt.Sprintf("\n[proc %d had already finished — %s]", p.ID, p.Status())
		out = strings.TrimSpace(out)
	}
	toolResult(cb, call.ID, label, out, kindOf(p))
	return anthropic.NewToolResultBlock(call.ID, out, false)
}

// stillRunning is the status line for a held process, with the choices that apply to it.
func (r *Registry) stillRunning(p *Proc) string {
	return fmt.Sprintf("proc %d still running after %s, last output %s ago — NOT killed. background_proc(%d, "+
		"check_in) to let it run on and be woken when it exits or the check-in passes, kill_shell(%d) to stop it. If "+
		"you do neither, it is killed when this turn ends. Do not start it again; next time, give bash a longer "+
		"seconds, or run a poll in the background.", p.ID, short(p.Elapsed()), short(p.Silent()), p.ID, p.ID)
}

// resolve reads the id argument and finds its process, returning the block that answers the call
// when it cannot. The error names what is running: a model guessing at an id should see the list.
func (r *Registry) resolve(call agent.Call) (*Proc, *anthropic.ContentBlockParamUnion) {
	var in struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return nil, block(anthropic.NewToolResultBlock(call.ID, err.Error(), true))
	}
	p, ok := r.Get(in.ID)
	if !ok {
		return nil, block(anthropic.NewToolResultBlock(call.ID,
			fmt.Sprintf("There is no proc %d. %s", in.ID, r.describeRunning()), true))
	}
	return p, nil
}

func (r *Registry) describeRunning() string {
	running := r.Running()
	if len(running) == 0 {
		return "Nothing is running in the background."
	}
	names := make([]string, 0, len(running))
	for _, p := range running {
		names = append(names, fmt.Sprintf("proc %d (%s)", p.ID, p.Command))
	}
	return "Still running: " + strings.Join(names, ", ") + "."
}

func block(b anthropic.ContentBlockParamUnion) *anthropic.ContentBlockParamUnion { return &b }

// oneOff is a call that asked to run outside the sandbox, which never shares the live shell.
func oneOff(sandboxArg *bool) bool { return sandboxArg != nil && !*sandboxArg }

// whereOf starts a fresh process where the agent's shell is; one outside the sandbox gets the base
// environment, so nothing exported inside it reaches a command run outside.
func whereOf(ctx context.Context, sandboxArg *bool) Where {
	sh, ok := shell.From(ctx)
	if !ok {
		return Where{}
	}
	if oneOff(sandboxArg) {
		return Where{Dir: sh.Dir()}
	}
	return Where{Dir: sh.Dir(), Env: sh.Env()}
}

func wrapOf(ctx context.Context) *sandbox.Wrap {
	if w, ok := sandbox.From(ctx); ok {
		return &w
	}
	return nil
}

func (p *Proc) hint(out string) string { return p.wrap.Hint(out) }

// Bash is the foreground shell; at the limit it holds the command for the agent instead of killing it.
func (r *Registry) Bash() agent.Handler {
	return func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct {
			Command string `json:"command"`
			Restart bool   `json:"restart"`
			Seconds int    `json:"seconds"`
			Head    *int   `json:"head"`
			Tail    *int   `json:"tail"`
			Sandbox *bool  `json:"sandbox"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
		}
		sh, live := shell.From(ctx)
		if in.Restart {
			if !live {
				return anthropic.NewToolResultBlock(call.ID, "There is no shell to restart; every call gets a fresh one.", false)
			}
			sh.Restart()
			return anthropic.NewToolResultBlock(call.ID, "Shell restarted at "+sh.Root()+".", false)
		}
		limit := foregroundLimit
		if in.Seconds > 0 {
			limit = min(time.Duration(in.Seconds)*time.Second, maxForeground)
		}

		label := "$ " + in.Command
		if cb.Approve != nil && !cb.Approve(ctx, label, "") {
			return anthropic.NewToolResultBlock(call.ID, "User declined this command.", true)
		}
		if ctx.Err() != nil {
			return anthropic.NewToolResultBlock(call.ID, "Interrupted by the user.", true)
		}
		toolStart(cb, call.ID, label, agent.KindBash)

		var p *Proc
		var err error
		switch {
		case live && !oneOff(in.Sandbox):
			p, err = Attach(ctx, sh, in.Command, wrapOf(ctx))
		default:
			p, err = StartAt(r.Context(), in.Command, wrapOf(ctx), whereOf(ctx, in.Sandbox))
		}
		if err != nil {
			toolResult(cb, call.ID, label, err.Error(), agent.KindBash)
			return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
		}
		p.SetLines(agent.Lines(in.Head, in.Tail))

		out := r.holdTurn(ctx, call.ID, p, limit)
		toolResult(cb, call.ID, label, out, agent.KindBash)
		return anthropic.NewToolResultBlock(call.ID, out, false)
	}
}

// holdTurn waits on p until it ends, the user cancels, or limit holds it for the agent to decide on.
func (r *Registry) holdTurn(ctx context.Context, call string, p *Proc, limit time.Duration) string {
	r.keepAwake(p)
	r.watch(call, p)
	defer r.unwatch(call)
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-p.Done():
		agent.SetExit(ctx, p.Exit())
		return p.hint(strings.TrimSpace(p.Take() + "\n[" + p.Status() + "]"))
	case <-ctx.Done():
		p.Kill()
		return strings.TrimSpace(p.Take() + "\n[cancelled by the user]")
	case <-timer.C:
		if p.detach != nil && !p.detach() {
			<-p.Done()
			agent.SetExit(ctx, p.Exit())
			return p.hint(strings.TrimSpace(p.Take() + "\n[" + p.Status() + "]"))
		}
		r.Hold(p, call)
		return p.hint(strings.TrimSpace(strings.TrimSpace(p.Take()) + "\n\n[" + r.stillRunning(p) + "]"))
	}
}

// brief is a set interval as a head reads it: 30s, 5m, 1h30m.
func brief(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.ContainsAny(s, "mh") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.Contains(s, "h") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// short is a duration a glance can read.
func short(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
