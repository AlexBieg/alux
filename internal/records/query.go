package records

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/taint"
)

const defaultLimit = 10

type Row struct {
	ID          string
	Kind        string
	Status      string
	Subject     string
	Title       string
	Text        string
	Body        string
	Attrs       string
	ValidFrom   string
	ValidUntil  string
	Supersedes  string
	Confidence  string
	Sensitivity string
	Sources     int
	Slot        string
	Session     string
	Episode     string
	Partial     bool
	Linked      bool
	Similar     bool
}

type SearchInput struct {
	Query          string `json:"query"`
	Kind           string `json:"kind,omitempty"`
	Subject        string `json:"subject,omitempty"`
	IncludeHistory bool   `json:"include_history,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	AsOf
	noLinks bool
}

// AsOf reads the store at a point in time: ValidAt is when in the world, KnownAt is what the store had recorded by then.
type AsOf struct {
	ValidAt string `json:"as_of,omitempty"`
	KnownAt string `json:"known_at,omitempty"`
}

func (a AsOf) zero() bool {
	return strings.TrimSpace(a.ValidAt) == "" && strings.TrimSpace(a.KnownAt) == ""
}

// statusSource is what a query reads record status from: the view for now, or the same rules evaluated at a.
func statusSource(a AsOf) (string, error) {
	if a.zero() {
		return "record_status", nil
	}
	valid := "date('now')"
	if v := strings.TrimSpace(a.ValidAt); v != "" {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			return "", fmt.Errorf("as_of must be a date, YYYY-MM-DD")
		}
		valid = "'" + d.Format("2006-01-02") + "'"
	}
	known := "'9999-12-31T23:59:59.999Z'"
	if k := strings.TrimSpace(a.KnownAt); k != "" {
		at, err := knownAt(k)
		if err != nil {
			return "", err
		}
		known = "'" + at + "'"
	}
	return fmt.Sprintf(`(SELECT r.rowid AS rid, r.*,
		CASE
			WHEN EXISTS (SELECT 1 FROM record_events e JOIN txns t ON t.id = e.tx_id
				WHERE e.record_id = r.id AND e.event = 'retracted' AND t.at <= %[2]s) THEN 'retracted'
			WHEN EXISTS (SELECT 1 FROM record_events e JOIN txns t ON t.id = e.tx_id
				WHERE e.record_id = r.id AND e.event = 'dropped' AND t.at <= %[2]s) THEN 'dropped'
			WHEN EXISTS (WITH RECURSIVE ahead(id) AS (
					SELECT s.successor FROM supersessions s JOIN txns t ON t.id = s.tx_id WHERE s.predecessor = r.id AND t.at <= %[2]s
					UNION SELECT s.successor FROM supersessions s JOIN txns t ON t.id = s.tx_id JOIN ahead a ON s.predecessor = a.id
					WHERE t.at <= %[2]s)
				SELECT 1 FROM ahead a JOIN records n ON n.id = a.id
				WHERE n.valid_from IS NULL OR substr(n.valid_from, 1, 10) <= %[1]s) THEN 'superseded'
			WHEN (SELECT e.event FROM record_events e JOIN txns t ON t.id = e.tx_id
				WHERE e.record_id = r.id AND t.at <= %[2]s ORDER BY e.id DESC LIMIT 1) = 'done' THEN 'done'
			WHEN r.valid_until IS NOT NULL AND substr(r.valid_until, 1, 10) < %[1]s THEN 'expired'
			WHEN r.valid_from IS NOT NULL AND substr(r.valid_from, 1, 10) > %[1]s THEN 'not_yet'
			ELSE 'active'
		END AS status
		FROM records r JOIN txns rt ON rt.id = r.tx_id WHERE rt.at <= %[2]s)`, valid, known), nil
}

// A bare date means the end of that day, in the UTC form txns.at is written in.
func knownAt(s string) (string, error) {
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.Format("2006-01-02") + "T23:59:59.999Z", nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", fmt.Errorf("known_at must be a date, YYYY-MM-DD, or an RFC 3339 time")
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z"), nil
}

// A slot set after writing is shown in attrs, as if the record had been written with it.
const rowColumns = `r.id, r.kind, r.status, e.name, r.title, coalesce(r.text, ''), coalesce(r.body, ''),
	coalesce((SELECT json_set(r.attrs, '$.slot', sl.slot) FROM record_slots sl WHERE sl.record_id = r.id), r.attrs),
	coalesce(r.valid_from, ''), coalesce(r.valid_until, ''),
	coalesce((SELECT group_concat(p.predecessor, ' ') FROM supersessions p WHERE p.successor = r.id), ''), r.confidence,
	CASE WHEN r.sensitivity = 'phi' OR e.sensitivity = 'phi' THEN 'phi' ELSE 'normal' END,
	1 + (SELECT count(*) FROM record_sources rs WHERE rs.record_id = r.id),
	coalesce(json_extract(r.attrs, '$.slot'), (SELECT sl.slot FROM record_slots sl WHERE sl.record_id = r.id), ''),
	coalesce((SELECT t.session FROM txns t WHERE t.id = r.tx_id), ''),
	coalesce((SELECT CASE WHEN ep.sensitivity = 'phi' THEN 'a gist marked phi' ELSE ep.title END
		FROM txns t JOIN episodes ep ON ep.session = t.session WHERE t.id = r.tx_id), '')`

// A record about a merged entity is shown under the entity it was merged into.
const subjectJoin = `JOIN entity_head sh ON sh.id = r.subject_id JOIN entities e ON e.id = sh.head`

const membersOf = `(SELECT id FROM entity_head WHERE head = ?1)`

// Records matching every word come first, then partial matches fill the limit; any PHI returned taints the process.
func (s *Store) Search(in SearchInput) ([]Row, error) {
	rows, _, err := s.SearchNote(in)
	return rows, err
}

// SearchNote is Search plus a note to show beside the results when search by meaning could not take part.
func (s *Store) SearchNote(in SearchInput) ([]Row, string, error) {
	limit := in.Limit
	if limit <= 0 || limit > 50 {
		limit = defaultLimit
	}
	terms := ftsTerms(in.Query)
	if len(terms) == 0 {
		rows, err := s.search(in, "", limit, nil)
		return rows, "", err
	}
	all, err := s.search(in, strings.Join(terms, " AND "), limit, nil)
	if err != nil || len(all) == limit {
		return all, "", err
	}
	seen := make([]string, len(all))
	for i, r := range all {
		seen[i] = r.ID
	}
	var some []Row
	if len(terms) > 1 {
		if some, err = s.search(in, strings.Join(terms, " OR "), limit-len(all), seen); err != nil {
			return nil, "", err
		}
		for i := range some {
			some[i].Partial = true
		}
	}
	if in.noLinks || in.IncludeHistory || !in.AsOf.zero() {
		return append(all, some...), "", nil
	}
	g, err := s.graph()
	if err != nil {
		return nil, "", err
	}
	near, note := s.similar(in.Query, g)
	rest, err := s.followLinks(in, g, all, some, near, limit-len(all))
	if err != nil {
		return nil, "", err
	}
	return append(all, rest...), note, nil
}

// Full matches keep their places; the rest of the limit goes to partial matches, linked records and records close
// in meaning, by PageRank from the top keyword matches interleaved with the nearest vectors.
func (s *Store) followLinks(in SearchInput, g *linkGraph, all, some []Row, near []string, room int) ([]Row, error) {
	var kw []string
	for _, r := range slices.Concat(all, some) {
		if len(kw) == linkSeeds {
			break
		}
		kw = append(kw, r.ID)
	}
	var seeds []string
	for i := range max(len(kw), len(near)) {
		if i < len(kw) {
			seeds = append(seeds, kw[i])
		}
		if i < len(near) && !slices.Contains(seeds, near[i]) {
			seeds = append(seeds, near[i])
		}
	}
	if len(seeds) == 0 {
		return some, nil
	}
	score := g.spread(seeds)
	shown := map[string]bool{}
	for _, r := range slices.Concat(all, some) {
		shown[r.ID] = true
	}
	var reached []string
	for id := range score {
		if !shown[id] {
			reached = append(reached, id)
		}
	}
	slices.SortFunc(reached, func(a, b string) int {
		return cmp.Or(cmp.Compare(score[b], score[a]), strings.Compare(a, b))
	})
	if in.Kind == "" && in.Subject == "" {
		reached = reached[:min(len(reached), room)]
	}
	linked, err := s.byID(in, reached)
	if err != nil {
		return nil, err
	}
	for i := range linked {
		if slices.Contains(near, linked[i].ID) {
			linked[i].Similar = true
		} else {
			linked[i].Linked = true
		}
	}
	rest := slices.Concat(some, linked)
	slices.SortStableFunc(rest, func(a, b Row) int {
		return cmp.Compare(score[b.ID], score[a.ID])
	})
	return rest[:min(len(rest), room)], nil
}

// byID reads the named current records that pass the search's kind and subject filters.
func (s *Store) byID(in SearchInput, ids []string) ([]Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	where := []string{"r.status = 'active'", "r.id IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + ")"}
	var args []any
	for _, id := range ids {
		args = append(args, id)
	}
	if in.Kind != "" {
		where = append(where, "r.kind = ?")
		args = append(args, in.Kind)
	}
	if in.Subject != "" {
		id, err := resolveEntity(s.db, in.Subject)
		if err != nil {
			return nil, err
		}
		where = append(where, "r.subject_id IN (SELECT id FROM entity_head WHERE head = ?)")
		args = append(args, id)
	}
	return s.rows(fmt.Sprintf(`SELECT %s FROM record_status r %s WHERE %s`, rowColumns, subjectJoin, strings.Join(where, " AND ")), args...)
}

func (s *Store) search(in SearchInput, match string, limit int, exclude []string) ([]Row, error) {
	status, err := statusSource(in.AsOf)
	if err != nil {
		return nil, err
	}
	where := []string{"1"}
	var args []any
	from := status + " r " + subjectJoin
	order := "r.rid DESC"

	if match != "" {
		from = "records_search f JOIN " + status + " r ON r.rid = f.rowid " + subjectJoin
		where = append(where, "records_search MATCH ?")
		args = append(args, match)
		order = "bm25(records_search, 5.0, 1.0, 1.0, 1.0, 2.0)"
	}
	if len(exclude) > 0 {
		where = append(where, "r.id NOT IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(exclude)), ", ")+")")
		for _, id := range exclude {
			args = append(args, id)
		}
	}
	if in.Kind != "" {
		where = append(where, "r.kind = ?")
		args = append(args, in.Kind)
	}
	if in.Subject != "" {
		id, err := resolveEntity(s.db, in.Subject)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, fmt.Errorf("unknown subject %q", in.Subject)
		}
		where = append(where, "r.subject_id IN (SELECT id FROM entity_head WHERE head = ?)")
		args = append(args, id)
	}
	if !in.IncludeHistory {
		where = append(where, "r.status = 'active'")
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT ?`, rowColumns, from, strings.Join(where, " AND "), order)
	return s.rows(q, args...)
}

