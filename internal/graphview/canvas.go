package graphview

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// S is one grapheme, or empty for the cell a wide grapheme to its left spills into. Node is the box the cell belongs to, or -1.
type Cell struct {
	S    string
	Node int
}

// Picture is a placed diagram drawn into cells. Boxes doubles as the hit map.
type Picture struct {
	Cells   [][]Cell
	Boxes   []Rect
	Diagram Diagram
}

const (
	up = 1 << iota
	down
	left
	right
)

// Glyphs are drawn by Ghostty's own box-drawing sprites; the arrowheads are one Menlo cell.
var lines = map[int]string{
	up: "│", down: "│", up | down: "│",
	left: "─", right: "─", left | right: "─",
	down | right: "┌", down | left: "┐", up | right: "└", up | left: "┘",
	up | down | right: "├", up | down | left: "┤",
	down | left | right: "┬", up | left | right: "┴",
	up | down | left | right: "┼",
}

var arrows = map[int]string{down: "▾", up: "▴", right: "▸", left: "◂"}

type traced struct {
	mask  [][]int
	solid [][]bool
	tips  [][]string
}

func (t traced) inked(x, y int) bool {
	return y >= 0 && y < len(t.mask) && x >= 0 && x < len(t.mask[y]) && (t.mask[y][x] != 0 || t.tips[y][x] != "")
}

func trace(p Placed) traced {
	t := traced{mask: grid[int](p.W, p.H), solid: grid[bool](p.W, p.H), tips: grid[string](p.W, p.H)}
	mark := func(x, y, dir int, isSolid bool) {
		if x < 0 || y < 0 || x >= p.W || y >= p.H {
			return
		}
		t.mask[y][x] |= dir
		t.solid[y][x] = t.solid[y][x] || isSolid
	}
	for _, r := range p.Routes {
		e := p.Diagram.Edges[r.Edge]
		pts := p.path(r)
		last := 0
		for i := 1; i < len(pts); i++ {
			a, b := pts[i-1], pts[i]
			dir, back, dx, dy := heading(a, b)
			if dir == 0 {
				continue
			}
			for x, y := a[0], a[1]; ; x, y = x+dx, y+dy {
				if x != a[0] || y != a[1] {
					mark(x, y, back, !e.Dashed)
				}
				if x == b[0] && y == b[1] {
					break
				}
				mark(x, y, dir, !e.Dashed)
			}
			last = dir
		}
		if n := len(pts); n > 1 && last != 0 && !e.NoArrow {
			x, y := pts[n-1][0], pts[n-1][1]
			switch last {
			case down:
				y--
			case up:
				y++
			case right:
				x--
			case left:
				x++
			}
			if x >= 0 && y >= 0 && x < p.W && y < p.H {
				t.tips[y][x] = arrows[last]
			}
		}
	}
	return t
}

// Boxes are drawn after edges so a route ending on a border never shows through it.
func Draw(p Placed) Picture {
	t := trace(p)
	cells := make([][]Cell, p.H)
	for y := range cells {
		cells[y] = make([]Cell, p.W)
		for x := range cells[y] {
			c := Cell{S: " ", Node: -1}
			switch {
			case t.tips[y][x] != "":
				c.S = t.tips[y][x]
			case t.mask[y][x] != 0:
				c.S = lines[t.mask[y][x]]
				if !t.solid[y][x] {
					c.S = dashed(c.S)
				}
			}
			cells[y][x] = c
		}
	}
	for i, b := range p.Boxes {
		box(cells, b, i, p.Diagram.Nodes[i])
	}
	for _, l := range p.Labels {
		write(cells, l.X, l.Y, l.Text, -1)
	}
	return Picture{Cells: cells, Boxes: p.Boxes, Diagram: p.Diagram}
}

func set(cells [][]Cell, x, y int, s string, node int) {
	if y >= 0 && y < len(cells) && x >= 0 && x < len(cells[y]) {
		cells[y][x] = Cell{S: s, Node: node}
	}
}

