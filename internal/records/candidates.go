package records

import (
	"database/sql"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/alexbieg/alux/internal/taint"
)

// Candidate is a current record or entity a new one may duplicate or link to, with the signals that found it.
type Candidate struct {
	ID, Kind, Subject, Title string
	Signals                  []string
	Score                    float64
	phi                      bool
	// features are the unscaled parts of Score, logged so a floor can compare candidates across drafts.
	features map[string]float64
}

// Weights fitted on dev notes against writers' own cross-note links; holdout recall@5 0.633.
const (
	maxCandidates   = 5
	titlePoolSize   = 400
	titleWeight     = 20.0
	subjectBonus    = 6.0
	commonIdentLine = 50
)

var (
	identToken = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_./#-]*[A-Za-z0-9]`)
	dateToken  = regexp.MustCompile(`^\d{4}-\d{2}(-\d{2})?$`)
	titleWord  = regexp.MustCompile(`[A-Za-z0-9]+`)
)

func logSuggestions(tx *sql.Tx, txID, target, draft, written string, cands []Candidate, sensitivity string) error {
	for i, c := range cands {
		features, err := json.Marshal(c.features)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO candidate_suggestions (tx_id, target, draft, written_id, candidate_id, rank, score, signals, features, sensitivity)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, txID, target, draft, written, c.ID, i+1, c.Score, strings.Join(c.Signals, "; "), string(features), sensitivity); err != nil {
			return err
		}
	}
	return nil
}

var nameWord = regexp.MustCompile(`[a-z0-9]+`)

func nameKey(s string) string {
	return strings.Join(nameWord.FindAllString(strings.ToLower(s), -1), "")
}

