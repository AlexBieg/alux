package graphview

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/goccy/go-graphviz"
	"github.com/goccy/go-graphviz/cgraph"
)

// A box is its text with a space and a border on every side.
const boxPad = 4

// A column is 0.1in and a row 0.2in, the rough 1:2 aspect of a terminal cell.
const (
	ptPerCol = 7.2
	ptPerRow = 14.4
)

// go-graphviz runs every instance in one shared WASM module, which fails fatally under concurrent calls.
var gvMu sync.Mutex

type Rect struct{ X, Y, W, H int }

func (r Rect) contains(x, y int) bool { return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H }

func (r Rect) center() (int, int) { return r.X + r.W/2, r.Y + r.H/2 }

// Route is an edge as a list of cells, from its source's border to its target's.
type Route struct {
	Edge   int
	Points [][2]int
}

type Label struct {
	Edge int
	Text string
	X, Y int
}

// Placed is a layout in cells, with every box, route and label shifted so the drawing starts at 0,0.
type Placed struct {
	Diagram Diagram
	Boxes   []Rect
	Routes  []Route
	Labels  []Label
	W, H    int
}

// LayoutDiagram places d with Graphviz dot and right-angled edges, and fails when a label has no room.
func LayoutDiagram(ctx context.Context, d Diagram) (Placed, error) {
	return layout(ctx, d, false)
}

// layout with routed set widens busy TD boxes and routes the edges itself, which only TD supports.
func layout(ctx context.Context, d Diagram, routed bool) (Placed, error) {
	for _, e := range d.Edges {
		if e.From < 0 || e.From >= len(d.Nodes) || e.To < 0 || e.To >= len(d.Nodes) {
			return Placed{}, fmt.Errorf("edge %d -> %d names a node that does not exist", e.From, e.To)
		}
	}
	if routed && d.Flow != TD {
		return Placed{}, fmt.Errorf("only a TD diagram can be routed")
	}
	widths, heights := sizes(d)
	if routed {
		widen(d, widths)
	}

	gvMu.Lock()
	defer gvMu.Unlock()
	gv, err := graphviz.New(ctx)
	if err != nil {
		return Placed{}, err
	}
	defer gv.Close()
	in, err := gv.Graph()
	if err != nil {
		return Placed{}, err
	}
	defer in.Close()
	if routed {
		in.SafeSet("splines", "polyline", "")
	} else {
		in.SafeSet("splines", "ortho", "")
	}
	ranksep, nodesep := gaps(d)
	in.SafeSet("ranksep", inches(ranksep), "")
	in.SafeSet("nodesep", inches(nodesep), "")
	in.SafeSet("forcelabels", "true", "")
	if d.Flow == LR {
		in.SafeSet("rankdir", "LR", "")
	}

	nodes := make([]*cgraph.Node, len(d.Nodes))
	for i := range d.Nodes {
		gn, err := in.CreateNodeByName(name(i))
		if err != nil {
			return Placed{}, err
		}
		gn.SafeSet("shape", "box", "")
		gn.SafeSet("fixedsize", "true", "")
		gn.SafeSet("label", "", "")
		gn.SafeSet("width", inches(float64(widths[i])*0.1), "")
		gn.SafeSet("height", inches(float64(heights[i])*0.2), "")
		nodes[i] = gn
	}
	for i, e := range d.Edges {
		ge, err := in.CreateEdgeByName(name(i), nodes[e.From], nodes[e.To])
		if err != nil {
			return Placed{}, err
		}
		ge.SafeSet("id", name(i), "")
		// Courier 12 makes Graphviz size each glyph as exactly one cell; it sizes a stand-in of the same width, never the text.
		ge.SafeSet("fontname", "Courier", "")
		ge.SafeSet("fontsize", "12", "")
		for attr, text := range map[string]string{"xlabel": e.Label, "taillabel": e.Tail, "headlabel": e.Head} {
			if text != "" {
				ge.SafeSet(attr, strings.Repeat("x", ansi.StringWidth(text)), "")
			}
		}
	}

	var dot strings.Builder
	if err := gv.Render(ctx, in, graphviz.XDOT, &dot); err != nil {
		return Placed{}, err
	}
	out, err := graphviz.ParseBytes([]byte(dot.String()))
	if err != nil {
		return Placed{}, err
	}
	defer out.Close()
	p, err := read(out, d, widths, heights)
	if err != nil {
		return Placed{}, err
	}
	p.normalize()
	if routed {
		p.channel()
		p.normalize()
	}
	if err := p.place(); err != nil {
		return Placed{}, err
	}
	p.normalize()
	return p, nil
}

