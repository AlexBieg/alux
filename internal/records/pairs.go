package records

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// PairFloor is the cosine at which two unlinked current records are put to the curator to judge.
const PairFloor = 0.75

// Like record_embeddings it sits outside the schema version, so harnesses on older builds keep opening the store and simply never read it.
const pairTable = `CREATE TABLE IF NOT EXISTS pair_verdicts (
  id       INTEGER PRIMARY KEY,
  a        TEXT NOT NULL REFERENCES records(id),
  b        TEXT NOT NULL REFERENCES records(id),
  verdict  TEXT NOT NULL CHECK (verdict IN ('same', 'related', 'distinct', 'reopened')),
  cosine   REAL,
  model    TEXT,
  reason   TEXT,
  tx_id    TEXT NOT NULL REFERENCES txns(id),
  CHECK (a < b)
) STRICT;
CREATE INDEX IF NOT EXISTS pair_verdicts_pair ON pair_verdicts (a, b, id)`

// ErrNoEmbedder means search by meaning is off, so there are no vectors to pair records by.
var ErrNoEmbedder = errors.New("search by meaning is off (no embedder), so record pairs cannot be found")

var pairVerdicts = []string{"same", "related", "distinct", "reopened"}

type Pair struct {
	A, B   Row
	Cosine float32
}

func orderPair(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}

// Pairs lists up to limit unlinked, unjudged current pairs at PairFloor or above, closest first, with the total; phi is never paired, so it never taints.
func (s *Store) Pairs(limit int) ([]Pair, int, error) {
	found, err := s.openPairs()
	if err != nil {
		return nil, 0, err
	}
	total := len(found)
	found = found[:min(limit, total)]
	if len(found) == 0 {
		return nil, total, nil
	}
	var ids []string
	for _, p := range found {
		ids = append(ids, p.a, p.b)
	}
	list, _ := json.Marshal(ids)
	rows, err := s.rows(`SELECT `+rowColumns+` FROM current_records r `+subjectJoin+` WHERE r.id IN (SELECT value FROM json_each(?))`, string(list))
	if err != nil {
		return nil, 0, err
	}
	byID := map[string]Row{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]Pair, 0, len(found))
	for _, p := range found {
		out = append(out, Pair{A: byID[p.a], B: byID[p.b], Cosine: p.sim})
	}
	return out, total, nil
}

// PairBacklog counts what Pairs would list, reading no content; with no embedder it is zero.
func (s *Store) PairBacklog() (int, error) {
	found, err := s.openPairs()
	if errors.Is(err, ErrNoEmbedder) {
		return 0, nil
	}
	return len(found), err
}

type openPair struct {
	a, b string
	sim  float32
}

func (s *Store) openPairs() ([]openPair, error) {
	if s.embed == nil {
		return nil, ErrNoEmbedder
	}
	eligible, err := s.strings(`SELECT r.id FROM current_records r ` + subjectJoin + ` WHERE r.sensitivity = 'normal' AND e.sensitivity = 'normal'`)
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{}
	for _, id := range eligible {
		ok[id] = true
	}
	settled := map[string]bool{}
	// A link joins its ends' current versions, as reads forward it; a verdict judged the content it saw, so it is not forwarded.
	joined, err := s.strings(`WITH RECURSIVE
			chain(id, cur) AS (SELECT id, id FROM records UNION ALL SELECT c.id, s.successor FROM chain c JOIN supersessions s ON s.predecessor = c.cur),
			head(id, head) AS (SELECT id, cur FROM chain WHERE cur NOT IN (SELECT predecessor FROM supersessions))
		SELECT min(f.head, t.head) || '|' || max(f.head, t.head) FROM links l JOIN head f ON f.id = l.from_record JOIN head t ON t.id = l.to_record
		UNION SELECT a || '|' || b FROM pair_verdicts v WHERE v.id = (SELECT max(id) FROM pair_verdicts w WHERE w.a = v.a AND w.b = v.b) AND v.verdict != 'reopened'`)
	if err != nil {
		return nil, err
	}
	for _, k := range joined {
		settled[k] = true
	}
	ids, vecs, err := s.vectors()
	if err != nil {
		return nil, err
	}
	var keep []int
	for i, id := range ids {
		if ok[id] {
			keep = append(keep, i)
		}
	}
	var out []openPair
	for x, i := range keep {
		vi := vecs[i]
		for _, j := range keep[x+1:] {
			vj := vecs[j]
			if len(vi) != len(vj) {
				continue
			}
			var sim float32
			for k := range vi {
				sim += vi[k] * vj[k]
			}
			if sim < PairFloor {
				continue
			}
			a, b := orderPair(ids[i], ids[j])
			if settled[a+"|"+b] {
				continue
			}
			out = append(out, openPair{a, b, sim})
		}
	}
	slices.SortFunc(out, func(p, q openPair) int {
		switch {
		case p.sim > q.sim:
			return -1
		case p.sim < q.sim:
			return 1
		}
		return strings.Compare(p.a+p.b, q.a+q.b)
	})
	return out, nil
}

// Judge records a verdict on two current records; reopened puts a judged pair back in the queue.
func (s *Store) Judge(a, b, verdict, reason string, actor Actor) error {
	err := s.judge(a, b, verdict, reason, actor)
	if reason, msg, ok := asRefusal(err); ok {
		return &Rejection{Reason: reason, Message: msg}
	}
	return err
}

func (s *Store) judge(a, b, verdict, reason string, actor Actor) error {
	a, b = orderPair(strings.TrimSpace(a), strings.TrimSpace(b))
	switch {
	case a == "" || b == "":
		return refuse("judge needs record and other, the two record ids of the pair")
	case a == b:
		return refuse("record and other are the same id")
	case !slices.Contains(pairVerdicts, verdict):
		return refuse("verdict must be same, related, distinct or reopened")
	}
	var cosine, model any
	if s.embed != nil {
		var va, vb []byte
		err := s.db.QueryRow(`SELECT (SELECT vector FROM record_embeddings WHERE record_id = ?1 AND model = ?3),
			(SELECT vector FROM record_embeddings WHERE record_id = ?2 AND model = ?3)`, a, b, s.embed.Model()).Scan(&va, &vb)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if x, y := decodeVector(va), decodeVector(vb); len(x) > 0 && len(x) == len(y) {
			var sim float32
			for k := range x {
				sim += x[k] * y[k]
			}
			cosine, model = float64(sim), s.embed.Model()
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range []string{a, b} {
		if err := mustBeCurrent(tx, id, "judge"); err != nil {
			return err
		}
	}
	var phi bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM records r `+subjectJoin+` WHERE r.id IN (?, ?) AND (r.sensitivity = 'phi' OR e.sensitivity = 'phi'))`, a, b).Scan(&phi); err != nil {
		return err
	}
	if phi {
		return refuse("pairs that include a phi record are not judged here")
	}
	txID, err := insertTxn(tx, actor)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO pair_verdicts (a, b, verdict, cosine, model, reason, tx_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a, b, verdict, cosine, model, null(reason), txID); err != nil {
		return err
	}
	return tx.Commit()
}
