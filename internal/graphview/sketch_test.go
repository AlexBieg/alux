package graphview

import (
	"strings"
	"testing"
)

func boxText(pic Picture, r Rect) string {
	var b strings.Builder
	for y := r.Y + 1; y < r.Y+r.H-1; y++ {
		for _, c := range pic.Cells[y][r.X+1 : r.X+r.W-1] {
			b.WriteString(c.S)
		}
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func TestSketchGivesEveryNodeItsOwnBox(t *testing.T) {
	d := threeTickets()
	pic, ok := SketchPicture(d)
	if !ok {
		t.Fatal("sketch could not pair its boxes with the nodes")
	}
	for i, r := range pic.Boxes {
		if got := boxText(pic, r); got != d.Nodes[i].Lines[0] {
			t.Errorf("box %d shows %q, want %q", i, got, d.Nodes[i].Lines[0])
		}
		if got, ok := pic.At(r.X+r.W/2, r.Y+r.H/2); !ok || got != i {
			t.Errorf("the hit map gives %d inside box %d", got, i)
		}
	}
	if !(pic.Boxes[0].Y < pic.Boxes[3].Y && pic.Boxes[3].Y < pic.Boxes[6].Y) {
		t.Errorf("w1, g1, c1 should run top to bottom, got rows %d, %d, %d", pic.Boxes[0].Y, pic.Boxes[3].Y, pic.Boxes[6].Y)
	}
}

// Listed bottom first, so pairing in node order would swap the two review boxes.
func TestSketchPairsNodesSharingALabelByPosition(t *testing.T) {
	d := Diagram{Nodes: nodes("review", "gate", "review"), Edges: []Edge{{From: 2, To: 1}, {From: 1, To: 0}}}
	pic, ok := SketchPicture(d)
	if !ok {
		t.Fatal("sketch could not pair its boxes with the nodes")
	}
	if !(pic.Boxes[2].Y < pic.Boxes[1].Y && pic.Boxes[1].Y < pic.Boxes[0].Y) {
		t.Errorf("first, mid, last should run top to bottom, got rows %d, %d, %d", pic.Boxes[2].Y, pic.Boxes[1].Y, pic.Boxes[0].Y)
	}
}