func write(cells [][]Cell, x, y int, text string, node int) {
	for text != "" {
		g, w := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
		text = text[len(g):]
		if w == 0 {
			continue
		}
		set(cells, x, y, g, node)
		for k := 1; k < w; k++ {
			set(cells, x+k, y, "", node)
		}
		x += w
	}
}

func box(cells [][]Cell, b Rect, node int, n Node) {
	for x := b.X; x < b.X+b.W; x++ {
		for y := b.Y; y < b.Y+b.H; y++ {
			set(cells, x, y, " ", node)
		}
		set(cells, x, b.Y, "─", node)
		set(cells, x, b.Y+b.H-1, "─", node)
	}
	for y := b.Y; y < b.Y+b.H; y++ {
		set(cells, b.X, y, "│", node)
		set(cells, b.X+b.W-1, y, "│", node)
	}
	set(cells, b.X, b.Y, "┌", node)
	set(cells, b.X+b.W-1, b.Y, "┐", node)
	set(cells, b.X, b.Y+b.H-1, "└", node)
	set(cells, b.X+b.W-1, b.Y+b.H-1, "┘", node)

	y := b.Y + (b.H-textRows(n))/2
	for i, l := range n.Lines {
		write(cells, b.X+2, y, l, node)
		y++
		if i == 0 && n.Header && len(n.Lines) > 1 {
			for x := b.X + 1; x < b.X+b.W-1; x++ {
				set(cells, x, y, "─", node)
			}
			set(cells, b.X, y, "├", node)
			set(cells, b.X+b.W-1, y, "┤", node)
			y++
		}
	}
}

// orthogonal splits any diagonal step, which rounding to cells can leave, into down-then-across.
func orthogonal(pts [][2]int) [][2]int {
	var out [][2]int
	for i, p := range pts {
		if i > 0 {
			a := out[len(out)-1]
			if a[0] != p[0] && a[1] != p[1] {
				out = append(out, [2]int{a[0], p[1]})
			}
		}
		out = append(out, p)
	}
	return out
}