// widen gives a box with two or more labelled edges leaving it room for a label beside each one.
func widen(d Diagram, widths []int) {
	count, widest := make([]int, len(d.Nodes)), make([]int, len(d.Nodes))
	for _, e := range d.Edges {
		count[e.From]++
		widest[e.From] = max(widest[e.From], ansi.StringWidth(e.Label))
	}
	for i := range widths {
		if count[i] >= 2 && widest[i] > 0 {
			widths[i] = max(widths[i], count[i]*(widest[i]+1)+2)
		}
	}
}

// sizes fits each box to its widest line and its rows; in LR a box also grows a row pair per edge on its busier side.
func sizes(d Diagram) (widths, heights []int) {
	in, out := make([]int, len(d.Nodes)), make([]int, len(d.Nodes))
	for _, e := range d.Edges {
		out[e.From]++
		in[e.To]++
	}
	widths, heights = make([]int, len(d.Nodes)), make([]int, len(d.Nodes))
	for i, n := range d.Nodes {
		for _, l := range n.Lines {
			widths[i] = max(widths[i], ansi.StringWidth(l))
		}
		widths[i] += boxPad
		heights[i] = textRows(n) + 2
		if d.Flow == LR {
			heights[i] = max(heights[i], 3+max(0, 2*max(in[i], out[i])-2))
		}
	}
	return widths, heights
}

// Graphviz leaves no room for an xlabel, so in LR, where labels run along the gap between ranks, it is widened to fit.
func gaps(d Diagram) (ranksep, nodesep float64) {
	ranksep, nodesep = 0.6, 0.4
	if d.Flow != LR {
		return ranksep, nodesep
	}
	widest := 0
	for _, e := range d.Edges {
		widest = max(widest, ansi.StringWidth(e.Label), ansi.StringWidth(e.Tail), ansi.StringWidth(e.Head))
	}
	if widest == 0 {
		return ranksep, nodesep
	}
	return max(ranksep, float64(widest+4)*0.1), nodesep
}

func textRows(n Node) int {
	rows := max(1, len(n.Lines))
	if n.Header && len(n.Lines) > 1 {
		rows++
	}
	return rows
}

