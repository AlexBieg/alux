package graphview

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestConcurrentLayoutsAreSafe(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := LayoutDiagram(context.Background(), threeTickets()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestAWideLabelIsSizedAndDrawnByDisplayWidth(t *testing.T) {
	d := Diagram{Nodes: []Node{{Lines: []string{"日本"}}, {Lines: []string{"ok"}}}, Edges: []Edge{{From: 0, To: 1}}}
	p, err := LayoutDiagram(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Boxes[0].W != 8 {
		t.Errorf("box for a 4-column label is %d wide, want 8", p.Boxes[0].W)
	}
	for _, row := range strings.Split(Draw(p).String(), "\n") {
		if strings.Contains(row, "日本") && !strings.Contains(row, "│ 日本 │") {
			t.Errorf("wide label row is misaligned: %q", row)
		}
	}
}

func TestAHeaderIsRuledOffFromTheLinesBelowIt(t *testing.T) {
	p := Placed{
		Diagram: Diagram{Nodes: []Node{{Lines: []string{"USER", "id", "name"}, Header: true}}},
		Boxes:   []Rect{{X: 0, Y: 0, W: 8, H: 6}},
		W:       8, H: 6,
	}
	want := strings.Join([]string{
		"┌──────┐",
		"│ USER │",
		"├──────┤",
		"│ id   │",
		"│ name │",
		"└──────┘",
	}, "\n") + "\n"
	if got := Draw(p).String(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestEdgeLabelsLandClearOfBoxesLinesAndEachOther(t *testing.T) {
	d := Diagram{
		Nodes: []Node{{Lines: []string{"CUSTOMER"}}, {Lines: []string{"ORDER"}}, {Lines: []string{"ITEM"}}},
		Edges: []Edge{
			{From: 0, To: 1, Label: "places", Tail: "1", Head: "many", NoArrow: true},
			{From: 1, To: 2, Label: "contains", Tail: "1", Head: "many", NoArrow: true},
			{From: 0, To: 2, Label: "reviews", Dashed: true},
		},
	}
	p, err := LayoutDiagram(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Labels) != 7 {
		t.Fatalf("placed %d labels, want 7", len(p.Labels))
	}
	tr := trace(p)
	cells := map[[2]int]int{}
	for i, l := range p.Labels {
		for x := l.X - 1; x <= l.X+ansi.StringWidth(l.Text); x++ {
			inside := x >= l.X && x < l.X+ansi.StringWidth(l.Text)
			for bi, b := range p.Boxes {
				if b.contains(x, l.Y) {
					t.Errorf("label %q touches box %d", l.Text, bi)
				}
			}
			if inside && tr.inked(x, l.Y) {
				t.Errorf("label %q is drawn over a line at %d,%d", l.Text, x, l.Y)
			}
			if j, ok := cells[[2]int{x, l.Y}]; ok {
				t.Errorf("labels %q and %q run together", p.Labels[j].Text, l.Text)
			}
		}
		for x := l.X; x < l.X+ansi.StringWidth(l.Text); x++ {
			cells[[2]int{x, l.Y}] = i
		}
	}
	pic := Draw(p).String()
	for _, l := range p.Labels {
		if !strings.Contains(pic, l.Text) {
			t.Errorf("label %q is missing from\n%s", l.Text, pic)
		}
	}
	if n := strings.Count(pic, "▾") + strings.Count(pic, "▴") + strings.Count(pic, "▸") + strings.Count(pic, "◂"); n != 1 {
		t.Errorf("only one edge has an arrowhead, but %d were drawn:\n%s", n, pic)
	}
}

// Two edges between the same pair of boxes, as in a pause/resume cycle: one down at column 3, one up at column 6.
func cycle(labels ...Label) Placed {
	return Placed{
		Diagram: Diagram{Nodes: []Node{{}, {}}, Edges: []Edge{{From: 0, To: 1}, {From: 1, To: 0}}},
		Boxes:   []Rect{{X: 0, Y: 0, W: 10, H: 3}, {X: 0, Y: 8, W: 10, H: 3}},
		Routes: []Route{
			{Edge: 0, Points: [][2]int{{3, 2}, {3, 8}}},
			{Edge: 1, Points: [][2]int{{6, 8}, {6, 2}}},
		},
		Labels: labels,
		W:      20, H: 11,
	}
}

func TestTwoEdgesOnOneLineAreUncleanUnlessTheyShareAnEnd(t *testing.T) {
	merged := cycle()
	merged.Routes[1].Points = [][2]int{{3, 8}, {3, 2}}
	if merged.Clean() {
		t.Error("a→b and b→a drawn on the same line passed as clean")
	}
	if !cycle().Clean() {
		t.Error("a→b and b→a on separate lines failed as unclean")
	}
	fan := Placed{
		Diagram: Diagram{Nodes: []Node{{}, {}, {}}, Edges: []Edge{{From: 0, To: 1}, {From: 0, To: 2}}},
		Boxes:   []Rect{{X: 4, Y: 0, W: 5, H: 3}, {X: 0, Y: 7, W: 5, H: 3}, {X: 8, Y: 7, W: 5, H: 3}},
		Routes: []Route{
			{Edge: 0, Points: [][2]int{{6, 2}, {6, 4}, {2, 4}, {2, 7}}},
			{Edge: 1, Points: [][2]int{{6, 2}, {6, 4}, {10, 4}, {10, 7}}},
		},
		W: 13, H: 10,
	}
	if !fan.Clean() {
		t.Error("a fan-out sharing its trunk from one box failed as unclean")
	}
}

func TestLRWidensTheRankGapToTheLongestLabel(t *testing.T) {
	d := Diagram{Flow: LR, Nodes: []Node{{Lines: []string{"a"}}, {Lines: []string{"b"}}}, Edges: []Edge{{From: 0, To: 1, Label: "closing report"}}}
	p, err := LayoutDiagram(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if gap := p.Boxes[1].X - (p.Boxes[0].X + p.Boxes[0].W); gap < len("closing report")+2 {
		t.Errorf("rank gap is %d columns, too narrow for a 14-column label:\n%s", gap, Draw(p))
	}
}

func TestALabelIsPlacedBesideItsOwnEdge(t *testing.T) {
	p := cycle(Label{Edge: 0, Text: "pause", X: 0, Y: 5}, Label{Edge: 1, Text: "resume", X: 0, Y: 5})
	if err := p.place(); err != nil {
		t.Fatal(err)
	}
	pause, resume := p.Labels[0], p.Labels[1]
	if pause.X+len(pause.Text) != 3 {
		t.Errorf("pause sits at %+v, want just left of its line at column 3", pause)
	}
	if resume.X != 7 {
		t.Errorf("resume sits at %+v, want just right of its line at column 6, away from pause's", resume)
	}
}

func TestLabelsOnOneSpotAreNudgedApartOrRefused(t *testing.T) {
	p := cycle(Label{Edge: 0, Text: "one", X: 0, Y: 5}, Label{Edge: 0, Text: "two", X: 0, Y: 5})
	if err := p.place(); err != nil {
		t.Fatal(err)
	}
	a, b := p.Labels[0], p.Labels[1]
	if a.Y == b.Y && b.X < a.X+len(a.Text)+1 && a.X < b.X+len(b.Text)+1 {
		t.Errorf("labels share a row with no gap: %+v %+v", a, b)
	}

	walled := cycle(Label{Edge: 0, Text: "lost", X: 0, Y: 5})
	walled.Boxes = append(walled.Boxes, Rect{X: -20, Y: 3, W: 60, H: 5})
	if err := walled.place(); err == nil {
		t.Error("a label with nowhere to go was placed anyway")
	}
}

func TestLRBoxesGrowSoEdgesLeaveTheirSideNotTheirCorners(t *testing.T) {
	d := Diagram{Flow: LR, Nodes: []Node{{Lines: []string{"hub"}}, {Lines: []string{"a"}}, {Lines: []string{"b"}}, {Lines: []string{"c"}}}}
	for i := 1; i <= 3; i++ {
		d.Edges = append(d.Edges, Edge{From: 0, To: i})
	}
	p, err := LayoutDiagram(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	hub := p.Boxes[0]
	if hub.H != 7 {
		t.Errorf("a box with three edges out is %d rows, want 7", hub.H)
	}
	for _, b := range p.Boxes[1:] {
		if b.X <= hub.X+hub.W {
			t.Errorf("in LR every child should sit right of the hub: hub %+v child %+v", hub, b)
		}
	}
	pic := Draw(p)
	for _, y := range []int{hub.Y, hub.Y + hub.H - 1} {
		if x := hub.X + hub.W; x < len(pic.Cells[y]) && pic.Cells[y][x].S != " " {
			t.Errorf("an edge leaves the hub at its corner on row %d:\n%s", y, pic)
		}
	}
	if n := strings.Count(pic.String(), "▸") + strings.Count(pic.String(), "▴") + strings.Count(pic.String(), "▾"); n != 3 {
		t.Errorf("drew %d arrowheads, want one per child:\n%s", n, pic)
	}
}