type Detail struct {
	Row
	Source       string
	RecordedAt   string
	RecordedBy   string
	Events       []string
	Links        []string
	Backlinks    []string
	Predecessors []string
	Successor    string
	Cited        []string
}

func (s *Store) Get(id string) (Detail, error) { return s.GetAt(id, AsOf{}) }

// GetAt shows the record's status as of a; its chain, links and citations are always shown in full.
func (s *Store) GetAt(id string, a AsOf) (Detail, error) {
	status, err := statusSource(a)
	if err != nil {
		return Detail{}, err
	}
	rows, err := s.rows(fmt.Sprintf(`SELECT %s FROM %s r %s WHERE r.id = ?`, rowColumns, status, subjectJoin), id)
	if err != nil {
		return Detail{}, err
	}
	if len(rows) == 0 {
		if !a.zero() {
			return Detail{}, fmt.Errorf("no record %q as of then: it may not have been recorded yet", id)
		}
		return Detail{}, fmt.Errorf("no record %q", id)
	}
	d := Detail{Row: rows[0]}
	if d.Cited, err = s.strings(`SELECT src.type || ': ' || src.ref || coalesce(' (' || src.title || ')', '') || ' (cited ' || substr(t.at, 1, 10) || ')'
		FROM record_sources rs JOIN sources src ON src.id = rs.source_id JOIN txns t ON t.id = rs.tx_id
		WHERE rs.record_id = ? ORDER BY t.at`, id); err != nil {
		return Detail{}, err
	}
	err = s.db.QueryRow(`SELECT src.type || ': ' || src.ref || coalesce(' (' || src.title || ')', ''), t.at,
		t.actor || coalesce('/' || t.agent, '') || coalesce(' on ' || t.model, '')
		FROM records r JOIN sources src ON src.id = r.derived_from JOIN txns t ON t.id = r.tx_id WHERE r.id = ?`, id).
		Scan(&d.Source, &d.RecordedAt, &d.RecordedBy)
	if err != nil {
		return Detail{}, err
	}
	if d.Events, err = s.strings(`SELECT ev.event || ' (' || t.at || '): ' || ev.reason FROM record_events ev
		JOIN txns t ON t.id = ev.tx_id WHERE ev.record_id = ? ORDER BY ev.id`, id); err != nil {
		return Detail{}, err
	}
	if d.Links, err = s.strings(`WITH RECURSIVE ahead(start, id, depth) AS (
			SELECT to_record, to_record, 0 FROM links WHERE from_record = ?1 AND to_record IS NOT NULL
			UNION ALL SELECT a.start, s.successor, a.depth + 1 FROM ahead a JOIN supersessions s ON s.predecessor = a.id)
		SELECT l.role || ' → ' || coalesce(l.to_record || ' ' || r.title || coalesce(' (now ' || n.id || ' ' || n.title || ')', ''), 'entity ' || e.name)
			|| CASE WHEN l.tx_id != (SELECT tx_id FROM records WHERE id = ?1) THEN ' (linked ' || substr(t.at, 1, 10) || ')' ELSE '' END
		FROM links l JOIN txns t ON t.id = l.tx_id LEFT JOIN records r ON r.id = l.to_record
			LEFT JOIN entity_head lh ON lh.id = l.to_entity LEFT JOIN entities e ON e.id = lh.head
		LEFT JOIN records n ON n.id = (SELECT a.id FROM ahead a WHERE a.start = l.to_record AND a.depth > 0 ORDER BY a.depth DESC LIMIT 1)
		WHERE l.from_record = ?1 ORDER BY l.id`, id); err != nil {
		return Detail{}, err
	}
	// A replaced linker is skipped when inheriting: its successor states its own links.
	if d.Backlinks, err = s.strings(`WITH RECURSIVE behind(id) AS (
			SELECT predecessor FROM supersessions WHERE successor = ?1
			UNION SELECT s.predecessor FROM supersessions s JOIN behind b ON s.successor = b.id)
		SELECT role || ' ← ' || from_record || ' ' || title || note FROM (
			SELECT l.id, l.role, l.from_record, r.title, '' AS note
			FROM links l JOIN records r ON r.id = l.from_record WHERE l.to_record = ?1
			UNION ALL
			SELECT l.id, l.role, l.from_record, r.title, ' (to ' || l.to_record || ', since replaced)'
			FROM links l JOIN records r ON r.id = l.from_record JOIN behind b ON b.id = l.to_record
			WHERE l.from_record != ?1
				AND NOT EXISTS (SELECT 1 FROM supersessions s WHERE s.predecessor = l.from_record)
				AND NOT EXISTS (SELECT 1 FROM links d WHERE d.from_record = l.from_record AND d.to_record = ?1 AND d.role = l.role))
		ORDER BY id`, id); err != nil {
		return Detail{}, err
	}
	// Merges make the chain a tree, so each ancestor is listed once, nearest first.
	if d.Predecessors, err = s.strings(`WITH RECURSIVE chain(id, depth) AS (
		SELECT predecessor, 1 FROM supersessions WHERE successor = ?
		UNION SELECT s.predecessor, c.depth + 1 FROM supersessions s JOIN chain c ON s.successor = c.id)
		SELECT r.id || ' ' || r.title FROM chain c JOIN records r ON r.id = c.id GROUP BY r.id ORDER BY min(c.depth), min(r.rowid)`, id); err != nil {
		return Detail{}, err
	}
	err = s.db.QueryRow(`SELECT r.id || ' ' || r.title FROM supersessions s JOIN records r ON r.id = s.successor
		WHERE s.predecessor = ?`, id).Scan(&d.Successor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Detail{}, err
	}
	return d, nil
}

