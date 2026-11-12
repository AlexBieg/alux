package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const flowchart = "```mermaid\ngraph TD\n    A[Start] --> B{Is it?}\n    B -->|Yes| C[OK]\n    B -->|No| D[End]\n```\n"

const chain = "```mermaid\ngraph LR\n    Request --> Router --> Authenticate --> LoadSession --> RunAgent --> RenderReply --> Done\n```\n"

func TestAMermaidFenceIsDrawn(t *testing.T) {
	out := ansi.Strip(Render("Here:\n\n"+flowchart, 60))
	if strings.Contains(out, "graph TD") || strings.Contains(out, "-->") {
		t.Errorf("source shown instead of the drawing:\n%s", out)
	}
	for _, want := range []string{"┌", "▼", "Start", "Is it?", "Yes", "End"} {
		if !strings.Contains(out, want) {
			t.Errorf("drawing is missing %q:\n%s", want, out)
		}
	}
}

// Render runs again on resize, so a chain that runs across in a wide pane runs down in a narrow one.
func TestALeftToRightChainTooWideForThePaneRunsDown(t *testing.T) {
	wide := ansi.Strip(Render(chain, 160))
	if strings.Contains(wide, "-->") || !strings.Contains(wide, "►") {
		t.Errorf("wide pane did not draw the chain across:\n%s", wide)
	}

	narrow := Render(chain, 60)
	if got := widest(narrow); got > 60 {
		t.Errorf("rendered %d columns into a 60-column pane:\n%s", got, ansi.Strip(narrow))
	}
	if out := ansi.Strip(narrow); strings.Contains(out, "-->") || !strings.Contains(out, "▼") {
		t.Errorf("narrow pane did not draw the chain down:\n%s", out)
	}
}

func TestADiagramThatCannotFitThePaneKeepsItsSource(t *testing.T) {
	src := "```mermaid\nflowchart LR\n    A[Supercalifragilisticexpialidocious] --> B[Antidisestablishmentarianism]\n```\n"
	out := Render(src, MinWidth)
	if got := widest(out); got > MinWidth {
		t.Errorf("rendered %d columns into a %d-column pane:\n%s", got, MinWidth, ansi.Strip(out))
	}
	if !strings.Contains(ansi.Strip(out), "flowchart LR") {
		t.Errorf("an undrawable diagram lost its source:\n%s", ansi.Strip(out))
	}
}

func TestDiagramTypesBeyondFlowchartsAreDrawn(t *testing.T) {
	for name, c := range map[string]struct{ src, want string }{
		"state": {"stateDiagram-v2\n    [*] --> idle\n    idle --> working: prompt\n    working --> idle: done", "working"},
		"class": {"classDiagram\n    class Session { +ID string }\n    Session --> Agent", "Session"},
		"pie":   {"pie title Pets\n    \"Dogs\" : 386\n    \"Cats\" : 85", "Dogs"},
		"gantt": {"gantt\n    dateFormat YYYY-MM-DD\n    section PR\n    review :2026-10-20, 2d", "review"},
	} {
		out := ansi.Strip(Render("```mermaid\n"+c.src+"\n```\n", 80))
		if first := strings.SplitN(c.src, "\n", 2)[0]; strings.Contains(out, first) {
			t.Errorf("%s: source shown instead of the drawing:\n%s", name, out)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: drawing is missing %q:\n%s", name, c.want, out)
		}
	}
}

func TestAnUnparseableDiagramKeepsItsSource(t *testing.T) {
	out := ansi.Strip(Render("```mermaid\nflowchart TD\n    A -->\n```\n", 60))
	if !strings.Contains(out, "flowchart TD") {
		t.Errorf("a broken diagram lost its source:\n%s", out)
	}
}

// A fence still streaming has no close yet, and is drawn only once it does.
func TestAnUnclosedFenceIsLeftAlone(t *testing.T) {
	partial := strings.TrimSuffix(flowchart, "```\n")
	if out := ansi.Strip(Render(partial, 60)); !strings.Contains(out, "graph TD") {
		t.Errorf("unclosed fence was drawn:\n%s", out)
	}
}

// An example of how to write a mermaid fence has to stay source.
func TestAMermaidFenceQuotedInsideALongerFenceIsNotDrawn(t *testing.T) {
	out := ansi.Strip(Render("````markdown\n"+flowchart+"````\n", 60))
	if !strings.Contains(out, "graph TD") {
		t.Errorf("quoted example was drawn:\n%s", out)
	}
}
