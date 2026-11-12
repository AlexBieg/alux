package mine

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	minSupport     = 5
	maxGap         = 3
	maxLen         = 8
	minCalls       = 3
	manyRounds     = 3
	loopMinTurns   = 5
	coveredShare   = 0.8
	familyJaccard  = 0.5
	listLimit      = 20
	exampleLimit   = 3
	extensionLimit = 3
	lockMaxAge     = 10 * time.Minute
)

const many = "×many"

// Result is the ranked output of one mining run.
type Result struct {
	Skills, Scripts, Covered []Family
}

// Family is a group of overlapping patterns, shown by its most significant one.
type Family struct {
	Pattern      []string
	Support      int
	Lift         float64
	Significance float64
	Examples     []string
	Skill        string
	Extensions   []Family
}

type pattern struct {
	items        []string
	convs        []int
	calls        map[string]bool
	lift, signif float64
}

// Run mines the log in dir and writes dir/candidates.md, unless another run holds a fresh lock.
func Run(dir string) error {
	if _, err := os.Stat(SequencesFile(dir)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	release, ok, err := acquireLock(filepath.Join(dir, "mine.lock"))
	if err != nil || !ok {
		return err
	}
	defer release()
	turns, err := ReadLog(SequencesFile(dir))
	if err != nil {
		return err
	}
	dismissed, err := readDismissed(filepath.Join(dir, "dismissed.txt"))
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "candidates.md"), render(mine(turns, dismissed), time.Now()))
}

// Mine finds recurring workflows across the conversations in turns.
func Mine(turns []Turn) Result { return mine(turns, nil) }

func mine(turns []Turn, dismissed map[string]bool) Result {
	keys, seqs, skills := conversations(turns)
	pats := maximal(prefixSpan(seqs))
	score(pats, seqs)
	var res Result
	var open []Family
	for _, g := range families(pats) {
		rep := g.rep
		if dismissed[strings.Join(rep.items, " > ")] {
			continue
		}
		f := Family{Pattern: rep.items, Support: len(rep.convs), Lift: rep.lift, Significance: rep.signif}
		for _, c := range rep.convs[:min(exampleLimit, len(rep.convs))] {
			f.Examples = append(f.Examples, keys[c])
		}
		for _, e := range g.extensions[:min(extensionLimit, len(g.extensions))] {
			f.Extensions = append(f.Extensions, Family{Pattern: e.items, Support: len(e.convs), Lift: e.lift, Significance: e.signif})
		}
		if f.Skill = coveringSkill(rep.convs, skills); f.Skill != "" {
			res.Covered = append(res.Covered, f)
		} else {
			open = append(open, f)
		}
	}
	res.Skills = open[:min(listLimit, len(open))]
	scripts := slices.Clone(open)
	sort.SliceStable(scripts, func(i, j int) bool {
		if scripts[i].Support != scripts[j].Support {
			return scripts[i].Support > scripts[j].Support
		}
		return len(scripts[i].Pattern) > len(scripts[j].Pattern)
	})
	res.Scripts = scripts[:min(listLimit, len(scripts))]
	return res
}

// conversations is each conversation's key, collapsed symbol sequence and skills, in log order.
func conversations(turns []Turn) (keys []string, seqs [][]string, skills []map[string]bool) {
	bucketed := make([][]string, len(turns))
	runs := make([][]loopRun, len(turns))
	loopTurns := map[string]int{}
	for i, t := range turns {
		bucketed[i] = bucket(t.Calls)
		runs[i] = findLoops(bucketed[i])
		seen := map[string]bool{}
		for _, r := range runs[i] {
			if !seen[r.key] {
				seen[r.key] = true
				loopTurns[r.key]++
			}
		}
	}
	index := map[string]int{}
	for i, t := range turns {
		c, ok := index[t.Key()]
		if !ok {
			c = len(keys)
			index[t.Key()] = c
			keys = append(keys, t.Key())
			seqs = append(seqs, nil)
			skills = append(skills, map[string]bool{})
		}
		if t.Skill != "" {
			skills[c][t.Skill] = true
		}
		for _, s := range collapse(bucketed[i], runs[i], loopTurns) {
			seqs[c] = appendNew(seqs[c], s)
		}
	}
	return keys, seqs, skills
}

func appendNew(seq []string, s string) []string {
	if len(seq) > 0 && seq[len(seq)-1] == s {
		return seq
	}
	return append(seq, s)
}

func bucket(calls []string) []string {
	var out []string
	for i := 0; i < len(calls); {
		j := i
		for j < len(calls) && calls[j] == calls[i] {
			j++
		}
		if j-i >= manyRounds {
			out = append(out, calls[i]+many)
		} else {
			out = append(out, calls[i])
		}
		i = j
	}
	return out
}

type loopRun struct {
	start, end int
	key        string
	many       bool
}

