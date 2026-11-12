// Package records is the authoritative knowledge store; every refused write is kept in unfiled.
package records

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexbieg/alux/internal/paths"
	"modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// steps[i] takes the store from version i+1 to i+2; schema.sql is version 1.
//
//go:embed migrations/002_vault_migration.sql
var step2 string

//go:embed migrations/003_search_and_entity_edits.sql
var step3 string

//go:embed migrations/004_as_of_slots_and_citations.sql
var step4 string

//go:embed migrations/005_merges_and_slots_in_place.sql
var step5 string

//go:embed migrations/006_status_follows_the_chain.sql
var step6 string

//go:embed migrations/007_candidate_verdicts.sql
var step7 string

//go:embed migrations/008_candidate_features.sql
var step8 string

//go:embed migrations/009_candidate_suggestions.sql
var step9 string

//go:embed migrations/010_dropped_records.sql
var step10 string

//go:embed migrations/011_rule_triggers.sql
var step11 string

//go:embed migrations/012_rule_conditions.sql
var step12 string

//go:embed migrations/013_episodes.sql
var step13 string

var steps = []string{step2, step3, step4, step5, step6, step7, step8, step9, step10, step11, step12, step13}

var schemaVersion = 1 + len(steps)

const sqliteConstraint = 19

// On by default; ALUX_RECORDS=0, false or off disables it, and any other non-flag value is a path.
func Path() string {
	v := strings.TrimSpace(os.Getenv("ALUX_RECORDS"))
	switch strings.ToLower(v) {
	case "0", "false", "off", "no":
		return ""
	case "", "1", "true", "on", "yes":
	default:
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.Share, "records.db")
}

type Store struct {
	db        *sql.DB
	links     graphCache
	embed     Embedder
	stopEmbed func()
	vecs      vectorCache
	minSim    float32
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(pairTable); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.stopEmbed != nil {
		s.stopEmbed()
	}
	return s.db.Close()
}