func nameWords(s string) []string {
	var out []string
	for _, w := range nameWord.FindAllString(strings.ToLower(s), -1) {
		if len(w) > 2 && !stopwords[w] && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// A word in at most this many entities' names is rare enough that sharing it suggests the same thing.
const rareNameWord = 3

// findEntityCandidates ranks existing entities whose names match the draft's once punctuation is gone, or share a rare word.
func findEntityCandidates(tx *sql.Tx, names []string, phiWrite bool) ([]Candidate, error) {
	rows, err := tx.Query(`SELECT h.head, e.type, e.name, e.sensitivity = 'phi', n.name FROM (
			SELECT h.head AS head, e.name AS name FROM entity_head h JOIN entities e ON e.id = h.id
			UNION SELECT h.head, a.alias FROM entity_head h JOIN entity_aliases a ON a.entity_id = h.id) n
		JOIN entity_head h ON h.id = n.head AND h.head = h.id JOIN entities e ON e.id = h.head`)
	if err != nil {
		return nil, err
	}
	type entity struct {
		kind, name string
		phi        bool
		keys       []string
		words      []string
	}
	all := map[string]*entity{}
	var order []string
	for rows.Next() {
		var id, kind, name, alias string
		var phi bool
		if err := rows.Scan(&id, &kind, &name, &phi, &alias); err != nil {
			rows.Close()
			return nil, err
		}
		e, ok := all[id]
		if !ok {
			e = &entity{kind: kind, name: name, phi: phi}
			all[id] = e
			order = append(order, id)
		}
		e.keys = append(e.keys, nameKey(alias))
		for _, w := range nameWords(alias) {
			if !slices.Contains(e.words, w) {
				e.words = append(e.words, w)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	df := map[string]int{}
	for _, e := range all {
		for _, w := range e.words {
			df[w]++
		}
	}
	var keys, words []string
	for _, n := range names {
		keys = append(keys, nameKey(n))
		for _, w := range nameWords(n) {
			if !slices.Contains(words, w) {
				words = append(words, w)
			}
		}
	}
	var out []Candidate
	for _, id := range order {
		e := all[id]
		if e.phi && !phiWrite {
			continue
		}
		c := Candidate{ID: id, Kind: e.kind, Subject: e.name, Title: e.name, phi: e.phi,
			features: map[string]float64{"same_name": 0, "rare_words": 0, "rare_word_idf": 0, "entities": float64(len(all))}}
		for _, k := range keys {
			if k != "" && slices.Contains(e.keys, k) {
				c.Score += 3
				c.features["same_name"] = 1
				c.Signals = append(c.Signals, "same name apart from punctuation")
				break
			}
		}
		var shared []string
		for _, w := range words {
			if slices.Contains(e.words, w) && df[w] <= rareNameWord {
				shared = append(shared, w)
				c.features["rare_word_idf"] += math.Log(float64(len(all)) / float64(df[w]))
			}
		}
		if len(shared) > 0 {
			c.Score += float64(len(shared))
			c.features["rare_words"] = float64(len(shared))
			c.Signals = append(c.Signals, "shares rare name word "+strings.Join(shared, ", "))
		}
		if c.Score > 0 {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	for _, c := range out {
		if all[c.ID].phi {
			taint.Set()
		}
	}
	return out, nil
}

// identifiers are the tokens a human would grep for: paths, snake_case names, ticket keys, change ids with digits.
func identifiers(text string) []string {
	var out []string
	for _, tok := range identToken.FindAllString(text, -1) {
		if len(tok) < 4 || dateToken.MatchString(tok) || strings.Trim(tok, "0123456789") == "" {
			continue
		}
		hasDigit := strings.ContainsAny(tok, "0123456789")
		hasLetter := strings.IndexFunc(tok, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }) >= 0
		if strings.ContainsAny(tok, "_./#") || hasDigit && hasLetter {
			if low := strings.ToLower(tok); !slices.Contains(out, low) {
				out = append(out, low)
			}
		}
	}
	return out
}

type candidateRow struct {
	kind, subject, subjectID, title, text string
	rid                                   int64
	phi                                   bool
	fts, bm25                             float64
	shared                                []string
	ident                                 float64
}

// findCandidates ranks current records from other sources against a draft, leaving out the ids in exclude.
func findCandidates(tx *sql.Tx, subject, title, text, srcID string, phiWrite bool, exclude []string) ([]Candidate, error) {
	if err := tx.QueryRow(`SELECT head FROM entity_head WHERE id = ?`, subject).Scan(&subject); err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, id := range exclude {
		skip[id] = true
	}

	pool := map[string]*candidateRow{}
	fetch := func(query string, args ...any) ([]string, error) {
		rows, err := tx.Query(`SELECT r.id, r.rid, r.kind, e.name, sh.head, r.title, coalesce(r.text, ''),
				r.sensitivity = 'phi' OR e.sensitivity = 'phi', f.score
			FROM (`+query+`) f JOIN current_records r ON r.rid = f.rowid `+subjectJoin+`
			WHERE r.derived_from != ? ORDER BY f.score DESC`, append(args, srcID)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var rid string
			var c candidateRow
			if err := rows.Scan(&rid, &c.rid, &c.kind, &c.subject, &c.subjectID, &c.title, &c.text, &c.phi, &c.fts); err != nil {
				return nil, err
			}
			if skip[rid] || c.phi && !phiWrite {
				continue
			}
			if _, ok := pool[rid]; !ok {
				pool[rid] = &c
			}
			ids = append(ids, rid)
		}
		return ids, rows.Err()
	}

	var words []string
	top := 0.0
	for _, w := range titleWord.FindAllString(title, -1) {
		if q := `"` + w + `"`; len(w) > 2 && !stopwords[strings.ToLower(w)] && !slices.Contains(words, q) {
			words = append(words, q)
		}
	}
	if len(words) > 0 {
		hits, err := fetch(`SELECT rowid, -bm25(records_search, 5.0, 1.0, 1.0, 1.0, 2.0) AS score FROM records_search
			WHERE records_search MATCH ? ORDER BY bm25(records_search, 5.0, 1.0, 1.0, 1.0, 2.0) LIMIT ?`,
			strings.Join(words, " OR "), titlePoolSize)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			top = math.Max(top, pool[h].fts)
		}
		for _, h := range hits {
			pool[h].bm25 = pool[h].fts
			if top > 0 {
				pool[h].fts = titleWeight * pool[h].fts / top
			}
		}
	}

	var total float64
	if err := tx.QueryRow(`SELECT count(*) FROM records`).Scan(&total); err != nil {
		return nil, err
	}
	for _, tok := range identifiers(title + " " + text) {
		phrase := `{title text} : "` + tok + `"`
		var df float64
		if err := tx.QueryRow(`SELECT count(*) FROM records_search WHERE records_search MATCH ?`, phrase).Scan(&df); err != nil {
			return nil, err
		}
		if df == 0 || df > commonIdentLine {
			continue
		}
		hits, err := fetch(`SELECT rowid, 0.0 AS score FROM records_search WHERE records_search MATCH ?`, phrase)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			c := pool[h]
			// The phrase match is token-wise; only an exact identifier counts as shared.
			if slices.Contains(identifiers(c.title+" "+c.text), tok) && !slices.Contains(c.shared, tok) {
				c.shared = append(c.shared, tok)
				c.ident += math.Log(total / df)
			}
		}
	}

	var out []Candidate
	for rid, c := range pool {
		if c.fts == 0 && len(c.shared) == 0 {
			continue
		}
		cand := Candidate{ID: rid, Kind: c.kind, Subject: c.subject, Title: c.title, Score: c.fts + c.ident, phi: c.phi,
			features: map[string]float64{"bm25": c.bm25, "bm25_top": top, "title": c.fts, "ident": c.ident,
				"shared_idents": float64(len(c.shared)), "same_subject": 0, "records": total}}
		if c.fts > 0 {
			cand.Signals = append(cand.Signals, "title words")
		}
		if len(c.shared) > 0 {
			cand.Signals = append(cand.Signals, "shares "+strings.Join(c.shared, ", "))
		}
		if c.subjectID == subject {
			cand.Score += subjectBonus
			cand.features["same_subject"] = 1
			cand.Signals = append(cand.Signals, "same subject")
		}
		out = append(out, cand)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return pool[out[i].ID].rid > pool[out[j].ID].rid
	})
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	for _, c := range out {
		if pool[c.ID].phi {
			taint.Set()
		}
	}
	return out, nil
}