// straight drops repeated points and the middle of any three in line, which also removes a run that doubles back on itself.
func straight(pts [][2]int) [][2]int {
	var out [][2]int
	for _, p := range pts {
		if n := len(out); n > 0 && out[n-1] == p {
			continue
		}
		if n := len(out); n > 1 {
			a, b := out[n-2], out[n-1]
			if (a[0] == b[0] && b[0] == p[0]) || (a[1] == b[1] && b[1] == p[1]) {
				out[n-1] = p
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// path is r as drawn: squared off, kept off its boxes' corners, and touching both boxes.
func (p Placed) path(r Route) [][2]int {
	e := p.Diagram.Edges[r.Edge]
	pts := straight(orthogonal(r.Points))
	fix := func(pts [][2]int, b Rect) [][2]int {
		if moved := offCorner(pts, b); !p.crosses(moved, e) {
			return moved
		}
		return pts
	}
	pts = fix(pts, p.Boxes[e.From])
	pts = reversed(fix(reversed(pts), p.Boxes[e.To]))
	pts = touch(pts, p.Boxes[e.To])
	return reversed(touch(reversed(pts), p.Boxes[e.From]))
}

func (p Placed) crosses(pts [][2]int, e Edge) bool {
	for _, c := range cells(pts) {
		for bi, b := range p.Boxes {
			if bi != e.From && bi != e.To && b.contains(c[0], c[1]) {
				return true
			}
		}
	}
	return false
}

func (p Placed) clearOfBoxes() bool {
	for _, r := range p.Routes {
		if p.crosses(p.path(r), p.Diagram.Edges[r.Edge]) {
			return false
		}
	}
	return true
}

// Clean reports whether every edge can be told apart: none crosses a box, and only edges sharing an end share a line.
func (p Placed) Clean() bool {
	type run struct {
		cell     [2]int
		vertical bool
	}
	owner := map[run][]int{}
	for _, r := range p.Routes {
		e := p.Diagram.Edges[r.Edge]
		pts := p.path(r)
		if p.crosses(pts, e) {
			return false
		}
		for i := 1; i < len(pts); i++ {
			vertical := pts[i-1][0] == pts[i][0]
			for _, c := range cells(pts[i-1 : i+1]) {
				k := run{c, vertical}
				for _, j := range owner[k] {
					if o := p.Diagram.Edges[j]; o.From != e.From && o.To != e.To {
						return false
					}
				}
				owner[k] = append(owner[k], r.Edge)
			}
		}
	}
	return true
}

// offCorner moves a route's first run off b's corner rows or columns, jogging two cells out when that run is the whole route.
func offCorner(pts [][2]int, b Rect) [][2]int {
	next := 1
	for next < len(pts) && pts[next] == pts[0] {
		next++
	}
	if next == len(pts) {
		return pts
	}
	_, _, dx, dy := heading(pts[0], pts[next])
	axis, lo, hi := 1, b.Y+1, b.Y+b.H-2
	if dx == 0 {
		axis, lo, hi = 0, b.X+1, b.X+b.W-2
	}
	from := pts[0][axis]
	to := min(max(from, lo), hi)
	if lo > hi || to == from {
		return pts
	}
	run := 0
	for run+1 < len(pts) && pts[run+1][axis] == from {
		run++
	}
	out := append([][2]int{}, pts...)
	if run < len(pts)-1 {
		for i := 0; i <= run; i++ {
			out[i][axis] = to
		}
		return out
	}
	start, end := pts[0], pts[len(pts)-1]
	knee := [2]int{start[0] + 2*dx, start[1] + 2*dy}
	if abs(end[0]-start[0])+abs(end[1]-start[1]) < 3 {
		return pts
	}
	moved, bent := start, knee
	moved[axis], bent[axis] = to, to
	return [][2]int{moved, bent, knee, end}
}

func reversed(pts [][2]int) [][2]int {
	out := make([][2]int, len(pts))
	for i, p := range pts {
		out[len(pts)-1-i] = p
	}
	return out
}

// Graphviz's tip can round to a cell short of the target or inside it, so it is moved onto the border.
func touch(pts [][2]int, target Rect) [][2]int {
	n := len(pts)
	if n < 2 {
		return pts
	}
	_, _, dx, dy := heading(pts[n-2], pts[n-1])
	if dx == 0 && dy == 0 {
		return pts
	}
	end := pts[n-1]
	for range 3 {
		if target.contains(end[0], end[1]) {
			break
		}
		end = [2]int{end[0] + dx, end[1] + dy}
	}
	for range 3 {
		back := [2]int{end[0] - dx, end[1] - dy}
		if !target.contains(back[0], back[1]) || back == pts[n-2] {
			break
		}
		end = back
	}
	if !target.contains(end[0], end[1]) {
		return pts
	}
	out := append([][2]int{}, pts[:n-1]...)
	return append(out, end)
}

func heading(a, b [2]int) (dir, back, dx, dy int) {
	switch {
	case b[1] > a[1]:
		return down, up, 0, 1
	case b[1] < a[1]:
		return up, down, 0, -1
	case b[0] > a[0]:
		return right, left, 1, 0
	case b[0] < a[0]:
		return left, right, -1, 0
	}
	return 0, 0, 0, 0
}

func dashed(s string) string {
	switch s {
	case "│":
		return "╎"
	case "─":
		return "╌"
	}
	return s
}

func grid[T any](w, h int) [][]T {
	g := make([][]T, h)
	for i := range g {
		g[i] = make([]T, w)
	}
	return g
}

// At reports the node whose box covers x, y.
func (p Picture) At(x, y int) (int, bool) {
	for i, b := range p.Boxes {
		if b.contains(x, y) {
			return i, true
		}
	}
	return 0, false
}

// String is the picture with no styling, one line per row.
func (p Picture) String() string {
	var b strings.Builder
	for _, row := range p.Cells {
		var line strings.Builder
		for _, c := range row {
			line.WriteString(c.S)
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