type Entity struct {
	ID          string
	Type        string
	Name        string
	Description string
	Aliases     []string
	Merged      []string
	Records     []Row
	Mentions    []Row
}

// About is everything current about one entity, the shape a note about it used to have; kind narrows it to one kind.
func (s *Store) About(name, kind string) (Entity, error) { return s.AboutAt(name, kind, AsOf{}) }

// AboutAt lists what was current about the entity as of a.
func (s *Store) AboutAt(name, kind string, a AsOf) (Entity, error) {
	status, err := statusSource(a)
	if err != nil {
		return Entity{}, err
	}
	id, err := resolveEntity(s.db, name)
	if err != nil {
		return Entity{}, err
	}
	if id == "" {
		return Entity{}, fmt.Errorf("unknown entity %q", name)
	}
	var e Entity
	var sensitivity string
	if err := s.db.QueryRow(`SELECT id, type, name, coalesce(description, ''), sensitivity FROM entities WHERE id = ?`, id).
		Scan(&e.ID, &e.Type, &e.Name, &e.Description, &sensitivity); err != nil {
		return Entity{}, err
	}
	if sensitivity == "phi" {
		taint.Set()
	}
	if e.Aliases, err = s.strings(`SELECT alias FROM entity_aliases WHERE entity_id IN `+membersOf+` ORDER BY alias`, id); err != nil {
		return Entity{}, err
	}
	if e.Merged, err = s.strings(`SELECT e.name FROM entity_merges m JOIN entities e ON e.id = m.from_entity WHERE m.into_entity = ? ORDER BY e.name`, id); err != nil {
		return Entity{}, err
	}
	if e.Records, err = s.rows(fmt.Sprintf(`SELECT %s FROM %s r %s
		WHERE r.subject_id IN %s AND r.status = 'active' AND (?2 = '' OR r.kind = ?2) ORDER BY r.kind, r.rid`, rowColumns, status, subjectJoin, membersOf), id, kind); err != nil {
		return Entity{}, err
	}
	e.Mentions, err = s.rows(fmt.Sprintf(`SELECT %s FROM %s r %s
		WHERE r.subject_id NOT IN %s AND r.status = 'active' AND (?2 = '' OR r.kind = ?2)
		AND EXISTS (SELECT 1 FROM links l WHERE l.from_record = r.id AND l.to_entity IN %s AND l.role = 'about')
		ORDER BY r.kind, r.rid`, rowColumns, status, subjectJoin, membersOf, membersOf), id, kind)
	return e, err
}

