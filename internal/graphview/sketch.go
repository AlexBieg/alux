package graphview

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"tuios.dev/sketch"
	"tuios.dev/sketch/text"
)

// sketchBorder is the colour sketch draws box borders in, and nothing else.
const sketchBorder = "36"

var mermaidEscapes = strings.NewReplacer("#", "#35;", `"`, "#quot;", "<", "#lt;", ">", "#gt;", "\n", " ")

type tinted struct {
	s, sgr string
}

// SketchPicture draws g with sketch, reading each node's box back from the border colour.
// It is false when the boxes cannot be paired one-to-one with the nodes.
func SketchPicture(g Diagram) (pic Picture, ok bool) {
	defer func() {
		if recover() != nil {
			pic, ok = Picture{}, false
		}
	}()
	if len(g.Nodes) == 0 {
		return Picture{}, false
	}
	for _, e := range g.Edges {
		if e.From < 0 || e.From >= len(g.Nodes) || e.To < 0 || e.To >= len(g.Nodes) {
			return Picture{}, false
		}
	}
	d, err := sketch.Parse(mermaidOf(g), sketch.ParseOptions{})
	if err != nil {
		return Picture{}, false
	}
	lines, err := text.Draw(d, text.Options{Color: true})
	if err != nil {
		return Picture{}, false
	}
	grid := tintedGrid(lines)
	groups := borderGroups(grid)
	if len(groups) != len(g.Nodes) {
		return Picture{}, false
	}
	owners, ok := pairBoxes(d, grid, groups, g)
	if !ok {
		return Picture{}, false
	}

	boxes := make([]Rect, len(g.Nodes))
	for k, r := range groups {
		boxes[owners[k]] = r
	}
	cells := make([][]Cell, len(grid))
	for y, row := range grid {
		cells[y] = make([]Cell, len(row))
		for x, c := range row {
			cells[y][x] = Cell{S: c.s, Node: -1}
			for i, b := range boxes {
				if b.contains(x, y) {
					cells[y][x].Node = i
				}
			}
		}
	}
	return Picture{Cells: cells, Boxes: boxes, Diagram: g}, true
}

func mermaidOf(g Diagram) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	for i, n := range g.Nodes {
		fmt.Fprintf(&b, "  n%d[\"%s\"]\n", i, mermaidEscapes.Replace(nodeText(n)))
	}
	for _, e := range g.Edges {
		arrow := "-->"
		if e.Dashed {
			arrow = "-.->"
		}
		fmt.Fprintf(&b, "  n%d %s n%d\n", e.From, arrow, e.To)
	}
	return b.String()
}

func nodeText(n Node) string { return strings.Join(n.Lines, " ") }

// tintedGrid splits sketch's coloured lines into cells, a wide glyph's second cell left empty.
func tintedGrid(lines []string) [][]tinted {
	grid := make([][]tinted, len(lines))
	for y, line := range lines {
		sgr := ""
		for line != "" {
			if strings.HasPrefix(line, "\x1b[") {
				end := strings.IndexByte(line, 'm')
				if end < 0 {
					break
				}
				sgr = line[2:end]
				line = line[end+1:]
				continue
			}
			g, w := ansi.FirstGraphemeCluster(line, ansi.GraphemeWidth)
			line = line[len(g):]
			if w == 0 {
				continue
			}
			grid[y] = append(grid[y], tinted{g, sgr})
			for k := 1; k < w; k++ {
				grid[y] = append(grid[y], tinted{"", sgr})
			}
		}
	}
	return grid
}

func isBorder(grid [][]tinted, x, y int) bool {
	if y < 0 || y >= len(grid) || x < 0 || x >= len(grid[y]) {
		return false
	}
	c := grid[y][x]
	return strings.TrimSpace(c.s) != "" && strings.Split(c.sgr, ";")[0] == sketchBorder
}

// borderGroups is the bounding rect of each 4-connected run of border cells, in reading order.
func borderGroups(grid [][]tinted) []Rect {
	seen := map[[2]int]bool{}
	var out []Rect
	for y := range grid {
		for x := range grid[y] {
			if !isBorder(grid, x, y) || seen[[2]int{x, y}] {
				continue
			}
			x0, y0, x1, y1 := x, y, x, y
			stack := [][2]int{{x, y}}
			seen[[2]int{x, y}] = true
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				x0, y0, x1, y1 = min(x0, p[0]), min(y0, p[1]), max(x1, p[0]), max(y1, p[1])
				for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					q := [2]int{p[0] + d[0], p[1] + d[1]}
					if isBorder(grid, q[0], q[1]) && !seen[q] {
						seen[q] = true
						stack = append(stack, q)
					}
				}
			}
			out = append(out, Rect{X: x0, Y: y0, W: x1 - x0 + 1, H: y1 - y0 + 1})
		}
	}
	return out
}

func inside(grid [][]tinted, r Rect) string {
	var b strings.Builder
	for y := r.Y + 1; y < r.Y+r.H-1; y++ {
		for x := r.X + 1; x < r.X+r.W-1 && x < len(grid[y]); x++ {
			b.WriteString(grid[y][x].s)
		}
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// pairBoxes gives each group its node by label; nodes sharing a label pair with their groups in reading order of the layout.
func pairBoxes(d *sketch.Diagram, grid [][]tinted, groups []Rect, g Diagram) ([]int, bool) {
	byLabel := map[string][]int{}
	for i, n := range g.Nodes {
		l := strings.Join(strings.Fields(nodeText(n)), " ")
		byLabel[l] = append(byLabel[l], i)
	}
	groupsBy := map[string][]int{}
	for k, r := range groups {
		l := inside(grid, r)
		groupsBy[l] = append(groupsBy[l], k)
	}
	var centre map[int][2]float64
	owners := make([]int, len(groups))
	for l, ks := range groupsBy {
		nodes := byLabel[l]
		if len(nodes) != len(ks) {
			return nil, false
		}
		if len(nodes) > 1 {
			if centre == nil {
				geo, err := d.Layout(sketch.RenderOptions{})
				if err != nil {
					return nil, false
				}
				centre = map[int][2]float64{}
				for _, n := range geo.Nodes {
					var i int
					if _, err := fmt.Sscanf(n.ID, "n%d", &i); err == nil {
						centre[i] = [2]float64{n.Y, n.X}
					}
				}
			}
			slices.SortFunc(nodes, func(a, b int) int {
				return cmp.Or(cmp.Compare(centre[a][0], centre[b][0]), cmp.Compare(centre[a][1], centre[b][1]))
			})
		}
		for j, k := range ks {
			owners[k] = nodes[j]
		}
	}
	return owners, true
}