func findLoops(seq []string) []loopRun {
	base := make([]string, len(seq))
	for i, s := range seq {
		base[i] = strings.TrimSuffix(s, many)
	}
	var runs []loopRun
	for i := 0; i < len(seq); {
		best, bestK := 0, 0
		for k := 2; k <= 5 && i+k <= len(seq); k++ {
			if !distinct(base[i : i+k]) {
				continue
			}
			j := i + k
			for j < len(seq) && base[j] == base[j-k] {
				j++
			}
			if n := j - i; n >= k+(k+1)/2 && n > best {
				best, bestK = n, k
			}
		}
		if bestK == 0 {
			i++
			continue
		}
		runs = append(runs, loopRun{start: i, end: i + best, key: loopKey(base[i : i+bestK]), many: best >= manyRounds*bestK})
		i += best
	}
	return runs
}

func distinct(block []string) bool {
	seen := map[string]bool{}
	for _, s := range block {
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

// loopKey names a block by its smallest rotation, so a loop entered at any step gets one key.
func loopKey(block []string) string {
	best := block
	for r := 1; r < len(block); r++ {
		rot := append(slices.Clone(block[r:]), block[:r]...)
		if slices.Compare(rot, best) < 0 {
			best = rot
		}
	}
	return "loop{" + strings.Join(best, ",") + "}"
}

func collapse(seq []string, runs []loopRun, loopTurns map[string]int) []string {
	var out []string
	pos := 0
	for _, r := range runs {
		if loopTurns[r.key] < loopMinTurns {
			continue
		}
		out = append(out, seq[pos:r.start]...)
		sym := r.key
		if r.many {
			sym += many
		}
		out = append(out, sym)
		pos = r.end
	}
	return append(out, seq[pos:]...)
}

// calls is the underlying tool calls a symbol stands for.
func calls(sym string) []string {
	sym = strings.TrimSuffix(sym, many)
	if inner, ok := strings.CutPrefix(sym, "loop{"); ok {
		return strings.Split(strings.TrimSuffix(inner, "}"), ",")
	}
	return []string{sym}
}

type occurrence struct {
	conv int
	ends []int
}

func prefixSpan(seqs [][]string) []*pattern {
	var out []*pattern
	var grow func(items []string, proj []occurrence)
	grow = func(items []string, proj []occurrence) {
		if len(proj) < minSupport {
			return
		}
		p := &pattern{items: slices.Clone(items)}
		for _, o := range proj {
			p.convs = append(p.convs, o.conv)
		}
		out = append(out, p)
		if len(items) == maxLen {
			return
		}
		next := map[string][]occurrence{}
		for _, o := range proj {
			seq := seqs[o.conv]
			ends := map[string][]int{}
			for _, e := range o.ends {
				for q := e + 1; q <= e+1+maxGap && q < len(seq); q++ {
					if es := ends[seq[q]]; len(es) == 0 || es[len(es)-1] < q {
						ends[seq[q]] = append(es, q)
					}
				}
			}
			for s, es := range ends {
				next[s] = append(next[s], occurrence{o.conv, es})
			}
		}
		for s, proj := range next {
			grow(append(items, s), proj)
		}
	}
	first := map[string][]occurrence{}
	for c, seq := range seqs {
		ends := map[string][]int{}
		for q, s := range seq {
			ends[s] = append(ends[s], q)
		}
		for s, es := range ends {
			first[s] = append(first[s], occurrence{c, es})
		}
	}
	for s, proj := range first {
		grow([]string{s}, proj)
	}
	return out
}

// maximal drops patterns contained in a longer one with the same conversations, and patterns under minCalls calls.
func maximal(pats []*pattern) []*pattern {
	byConvs := map[string][]*pattern{}
	for _, p := range pats {
		k := fmt.Sprint(p.convs)
		byConvs[k] = append(byConvs[k], p)
	}
	var out []*pattern
	for _, p := range pats {
		n := 0
		for _, s := range p.items {
			n += len(calls(s))
		}
		if n < minCalls {
			continue
		}
		contained := false
		for _, q := range byConvs[fmt.Sprint(p.convs)] {
			if len(q.items) > len(p.items) && isSubsequence(p.items, q.items) {
				contained = true
				break
			}
		}
		if !contained {
			out = append(out, p)
		}
	}
	return out
}

func isSubsequence(sub, seq []string) bool {
	i := 0
	for _, s := range seq {
		if i < len(sub) && sub[i] == s {
			i++
		}
	}
	return i == len(sub)
}

func score(pats []*pattern, seqs [][]string) {
	n := len(seqs)
	present := map[string]int{}
	for _, seq := range seqs {
		seen := map[string]bool{}
		for _, s := range seq {
			if !seen[s] {
				seen[s] = true
				present[s]++
			}
		}
	}
	for _, p := range pats {
		prob := 1.0
		seen := map[string]bool{}
		p.calls = map[string]bool{}
		for _, s := range p.items {
			for _, c := range calls(s) {
				p.calls[c] = true
			}
			if !seen[s] {
				seen[s] = true
				prob *= float64(present[s]) / float64(n)
			}
		}
		p.lift = float64(len(p.convs)) / (float64(n) * prob)
		p.signif = significance(n, len(p.convs), prob)
	}
	sort.SliceStable(pats, func(i, j int) bool {
		a, b := pats[i], pats[j]
		if a.signif != b.signif {
			return a.signif > b.signif
		}
		if len(a.convs) != len(b.convs) {
			return len(a.convs) > len(b.convs)
		}
		return strings.Join(a.items, " > ") < strings.Join(b.items, " > ")
	})
}

// significance is -log10 P(X >= k) for X ~ Bin(n, p), summed in log space so rare patterns stay finite.
func significance(n, k int, p float64) float64 {
	if k <= 0 || p >= 1 {
		return 0
	}
	lp, lq := math.Log(p), math.Log1p(-p)
	lnN, _ := math.Lgamma(float64(n + 1))
	terms := make([]float64, 0, n-k+1)
	peak := math.Inf(-1)
	for i := k; i <= n; i++ {
		lnI, _ := math.Lgamma(float64(i + 1))
		lnR, _ := math.Lgamma(float64(n - i + 1))
		t := lnN - lnI - lnR + float64(i)*lp + float64(n-i)*lq
		terms = append(terms, t)
		peak = math.Max(peak, t)
	}
	sum := 0.0
	for _, t := range terms {
		sum += math.Exp(t - peak)
	}
	return math.Max(0, -(peak+math.Log(sum))/math.Ln10)
}

type family struct {
	rep        *pattern
	extensions []*pattern
}

// families groups patterns, which must already be sorted by descending significance.
// A pattern holding a family's top pattern in order is kept as an extension, since
// its conversations are too few for the overlap test to ever join it.
func families(pats []*pattern) []*family {
	var fams []*family
	for _, p := range pats {
		joined := false
		for _, f := range fams {
			if isSubsequence(f.rep.items, p.items) {
				f.extensions = append(f.extensions, p)
				joined = true
				break
			}
			if jaccardSet(p.calls, f.rep.calls) >= familyJaccard && jaccardConvs(p.convs, f.rep.convs) >= familyJaccard {
				joined = true
				break
			}
		}
		if !joined {
			fams = append(fams, &family{rep: p})
		}
	}
	return fams
}

func jaccardSet(a, b map[string]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

func jaccardConvs(a, b []int) float64 {
	set := map[int]bool{}
	for _, c := range a {
		set[c] = true
	}
	inter := 0
	for _, c := range b {
		if set[c] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

func coveringSkill(convs []int, skills []map[string]bool) string {
	counts := map[string]int{}
	for _, c := range convs {
		for s := range skills[c] {
			counts[s]++
		}
	}
	best := ""
	for s, n := range counts {
		if n > counts[best] || (n == counts[best] && s < best) {
			best = s
		}
	}
	if best == "" || float64(counts[best]) < coveredShare*float64(len(convs)) {
		return ""
	}
	return best
}

func acquireLock(path string) (release func(), ok bool, err error) {
	for range 2 {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, err
		}
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if time.Since(info.ModTime()) < lockMaxAge {
			return nil, false, nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func readDismissed(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out[line] = true
		}
	}
	return out, sc.Err()
}

func render(res Result, now time.Time) []byte {
	var b strings.Builder
	b.WriteString("# Workflow candidates\n\nGenerated at " + now.UTC().Format(time.RFC3339) + ".\n")
	section := func(title string, fams []Family) {
		b.WriteString("\n## " + title + "\n\n")
		if len(fams) == 0 {
			b.WriteString("None.\n")
		}
		for i, f := range fams {
			marker := strconv.Itoa(i+1) + ". "
			fmt.Fprintf(&b, "%s%s — %d conversations, lift %s, significance %s — %s",
				marker, strings.Join(f.Pattern, " > "), f.Support,
				strconv.FormatFloat(f.Lift, 'f', 1, 64), strconv.FormatFloat(f.Significance, 'f', 1, 64),
				strings.Join(f.Examples, ", "))
			if f.Skill != "" {
				b.WriteString(" — " + f.Skill)
			}
			b.WriteString("\n")
			// Markdown nests a sub-item only when it is indented to the entry's content column.
			indent := strings.Repeat(" ", len(marker))
			for _, e := range f.Extensions {
				fmt.Fprintf(&b, "%s- %s — %d conversations, significance %s\n",
					indent, strings.Join(e.Pattern, " > "), e.Support, strconv.FormatFloat(e.Significance, 'f', 1, 64))
			}
		}
	}
	section("Skill candidates", res.Skills)
	section("Script candidates", res.Scripts)
	section("Covered by existing skills", res.Covered)
	return []byte(b.String())
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".candidates-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