// Conflict is one subject whose current states may describe the same thing twice.
type Conflict struct {
	Subject string
	States  []Row
	Shared  []string
}

// overlapping is every subject head with two or more current states where a slot is shared or a state has none.
const overlapping = `SELECT h.head FROM current_records r JOIN entity_head h ON h.id = r.subject_id
	JOIN entities e ON e.id = h.head JOIN record_slot sl ON sl.record_id = r.id
	WHERE r.kind = 'state' GROUP BY h.head
	HAVING count(*) >= 2 AND (count(sl.slot) < count(*) OR count(DISTINCT sl.slot) < count(sl.slot))`

// Conflicts lists subjects whose current states overlap; subjects holding any phi state are only counted, so the read never taints.
func (s *Store) Conflicts() ([]Conflict, int, error) {
	var phi int
	if err := s.db.QueryRow(`SELECT count(*) FROM (` + overlapping + `
		AND max(r.sensitivity = 'phi' OR e.sensitivity = 'phi') = 1)`).Scan(&phi); err != nil {
		return nil, 0, err
	}
	rows, err := s.rows(fmt.Sprintf(`SELECT %s FROM current_records r %s
		WHERE r.kind = 'state' AND sh.head IN (`+overlapping+` AND max(r.sensitivity = 'phi' OR e.sensitivity = 'phi') = 0)
		ORDER BY e.name COLLATE NOCASE, r.rid`, rowColumns, subjectJoin))
	if err != nil {
		return nil, 0, err
	}
	var out []Conflict
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].Subject != r.Subject {
			out = append(out, Conflict{Subject: r.Subject})
		}
		c := &out[len(out)-1]
		c.States = append(c.States, r)
	}
	for i := range out {
		n := map[string]int{}
		for _, r := range out[i].States {
			if r.Slot != "" {
				n[r.Slot]++
			}
		}
		for slot, k := range n {
			if k > 1 {
				out[i].Shared = append(out[i].Shared, slot)
			}
		}
		slices.Sort(out[i].Shared)
	}
	return out, phi, nil
}

