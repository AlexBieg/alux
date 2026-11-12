package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func namedTool(name string) anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        name,
		Description: anthropic.String(strings.Repeat("schema prose ", 20)),
	}}
}

// A breakdown whose rows do not add up to the total is worse than none: it invites the reader to
// go hunting for the missing tokens. The rows are differences between overlapping measurements,
// so this is the property that can quietly break.
func TestBreakdownRowsAddUpToTheTotal(t *testing.T) {
	a, _ := testAgent(t)
	a.System = strings.Repeat("standing instructions. ", 50)
	a.Tools = func() []anthropic.ToolUnionParam {
		return []anthropic.ToolUnionParam{namedTool("grep"), namedTool("mcp__notes__search")}
	}

	report, err := a.Breakdown(context.Background(), user("hello"))
	if err != nil {
		t.Fatal(err)
	}

	var sum int64
	for _, row := range report.Rows {
		sum += row.Tokens
	}
	if sum != report.Total {
		t.Errorf("rows sum to %d but the total is %d:\n%s", sum, report.Total, report)
	}
}

// Grouping by server is the point of the report: an MCP server's whole schema set rides on every
// request, and turning that server off is the lever the numbers are meant to justify.
func TestBreakdownSeparatesEachMcpServerFromTheBuiltins(t *testing.T) {
	a, _ := testAgent(t)
	a.Tools = func() []anthropic.ToolUnionParam {
		return []anthropic.ToolUnionParam{
			namedTool("grep"),
			namedTool("vault_read"),
			namedTool("mcp__notes__search"),
			namedTool("mcp__notes__read"),
			namedTool("mcp__linear__issue"),
		}
	}

	report, err := a.Breakdown(context.Background(), user("hello"))
	if err != nil {
		t.Fatal(err)
	}

	var labels []string
	for _, row := range report.Rows {
		labels = append(labels, row.Label)
	}
	for _, want := range []string{"built-in tools", "vault tools", "notes tools", "linear tools"} {
		if !contains(labels, want) {
			t.Errorf("no %q row in %v", want, labels)
		}
	}
}

// The conversation is split into what wrote it, and every partial history sent along the way has
// to be one the API accepts: a tool result whose call was stripped is a 400 the mock cannot raise.
func TestTheConversationIsSplitByKindWithEveryResultBesideItsCall(t *testing.T) {
	a, _ := testAgent(t)
	history := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("find the bug")),
		anthropic.NewAssistantMessage(
			anthropic.NewThinkingBlock("sig", strings.Repeat("pondering ", 30)),
			anthropic.NewTextBlock("Looking."),
			anthropic.NewToolUseBlock("t1", map[string]any{"pattern": "bug"}, "grep"),
		),
		anthropic.NewUserMessage(anthropic.NewToolResultBlock("t1", strings.Repeat("match ", 40), false)),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("Found it.")),
	}

	for _, l := range layers(history) {
		calls := map[string]bool{}
		for _, m := range l.messages {
			for _, b := range m.Content {
				if b.OfToolUse != nil {
					calls[b.OfToolUse.ID] = true
				}
				if b.OfToolResult != nil && !calls[b.OfToolResult.ToolUseID] {
					t.Errorf("the %q layer sends a result for %s without its call", l.label, b.OfToolResult.ToolUseID)
				}
			}
		}
	}

	report, err := a.Breakdown(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	rows := map[string]int64{}
	for _, row := range report.Rows {
		sum += row.Tokens
		rows[row.Label] = row.Tokens
	}
	for _, want := range []string{"your messages", "replies", "grep calls", "thinking"} {
		if rows[want] <= 0 {
			t.Errorf("the %q row measured %d:\n%s", want, rows[want], report)
		}
	}
	if sum != report.Total {
		t.Errorf("rows sum to %d but the total is %d:\n%s", sum, report.Total, report)
	}
}