// The version is read inside the write lock, so two processes opening an old store upgrade it once.
func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == schemaVersion {
		return nil
	}
	if v > schemaVersion {
		return fmt.Errorf("records store is at schema %d, this build knows %d", v, schemaVersion)
	}
	if v == 0 {
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("creating records schema: %w", err)
		}
		v = 1
	}
	for ; v < schemaVersion; v++ {
		if _, err := tx.Exec(steps[v-1]); err != nil {
			return fmt.Errorf("migrating records schema %d to %d: %w", v, v+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

type Actor struct {
	Kind    string // user, agent or import
	Agent   string
	Model   string
	Session string
}

type Source struct {
	Type       string `json:"type"`
	Ref        string `json:"ref"`
	Title      string `json:"title,omitempty"`
	OccurredAt string `json:"occurred_at,omitempty"`
}

type Link struct {
	Role   string `json:"role"`
	Record string `json:"record,omitempty"`
	Entity string `json:"entity,omitempty"`
}

type RecordInput struct {
	Kind        string         `json:"kind"`
	Subject     string         `json:"subject"`
	Title       string         `json:"title"`
	Text        string         `json:"text,omitempty"`
	Body        string         `json:"body,omitempty"`
	Attrs       map[string]any `json:"attrs,omitempty"`
	ValidFrom   string         `json:"valid_from,omitempty"`
	ValidUntil  string         `json:"valid_until,omitempty"`
	Supersedes  IDs            `json:"supersedes,omitempty"`
	Confidence  string         `json:"confidence,omitempty"`
	Sensitivity string         `json:"sensitivity"`
	Source      Source         `json:"source"`
	Links       []Link         `json:"links,omitempty"`
	DropLinks   []Link         `json:"drop_links,omitempty"`
}

// IDs accepts one id as a bare string or a list of them.
type IDs []string

func (ids *IDs) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*ids = nil
		if strings.TrimSpace(one) != "" {
			*ids = IDs{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("supersedes must be a record id or a list of them")
	}
	*ids = many
	return nil
}

// Topical links survive a rewrite; links that assert something about the old content are left for the writer to restate.
var carriedRoles = []string{"about", "see_also", "refines", "depends_on"}

type EntityInput struct {
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Sensitivity string   `json:"sensitivity"`
	Aliases     []string `json:"aliases,omitempty"`
}

type EntityResult struct {
	ID string
	// Similar are existing entities whose names come close, for the writer to merge into if one is the same thing.
	Similar []Candidate
}

type Result struct {
	ID             string
	Notes          []string
	Carried        []string
	Withheld       []string
	Dropped        []string
	UncitedSources []string
	// Similar are existing records the new one may restate or want to link to; nothing links them yet.
	Similar []Candidate
}

// Nothing is written on a rejection; the caller fixes the request or unfiles it deliberately.
type Rejection struct {
	Reason  string
	Message string
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("rejected, nothing was written: %s. Fix the request and try again, or keep it for review with op unfile.", r.Message)
}

type refusal struct{ reason, msg string }

func (r refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error {
	return refusal{"rejected", fmt.Sprintf(format, args...)}
}

func (s *Store) Add(in RecordInput, a Actor) (Result, error) {
	res, err := s.add(in, a)
	if err == nil {
		return res, nil
	}
	reason, msg, ok := asRefusal(err)
	if !ok {
		return Result{}, err
	}
	return Result{}, &Rejection{Reason: reason, Message: msg}
}

func (s *Store) add(in RecordInput, a Actor) (Result, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Title = strings.TrimSpace(in.Title)
	if in.Sensitivity == "" {
		return Result{}, refuse("sensitivity is required on every add and has no default: phi for sensitive personal data (health or patient details, for example), normal otherwise")
	}
	if in.Sensitivity != "normal" && in.Sensitivity != "phi" {
		return Result{}, refuse("sensitivity must be normal or phi")
	}
	if in.Source.Type == "" || strings.TrimSpace(in.Source.Ref) == "" {
		return Result{}, refuse("source.type and source.ref are required: say where this came from")
	}
	if err := lengthsFit(in); err != nil {
		return Result{}, err
	}
	var predecessors []string
	for _, p := range in.Supersedes {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(predecessors, p) {
			predecessors = append(predecessors, p)
		}
	}
	if len(in.DropLinks) > 0 && len(predecessors) == 0 {
		return Result{}, refuse("drop_links only applies when superseding a record")
	}
	draftVec := s.draftVector(in.Title, in.Text)

	tx, err := s.db.Begin()
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()

	var known bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM kind_fields WHERE kind = ?)`, in.Kind).Scan(&known); err != nil {
		return Result{}, err
	}
	if !known {
		return Result{}, refuse("unknown kind %q: use one of claim, state, decision, measurement, todo, question, procedure, rule, argument", in.Kind)
	}

	subject, err := resolveEntity(tx, in.Subject)
	if err != nil {
		return Result{}, err
	}
	if subject == "" {
		return Result{}, refusal{"no_subject", fmt.Sprintf("unknown subject %q: use an existing entity name or alias, or create the entity first", in.Subject)}
	}

	for _, p := range predecessors {
		if err := mustBeCurrent(tx, p, "supersede"); err != nil {
			return Result{}, err
		}
	}

	if in.Kind == "decision" && strings.TrimSpace(in.ValidUntil) != "" && strings.TrimSpace(in.ValidUntil) < time.Now().Format("2006-01-02") {
		return Result{}, refuse("valid_until %s is already past: valid_until is when a decision stopped holding in the world, not when it was replaced. If a later decision replaced it, leave valid_until unset and supersede it, or say in the title what replaced it", in.ValidUntil)
	}

	attrs, notes, err := normalizeAttrs(tx, in.Kind, in.Attrs)
	if err != nil {
		return Result{}, err
	}
	if slot, ok := attrs["slot"].(string); ok {
		slot = strings.ToLower(strings.TrimSpace(slot))
		attrs["slot"] = slot
		if err := slotFree(tx, subject, in.Kind, slot, predecessors); err != nil {
			return Result{}, err
		}
	}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return Result{}, err
	}

	txID, err := insertTxn(tx, a)
	if err != nil {
		return Result{}, err
	}
	srcID, err := upsertSource(tx, in.Source, txID)
	if err != nil {
		return Result{}, err
	}

	predJSON, _ := json.Marshal(predecessors)
	// A replacement is not shown again what its predecessor linked, was judged distinct from, or was already shown.
	settled, err := txStrings(tx, `SELECT to_record FROM links WHERE to_record IS NOT NULL AND from_record IN (SELECT value FROM json_each(?1))
		UNION SELECT candidate_id FROM candidate_verdicts WHERE target = 'record' AND verdict = 'distinct' AND written_id IN (SELECT value FROM json_each(?1))
		UNION SELECT candidate_id FROM candidate_suggestions WHERE target = 'record' AND written_id IN (SELECT value FROM json_each(?1))`, string(predJSON))
	if err != nil {
		return Result{}, err
	}
	skip := append(slices.Clone(predecessors), settled...)
	for _, l := range in.Links {
		if l.Record != "" {
			skip = append(skip, l.Record)
		}
	}
	// Ranked inside the write's transaction, so a sibling writer's record that landed a moment ago is shown too.
	cands, err := findCandidates(tx, subject, in.Title, in.Text, srcID, in.Sensitivity == "phi", skip)
	if err != nil {
		return Result{}, err
	}
	if err := s.logCosine(tx, cands, draftVec); err != nil {
		return Result{}, err
	}

	confidence := in.Confidence
	if confidence == "" {
		confidence = "high"
	}
	// The first predecessor goes in records.supersedes, which a trigger mirrors into supersessions; the rest are inserted below.
	first := ""
	if len(predecessors) > 0 {
		first = predecessors[0]
	}
	id := newID("rec_")
	_, err = tx.Exec(`INSERT INTO records (id, kind, subject_id, title, text, body, attrs, valid_from, valid_until,
		supersedes, confidence, sensitivity, derived_from, tx_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Kind, subject, in.Title, null(in.Text), null(in.Body), string(attrsJSON), null(in.ValidFrom), null(in.ValidUntil),
		null(first), confidence, in.Sensitivity, srcID, txID)
	if err != nil {
		return Result{}, err
	}
	if draftVec != nil {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO record_embeddings (record_id, model, vector) VALUES (?, ?, ?)`,
			id, s.embed.Model(), encodeVector(draftVec)); err != nil {
			return Result{}, err
		}
	}
	for _, p := range predecessors[min(1, len(predecessors)):] {
		if _, err := tx.Exec(`INSERT INTO supersessions (predecessor, successor, tx_id) VALUES (?, ?, ?)`, p, id, txID); err != nil {
			return Result{}, err
		}
	}

	for _, l := range in.Links {
		if _, err := insertLink(tx, id, l, txID, false); err != nil {
			return Result{}, err
		}
	}
	if err := linkEntityAttrs(tx, id, in.Kind, subject, attrs, txID); err != nil {
		return Result{}, err
	}
	res := Result{ID: id, Notes: notes}
	if len(predecessors) > 0 {
		if err := carryLinks(tx, predecessors, id, in.DropLinks, txID, &res); err != nil {
			return Result{}, err
		}
		// A record folded in beyond the first may rest on a source of its own, which the writer should get to re-cite.
		ids, _ := json.Marshal(predecessors)
		if res.UncitedSources, err = txStrings(tx, `SELECT src.type || ': ' || src.ref FROM (
				SELECT rs.source_id AS sid, rs.rowid AS n FROM record_sources rs JOIN json_each(?1) p ON p.value = rs.record_id
				UNION SELECT r.derived_from, -1 FROM records r JOIN json_each(?1) p ON p.value = r.id WHERE p.key > 0) c
			JOIN sources src ON src.id = c.sid WHERE c.sid != ?2 GROUP BY c.sid ORDER BY min(c.n)`, string(ids), srcID); err != nil {
			return Result{}, err
		}
	}
	res.Similar = cands
	if err := logSuggestions(tx, txID, "record", in.Title, id, cands, in.Sensitivity); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return res, nil
}

type priorLink struct {
	role, toRecord, toEntity, head, label, gone string
}

// With several predecessors their links are pooled: a link two of them share is carried once, and a link to one another is not carried at all.
func carryLinks(tx *sql.Tx, from []string, to string, drops []Link, txID string, res *Result) error {
	ids, _ := json.Marshal(from)
	rows, err := tx.Query(`WITH RECURSIVE
			src(id) AS (SELECT value FROM json_each(?1)),
			ahead(start, id, depth) AS (
				SELECT DISTINCT to_record, to_record, 0 FROM links WHERE from_record IN src AND to_record IS NOT NULL
				UNION ALL SELECT a.start, s.successor, a.depth + 1 FROM ahead a JOIN supersessions s ON s.predecessor = a.id)
		SELECT l.role, coalesce(l.to_record, ''), coalesce(l.to_entity, ''),
			coalesce((SELECT a.id FROM ahead a WHERE a.start = l.to_record ORDER BY a.depth DESC LIMIT 1), ''),
			coalesce(e.name, ''),
			coalesce((SELECT ev.event FROM record_events ev WHERE ev.event IN ('retracted', 'dropped') AND ev.record_id =
				(SELECT a.id FROM ahead a WHERE a.start = l.to_record ORDER BY a.depth DESC LIMIT 1)), '')
		FROM links l LEFT JOIN entities e ON e.id = l.to_entity
		WHERE l.from_record IN src AND (l.to_record IS NULL OR l.to_record NOT IN src) ORDER BY l.id`, string(ids))
	if err != nil {
		return err
	}
	var prior []priorLink
	seen := map[string]bool{}
	for rows.Next() {
		var p priorLink
		var name string
		if err := rows.Scan(&p.role, &p.toRecord, &p.toEntity, &p.head, &name, &p.gone); err != nil {
			rows.Close()
			return err
		}
		if p.toRecord != "" {
			p.label = p.role + " → " + p.head
		} else {
			p.label = p.role + " → entity " + name
		}
		if seen[p.label] || p.head == to || slices.Contains(from, p.head) {
			continue
		}
		seen[p.label] = true
		prior = append(prior, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	dropped := make([]bool, len(prior))
	for _, d := range drops {
		entity := ""
		if d.Entity != "" {
			if entity, err = resolveEntity(tx, d.Entity); err != nil {
				return err
			}
		}
		matched := false
		for i, p := range prior {
			if p.role != d.Role {
				continue
			}
			if (d.Record != "" && (d.Record == p.toRecord || d.Record == p.head)) || (entity != "" && entity == p.toEntity) {
				dropped[i], matched = true, true
			}
		}
		if !matched {
			target := d.Record
			if target == "" {
				target = "entity " + d.Entity
			}
			return refuse("drop_links names %s → %s, which %s does not have", d.Role, target, strings.Join(from, " or "))
		}
	}

	for i, p := range prior {
		switch {
		case dropped[i]:
			res.Dropped = append(res.Dropped, p.label)
		case !slices.Contains(carriedRoles, p.role):
			res.Withheld = append(res.Withheld, p.label)
		case p.gone != "":
			res.Withheld = append(res.Withheld, p.label+" ("+p.gone+")")
		default:
			l := Link{Role: p.role, Record: p.head}
			if p.toRecord == "" {
				l = Link{Role: p.role, Entity: p.toEntity}
			}
			if _, err := insertLink(tx, to, l, txID, true); err != nil {
				return err
			}
			res.Carried = append(res.Carried, p.label)
		}
	}
	return nil
}

type LinkResult struct {
	Added         int
	AlreadyLinked int
}

// Links are relationships, not content, so a current record may gain them after it is written.
func (s *Store) Link(from string, links []Link, a Actor) (LinkResult, error) {
	res, err := s.link(from, links, a)
	if err == nil {
		return res, nil
	}
	reason, msg, ok := asRefusal(err)
	if !ok {
		return LinkResult{}, err
	}
	return LinkResult{}, &Rejection{Reason: reason, Message: msg}
}

func (s *Store) link(from string, links []Link, a Actor) (LinkResult, error) {
	if len(links) == 0 {
		return LinkResult{}, refuse("give at least one link")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return LinkResult{}, err
	}
	defer tx.Rollback()
	if err := mustBeCurrent(tx, from, "link from"); err != nil {
		return LinkResult{}, err
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return LinkResult{}, err
	}
	var res LinkResult
	for _, l := range links {
		if l.Record != "" {
			if err := mustBeCurrent(tx, l.Record, "link to"); err != nil {
				return LinkResult{}, err
			}
		}
		added, err := insertLink(tx, from, l, txID, true)
		if err != nil {
			return LinkResult{}, err
		}
		if added {
			res.Added++
		} else {
			res.AlreadyLinked++
		}
	}
	return res, tx.Commit()
}

func mustBeCurrent(tx *sql.Tx, id, verb string) error {
	var exists bool
	var gone string
	var successor sql.NullString
	err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM records WHERE id = ?1),
		coalesce((SELECT event FROM record_events WHERE record_id = ?1 AND event IN ('retracted', 'dropped')), ''),
		(SELECT successor FROM supersessions WHERE predecessor = ?1)`, id).Scan(&exists, &gone, &successor)
	if err != nil {
		return err
	}
	switch {
	case !exists:
		return refuse("no record %q", id)
	case gone != "":
		return refuse("cannot %s %s: it is %s", verb, id, gone)
	case successor.Valid:
		return refuse("cannot %s %s: it was replaced by %s, use that id", verb, id, successor.String)
	}
	return nil
}

// Two current records cannot fill one slot; the writer supersedes the holder, so someone reads what is replaced.
func slotFree(tx *sql.Tx, subject, kind, slot string, except []string) error {
	ids, _ := json.Marshal(append([]string{}, except...))
	// A record already given a successor no longer holds the slot for writing, even while that successor is not yet in effect.
	var holder, title string
	err := tx.QueryRow(`SELECT r.id, CASE WHEN r.sensitivity = 'phi' OR e.sensitivity = 'phi' THEN 'title withheld: phi' ELSE r.title END
		FROM current_records r JOIN entity_head h ON h.id = r.subject_id JOIN entities e ON e.id = h.head
		JOIN record_slot sl ON sl.record_id = r.id
		WHERE h.head = ? AND r.kind = ? AND sl.slot = ? AND r.id NOT IN (SELECT value FROM json_each(?))
		AND NOT EXISTS (SELECT 1 FROM supersessions s WHERE s.predecessor = r.id)
		ORDER BY r.rid LIMIT 1`, subject, kind, slot, string(ids)).Scan(&holder, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return refuse("slot %q already holds current %s %s (%s): set supersedes to %s to replace it, or use another slot if both are true at once", slot, kind, holder, title, holder)
}

// SetSlot files a current state written without a slot into one, keeping its id, links and citations.
func (s *Store) SetSlot(recordID, slot string, a Actor) (string, error) {
	got, err := s.setSlot(recordID, slot, a)
	if err == nil {
		return got, nil
	}
	reason, msg, ok := asRefusal(err)
	if !ok {
		return "", err
	}
	return "", &Rejection{Reason: reason, Message: msg}
}

func (s *Store) setSlot(recordID, slot string, a Actor) (string, error) {
	slot = strings.ToLower(strings.TrimSpace(slot))
	if slot == "" {
		return "", refuse("give the slot name")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := mustBeCurrent(tx, recordID, "set the slot of"); err != nil {
		return "", err
	}
	var kind, subject string
	var existing sql.NullString
	if err := tx.QueryRow(`SELECT r.kind, h.head, sl.slot FROM records r JOIN entity_head h ON h.id = r.subject_id
		JOIN record_slot sl ON sl.record_id = r.id WHERE r.id = ?`, recordID).Scan(&kind, &subject, &existing); err != nil {
		return "", err
	}
	switch {
	case kind != "state":
		return "", refuse("%s is a %s: only a state fills a slot", recordID, kind)
	case existing.Valid:
		return "", refuse("%s already fills slot %q: a slot is set once, so supersede the record to change it", recordID, existing.String)
	}
	if err := slotFree(tx, subject, kind, slot, []string{recordID}); err != nil {
		return "", err
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO record_slots (record_id, slot, tx_id) VALUES (?, ?, ?)`, recordID, slot, txID); err != nil {
		return "", err
	}
	return slot, tx.Commit()
}

type CiteResult struct {
	Added   bool
	Sources int
}

// A further source corroborates a record's content without changing it, so a current record may gain one.
func (s *Store) Cite(recordID string, src Source, a Actor) (CiteResult, error) {
	res, err := s.cite(recordID, src, a)
	if err == nil {
		return res, nil
	}
	reason, msg, ok := asRefusal(err)
	if !ok {
		return CiteResult{}, err
	}
	return CiteResult{}, &Rejection{Reason: reason, Message: msg}
}

func (s *Store) cite(recordID string, src Source, a Actor) (CiteResult, error) {
	if src.Type == "" || strings.TrimSpace(src.Ref) == "" {
		return CiteResult{}, refuse("source.type and source.ref are required: say which source backs the record")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CiteResult{}, err
	}
	defer tx.Rollback()
	if err := mustBeCurrent(tx, recordID, "cite a source for"); err != nil {
		return CiteResult{}, err
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return CiteResult{}, err
	}
	srcID, err := upsertSource(tx, src, txID)
	if err != nil {
		return CiteResult{}, err
	}
	var own bool
	if err := tx.QueryRow(`SELECT derived_from = ? FROM records WHERE id = ?`, srcID, recordID).Scan(&own); err != nil {
		return CiteResult{}, err
	}
	var res CiteResult
	if !own {
		r, err := tx.Exec(`INSERT INTO record_sources (record_id, source_id, tx_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, recordID, srcID, txID)
		if err != nil {
			return CiteResult{}, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return CiteResult{}, err
		}
		res.Added = n > 0
	}
	if err := tx.QueryRow(`SELECT 1 + count(*) FROM record_sources WHERE record_id = ?`, recordID).Scan(&res.Sources); err != nil {
		return CiteResult{}, err
	}
	return res, tx.Commit()
}

func txStrings(tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(q, args...)
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

// A refused event loses no knowledge, so it is returned rather than kept in unfiled.
func (s *Store) Event(recordID, event, reason string, a Actor) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM records WHERE id = ?)`, recordID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no record %q", recordID)
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO record_events (record_id, event, reason, tx_id) VALUES (?, ?, ?, ?)`,
		recordID, event, strings.TrimSpace(reason), txID); err != nil {
		return cleanErr(err)
	}
	return tx.Commit()
}

// Two entities for one thing would split every record about it, so a taken name is refused.
func (s *Store) AddEntity(in EntityInput, a Actor) (EntityResult, error) {
	res, err := s.addEntity(in, a)
	if err == nil {
		return res, nil
	}
	reason, msg, ok := asRefusal(err)
	if !ok {
		return EntityResult{}, err
	}
	return EntityResult{}, &Rejection{Reason: reason, Message: msg}
}

func (s *Store) addEntity(in EntityInput, a Actor) (EntityResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return EntityResult{}, err
	}
	defer tx.Rollback()
	names := append([]string{in.Name}, in.Aliases...)
	for _, n := range names {
		existing, err := resolveEntity(tx, n)
		if err != nil {
			return EntityResult{}, err
		}
		if existing != "" {
			return EntityResult{}, fmt.Errorf("%q already names entity %s: write records about it, or add a different alias", n, existing)
		}
	}
	cands, err := findEntityCandidates(tx, names, in.Sensitivity == "phi")
	if err != nil {
		return EntityResult{}, err
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return EntityResult{}, err
	}
	id := newID("ent_")
	if _, err := tx.Exec(`INSERT INTO entities (id, type, name, description, sensitivity, tx_id) VALUES (?, ?, ?, ?, ?, ?)`,
		id, in.Type, strings.TrimSpace(in.Name), null(in.Description), in.Sensitivity, txID); err != nil {
		return EntityResult{}, cleanErr(err)
	}
	for _, alias := range in.Aliases {
		if _, err := tx.Exec(`INSERT INTO entity_aliases (alias, entity_id, tx_id) VALUES (?, ?, ?)`, strings.TrimSpace(alias), id, txID); err != nil {
			return EntityResult{}, cleanErr(err)
		}
	}
	if err := logSuggestions(tx, txID, "entity", in.Name, id, cands, in.Sensitivity); err != nil {
		return EntityResult{}, err
	}
	return EntityResult{ID: id, Similar: cands}, tx.Commit()
}

type EntityEdit struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	Description string   `json:"description,omitempty"`
	MergeInto   string   `json:"merge_into,omitempty"`
}

type EntityEditResult struct {
	ID           string
	MergedFrom   string
	AddedAliases []string
	KnownAliases []string
	Described    bool
}

// With MergeInto, aliases and description land on the entity merged into, which is the one that remains.
func (s *Store) EditEntity(in EntityEdit, reason string, a Actor) (EntityEditResult, error) {
	res, err := s.editEntity(in, reason, a)
	if err == nil {
		return res, nil
	}
	r, msg, ok := asRefusal(err)
	if !ok {
		return EntityEditResult{}, err
	}
	return EntityEditResult{}, &Rejection{Reason: r, Message: msg}
}

func (s *Store) editEntity(in EntityEdit, reason string, a Actor) (EntityEditResult, error) {
	in.Description = strings.TrimSpace(in.Description)
	in.MergeInto = strings.TrimSpace(in.MergeInto)
	if len(in.Aliases) == 0 && in.Description == "" && in.MergeInto == "" {
		return EntityEditResult{}, refuse("give aliases to add, a description, or merge_into")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return EntityEditResult{}, err
	}
	defer tx.Rollback()
	id, err := resolveEntity(tx, in.Name)
	if err != nil {
		return EntityEditResult{}, err
	}
	if id == "" {
		return EntityEditResult{}, refusal{"no_subject", fmt.Sprintf("unknown entity %q", in.Name)}
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return EntityEditResult{}, err
	}
	res := EntityEditResult{ID: id}

	if in.MergeInto != "" {
		into, err := resolveEntity(tx, in.MergeInto)
		if err != nil {
			return EntityEditResult{}, err
		}
		switch {
		case into == "":
			return EntityEditResult{}, refusal{"no_subject", fmt.Sprintf("unknown entity %q to merge into", in.MergeInto)}
		case into == id:
			return EntityEditResult{}, refuse("%q and %q are already the same entity", in.Name, in.MergeInto)
		case strings.TrimSpace(reason) == "":
			return EntityEditResult{}, refuse("a merge is final: give a reason")
		}
		if _, err := tx.Exec(`INSERT INTO entity_merges (from_entity, into_entity, reason, tx_id) VALUES (?, ?, ?, ?)`,
			id, into, strings.TrimSpace(reason), txID); err != nil {
			return EntityEditResult{}, err
		}
		res.MergedFrom, res.ID, id = id, into, into
	}

	for _, alias := range in.Aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		existing, err := resolveEntity(tx, alias)
		if err != nil {
			return EntityEditResult{}, err
		}
		switch existing {
		case id:
			res.KnownAliases = append(res.KnownAliases, alias)
			continue
		case "":
		default:
			return EntityEditResult{}, refuse("%q already names entity %s: merge the two instead", alias, existing)
		}
		if _, err := tx.Exec(`INSERT INTO entity_aliases (alias, entity_id, tx_id) VALUES (?, ?, ?)`, alias, id, txID); err != nil {
			return EntityEditResult{}, err
		}
		res.AddedAliases = append(res.AddedAliases, alias)
	}

	if in.Description != "" {
		if _, err := tx.Exec(`UPDATE entities SET description = ? WHERE id = ?`, in.Description, id); err != nil {
			return EntityEditResult{}, err
		}
		res.Described = true
	}
	return res, tx.Commit()
}

