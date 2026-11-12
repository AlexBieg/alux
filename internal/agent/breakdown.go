package agent

import (
	"context"
	"fmt"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/llm"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Row is one line of a Report: what occupies the prompt, and how much of it.
type Row struct {
	Label  string
	Tokens int64
}

// Report is where the next request's prompt would go, measured rather than estimated.
// Managed is the same prompt after the server-side edit, so Total-Managed is what it reclaims —
// since 2026-10-02 that is thinking older than the last answered turn and nothing else.
type Report struct {
	Rows    []Row
	Total   int64
	Managed int64
	Window  int64
}

// barWidth is the drawing room a row's share gets. Fixed rather than fitted to the pane, so the
// same prompt draws the same picture whatever the window is doing.
const barWidth = 24

// String renders the report as the pane shows it: a column of labels, a column of counts, and a
// bar per row showing its share of the total. Every column is padded to a common width and the
// bars carry colour, so this is preformatted, styled text — the pane must print it verbatim
// rather than through the markdown renderer, which would reflow it into a paragraph and lose the
// alignment entirely.
func (r Report) String() string {
	labels := len("after trimming")
	counts := len(commas(r.Total))
	for _, row := range r.Rows {
		labels = max(labels, len(row.Label))
		counts = max(counts, len(commas(row.Tokens)))
	}
	// One row, rendered into the same columns whether or not it has a bar and a share to draw.
	line := func(label, count, tail string) string {
		return strings.TrimRight(fmt.Sprintf("  %-*s  %*s  %s", labels, label, counts, count, tail), " ")
	}

	var b strings.Builder
	for _, row := range r.Rows {
		share := float64(row.Tokens) / float64(max(r.Total, 1))
		// The bar pads itself to barWidth with its own track, and carries colour escapes, so it
		// must not go through a %-*s width — that counts bytes, and would pad by the escapes too.
		fmt.Fprintln(&b, line(row.Label, commas(row.Tokens),
			bar(share)+fmt.Sprintf(" %5.1f%%", 100*share)))
	}
	// The rule spans the columns a row fills: two gaps, the bar, a gap and the share.
	fmt.Fprintln(&b, "  "+strings.Repeat("─", labels+2+counts+2+barWidth+1+len(" 00.0%")))

	// The total's own bar would be a full-width block saying 100%, so its column carries the one
	// figure the rows cannot: how much of the window all of this occupies.
	window := ""
	if r.Window > 0 {
		window = fmt.Sprintf("%.1f%% of the %s window", 100*float64(r.Total)/float64(r.Window), commas(r.Window))
	}
	fmt.Fprintln(&b, line("total", commas(r.Total), window))

	switch {
	// Managed is zero when there is no conversation yet, which is not a saving of the whole prompt.
	case r.Managed > 0 && r.Managed < r.Total:
		fmt.Fprintln(&b, line("after trimming", commas(r.Managed),
			commas(r.Total-r.Managed)+" reclaimed server-side"))
	default:
		// No trigger to quote any more: the one remaining edit clears old thinking on every
		// request, so the only reason for no saving is that fewer than two turns have been answered.
		fmt.Fprint(&b, "\n  Nothing trimmed yet — thinking clears after two answered turns.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// The bars are drawn by bubbles' progress component, whose fill is a whole number of cells:
// internally it rounds width×percent to an integer, so every row under half a cell — vault tools
// at 2.4%, request framing at 0.0% — would round away to an empty bar and read as costing
// nothing. So the bar is rounded UP to the next whole cell and that trailing cell is dimmed
// towards the track colour in proportion to the fraction it actually earns. The resolution the
// old eighth-blocks carried in width is carried in brightness instead: a tiny row is one faint
// cell that brightens as it grows.
var (
	barFull  = lipgloss.Color("#7571F9") // bubbles' own blueberry, kept so the bar looks stock
	barEmpty = lipgloss.Color("#606060")
	// Finer than any terminal will resolve, and it costs one slice built once.
	barBlend = lipgloss.Blend1D(64, barEmpty, barFull)
)

func bar(share float64) string {
	// Cells the share has genuinely earned, and the whole cells that takes to draw.
	earned := min(max(share, 0), 1) * barWidth
	cells := int(math.Ceil(earned))
	// How much of the trailing cell is real, in (0,1]. Full rows earn all of it.
	frac, last := 1.0, cells-1
	if cells > 0 {
		frac = earned - float64(last)
	}

	m := progress.New(
		progress.WithWidth(barWidth),
		progress.WithoutPercentage(),
		// A space track rather than bubbles' '░': the empty run is padding to the percentage
		// column, and six rows of dots would be six rows of noise saying nothing.
		progress.WithFillCharacters('█', ' '),
		progress.WithColorFunc(func(_, current float64) color.Color {
			if int(math.Round(current*barWidth)) == last {
				return barBlend[int(frac*float64(len(barBlend)-1))]
			}
			return barFull
		}),
	)
	// Asking for exactly the cells we counted: bubbles rounds width×percent, and this is integral.
	return m.ViewAs(float64(cells) / barWidth)
}

func commas(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}

// probe stands in for the conversation while the fixed parts are measured; count_tokens rejects
// a request with no messages at all.
var probe = []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("."))}

// Breakdown measures the prompt a turn would send, a part at a time, on the count_tokens
// endpoint. Counting is not billed, so this costs latency only.
func (a *Agent) Breakdown(ctx context.Context, history []anthropic.MessageParam) (Report, error) {
	tools := a.tools()
	groups := groupTools(tools)

	// Each step adds one more part to the same request, so a row is the difference between two
	// neighbouring counts. Measuring parts in isolation instead would leave the rows failing to
	// add up, because tokenizing a fragment alone does not cost exactly its share of the whole.
	steps := []step{
		{messages: probe},
		{label: "system prompt", system: a.system(), messages: probe},
	}
	sofar := []anthropic.ToolUnionParam{}
	for _, g := range groups {
		sofar = append(sofar, g.tools...)
		steps = append(steps, step{label: g.label + " tools", system: a.system(),
			tools: slices.Clone(sofar), messages: probe})
	}
	for _, layer := range layers(history) {
		steps = append(steps, step{label: layer.label, system: a.system(), tools: tools, messages: layer.messages})
	}

	// Independent requests, so the whole report costs one round trip rather than a dozen.
	var wg sync.WaitGroup
	counts := make([]tally, len(steps))
	var managed tally
	run := func(into *tally, fn func() (int64, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			into.n, into.err = fn()
		}()
	}
	for i, s := range steps {
		run(&counts[i], func() (int64, error) { return a.count(ctx, s.messages, s.system, s.tools, false) })
	}
	run(&managed, func() (int64, error) { return a.count(ctx, history, a.system(), tools, true) })
	wg.Wait()

	for _, c := range append(slices.Clone(counts), managed) {
		if c.err != nil {
			return Report{}, c.err
		}
	}

	var rows []Row
	for i := 1; i < len(steps); i++ {
		rows = append(rows, Row{steps[i].label, counts[i].n - counts[i-1].n})
	}
	// What is left is the empty request: the message envelope every call pays for.
	rows = append(rows, Row{"request framing", counts[0].n})

	return Report{Rows: rows, Total: counts[len(counts)-1].n, Managed: managed.n, Window: a.window()}, nil
}

// Baseline is what an empty conversation costs: the system prompt and tools, counted, not sent.
func (a *Agent) Baseline(ctx context.Context) (Usage, error) {
	n, err := a.count(ctx, probe, a.system(), a.tools(), false)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Prompt: n, Window: a.window(), Counted: true}, nil
}

type layer struct {
	label    string
	messages []anthropic.MessageParam
}

// layers rebuilds the history one kind of content at a time — the user's messages, then the
// replies, then each tool's calls and results, then thinking — so each layer's row is a measured
// difference and the last layer is the whole conversation. Kinds absent from the history get none.
func layers(history []anthropic.MessageParam) []layer {
	if len(history) == 0 {
		return nil
	}
	names := map[string]string{}
	var tools []string
	thinking := false
	for _, m := range history {
		for _, b := range m.Content {
			if id, name := b.GetID(), b.GetName(); id != nil && name != nil {
				names[*id] = *name
				if !slices.Contains(tools, *name) {
					tools = append(tools, *name)
				}
			}
			thinking = thinking || b.OfThinking != nil || b.OfRedactedThinking != nil
		}
	}
	slices.Sort(tools)

	kinds := []string{"your messages", "replies"}
	for _, t := range tools {
		kinds = append(kinds, t+" calls")
	}
	if thinking {
		kinds = append(kinds, "thinking")
	}

	kept := map[string]bool{}
	out := make([]layer, 0, len(kinds))
	for _, k := range kinds {
		kept[k] = true
		messages := keep(history, kept, names)
		if len(messages) == 0 {
			messages = probe
		}
		out = append(out, layer{k, messages})
	}
	return out
}

// keep is the history with only the blocks whose kind is kept, dropping messages left empty. The
// API merges the same-role neighbours that dropping leaves behind.
func keep(history []anthropic.MessageParam, kept map[string]bool, names map[string]string) []anthropic.MessageParam {
	var out []anthropic.MessageParam
	for _, m := range history {
		var blocks []anthropic.ContentBlockParamUnion
		for _, b := range m.Content {
			if kept[blockKind(m.Role, b, names)] {
				blocks = append(blocks, b)
			}
		}
		if len(blocks) > 0 {
			out = append(out, anthropic.MessageParam{Role: m.Role, Content: blocks})
		}
	}
	return out
}

func blockKind(role anthropic.MessageParamRole, b anthropic.ContentBlockParamUnion, names map[string]string) string {
	switch {
	case b.OfThinking != nil || b.OfRedactedThinking != nil:
		return "thinking"
	case b.GetID() != nil && b.GetName() != nil:
		return *b.GetName() + " calls"
	case b.GetToolUseID() != nil:
		return names[*b.GetToolUseID()] + " calls"
	case role == anthropic.MessageParamRoleAssistant:
		return "replies"
	}
	return "your messages"
}

// step is one cumulative prefix of the full request.
type step struct {
	label    string
	system   string
	tools    []anthropic.ToolUnionParam
	messages []anthropic.MessageParam
}

// tally is one measurement in flight; each goroutine owns exactly one, so no lock is needed.
type tally struct {
	n   int64
	err error
}

func (a *Agent) window() int64 {
	if a.Window > 0 {
		return a.Window
	}
	return llm.ContextWindow()
}

// count measures one slice of a request. Tools and the context edits go in through WithJSONSet:
// the count_tokens params carry their own tool type, and context_management is Beta-only.
func (a *Agent) count(ctx context.Context, messages []anthropic.MessageParam, system string,
	tools []anthropic.ToolUnionParam, managed bool) (int64, error) {
	if len(messages) == 0 {
		return 0, nil
	}
	c := a.Client
	if c == nil {
		shared := llm.Client()
		c = &shared
	}
	model := a.Model
	if model == "" {
		model = llm.Model()
	}

	params := anthropic.MessageCountTokensParams{Model: anthropic.Model(model), Messages: messages}
	if system != "" {
		params.System = anthropic.MessageCountTokensParamsSystemUnion{
			OfTextBlockArray: []anthropic.TextBlockParam{{Text: system}},
		}
	}
	var opts []option.RequestOption
	if len(tools) > 0 {
		opts = append(opts, option.WithJSONSet("tools", tools))
	}
	if managed {
		opts = append(opts,
			option.WithHeaderAdd("anthropic-beta", contextManagementBeta),
			option.WithJSONSet("context_management", a.contextEdits()))
	}

	res, err := c.Messages.CountTokens(ctx, params, opts...)
	if err != nil {
		return 0, err
	}
	return res.InputTokens, nil
}

type toolGroup struct {
	label string
	tools []anthropic.ToolUnionParam
}

// Tools are grouped by where they came from, because that is the unit you can actually turn off:
// an MCP server's whole schema set rides on every request whether or not the session uses it.
func groupTools(tools []anthropic.ToolUnionParam) []toolGroup {
	byLabel := map[string][]anthropic.ToolUnionParam{}
	for _, t := range tools {
		name := ""
		if n := t.GetName(); n != nil {
			name = *n
		}
		byLabel[toolSource(name)] = append(byLabel[toolSource(name)], t)
	}
	labels := make([]string, 0, len(byLabel))
	for label := range byLabel {
		labels = append(labels, label)
	}
	slices.Sort(labels)

	groups := make([]toolGroup, 0, len(labels))
	for _, label := range labels {
		groups = append(groups, toolGroup{label, byLabel[label]})
	}
	return groups
}

func toolSource(name string) string {
	if rest, found := strings.CutPrefix(name, McpPrefix); found {
		if server, _, ok := strings.Cut(rest, "__"); ok {
			return server
		}
		return "mcp"
	}
	for _, prefix := range []string{"vault", "pr", "calendar", "agent"} {
		if strings.HasPrefix(name, prefix+"_") {
			return prefix
		}
	}
	if source, ok := builtinSources[name]; ok {
		return source
	}
	return "built-in"
}

// Built-ins whose names carry no source prefix; anything unlisted stays in the built-in row.
var builtinSources = map[string]string{
	"review_load":     "pr",
	"task":            "agent",
	"land_run":        "agent",
	"run_background":  "shell",
	"bash_output":     "shell",
	"kill_shell":      "shell",
	"background_proc": "shell",
	"poll":            "shell",
	"start_skill":     "skill",
	"finish_skill":    "skill",
	"fail_run":        "skill",
}
