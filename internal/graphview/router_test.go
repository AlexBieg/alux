package graphview

import (
	"context"
	"strings"
	"testing"
)

type e struct {
	from, to, label string
	dashed          bool
}

// build is a diagram of edges between nodes named by their labels, a \n starting a second line.
func build(flow Flow, edges ...e) Diagram {
	d := Diagram{Flow: flow}
	index := map[string]int{}
	node := func(name string) int {
		if i, ok := index[name]; ok {
			return i
		}
		index[name] = len(d.Nodes)
		d.Nodes = append(d.Nodes, Node{Lines: strings.Split(name, "\n")})
		return index[name]
	}
	for _, x := range edges {
		from, to := node(x.from), node(x.to)
		d.Edges = append(d.Edges, Edge{From: from, To: to, Label: x.label, Dashed: x.dashed})
	}
	return d
}

func fit(d Diagram, width int) (string, bool) {
	pic, ok := FitPicture(context.Background(), d, width)
	return strings.TrimRight(pic.String(), "\n"), ok
}

func fanOut(flow Flow) Diagram {
	return build(flow,
		e{"hub", "alpha", "", false}, e{"hub", "bravo", "", false}, e{"hub", "charlie", "", false},
		e{"hub", "delta", "", false}, e{"hub", "echo", "", false}, e{"hub", "foxtrot", "", false},
		e{"alpha", "hub", "ack", false}, e{"hub", "alpha", "ping", false})
}

// dot ortho merges this machine's two-way pairs in either direction, so only the routed TD drawing is clean.
func TestATwoWayStateMachineDrawsThroughTheRouter(t *testing.T) {
	d := build(LR,
		e{"Dispatched", "Running", "", false},
		e{"Running", "Awaiting\nreply", "ask_main", false}, e{"Running", "Out of\nbudget", "turns used", false},
		e{"Running", "Cancelled", "agent_cancel", false}, e{"Running", "Reported", "work done", false},
		e{"Awaiting\nreply", "Running", "agent_reply", false},
		e{"Out of\nbudget", "Running", "extend: grant", false}, e{"Out of\nbudget", "Reported", "extend: refuse", false},
		e{"Cancelled", "Reported", "closing report", false})
	out, ok := fit(d, 80)
	if !ok {
		t.Fatal("the state machine fell back to source")
	}
	for _, want := range []string{"Dispatched", "Awaiting", "Cancelled", "Reported", "ask_main", "agent_reply", "turns used", "extend: grant", "extend: refuse", "agent_cancel", "closing report", "work done"} {
		if !strings.Contains(out, want) {
			t.Errorf("drawing is missing %q:\n%s", want, out)
		}
	}
	d.Flow = TD
	p, err := layout(context.Background(), d, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Clean() {
		t.Errorf("the routed drawing merges edges or crosses a box:\n%s", Draw(p))
	}
}

func TestADiagramDotDrawsCleanlyIsNotRouted(t *testing.T) {
	d := build(TD,
		e{"user", "build", "", false}, e{"build", "call", "", false}, e{"call", "tool", "", false},
		e{"tool", "run", "yes", false}, e{"tool", "reply", "no", false}, e{"run", "call", "results", true})
	p, err := LayoutDiagram(context.Background(), d)
	if err != nil || !p.Clean() {
		t.Fatalf("dot should draw this cleanly: %v\n%s", err, Draw(p))
	}
	out, ok := fit(d, 80)
	if !ok || out != strings.TrimRight(Draw(p).String(), "\n") {
		t.Errorf("FitPicture changed a drawing dot already draws cleanly:\n%s\nwant\n%s", out, Draw(p))
	}
}

// An edge rising from the column another drops into must take the higher lane, or their uprights merge.
func TestRoutedLanesKeepUprightsInOneColumnApart(t *testing.T) {
	p, err := layout(context.Background(), fanOut(TD), true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Clean() {
		t.Errorf("routed lanes merged two edges:\n%s", Draw(p))
	}
}

// Too wide for TD at 60 columns, this fan-out's only drawing is LR with its two-way pair on one line, which beats source.
func TestAMergedLRDrawingIsTheLastResort(t *testing.T) {
	out, ok := fit(fanOut(LR), 60)
	if !ok {
		t.Fatal("fell back to source")
	}
	if !strings.Contains(out, "foxtrot") || strings.Count(out, "\n") < 20 {
		t.Errorf("expected the tall LR drawing:\n%s", out)
	}
	if _, ok := fit(build(TD, e{"a", "b", "", false}), 3); ok {
		t.Error("drew into 3 columns")
	}
}

func TestFitPictureTurnsAWideFanOutOnItsSide(t *testing.T) {
	d := build(TD, e{"root", "alpha", "", false}, e{"root", "bravo", "", false}, e{"root", "charlie", "", false},
		e{"root", "delta", "", false}, e{"root", "echo", "", false})
	td, err := LayoutDiagram(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	width := td.W - 1
	out, ok := fit(d, width)
	if !ok {
		t.Fatalf("a fan-out %d wide in TD did not fit %d columns either way", td.W, width)
	}
	for _, row := range strings.Split(out, "\n") {
		if w := len([]rune(row)); w > width {
			t.Errorf("row is %d wide, over %d: %q", w, width, row)
		}
	}
	if _, ok := fit(d, 5); ok {
		t.Error("drew into 5 columns")
	}
}