// The built-in row is split by the feature that adds each tool, since that is what could be
// loaded only when needed; an unprefixed name falls back to built-in silently, so pin them.
func TestBuiltinToolsAreGroupedByTheFeatureThatAddsThem(t *testing.T) {
	for name, want := range map[string]string{
		"pr_detail":            "pr",
		"review_load":          "pr",
		"calendar_list_events": "calendar",
		"task":                 "agent",
		"agent_status":         "agent",
		"poll":                 "shell",
		"run_background":       "shell",
		"start_skill":          "skill",
		"bash":                 "built-in",
		"grep":                 "built-in",
	} {
		if got := toolSource(name); got != want {
			t.Errorf("%s is grouped under %q, want %q", name, got, want)
		}
	}
}

// With nothing to trim the managed count is zero, and reporting that as a saving would claim the
// edits reclaim the entire prompt.
func TestAnEmptyConversationDoesNotClaimTheWholePromptIsReclaimed(t *testing.T) {
	r := Report{Rows: []Row{{"system prompt", 40}}, Total: 40, Managed: 0, Window: 1000}
	if strings.Contains(r.String(), "reclaimed") {
		t.Errorf("an unmeasured trim was reported as a saving:\n%s", r)
	}
	if !strings.Contains(r.String(), "Nothing trimmed yet") {
		t.Errorf("the report did not say why there is no saving:\n%s", r)
	}
}

// The report is read as columns, so the counts have to land in one column whatever the labels
// around them measure. Unaligned numbers is what the first cut of this report got wrong.
func TestReportLinesUpItsCountsWhateverTheLabelsMeasure(t *testing.T) {
	r := Report{
		Rows: []Row{
			{"system prompt", 12_982},
			{"a considerably longer label", 2_888},
			{"x", 7_001},
		},
		Total:  22_871,
		Window: 200_000,
	}

	counts := []string{"12,982", "2,888", "7,001", "22,871"}
	var columns []int
	for _, line := range strings.Split(r.String(), "\n") {
		for _, count := range counts {
			if i := strings.Index(line, count); i >= 0 {
				columns = append(columns, i+len(count))
			}
		}
	}
	if len(columns) != len(counts) {
		t.Fatalf("found %d of the %d counts:\n%s", len(columns), len(counts), r)
	}
	for _, c := range columns {
		if c != columns[0] {
			t.Errorf("counts end in columns %v rather than one:\n%s", columns, r)
		}
	}
}

// A row worth a fraction of a cell still has to draw something. A blank bar reads as "this part
// is free", and the small rows are exactly the ones a reader is deciding to ignore. bubbles'
// progress fill is whole cells, so the sub-cell rows survive by being dimmed rather than
// shortened: every row draws at least one block, and a smaller share draws a fainter one.
func TestASubCellRowDrawsAFaintBlockRatherThanNothing(t *testing.T) {
	for _, share := range []float64{0.4, 0.02, 0.004, 0.000_03} {
		if !strings.Contains(bar(share), "█") {
			t.Errorf("a %.4f share drew no block at all: %q", share, bar(share))
		}
	}

	// Dimmer with every step down, which is the whole mechanism: were the trailing cell painted
	// at full brightness, a 0.003% row would look like a 4% one.
	var last float64
	for i, share := range []float64{0.041, 0.024, 0.012, 0.004} {
		lum := luminance(t, bar(share))
		if i > 0 && lum >= last {
			t.Errorf("a %.3f share is not dimmer than the share above it: %.1f then %.1f",
				share, last, lum)
		}
		last = lum
	}
}

// luminance weighs the first colour a bar sets. A sub-cell bar is one cell, so that first colour
// is the dimmed trailing cell, and its brightness is what the reader is comparing between rows.
func luminance(t *testing.T, bar string) float64 {
	t.Helper()
	var r, g, b int
	if _, err := fmt.Sscanf(bar, "\x1b[38;2;%d;%d;%dm", &r, &g, &b); err != nil {
		t.Fatalf("no truecolor escape at the head of %q: %v", bar, err)
	}
	return 0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
