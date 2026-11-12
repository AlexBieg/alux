package graphview

import (
	"cmp"
	"math"
	"slices"
)

// channel reroutes a polyline TD layout through shared lanes between ranks, each gap sized to its lanes.
func (p *Placed) channel() {
	d := p.Diagram
	var cs []int
	for _, b := range p.Boxes {
		c := b.Y + b.H/2
		if !slices.ContainsFunc(cs, func(v int) bool { return abs(v-c) <= 1 }) {
			cs = append(cs, c)
		}
	}
	slices.Sort(cs)
	rank := make([]int, len(p.Boxes))
	for i, b := range p.Boxes {
		c := b.Y + b.H/2
		rank[i] = slices.IndexFunc(cs, func(v int) bool { return abs(v-c) <= 1 })
	}
	type span struct{ lo, hi int }
	band := make([]span, len(cs))
	for i := range band {
		band[i] = span{math.MaxInt, math.MinInt}
	}
	for i, b := range p.Boxes {
		r := rank[i]
		band[r] = span{min(band[r].lo, b.Y), max(band[r].hi, b.Y+b.H-1)}
	}
	centre := func(i int) int { return p.Boxes[i].X + p.Boxes[i].W/2 }

	type plan struct {
		route, a, b int
		reversed    bool
		way         []int
	}
	at := func(pts [][2]int, y int) int {
		for k := 1; k < len(pts); k++ {
			x0, y0, x1, y1 := pts[k-1][0], pts[k-1][1], pts[k][0], pts[k][1]
			if (y0 <= y && y <= y1) || (y1 <= y && y <= y0) {
				if y0 == y1 {
					return x0
				}
				return x0 + (x1-x0)*(y-y0)/(y1-y0)
			}
		}
		return -1
	}
	var plans []plan
	for ri, r := range p.Routes {
		e := d.Edges[r.Edge]
		if rank[e.From] == rank[e.To] {
			continue
		}
		pl := plan{route: ri, a: e.From, b: e.To}
		if rank[pl.b] < rank[pl.a] {
			pl.a, pl.b, pl.reversed = pl.b, pl.a, true
		}
		for k := rank[pl.a] + 1; k < rank[pl.b]; k++ {
			c := at(r.Points, cs[k])
			if c < 0 {
				c = centre(pl.a)
			}
			pl.way = append(pl.way, c)
		}
		plans = append(plans, pl)
	}

	// Ports on one side are spread evenly, ordered by where each edge heads, so edges never share a cell.
	type pref struct{ plan, goal int }
	outs, ins := map[int][]pref{}, map[int][]pref{}
	for pi, pl := range plans {
		next, prev := centre(pl.b), centre(pl.a)
		if len(pl.way) > 0 {
			next, prev = pl.way[0], pl.way[len(pl.way)-1]
		}
		outs[pl.a] = append(outs[pl.a], pref{pi, next})
		ins[pl.b] = append(ins[pl.b], pref{pi, prev})
	}
	from, to := make([]int, len(plans)), make([]int, len(plans))
	assign := func(m map[int][]pref, dst []int) {
		for n, ps := range m {
			slices.SortStableFunc(ps, func(x, y pref) int { return cmp.Compare(x.goal, y.goal) })
			lo, hi := p.Boxes[n].X+1, p.Boxes[n].X+p.Boxes[n].W-2
			k := len(ps)
			for j, q := range ps {
				switch {
				case k == 1:
					dst[q.plan] = (lo + hi) / 2
				case hi-lo+1 > k+1:
					dst[q.plan] = lo + (hi-lo+1)*(j+1)/(k+1) - 1
				default:
					dst[q.plan] = lo + (hi-lo)*j/(k-1)
				}
			}
		}
	}
	assign(outs, from)
	assign(ins, to)
	crossAt := func(pi int) []int {
		return append(append([]int{from[pi]}, plans[pi].way...), to[pi])
	}

	type crossing struct{ plan, c0, c1 int }
	gaps := map[int][]crossing{}
	for pi, pl := range plans {
		cross := crossAt(pi)
		for k := rank[pl.a] + 1; k <= rank[pl.b]; k++ {
			j := k - rank[pl.a]
			gaps[k] = append(gaps[k], crossing{pi, cross[j-1], cross[j]})
		}
	}
	// Edges whose runs across a gap overlap, or come within a cell, take separate tracks; the rest share.
	track := map[[2]int]int{}
	tracks := map[int]int{}
	for g, list := range gaps {
		// An edge heading left turns first, so tracks do not cross.
		slices.SortStableFunc(list, func(x, y crossing) int {
			dx, dy := x.c1-x.c0, y.c1-y.c0
			if (dx < 0) != (dy < 0) {
				return cmp.Compare(dx, dy)
			}
			if dx < 0 {
				return cmp.Compare(x.c0, y.c0)
			}
			return cmp.Compare(y.c0, x.c0)
		})
		// An edge dropping in at the column another edge rises from must run above it, or their uprights would merge.
		above := func(x, y crossing) bool { return x.c0 == y.c1 }
		var used [][]span
		placed := make([]bool, len(list))
		for range list {
			next := -1
			for i := range list {
				if placed[i] {
					continue
				}
				if next < 0 {
					next = i
				}
				blocked := false
				for j := range list {
					if !placed[j] && j != i && above(list[j], list[i]) {
						blocked = true
						break
					}
				}
				if !blocked {
					next = i
					break
				}
			}
			placed[next] = true
			c := list[next]
			floor := 0
			for j, o := range list {
				if placed[j] && j != next && above(o, c) {
					floor = max(floor, track[[2]int{o.plan, g}]+1)
				}
			}
			iv := span{min(c.c0, c.c1), max(c.c0, c.c1)}
			k := floor
			for ; k < len(used); k++ {
				if !slices.ContainsFunc(used[k], func(o span) bool { return iv.lo <= o.hi+1 && o.lo <= iv.hi+1 }) {
					break
				}
			}
			for k >= len(used) {
				used = append(used, nil)
			}
			used[k] = append(used[k], iv)
			track[[2]int{c.plan, g}] = k
		}
		tracks[g] = len(used)
	}

	labelled := map[int]bool{}
	for _, pl := range plans {
		if e := d.Edges[p.Routes[pl.route].Edge]; e.Label+e.Tail+e.Head != "" {
			for k := rank[pl.a] + 1; k <= rank[pl.b]; k++ {
				labelled[k] = true
			}
		}
	}
	shift := make([]int, len(cs))
	for g := 1; g < len(cs); g++ {
		rows := max(tracks[g], 1)
		if labelled[g] {
			rows *= 2
		}
		shift[g] = band[g-1].hi + shift[g-1] + rows + 3 - band[g].lo
	}
	for i := range p.Boxes {
		p.Boxes[i].Y += shift[rank[i]]
	}
	for _, r := range p.Routes {
		if e := d.Edges[r.Edge]; rank[e.From] == rank[e.To] {
			for k := range r.Points {
				r.Points[k][1] += shift[rank[e.From]]
			}
		}
	}
	for g := range band {
		band[g] = span{band[g].lo + shift[g], band[g].hi + shift[g]}
	}

	for pi, pl := range plans {
		a, b := p.Boxes[pl.a], p.Boxes[pl.b]
		cross := crossAt(pi)
		pts := [][2]int{{cross[0], a.Y + a.H - 1}}
		for k := rank[pl.a] + 1; k <= rank[pl.b]; k++ {
			j := k - rank[pl.a]
			lo, hi, n := band[k-1].hi+2, band[k].lo-2, tracks[k]
			t := track[[2]int{pi, k}]
			y := lo + (hi-lo+1)*(t+1)/(n+1)
			if hi-lo+1 < n+1 {
				y = lo + min(t, max(hi-lo, 0))
			}
			pts = append(pts, [2]int{cross[j-1], y}, [2]int{cross[j], y})
		}
		pts = append(pts, [2]int{to[pi], b.Y})
		if pl.reversed {
			slices.Reverse(pts)
		}
		p.Routes[pl.route].Points = pts
	}

	// Labels start at the end of the route they belong to, and place() moves them beside it.
	p.Labels = nil
	for _, r := range p.Routes {
		e := d.Edges[r.Edge]
		first, last := r.Points[0], r.Points[len(r.Points)-1]
		for _, l := range []struct {
			text string
			at   [2]int
		}{{e.Label, first}, {e.Tail, first}, {e.Head, last}} {
			if l.text != "" {
				p.Labels = append(p.Labels, Label{Edge: r.Edge, Text: l.text, X: l.at[0], Y: l.at[1]})
			}
		}
	}
}
