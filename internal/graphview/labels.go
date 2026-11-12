package graphview

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/charmbracelet/x/ansi"
)

// place puts each label beside its own edge, off every line and a cell clear of boxes and labels, nearest where Graphviz put it.
func (p *Placed) place() error {
	if len(p.Labels) == 0 {
		return nil
	}
	t := trace(*p)
	own := map[int][][2]int{}
	for _, r := range p.Routes {
		own[r.Edge] = cells(p.path(r))
	}
	taken := map[[2]int]bool{}
	free := func(x, y, w int) bool {
		for cx := x - 1; cx <= x+w; cx++ {
			if taken[[2]int{cx, y}] {
				return false
			}
			for _, b := range p.Boxes {
				if b.contains(cx, y) {
					return false
				}
			}
			if cx >= x && cx < x+w && t.inked(cx, y) {
				return false
			}
		}
		return true
	}
	for i := range p.Labels {
		l := &p.Labels[i]
		w := ansi.StringWidth(l.Text)
		var spots [][2]int
		for _, c := range own[l.Edge] {
			spots = append(spots, [2]int{c[0] + 1, c[1]}, [2]int{c[0] - w, c[1]}, [2]int{c[0] - w/2, c[1] - 1}, [2]int{c[0] - w/2, c[1] + 1})
		}
		cost := func(s [2]int) int { return abs(s[0]-l.X) + 2*abs(s[1]-l.Y) }
		slices.SortStableFunc(spots, func(a, b [2]int) int {
			return cmp.Or(cmp.Compare(cost(a), cost(b)), cmp.Compare(a[1], b[1]), cmp.Compare(a[0], b[0]))
		})
		at := slices.IndexFunc(spots, func(s [2]int) bool { return free(s[0], s[1], w) })
		if at < 0 {
			return fmt.Errorf("no room to draw the label %q", l.Text)
		}
		l.X, l.Y = spots[at][0], spots[at][1]
		for cx := l.X; cx < l.X+w; cx++ {
			taken[[2]int{cx, l.Y}] = true
		}
	}
	return nil
}

func cells(pts [][2]int) [][2]int {
	var out [][2]int
	for i := 1; i < len(pts); i++ {
		_, _, dx, dy := heading(pts[i-1], pts[i])
		for x, y := pts[i-1][0], pts[i-1][1]; ; x, y = x+dx, y+dy {
			out = append(out, [2]int{x, y})
			if (x == pts[i][0] && y == pts[i][1]) || (dx == 0 && dy == 0) {
				break
			}
		}
	}
	return out
}