// The payload is unvalidated, so an unclear sensitivity is kept as phi.
func (s *Store) Unfile(reason, errText string, payload []byte, summary, sensitivity string, a Actor) (int64, error) {
	if !json.Valid(payload) {
		payload, _ = json.Marshal(string(payload))
	}
	if sensitivity != "normal" {
		sensitivity = "phi"
	}
	summary = strings.TrimSpace(summary)
	if len(summary) < 3 {
		summary = "unsummarised write"
	}
	if len(summary) > 300 {
		summary = summary[:297] + "..."
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	txID, err := insertTxn(tx, a)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO unfiled (reason, error, payload, summary, sensitivity, tx_id) VALUES (?, ?, ?, ?, ?, ?)`,
		reason, null(errText), string(payload), summary, sensitivity, txID)
	if err != nil {
		return 0, cleanErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// A resolution is final, so it must say where the entry went or why it was closed.
func (s *Store) ResolveUnfiled(id int64, outcome, recordID, note string, a Actor) error {
	recordID, note = strings.TrimSpace(recordID), strings.TrimSpace(note)
	switch outcome {
	case "filed":
		if recordID == "" {
			return errors.New("outcome filed needs the id of the record it became")
		}
	case "discarded", "schema_changed":
		if recordID == "" && note == "" {
			return fmt.Errorf("outcome %s needs a reason", outcome)
		}
	default:
		return fmt.Errorf("unknown outcome %q: use filed, discarded or schema_changed", outcome)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists, resolved bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM unfiled WHERE id = ?1),
		EXISTS (SELECT 1 FROM unfiled_resolutions WHERE unfiled_id = ?1)`, id).Scan(&exists, &resolved); err != nil {
		return err
	}
	switch {
	case !exists:
		return fmt.Errorf("no unfiled entry #%d", id)
	case resolved:
		return fmt.Errorf("unfiled #%d is already resolved", id)
	}
	if recordID != "" {
		var found bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM records WHERE id = ?)`, recordID).Scan(&found); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no record %q", recordID)
		}
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO unfiled_resolutions (unfiled_id, outcome, record_id, note, tx_id) VALUES (?, ?, ?, ?, ?)`,
		id, outcome, null(recordID), null(note), txID); err != nil {
		return cleanErr(err)
	}
	return tx.Commit()
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func resolveEntity(q querier, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	var id string
	err := q.QueryRow(`SELECT h.head FROM (SELECT id FROM entities WHERE name = ?1 OR id = ?1
		UNION ALL SELECT entity_id FROM entity_aliases WHERE alias = ?1 LIMIT 1) f JOIN entity_head h ON h.id = f.id`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

type field struct {
	typ      string
	required bool
	enum     []string
}

// Unknown fields go under extra, so recurring ones can be promoted into kind_fields.
func normalizeAttrs(tx *sql.Tx, kind string, in map[string]any) (map[string]any, []string, error) {
	rows, err := tx.Query(`SELECT field, type, required, coalesce(enum_values, '') FROM kind_fields WHERE kind = ?`, kind)
	if err != nil {
		return nil, nil, err
	}
	fields := map[string]field{}
	for rows.Next() {
		var name, typ, enum string
		var f field
		if err := rows.Scan(&name, &typ, &f.required, &enum); err != nil {
			rows.Close()
			return nil, nil, err
		}
		f.typ = typ
		if enum != "" {
			_ = json.Unmarshal([]byte(enum), &f.enum)
		}
		fields[name] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	out := map[string]any{}
	extra := map[string]any{}
	for k, v := range in {
		f, ok := fields[k]
		if !ok {
			extra[k] = v
			continue
		}
		nv, err := coerce(tx, k, f, v)
		if err != nil {
			return nil, nil, err
		}
		out[k] = nv
		if f.typ == "condition" && in["mode"] != "hold" {
			return nil, nil, refuse(`attrs.%s applies only to a rule with mode "hold": an advise rule is never narrowed`, k)
		}
	}
	var missing []string
	for name, f := range fields {
		if _, ok := out[name]; f.required && !ok {
			missing = append(missing, "attrs."+name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, nil, refuse("a %s requires %s", kind, strings.Join(missing, ", "))
	}
	var notes []string
	if len(extra) > 0 {
		out["extra"] = extra
		keys := make([]string, 0, len(extra))
		for k := range extra {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		notes = append(notes, fmt.Sprintf("attrs not defined for a %s were kept under extra: %s", kind, strings.Join(keys, ", ")))
	}
	return out, notes, nil
}

func coerce(tx *sql.Tx, name string, f field, v any) (any, error) {
	switch f.typ {
	case "number":
		if n, ok := v.(float64); ok {
			return n, nil
		}
		return nil, refuse("attrs.%s must be a number", name)
	case "list":
		if l, ok := v.([]any); ok {
			return l, nil
		}
		return nil, refuse("attrs.%s must be a list", name)
	case "trigger":
		return coerceTrigger(tx, name, v)
	case "condition":
		return coerceCondition(name, v)
	}
	str, ok := v.(string)
	if !ok || strings.TrimSpace(str) == "" {
		return nil, refuse("attrs.%s must be a non-empty string", name)
	}
	switch f.typ {
	case "date":
		if len(str) < 10 {
			return nil, refuse("attrs.%s must be a date, YYYY-MM-DD", name)
		}
		if _, err := time.Parse("2006-01-02", str[:10]); err != nil {
			return nil, refuse("attrs.%s must be a date, YYYY-MM-DD", name)
		}
	case "enum":
		if !slices.Contains(f.enum, str) {
			return nil, refuse("attrs.%s must be one of %s", name, strings.Join(f.enum, ", "))
		}
	case "entity":
		id, err := resolveEntity(tx, str)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, refuse("attrs.%s names unknown entity %q", name, str)
		}
		return id, nil
	}
	return str, nil
}

func insertTxn(tx *sql.Tx, a Actor) (string, error) {
	kind := a.Kind
	if kind == "" {
		kind = "agent"
	}
	id := newID("tx_")
	_, err := tx.Exec(`INSERT INTO txns (id, actor, agent, model, session) VALUES (?, ?, ?, ?, ?)`,
		id, kind, null(a.Agent), null(a.Model), null(a.Session))
	return id, err
}

func upsertSource(tx *sql.Tx, src Source, txID string) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT id FROM sources WHERE type = ? AND ref = ?`, src.Type, src.Ref).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = newID("src_")
	_, err = tx.Exec(`INSERT INTO sources (id, type, ref, title, occurred_at, tx_id) VALUES (?, ?, ?, ?, ?, ?)`,
		id, src.Type, src.Ref, null(src.Title), null(src.OccurredAt), txID)
	return id, err
}

func insertLink(tx *sql.Tx, from string, l Link, txID string, ignoreDuplicate bool) (bool, error) {
	if (l.Record == "") == (l.Entity == "") {
		return false, refuse("each link names exactly one of record or entity")
	}
	var toRecord, toEntity any
	if l.Record != "" {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM records WHERE id = ?)`, l.Record).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, refuse("link to unknown record %q", l.Record)
		}
		toRecord = l.Record
	} else {
		id, err := resolveEntity(tx, l.Entity)
		if err != nil {
			return false, err
		}
		if id == "" {
			return false, refuse("link to unknown entity %q", l.Entity)
		}
		toEntity = id
	}
	conflict := ""
	if ignoreDuplicate {
		conflict = " ON CONFLICT DO NOTHING"
	}
	res, err := tx.Exec(`INSERT INTO links (from_record, to_record, to_entity, role, tx_id) VALUES (?, ?, ?, ?, ?)`+conflict,
		from, toRecord, toEntity, l.Role, txID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// An entity named in attrs, such as decided_by or owner, gets an about link so the record shows up under it.
func linkEntityAttrs(tx *sql.Tx, from, kind, subject string, attrs map[string]any, txID string) error {
	fields, err := tx.Query(`SELECT field FROM kind_fields WHERE kind = ? AND type = 'entity' ORDER BY field`, kind)
	if err != nil {
		return err
	}
	var ids []string
	for fields.Next() {
		var name string
		if err := fields.Scan(&name); err != nil {
			fields.Close()
			return err
		}
		if id, ok := attrs[name].(string); ok && id != subject {
			ids = append(ids, id)
		}
	}
	fields.Close()
	if err := fields.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO links (from_record, to_entity, role, tx_id) VALUES (?, ?, 'about', ?)`,
			from, id, txID); err != nil {
			return err
		}
	}
	return nil
}

const (
	titleMin = 3
	titleMax = 200
	textMax  = 600
)

// Counts characters as SQLite's length() does, so the refusal matches the schema's CHECK.
func lengthsFit(in RecordInput) error {
	var over []string
	if n := utf8.RuneCountInString(in.Title); n < titleMin {
		over = append(over, fmt.Sprintf("title is %d characters, at least %d are needed", n, titleMin))
	} else if n > titleMax {
		over = append(over, fmt.Sprintf("title is %d characters, %d over the limit of %d", n, n-titleMax, titleMax))
	}
	if strings.TrimSpace(in.Text) != "" {
		if n := utf8.RuneCountInString(in.Text); n > textMax {
			over = append(over, fmt.Sprintf("text is %d characters, %d over the limit of %d: move detail into linked records, or into an argument or procedure body", n, n-textMax, textMax))
		}
	}
	if len(over) == 0 {
		return nil
	}
	return refuse("%s", strings.Join(over, "; "))
}

// A schema constraint is a refusal; any other database error is not.
func asRefusal(err error) (reason, msg string, ok bool) {
	var r refusal
	if errors.As(err, &r) {
		return r.reason, r.msg, true
	}
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqliteConstraint {
		return "rejected", cleanErr(err).Error(), true
	}
	return "", "", false
}

// Drops the driver's code suffix so a model reads the schema's own words.
func cleanErr(err error) error {
	msg := err.Error()
	if i := strings.LastIndex(msg, " ("); i > 0 && strings.HasSuffix(msg, ")") {
		msg = msg[:i]
	}
	msg = strings.TrimPrefix(msg, "constraint failed: ")
	return errors.New(msg)
}

func null(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
