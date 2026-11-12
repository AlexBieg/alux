package records

import (
	"slices"
	"strings"
	"sync"
)

const (
	linkSeeds   = 5
	linkRestart = 0.3
	linkSteps   = 50
)

// linkGraph joins current records only through their written links, never a shared subject, with superseded ends resolved to successors.
type linkGraph struct {
	index  map[string]int
	ids    []string
	record []bool
	adj    [][]int
	degree []float64
}

type graphCache struct {
	mu  sync.Mutex
	gen string
	g   *linkGraph
}

// Rows are only inserted, so these counters change whenever the graph could.
const graphGeneration = `SELECT coalesce((SELECT max(rowid) FROM records), 0) || ':' ||
	coalesce((SELECT max(id) FROM links), 0) || ':' || coalesce((SELECT max(id) FROM record_events), 0) || ':' ||
	(SELECT count(*) FROM supersessions) || ':' || (SELECT count(*) FROM entity_merges) || ':' || date('now')`

func (s *Store) graph() (*linkGraph, error) {
	var gen string
	if err := s.db.QueryRow(graphGeneration).Scan(&gen); err != nil {
		return nil, err
	}
	s.links.mu.Lock()
	defer s.links.mu.Unlock()
	if s.links.g != nil && s.links.gen == gen {
		return s.links.g, nil
	}
	g, err := s.loadGraph()
	if err != nil {
		return nil, err
	}
	s.links.g, s.links.gen = g, gen
	return g, nil
}

func (s *Store) loadGraph() (*linkGraph, error) {
	g := &linkGraph{index: map[string]int{}}
	node := func(id string, record bool) int {
		if i, ok := g.index[id]; ok {
			return i
		}
		g.index[id] = len(g.ids)
		g.ids = append(g.ids, id)
		g.record = append(g.record, record)
		g.adj = append(g.adj, nil)
		g.degree = append(g.degree, 0)
		return len(g.ids) - 1
	}
	join := func(a, b int) {
		if a == b {
			return
		}
		g.adj[a] = append(g.adj[a], b)
		g.adj[b] = append(g.adj[b], a)
		g.degree[a]++
		g.degree[b]++
	}

	current, err := s.strings(`SELECT id FROM current_records`)
	if err != nil {
		return nil, err
	}
	for _, id := range current {
		node(id, true)
	}
	succ, err := s.pairs(`SELECT predecessor, successor FROM supersessions`)
	if err != nil {
		return nil, err
	}
	next := make(map[string]string, len(succ))
	for _, p := range succ {
		next[p[0]] = p[1]
	}
	resolve := func(id string) (int, bool) {
		for range len(next) + 1 {
			n, ok := next[id]
			if !ok {
				break
			}
			id = n
		}
		i, ok := g.index[id]
		return i, ok && g.record[i]
	}
	links, err := s.pairs(`SELECT l.from_record, coalesce(l.to_record, 'ent:' || h.head) FROM links l
		LEFT JOIN entity_head h ON h.id = l.to_entity`)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		from, ok := resolve(l[0])
		if !ok {
			continue
		}
		if ent, isEnt := strings.CutPrefix(l[1], "ent:"); isEnt {
			join(from, node(ent, false))
		} else if to, ok := resolve(l[1]); ok {
			join(from, to)
		}
	}
	return g, nil
}

func (s *Store) pairs(q string) ([][2]string, error) {
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// spread runs personalized PageRank from the seeds, weighted by rank, and scores every record it reaches.
func (g *linkGraph) spread(seeds []string) map[string]float64 {
	start := make([]float64, len(g.ids))
	total := 0.0
	for i, id := range seeds {
		if n, ok := g.index[id]; ok {
			start[n] += 1 / float64(i+1)
			total += 1 / float64(i+1)
		}
	}
	if total == 0 {
		return nil
	}
	for i := range start {
		start[i] /= total
	}
	score := slices.Clone(start)
	next := make([]float64, len(score))
	for range linkSteps {
		for i := range next {
			next[i] = linkRestart * start[i]
		}
		for n, v := range score {
			if v == 0 || g.degree[n] == 0 {
				continue
			}
			for _, m := range g.adj[n] {
				next[m] += (1 - linkRestart) * v / g.degree[n]
			}
		}
		score, next = next, score
	}
	out := map[string]float64{}
	for n, v := range score {
		if v > 0 && g.record[n] {
			out[g.ids[n]] = v
		}
	}
	return out
}
