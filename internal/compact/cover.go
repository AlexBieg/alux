package compact

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

const (
	// recentTurns is how many of the last user turns keep their replies whole.
	recentTurns = 2
	// coverK and coverShare: a sentence drops when a non-negative combination of its coverK nearest newer
	// sentences explains coverShare of its centered direction; k=3 let loosely related neighbours fake coverage.
	coverK     = 2
	coverShare = 0.12
	// coverFloor is the centered cosine a newer sentence needs to join a covering set; unrelated ones sit near 0.
	coverFloor = 0.15
)

var (
	boldLeadRe = regexp.MustCompile(`^(\s*(?:[-*•]|\d+[.)])?\s*\*\*[^*]+\*\*:?)\s*(.*)$`)
	// injectedRe marks a user turn the harness wrote, which compresses like a reply.
	injectedRe = regexp.MustCompile(`^\s*<(background-agents|subagent-requests|background-commands)>`)
)

// sentences splits a line at a sentence end followed by a space and a capital, backtick, quote or bold mark.
func sentences(line string) []string {
	var out []string
	start := 0
	for i := 0; i+2 < len(line); i++ {
		if (line[i] == '.' || line[i] == '!' || line[i] == '?') && line[i+1] == ' ' {
			c := line[i+2]
			if c >= 'A' && c <= 'Z' || c == '`' || c == '"' || c == '*' || c == '(' {
				out = append(out, line[start:i+1])
				start = i + 2
			}
		}
	}
	return append(out, line[start:])
}

// units splits text the way the walk does: lines, a bold lead set aside, then sentences.
func units(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if m := boldLeadRe.FindStringSubmatch(l); m != nil {
			l = m[2]
		}
		for _, s := range sentences(l) {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// centered subtracts the session's mean vector and renormalizes, so cosine reflects what a sentence says
// rather than what the session is about.
func centered(vecs map[string][]float32) map[string][]float32 {
	var mean []float64
	n := 0
	for _, v := range vecs {
		if v == nil {
			continue
		}
		if mean == nil {
			mean = make([]float64, len(v))
		}
		for i, x := range v {
			mean[i] += float64(x)
		}
		n++
	}
	out := make(map[string][]float32, len(vecs))
	for k, v := range vecs {
		if v == nil {
			continue
		}
		c := make([]float32, len(v))
		var norm float64
		for i, x := range v {
			d := float64(x) - mean[i]/float64(n)
			c[i] = float32(d)
			norm += d * d
		}
		if norm > 0 {
			inv := float32(1 / math.Sqrt(norm))
			for i := range c {
				c[i] *= inv
			}
		}
		out[k] = c
	}
	return out
}

type coverJudge struct {
	vecs map[string][]float32
	seen [][]float32
}

func (c *coverJudge) see(text string) {
	for _, u := range units(text) {
		if v := c.vecs[u]; v != nil {
			c.seen = append(c.seen, v)
		}
	}
}

func (c *coverJudge) novel(s string) bool {
	v := c.vecs[strings.TrimSpace(s)]
	return v == nil || coverage(v, c.seen) < coverShare
}

func dot(a, b []float32) float32 {
	var d float32
	for i := range a {
		d += a[i] * b[i]
	}
	return d
}

// coverage is the largest squared norm of v's projection onto the span of a subset of its coverK nearest
// vectors in seen, counting only subsets whose least-squares weights are all positive.
func coverage(v []float32, seen [][]float32) float32 {
	type cand struct {
		u   []float32
		cos float32
	}
	var near []cand
	for _, u := range seen {
		if d := dot(v, u); d > coverFloor {
			near = append(near, cand{u, d})
		}
	}
	sort.Slice(near, func(a, b int) bool { return near[a].cos > near[b].cos })
	near = near[:min(coverK, len(near))]
	best := float32(0)
	for mask := 1; mask < 1<<len(near); mask++ {
		var us [][]float32
		var b []float64
		for i := range near {
			if mask&(1<<i) != 0 {
				us = append(us, near[i].u)
				b = append(b, float64(near[i].cos))
			}
		}
		g := make([][]float64, len(us))
		for i := range us {
			g[i] = make([]float64, len(us))
			for j := range us {
				g[i][j] = float64(dot(us[i], us[j]))
			}
		}
		w, ok := solve(g, b)
		if !ok {
			continue
		}
		cov := 0.0
		for i := range w {
			if w[i] <= 0 {
				ok = false
			}
			cov += w[i] * b[i]
		}
		if ok && float32(cov) > best {
			best = float32(cov)
		}
	}
	return best
}

// solve returns x with g x = b by Gaussian elimination with partial pivoting, or false when g is near singular.
func solve(g [][]float64, b []float64) ([]float64, bool) {
	n := len(b)
	a := make([][]float64, n)
	for i := range a {
		a[i] = append(append([]float64(nil), g[i]...), b[i])
	}
	for col := 0; col < n; col++ {
		p := col
		for r := col + 1; r < n; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[p][col]) {
				p = r
			}
		}
		if math.Abs(a[p][col]) < 1e-6 {
			return nil, false
		}
		a[col], a[p] = a[p], a[col]
		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			f := a[r][col] / a[col][col]
			for c := col; c <= n; c++ {
				a[r][c] -= f * a[col][c]
			}
		}
	}
	x := make([]float64, n)
	for i := range x {
		x[i] = a[i][n] / a[i][i]
	}
	return x, true
}

// reverseMMR renders the ledger with sentences of older replies and harness-written turns dropped when
// newer user turns and replies already cover them, walking newest first so a fact survives in its newest
// wording. With no vectors every sentence is novel and the plain ledger comes back.
func reverseMMR(ev []Event, ws string, vecs map[string][]float32, keep bool) string {
	j := &coverJudge{vecs: centered(vecs)}
	ages := map[int]int{}
	ledger(ev, ws, keep, func(ev []Event, i, age int) string {
		ages[i] = age
		return ev[i].Text
	})
	users := 0
	for i := len(ev) - 1; i >= 0; i-- {
		if ev[i].Kind == "user" {
			if injectedRe.MatchString(ev[i].Text) {
				ages[i] = users
			}
			users++
		}
	}
	compress := func(text string, age int) string {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		if age < recentTurns || len(lines) <= 2 {
			j.see(text)
			return text
		}
		j.see(lines[len(lines)-1])
		var mid []string
		for k := len(lines) - 2; k >= 1; k-- {
			prefix, body := "", lines[k]
			if m := boldLeadRe.FindStringSubmatch(lines[k]); m != nil {
				prefix, body = m[1], m[2]
			}
			ss := sentences(body)
			var keep []string
			for n := len(ss) - 1; n >= 0; n-- {
				if j.novel(ss[n]) {
					keep = append([]string{ss[n]}, keep...)
				}
				j.see(ss[n])
			}
			if len(keep) > 0 {
				mid = append([]string{strings.TrimSpace(prefix + " " + strings.Join(keep, " "))}, mid...)
			}
		}
		j.see(lines[0])
		return strings.Join(append(append([]string{lines[0]}, mid...), lines[len(lines)-1]), "\n")
	}
	out := append([]Event(nil), ev...)
	kept := map[int]string{}
	for i := len(ev) - 1; i >= 0; i-- {
		age, ok := ages[i]
		switch {
		case ev[i].Kind == "user" && ok:
			out[i].Text = compress(ev[i].Text, age)
		case ev[i].Kind == "user":
			j.see(ev[i].Text)
		case ok:
			kept[i] = compress(ev[i].Text, age)
		}
	}
	return ledger(out, ws, keep, func(_ []Event, i, _ int) string { return kept[i] })
}