func inches(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func name(i int) string { return "n" + strconv.Itoa(i) }

func read(out *cgraph.Graph, d Diagram, widths, heights []int) (Placed, error) {
	_, _, _, top, err := floats4(out.GetStr("bb"))
	if err != nil {
		return Placed{}, fmt.Errorf("graphviz returned no bounding box: %w", err)
	}
	toCell := func(x, y float64) [2]int {
		return [2]int{int(math.Round(x / ptPerCol)), int(math.Round((top - y) / ptPerRow))}
	}

	p := Placed{Diagram: d, Boxes: make([]Rect, len(d.Nodes))}
	seen := make([]bool, len(d.Nodes))
	for n, err := out.FirstNode(); n != nil && err == nil; n, err = out.NextNode(n) {
		nm, _ := n.Name()
		i, ok := indexOf(nm, len(d.Nodes))
		if !ok {
			continue
		}
		x, y, err := floats2(n.GetStr("pos"))
		if err != nil {
			return Placed{}, fmt.Errorf("node %d has no position: %w", i, err)
		}
		c := toCell(x, y)
		p.Boxes[i] = Rect{X: c[0] - widths[i]/2, Y: c[1] - heights[i]/2, W: widths[i], H: heights[i]}
		seen[i] = true

		for e, err := out.FirstOut(n); e != nil && err == nil; e, err = out.NextOut(e) {
			j, ok := indexOf(e.GetStr("id"), len(d.Edges))
			if !ok {
				continue
			}
			p.Routes = append(p.Routes, Route{Edge: j, Points: points(e.GetStr("pos"), toCell)})
			de := d.Edges[j]
			for _, l := range []struct{ attr, text string }{{"xlp", de.Label}, {"tail_lp", de.Tail}, {"head_lp", de.Head}} {
				if l.text == "" {
					continue
				}
				x, y, err := floats2(e.GetStr(l.attr))
				if err != nil {
					return Placed{}, fmt.Errorf("graphviz did not place the label %q", l.text)
				}
				c := toCell(x, y)
				p.Labels = append(p.Labels, Label{Edge: j, Text: l.text, X: c[0] - ansi.StringWidth(l.text)/2, Y: c[1]})
			}
		}
	}
	for i, ok := range seen {
		if !ok {
			return Placed{}, fmt.Errorf("graphviz did not place node %d", i)
		}
	}
	return p, nil
}

// points reads an edge's pos: control points, then an "e,x,y" arrow tip that belongs last.
func points(pos string, toCell func(x, y float64) [2]int) [][2]int {
	var pts [][2]int
	var tip *[2]int
	for _, tok := range strings.Fields(pos) {
		if strings.HasPrefix(tok, "s,") {
			continue
		}
		end := strings.HasPrefix(tok, "e,")
		x, y, err := floats2(strings.TrimPrefix(tok, "e,"))
		if err != nil {
			continue
		}
		c := toCell(x, y)
		if end {
			tip = &c
			continue
		}
		pts = append(pts, c)
	}
	if tip != nil {
		pts = append(pts, *tip)
	}
	return pts
}

func (p *Placed) normalize() {
	minX, minY, maxX, maxY := math.MaxInt, math.MaxInt, math.MinInt, math.MinInt
	grow := func(x, y int) {
		minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x), max(maxY, y)
	}
	for _, b := range p.Boxes {
		grow(b.X, b.Y)
		grow(b.X+b.W-1, b.Y+b.H-1)
	}
	for _, r := range p.Routes {
		for _, pt := range r.Points {
			grow(pt[0], pt[1])
		}
	}
	for _, l := range p.Labels {
		grow(l.X, l.Y)
		grow(l.X+ansi.StringWidth(l.Text)-1, l.Y)
	}
	if len(p.Boxes) == 0 {
		return
	}
	for i := range p.Boxes {
		p.Boxes[i].X -= minX
		p.Boxes[i].Y -= minY
	}
	for _, r := range p.Routes {
		for i := range r.Points {
			r.Points[i][0] -= minX
			r.Points[i][1] -= minY
		}
	}
	for i := range p.Labels {
		p.Labels[i].X -= minX
		p.Labels[i].Y -= minY
	}
	p.W, p.H = maxX-minX+1, maxY-minY+1
}

func indexOf(name string, n int) (int, bool) {
	i, err := strconv.Atoi(strings.TrimPrefix(name, "n"))
	if err != nil || !strings.HasPrefix(name, "n") || i < 0 || i >= n {
		return 0, false
	}
	return i, true
}

func floats2(s string) (float64, float64, error) {
	var a, b float64
	_, err := fmt.Sscanf(strings.ReplaceAll(s, ",", " "), "%g %g", &a, &b)
	return a, b, err
}

func floats4(s string) (float64, float64, float64, float64, error) {
	var a, b, c, d float64
	_, err := fmt.Sscanf(strings.ReplaceAll(s, ",", " "), "%g %g %g %g", &a, &b, &c, &d)
	return a, b, c, d, err
}