// Backlog counts what a cleanup run could work: overlapping subjects with no phi state, and open unfiled entries. It reads no content, so it never taints.
func (s *Store) Backlog() (conflicts, unfiled int, err error) {
	if err := s.db.QueryRow(`SELECT count(*) FROM (` + overlapping + `
		AND max(r.sensitivity = 'phi' OR e.sensitivity = 'phi') = 0)`).Scan(&conflicts); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM unfiled_open`).Scan(&unfiled); err != nil {
		return 0, 0, err
	}
	return conflicts, unfiled, nil
}

type Unfiled struct {
	ID          int64
	Reason      string
	Error       string
	Summary     string
	Payload     string
	At          string
	Sensitivity string
}

func (s *Store) OpenUnfiled(limit int) ([]Unfiled, error) {
	if limit <= 0 || limit > 50 {
		limit = defaultLimit
	}
	rows, err := s.db.Query(`SELECT u.id, u.reason, coalesce(u.error, ''), u.summary, u.payload, t.at, u.sensitivity
		FROM unfiled_open u JOIN txns t ON t.id = u.tx_id ORDER BY u.id DESC LIMIT ?`, limit)
	if err != nil {
		taint.Set()
		return nil, err
	}
	defer rows.Close()
	var out []Unfiled
	for rows.Next() {
		var u Unfiled
		if err := rows.Scan(&u.ID, &u.Reason, &u.Error, &u.Summary, &u.Payload, &u.At, &u.Sensitivity); err != nil {
			taint.Set()
			return nil, err
		}
		if u.Sensitivity == "phi" {
			taint.Set()
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// A read that fails partway cannot say what it saw, so it taints.
func (s *Store) rows(q string, args ...any) ([]Row, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		taint.Set()
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Kind, &r.Status, &r.Subject, &r.Title, &r.Text, &r.Body, &r.Attrs,
			&r.ValidFrom, &r.ValidUntil, &r.Supersedes, &r.Confidence, &r.Sensitivity, &r.Sources, &r.Slot, &r.Session, &r.Episode); err != nil {
			taint.Set()
			return nil, err
		}
		if r.Sensitivity == "phi" {
			taint.Set()
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		taint.Set()
		return nil, err
	}
	return out, nil
}

func (s *Store) strings(q string, args ...any) ([]string, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a about after all also an and any are as at be been before but by can could did do does
		for from had has have how if in into is it its may might more most must no not of on or our over should so such
		than that the their them then there these they this those through to under up was we were what when where which
		while who whom why will with would you your`) {
		stopwords[w] = true
	}
}

// Quoted so FTS5 syntax in a question is read as words; stopwords are dropped unless nothing else is left.
func ftsTerms(query string) []string {
	var terms, stops []string
	for _, w := range strings.FieldsFunc(query, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r > 127)
	}) {
		if len(w) < 2 {
			continue
		}
		q := `"` + w + `"`
		if stopwords[strings.ToLower(w)] {
			stops = append(stops, q)
		} else if !slices.Contains(terms, q) {
			terms = append(terms, q)
		}
	}
	if len(terms) == 0 {
		return stops
	}
	return terms
}

// AttrsLine renders attrs compactly, with extra flattened so a reader sees every field.
func AttrsLine(attrs string) string {
	var m map[string]any
	if json.Unmarshal([]byte(attrs), &m) != nil || len(m) == 0 {
		return ""
	}
	if extra, ok := m["extra"].(map[string]any); ok {
		delete(m, "extra")
		for k, v := range extra {
			m["extra."+k] = v
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}
