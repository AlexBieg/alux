package graphview

import (
	"strings"
	"testing"
)

func threeTickets() Diagram {
	titles := []string{
		"ticket 1", "ticket 2", "ticket 3",
		"review PR1", "review PR2", "review PR3",
		"PR1 merged", "PR2 merged", "PR3 merged",
	}
	var d Diagram
	for _, t := range titles {
		d.Nodes = append(d.Nodes, Node{Lines: []string{t}})
	}
	b := func(from, to int) Edge { return Edge{From: from, To: to} }
	s := func(from, to int) Edge { return Edge{From: from, To: to, Dashed: true} }
	d.Edges = []Edge{
		s(0, 1), s(0, 2),
		b(0, 3), b(1, 4), b(2, 5),
		b(3, 4), b(3, 5), b(3, 6),
		b(6, 7), b(6, 8), b(4, 7), b(5, 8),
	}
	return d
}

func nodes(labels ...string) []Node {
	var ns []Node
	for _, l := range labels {
		ns = append(ns, Node{Lines: []string{l}})
	}
	return ns
}

func TestRoutesJoinAndStacksOnEdgesAreDashed(t *testing.T) {
	p := Placed{
		Diagram: Diagram{Nodes: nodes("a", "b", "c"), Edges: []Edge{{From: 0, To: 1}, {From: 0, To: 2, Dashed: true}}},
		Boxes:   []Rect{{X: 4, Y: 0, W: 5, H: 3}, {X: 0, Y: 7, W: 5, H: 3}, {X: 8, Y: 7, W: 5, H: 3}},
		Routes: []Route{
			{Edge: 0, Points: [][2]int{{6, 2}, {6, 4}, {2, 4}, {2, 7}}},
			{Edge: 1, Points: [][2]int{{6, 2}, {6, 4}, {10, 4}, {10, 7}}},
		},
		W: 13, H: 10,
	}
	want := strings.Join([]string{
		"    ┌───┐",
		"    │ a │",
		"    └───┘",
		"      │",
		"  ┌───┴╌╌╌┐",
		"  │       ╎",
		"  ▾       ▾",
		"┌───┐   ┌───┐",
		"│ b │   │ c │",
		"└───┘   └───┘",
	}, "\n") + "\n"
	if got := Draw(p).String(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestAnArrowTouchesItsTargetWhereverTheRouteStopped(t *testing.T) {
	d := Diagram{Nodes: nodes("a", "b"), Edges: []Edge{{From: 0, To: 1}}}
	want := strings.Join([]string{
		"┌───┐",
		"│ a │",
		"└───┘",
		"  │",
		"  │",
		"  ▾",
		"┌───┐",
		"│ b │",
		"└───┘",
	}, "\n") + "\n"
	for name, tip := range map[string][2]int{"short": {2, 4}, "inside": {2, 7}} {
		p := Placed{
			Diagram: d,
			Boxes:   []Rect{{X: 0, Y: 0, W: 5, H: 3}, {X: 0, Y: 6, W: 5, H: 3}},
			Routes:  []Route{{Edge: 0, Points: [][2]int{{2, 2}, tip}}},
			W:       5, H: 9,
		}
		if got := Draw(p).String(); got != want {
			t.Errorf("%s: got\n%s\nwant\n%s", name, got, want)
		}
	}
}

func TestARouteThatOvershootsItsRowDrawsNoSpur(t *testing.T) {
	p := Placed{
		Diagram: Diagram{Nodes: nodes("a", "b"), Edges: []Edge{{From: 0, To: 1}}},
		Boxes:   []Rect{{X: 6, Y: 0, W: 5, H: 3}, {X: 0, Y: 5, W: 5, H: 3}},
		Routes:  []Route{{Edge: 0, Points: [][2]int{{8, 2}, {8, 7}, {5, 6}}}},
		W:       11, H: 8,
	}
	want := strings.Join([]string{
		"      ┌───┐",
		"      │ a │",
		"      └───┘",
		"        │",
		"        │",
		"┌───┐   │",
		"│ b │◂──┘",
		"└───┘",
	}, "\n") + "\n"
	if got := Draw(p).String(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
