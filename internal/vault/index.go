package vault

// A SQLite full-text index over the vault, addressed by SECTION rather than by note.
//
// The markdown on disk stays the source of truth; this file is derived and disposable, and
// deleting it costs one rebuild. It exists because the regex search in vault.go answers five of
// seven realistic session questions with nothing at all — "why did we drop the nightly export"
// finds no line holding those words together — while the same words ORed as FTS terms rank the
// right note first. Retrieval is per section because reading the top hit then costs a 623-byte
// median unit instead of a 4 KB note.
//
// Search may not fail loudly: it returns an error so vault.go's Search can fall back to the
// ripgrep path rather than lose search altogether. ReadSection is the exception — there is no
// fallback for "that section of that note", so its errors are written for the model that reads
// them, and must never claim more than the index knows.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/paths"
	_ "modernc.org/sqlite"
)

// Eighteen tokens of the body around the match: wide enough to read as a sentence, narrow enough
// that a page of hits stays smaller than one note.
//
// Column 1 is the body, and must stay the body: a snippet taken from the alias column would show
// the reader a machine paraphrase in the place where they expect the note's own words, and they
// would quote it back as if the note said it.
const snippetExpr = `snippet(sections_fts, 1, '«', '»', ' … ', 18)`

// The heading is weighted five times the body in the ranking, because a section whose heading
// names the query is almost always the section that answers it.
//
// Aliases sit at 2.5 — above body prose, below a heading. Above, because an alias is written to
// be matched: the generator is told to avoid the words already in the text, so a term that hits
// an alias and nothing else is evidence about what the section is for, where the same term in
// the body may be a passing mention. Below, and below by more than it is above the body, because
// aliases are generated and a wrong one is invisible: a heading that literally names the subject
// must still win against any paraphrase of another section. 2.5 also keeps one alias term worth
// less than a heading term on its own, so no amount of alias matching can displace the section
// the user actually named. See TestAnAliasOutranksABodyMatchAndLosesToAHeading, which pins the
// ordering this number is for.
const bm25Rank = `bm25(sections_fts, 5.0, 1.0, 2.5)`

// mtime and size together are the staleness key: mtime granularity is coarse on some
// filesystems, and an external sync can rewrite notes in the vault with no lock, so a second write
// inside the same timestamp is a real case rather than a theoretical one.
const indexSchema = `
create table if not exists notes (
  id     integer primary key,
  path   text not null unique,
  mtime  integer not null,
  size   integer not null,
  -- The only frontmatter kept, and kept here rather than in the text: the block itself is
  -- stripped before indexing (see stripFrontmatter). status is read by Search.
  status text not null default '',
  type   text not null default '',
  -- A preference note's instruction, one imperative sentence. A column and not a section: it must stay out of sections_fts, because a rule indexed
  -- as text is a second body for the note, written in the very words a question about the user's
  -- preferences uses.
  rule   text not null default '',
  -- The note's name as a wikilink is written, which is what links.target_key joins against. See
  -- links.go for why resolution is a join rather than a path stored on the link row.
  key    text not null default '',
  -- The authoritative supersession edge, and authoritative because it is authored: written only
  -- from this note's own frontmatter and never by the heuristic detector. 19 of 112 decisions in
  -- this vault are named as superseded by another note while still carrying status: active, so
  -- this is the column that decides what is current.
  superseded_by text not null default '',
  -- A validity window the note states about itself, as written. Dates are the one thing a model
  -- cannot work out from prose, which is the whole argument for structuring this tier and nothing
  -- else.
  valid_from  text not null default '',
  valid_until text not null default '',
  -- The note's own sensitivity marking, as written. A column, because it gates what may leave
  -- this machine: a ranked search has to say whether any note behind its snippets is marked
  -- before the answer is handed on, and opening six notes to find out would cost more than the
  -- search did.
  sensitivity text not null default '',
  -- The note's tags, space-separated with a space at each end so one tag matches by instr.
  tags text not null default ''
);

create table if not exists sections (
  id      integer primary key,
  note_id integer not null references notes(id) on delete cascade,
  seq     integer not null,
  heading text not null,
  body    text not null,
  -- The section's search aliases, one per line, copied in from the persistent store on every
  -- refresh (see aliases.go). A column here rather than a join because sections_fts is an
  -- external-content table: FTS5 reads a column's text back out of this table by rowid, so a
  -- searchable column that does not exist here cannot be snippeted or retired. The money is in
  -- the store, not in this copy, which is why dropping the whole index stays cheap.
  aliases text not null default '',
  -- 0 for a section deliberately withheld from sections_fts (see isLinkTrailer). It records what
  -- was indexed rather than leaving dropSections to re-derive the judgement: retiring an FTS row
  -- that was never inserted makes FTS5 report the whole database as malformed.
  searchable integer not null default 1,
  unique (note_id, seq)
);

create index if not exists sections_note on sections(note_id);

-- One row, naming the vault these tables describe. See current().
create table if not exists meta (
  vault text not null
);

create virtual table if not exists sections_fts using fts5(
  heading, body, aliases,
  content='sections', content_rowid='id',
  tokenize='porter unicode61'
);
` + linksSchema

// Hit is one section that matched, in the order the ranking put it. Sensitive carries the note's
// marking, because a snippet of a marked note is as sensitive as the note.
type Hit struct {
	Path      string
	Heading   string
	Snippet   string
	Sensitive bool
}

// Stats is what one Refresh did. An unchanged vault produces zeros and writes nothing.
// Unreadable counts notes recorded with no sections because their text could not be read.
type Stats struct {
	Indexed    int
	Reindexed  int
	Deleted    int
	Unreadable int
}

// Index is the derived section index over one vault.
type Index struct {
	db    *sql.DB
	vault string
}

// indexPath keeps the database in the cache directory, never inside the vault: the vault is the
// user's Obsidian folder, and a stray .db there shows up in their file tree, their search and
// their sync. ~/.cache is also where the write lock lives, so there is one place to delete.
func indexPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, paths.Cache, "vault-index.db")
}

// OpenIndex opens the index over vault, creating the file and its schema on first use.
//
// WAL plus busy_timeout is the combination measured at zero failures under eight concurrent
// writers: several alux sessions share this one file, and without them the second writer gets
// SQLITE_BUSY immediately instead of waiting out the first.
func OpenIndex(vault string) (*Index, error) {
	if vault == "" {
		return nil, fmt.Errorf("no vault configured")
	}
	path := indexPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// _txlock=immediate takes the write lock when a transaction begins rather than when it first
	// writes. Refresh reads the stored state and then writes based on it, and under the default
	// deferred locking two sessions could both read "note X is missing" and then both insert it,
	// which fails the unique index on path. Measured: six concurrent processes hit that within
	// 40 ms of a cold start.
	db, err := sql.Open("sqlite", path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// Switching the journal to WAL needs a brief exclusive lock, and several sessions starting at
	// once contend for it before busy_timeout is in force on their connections. The loser sees
	// SQLITE_BUSY on the very first statement, so this retries rather than reporting a broken
	// index on what is a transient collision at startup.
	//
	// Twenty attempts with a 10 ms step wait 2.1 s in total, which is the right order of
	// magnitude for the same reason busy_timeout is 5 s: the collision lasts as long as one
	// session's WAL switch or schema rebuild — milliseconds, 200 ms for a full rebuild — so
	// seconds of patience covers a pile-up of them, while waiting longer would mean a session
	// hanging on a lock that is never coming free.
	for attempt := range 20 {
		if err = prepare(db, vault); err == nil {
			return &Index{db: db, vault: vault}, nil
		}
		if !busy(err) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	db.Close()
	return nil, fmt.Errorf("vault index at %s: %w", path, err)
}

// ErrQueryUnusable marks a query the index cannot act on, as opposed to an index that cannot
// answer. The two must not be confused: a caller that treats them alike tells the reader a
// healthy index is broken, and offers to delete it.
var ErrQueryUnusable = errors.New("query has no searchable terms")

// schemaVersion is stamped into the database and must be incremented by anyone changing
// indexSchema, the FTS tokenizer or the supersession detector — the last two alter what the rows
// say without altering a column, and Refresh only rewrites notes whose bytes changed.
const schemaVersion = 9

// prepare creates the schema, rebuilding from scratch when the file on disk was written by a
// different version of this code.
//
// Every statement in indexSchema is CREATE ... IF NOT EXISTS, so without this a changed schema
// would silently do nothing to an existing index: the session would keep querying last week's
// shape and fail on a missing column, or worse, succeed against a stale one. The index is
// derived state and rebuilds from the markdown in about 200 ms, so throwing it away is the
// cheap and correct answer rather than writing migrations for a cache.
func prepare(db *sql.DB, vault string) error {
	// Read the stamps outside any transaction, and do nothing at all when they match. Beginning
	// a transaction here takes the write lock immediately (see _txlock=immediate), and the
	// package-level calls open the index once per tool call — so stamping a value that is
	// already correct would serialise every session's every search behind every other one,
	// before the lock-free staleness check gets the chance to decide there is no write to do.
	if current(db, vault) {
		return nil
	}

	// One transaction, and _txlock=immediate makes it exclusive from the start: two sessions
	// upgrading at once must not both drop the tables, and the loser needs to see the winner's
	// finished work rather than a half-built schema.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Re-read under the lock. The check above is unsynchronised, so two sessions can both arrive
	// here on the same stale stamp; the loser must see that the winner already rebuilt, rather
	// than dropping tables the winner has since filled.
	if current(tx, vault) {
		return nil
	}
	if err := recreate(tx, vault); err != nil {
		return err
	}
	return tx.Commit()
}

// current reports whether this database was built by this code, over this vault.
//
// The schema half is what makes a schema change safe to ship. The vault half is what stops one
// index file being shared between two different vaults: the path is keyed to the cache
// directory, not to the vault, so pointing alux at a second vault would otherwise leave
// every note of the first looking deleted and every note of the second looking new — a
// correct-but-total rebuild on every single alternating search.
func current(q querier, vault string) bool {
	var version int
	if q.QueryRow(`pragma user_version`).Scan(&version) != nil || version != schemaVersion {
		return false
	}
	// Reading meta is the same question as "does meta exist": on a database from before this
	// stamp, or a fresh file, the query errors and the answer is no either way.
	var indexed string
	return q.QueryRow(`select vault from meta`).Scan(&indexed) == nil && indexed == vault
}

// recreate drops the index's tables and rebuilds them empty, stamped with the current version
// and vault. Shared by the schema upgrade and by an explicit rebuild, so the two cannot drift
// into disagreeing about what a fresh index looks like.
func recreate(tx *sql.Tx, vault string) error {
	for _, stmt := range []string{
		`drop table if exists sections_fts`,
		`drop table if exists sections`,
		`drop table if exists links`,
		`drop table if exists supersessions`,
		`drop table if exists notes`,
		`drop table if exists meta`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(indexSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(`insert into meta (vault) values (?)`, vault); err != nil {
		return err
	}
	// Not parameterizable: pragma takes a literal, and schemaVersion is a constant in this file.
	_, err := tx.Exec(fmt.Sprintf(`pragma user_version = %d`, schemaVersion))
	return err
}

// busy reports whether an error is SQLite's "try again later" rather than a real fault. Matched
// on the message because the driver's own error type is not exported in a usable form.
func busy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

func (x *Index) Close() error { return x.db.Close() }

type indexedNote struct {
	id    int64
	mtime int64
	size  int64
}

// Refresh brings the index up to date with the files on disk: new and changed notes are
// reindexed, vanished ones are dropped, and everything else is left alone. A full build of the
// real vault is under a second; an unchanged pass writes nothing and takes milliseconds.
//
// A walk that could not read part of the vault still indexes what it saw, but makes no
// deletions and returns the walk's error with the stats: the pass was partial, and the caller
// has to be able to know that.
func (x *Index) Refresh() (Stats, error) {
	var st Stats

	// Nothing-to-do is overwhelmingly the common case — a session searches far more often than
	// the vault changes — and taking the write lock to discover that would serialise every
	// session's every search behind every other one. Measured at 40 ms a search with fifteen
	// workers against the real vault, against 4 ms alone. So the cheap check comes first, and
	// the write lock is taken only when there is a write to do.
	found, walkErr := notePathsChecked(x.vault)
	dirty, stored, err := x.stale(found)
	if err != nil {
		return st, err
	}
	if !dirty {
		return st, walkErr
	}
	// A walk that finds nothing where the index holds notes is almost never a mass deletion —
	// it is a vault that moved, an unmounted disk, or a sync mid-rename. Treating it as one
	// would empty the index and leave the session silently answering "no matches" about
	// everything. Refuse, and keep serving what is already indexed.
	if len(found) == 0 && len(stored) > 0 {
		return st, fmt.Errorf("vault at %s looks empty but %d notes are indexed; refusing to drop them", x.vault, len(stored))
	}

	// Read before the write lock is taken, and never generated: this is the whole of what the
	// search path knows about aliases. A store that cannot be read yields nil, and every section
	// is then indexed exactly as it was before aliases existed.
	aliases := aliasesByHash()

	tx, err := x.db.Begin()
	if err != nil {
		return st, err
	}
	defer tx.Rollback()

	// Read inside the transaction, not before it. The write lock is already held (see
	// _txlock=immediate), so what this sees cannot change before the writes below land — which
	// is what stops two sessions both deciding the same note is missing and both inserting it.
	stored, err = storedNotes(tx)
	if err != nil {
		return st, err
	}

	seen := make(map[string]bool, len(stored))
	for _, rel := range found {
		info, err := os.Stat(filepath.Join(x.vault, rel))
		if err != nil {
			// An external sync can write the vault without a lock, so a note can vanish between the
			// walk and the stat. Skipping leaves its row to be deleted on the next pass.
			continue
		}
		seen[rel] = true
		prev, known := stored[rel]
		if known && prev.mtime == info.ModTime().UnixNano() && prev.size == info.Size() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(x.vault, rel))
		if err != nil {
			// A note with no row is what stale() calls dirty, so skipping one unreadable note
			// would make every later search open the write transaction, find nothing to do and
			// commit empty — the serialisation the staleness check exists to avoid. Record the
			// attempt instead, with no sections: the staleness key then matches on the next pass
			// and the note is retried only once it actually changes.
			if known {
				if err := dropNoteRows(tx, prev.id); err != nil {
					return st, err
				}
			}
			if _, err := writeNote(tx, rel, info, noteMeta{}, prev.id, known); err != nil {
				return st, err
			}
			st.Unreadable++
			continue
		}
		if known {
			if err := dropNoteRows(tx, prev.id); err != nil {
				return st, err
			}
		}
		body := stripFrontmatter(string(data))
		meta := parseFrontmatter(string(data))
		id, err := writeNote(tx, rel, info, meta, prev.id, known)
		if err != nil {
			return st, err
		}
		if err := writeSections(tx, id, body, aliases); err != nil {
			return st, err
		}
		if err := writeLinks(tx, id, rel, meta, body); err != nil {
			return st, err
		}
		if known {
			st.Reindexed++
		} else {
			st.Indexed++
		}
	}

	// Only a walk that saw the whole vault may delete. A directory that could not be read
	// contributes no paths, so its notes look exactly like deleted ones — and dropping their
	// rows would leave the session answering "no matches" about a folder that is still there.
	if walkErr == nil {
		for rel, prev := range stored {
			if seen[rel] {
				continue
			}
			if err := dropNoteRows(tx, prev.id); err != nil {
				return st, err
			}
			if _, err := tx.Exec(`delete from notes where id = ?`, prev.id); err != nil {
				return st, err
			}
			st.Deleted++
		}
	}

	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, walkErr
}

// stale reports whether disk and index disagree, without taking the write lock. A false
// positive only costs a transaction that finds less to do than expected; a false negative would
// serve a stale answer, so it errs towards saying yes.
func (x *Index) stale(found []string) (bool, map[string]indexedNote, error) {
	stored, err := storedNotes(x.db)
	if err != nil {
		return false, nil, err
	}
	seen := 0
	for _, rel := range found {
		info, err := os.Stat(filepath.Join(x.vault, rel))
		if err != nil {
			continue
		}
		seen++
		prev, known := stored[rel]
		if !known || prev.mtime != info.ModTime().UnixNano() || prev.size != info.Size() {
			return true, stored, nil
		}
	}
	// Fewer files on disk than rows in the index means notes were deleted.
	return seen != len(stored), stored, nil
}

// querier is satisfied by both *sql.DB and *sql.Tx, so a read can be made either outside a
// transaction (cheap, unsynchronised) or inside one (authoritative) without duplicating it.
type querier interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func storedNotes(q querier) (map[string]indexedNote, error) {
	rows, err := q.Query(`select id, path, mtime, size from notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stored := map[string]indexedNote{}
	for rows.Next() {
		var path string
		var n indexedNote
		if err := rows.Scan(&n.id, &path, &n.mtime, &n.size); err != nil {
			return nil, err
		}
		stored[path] = n
	}
	return stored, rows.Err()
}

// mtime and size are the file as it sits on disk, which is the staleness key; status, type and
// rule are the note's frontmatter, rewritten on every pass because an edit can change them
// without changing anything the sections show.
func writeNote(tx *sql.Tx, rel string, info os.FileInfo, meta noteMeta, id int64, known bool) (int64, error) {
	if known {
		_, err := tx.Exec(`update notes set mtime=?, size=?, status=?, type=?, rule=?, key=?,
			superseded_by=?, valid_from=?, valid_until=?, sensitivity=?, tags=? where id=?`,
			info.ModTime().UnixNano(), info.Size(), meta.status, meta.kind, meta.rule, noteKey(rel),
			meta.supersededBy, meta.validFrom, meta.validUntil, meta.sensitivity, meta.tags, id)
		return id, err
	}
	res, err := tx.Exec(`insert into notes
		(path, mtime, size, status, type, rule, key, superseded_by, valid_from, valid_until, sensitivity, tags)
		values (?,?,?,?,?,?,?,?,?,?,?,?)`,
		rel, info.ModTime().UnixNano(), info.Size(), meta.status, meta.kind, meta.rule, noteKey(rel),
		meta.supersededBy, meta.validFrom, meta.validUntil, meta.sensitivity, meta.tags)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// aliases is keyed by section content hash and may be nil; a section with no entry is indexed
// with an empty alias column, which is what every section looked like before aliases existed.
func writeSections(tx *sql.Tx, noteID int64, body string, aliases map[string]string) error {
	for _, s := range splitSections(body) {
		searchable := !isLinkTrailer(s.heading)
		// A withheld section is never searched, so looking an alias up for one would only put
		// text in a column nothing reads.
		var alias string
		if searchable {
			alias = aliases[sectionHash(s.heading, s.body)]
		}
		res, err := tx.Exec(`insert into sections (note_id, seq, heading, body, aliases, searchable) values (?,?,?,?,?,?)`,
			noteID, s.seq, s.heading, s.body, alias, searchable)
		if err != nil {
			return err
		}
		if !searchable {
			continue
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`insert into sections_fts(rowid, heading, body, aliases) values (?,?,?,?)`,
			id, s.heading, s.body, alias); err != nil {
			return err
		}
	}
	return nil
}

// dropNoteRows retires everything derived from one note's text: its sections, their full-text
// rows, and its links and supersession claims. Grouped so that a caller cannot drop half of it —
// writeNote updates the notes row in place rather than replacing it, so the foreign key never
// cascades on a reindex and every edit would otherwise add a second copy of every link.
func dropNoteRows(tx *sql.Tx, noteID int64) error {
	if err := dropSections(tx, noteID); err != nil {
		return err
	}
	return dropLinks(tx, noteID)
}

// dropSections removes a note's sections and their index entries.
//
// sections_fts is an external-content table, so it never sees a delete from sections: its rows
// have to be retired explicitly, and with their OLD text, because that is what FTS5 needs to
// unindex the terms. Skip this and a reindexed note keeps answering searches for wording it no
// longer contains.
//
// Only the rows that were indexed may be retired: 'delete' over a rowid the index never held
// reports the database as malformed, which would take the whole index out over a section
// writeSections withheld on purpose.
func dropSections(tx *sql.Tx, noteID int64) error {
	if _, err := tx.Exec(`insert into sections_fts(sections_fts, rowid, heading, body, aliases)
		select 'delete', id, heading, body, aliases from sections where note_id = ? and searchable`, noteID); err != nil {
		return err
	}
	_, err := tx.Exec(`delete from sections where note_id = ?`, noteID)
	return err
}

// refreshForRead updates the index before a read, and decides what a failed update costs. It
// reports whether the read may proceed, and the refresh's own error when there was one.
//
// A refresh that loses a race is the common failure, and it is nearly always harmless: whoever
// won it did the same work. Failing the read would send the caller back to regex search, which
// is the worse answer — so a populated index is searched even when its last refresh failed, at
// the price of results that may be seconds stale. An EMPTY index is different: there is nothing
// to search, and answering "no matches" would be a confident lie, so the read does not proceed
// and the caller falls back.
//
// The error is handed back even when the read proceeds, because "this note is not indexed" and
// "this note is not indexed and the index could not be updated" are different answers, and only
// the caller knows which one it is about to give.
func (x *Index) refreshForRead() (refreshErr error, proceed bool) {
	_, err := x.Refresh()
	if err == nil {
		return nil, true
	}
	var notes int
	if x.db.QueryRow(`select count(*) from notes`).Scan(&notes) == nil && notes > 0 {
		return err, true
	}
	return err, false
}

// Search ranks sections against a natural-language query, refreshing first so a caller never has
// to remember to.
func (x *Index) Search(query string, limit int) ([]Hit, error) {
	if err, proceed := x.refreshForRead(); !proceed {
		return nil, err
	}
	terms := ftsTerms(query)
	if len(terms) == 0 {
		return nil, fmt.Errorf("%w: %q is all stopwords or words shorter than three characters — "+
			"search for the distinctive words of the question", ErrQueryUnusable, query)
	}
	// No default: how many hits a session can afford is a prompt-side judgement, and searchHits
	// in vault.go is the one place that makes it. A second number here would quietly override it.
	if limit <= 0 {
		return nil, fmt.Errorf("search limit must be positive, got %d", limit)
	}

	// Archived notes are filtered rather than left out of the index: they stay readable by path,
	// because a title or a link still has to lead somewhere, and un-archiving one is an ordinary
	// edit that the next refresh picks up. 36 notes in the real vault are archived, and they
	// competed for result slots on equal terms — an archived duplicate measured as the top hit
	// for a query, above the note that superseded it.
	rows, err := x.db.Query(`
		select n.path, s.heading, `+snippetExpr+`, n.sensitivity <> ''
		from sections_fts join sections s on s.id = sections_fts.rowid
		join notes n on n.id = s.note_id
		where sections_fts match ? and n.status <> 'archived'
		order by `+bm25Rank+` limit ?`,
		strings.Join(terms, " OR "), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Path, &h.Heading, &h.Snippet, &h.Sensitive); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ReadSection returns one section's text, refreshing first. An unknown heading is answered with
// the headings the note does have, so the caller can retry without reading the whole note.
func (x *Index) ReadSection(path, heading string) (string, error) {
	refreshErr, proceed := x.refreshForRead()
	if !proceed {
		return "", refreshErr
	}
	var noteID int64
	switch err := x.db.QueryRow(`select id from notes where path = ?`, path).Scan(&noteID); {
	case errors.Is(err, sql.ErrNoRows):
		// Saying "no note at X" after a failed refresh would be a confident falsehood about the
		// source of truth: a note filed since the last good refresh is on disk and simply not in
		// this index yet. Name the failure instead, so the reader knows what to retry.
		if refreshErr != nil {
			return "", fmt.Errorf("%s is not in the section index, and the index could not be "+
				"brought up to date (%v), so the note may exist on disk — read it whole, or retry",
				path, refreshErr)
		}
		return "", fmt.Errorf("no note at %s in the vault", path)
	case err != nil:
		return "", err
	}

	var body string
	// Lowest seq wins: a note may repeat a heading, and answering with the first keeps the same
	// query returning the same text.
	err := x.db.QueryRow(`select body from sections
		where note_id = ? and lower(heading) = lower(?) order by seq limit 1`, noteID, heading).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		headings, herr := x.headings(noteID)
		if herr != nil {
			return "", herr
		}
		return "", fmt.Errorf("%s has no section %q; its headings are: %s",
			path, heading, strings.Join(headings, " | "))
	}
	if err != nil {
		return "", err
	}
	return body, nil
}

// Sensitive reports whether the note at path is marked sensitive in its frontmatter. Any value
// counts: the convention writes one word, and a marking this code does not recognise is still a
// marking its author meant something by.
//
// A note the index cannot find is an error rather than a no, because the caller asking this is
// about to decide whether text may leave the machine.
func (x *Index) Sensitive(path string) (bool, error) {
	if err, proceed := x.refreshForRead(); !proceed {
		return false, err
	}
	var sensitivity string
	switch err := x.db.QueryRow(`select sensitivity from notes where path = ?`, path).Scan(&sensitivity); {
	case errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("no note at %s in the vault", path)
	case err != nil:
		return false, err
	}
	return sensitivity != "", nil
}

func (x *Index) headings(noteID int64) ([]string, error) {
	rows, err := x.db.Query(`select heading from sections where note_id = ? order by seq`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// The package-level calls open the index, answer, and close it again. A session holds no
// connection between tool calls, so an alux left idle for an hour is never the reason another
// one — or the user's own Obsidian — finds the file busy.

func SearchIndex(vault, query string, limit int) ([]Hit, error) {
	x, err := OpenIndex(vault)
	if err != nil {
		return nil, err
	}
	defer x.Close()
	return x.Search(query, limit)
}

func ReadSection(vault, path, heading string) (string, error) {
	x, err := OpenIndex(vault)
	if err != nil {
		return "", err
	}
	defer x.Close()
	return x.ReadSection(path, heading)
}

func Sensitive(vault, path string) (bool, error) {
	x, err := OpenIndex(vault)
	if err != nil {
		return false, err
	}
	defer x.Close()
	return x.Sensitive(path)
}

// ---------------------------------------------------------------- parsing

var (
	frontmatterRe = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n?`)
	headingRe     = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+)$`)
	fenceRe       = regexp.MustCompile("(?m)^```")
)

// Only the body is indexed. Frontmatter is bookkeeping, and letting it into the index makes every
// note sharing a tag or a date rank for it.
func stripFrontmatter(text string) string {
	if m := frontmatterRe.FindString(text); m != "" {
		return strings.TrimPrefix(text, m)
	}
	return text
}

// noteMeta is the frontmatter a note row carries. kind is the `type` key, spelled differently
// here only because type is a Go keyword.
type noteMeta struct {
	status string
	kind   string
	rule   string
	// The temporal tier, and all of it authored: supersededBy is the only thing notes.superseded_by
	// is ever written from, because a detector over this vault was wrong a quarter of the time and
	// this is the column that decides what is current.
	supersededBy   string
	supersededLine string
	validFrom      string
	validUntil     string
	sensitivity    string
	tags           string
}

// plainTarget is a frontmatter value written as a wikilink, read as the note name it names.
func plainTarget(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[[")
	return strings.TrimSpace(strings.TrimSuffix(value, "]]"))
}

// parseFrontmatter reads status, type and rule out of the frontmatter block, by scanning lines
// rather than with a YAML parser. No dependency is worth it for three keys, and the scan is the
// more tolerant reader: one note in this vault defines the same key twice, which a strict parse
// rejects outright and which this resolves to the last value given.
//
// Indented lines are skipped so a `status:` nested under another key cannot be mistaken for the
// note's own.
func parseFrontmatter(text string) noteMeta {
	m := frontmatterRe.FindStringSubmatch(text)
	if m == nil {
		return noteMeta{}
	}
	var meta noteMeta
	for _, line := range strings.Split(m[1], "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '-' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value == "" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "status":
			meta.status = value
		case "type":
			meta.kind = value
		case "rule":
			meta.rule = value
		// Both spellings, because the vault was written by hand and holds both.
		case "superseded_by", "superseded-by":
			meta.supersededBy = plainTarget(value)
			// The line as the note wrote it, which is the evidence for a declared edge: a reader
			// auditing the table sees the statement rather than a paraphrase of it.
			meta.supersededLine = strings.TrimSpace(line)
		case "valid_from", "valid-from":
			meta.validFrom = value
		case "valid_until", "valid-until", "valid_to":
			meta.validUntil = value
		case "sensitivity":
			meta.sensitivity = value
		case "tags":
			meta.tags = parseTags(value)
		}
	}
	return meta
}

// parseTags reads an inline tag list, `[a, b]` or `a, b`, into the column's padded form.
func parseTags(value string) string {
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	var tags []string
	for _, t := range strings.Split(value, ",") {
		if t = strings.Trim(strings.TrimSpace(t), `"'#`); t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return ""
	}
	return " " + strings.Join(tags, " ") + " "
}

// A section headed by nothing but "Related" or "See also" is a link trailer: wikilinked note
// titles, no prose. 497 of the real vault's 4,389 sections are one, and they took 8% of all
// search result slots while answering nothing — they rank for any query that mentions a linked
// note's words, and in measured cases outranked the section that held the answer.
//
// Only the bare heading counts. "Related work" introduces prose, and withholding it would lose
// answers rather than noise.
var linkTrailerHeadings = map[string]bool{"related": true, "see also": true}

func isLinkTrailer(heading string) bool {
	bare := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(heading), "#"))
	return linkTrailerHeadings[strings.ToLower(bare)]
}

type section struct {
	seq     int
	heading string
	body    string
}

// splitSections cuts a body at its ATX headings. The concatenation of the result in seq order
// reproduces the body byte for byte, which is what makes a section safe to hand back as if it
// were the file.
func splitSections(body string) []section {
	masked := maskFences(body)
	idx := headingRe.FindAllStringSubmatchIndex(masked, -1)

	var out []section
	add := func(from, to int, heading string) {
		out = append(out, section{seq: len(out), heading: heading, body: body[from:to]})
	}
	if len(idx) == 0 {
		add(0, len(body), "(whole note)")
		return out
	}
	// Text before the first heading is where a hub note's conclusions actually sit, so it is a
	// section of its own rather than something to drop.
	if idx[0][0] > 0 {
		add(0, idx[0][0], "(preamble)")
	}
	for i, m := range idx {
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		add(m[0], end, strings.TrimSpace(masked[m[4]:m[5]]))
	}
	return out
}

// maskFences blanks fenced-code content while preserving byte offsets, so a "# comment" in a
// shell block is not read as a heading and the offsets still index into the unmasked body.
//
// Known limitation, accepted: fences are paired off in order, so a trailing unterminated fence
// masks nothing and its "# comment" lines are split out as headings. That only happens in a note
// caught mid-write, where the next refresh fixes it; guessing that the fence runs to end of file
// would instead swallow the real headings of every note whose body merely contains an odd number
// of ``` lines, which is the worse and more permanent error.
func maskFences(s string) string {
	marks := fenceRe.FindAllStringIndex(s, -1)
	if len(marks) < 2 {
		return s
	}
	b := []byte(s)
	for i := 0; i+1 < len(marks); i += 2 {
		for j := marks[i][0]; j < marks[i+1][1] && j < len(b); j++ {
			if b[j] != '\n' {
				b[j] = ' '
			}
		}
	}
	return string(b)
}

var stopWords = map[string]bool{"the": true, "was": true, "why": true, "who": true, "for": true,
	"and": true, "what": true, "does": true, "how": true, "are": true}

// ftsTerms turns a question into an OR of quoted terms. This is the whole reason the index beats
// the regex search: the words of a real question never appear adjacent on one line, so a single
// pattern matches nothing while the terms separately match the section that answers.
func ftsTerms(query string) []string {
	var terms []string
	for _, w := range strings.Fields(query) {
		w = strings.Trim(w, `"'?.,/!;:()[]`)
		// An interior quote would end the term early and leave a malformed MATCH expression,
		// which SQLite reports as a syntax error rather than as no matches.
		w = strings.ReplaceAll(w, `"`, "")
		if len(w) > 2 && !stopWords[strings.ToLower(w)] {
			terms = append(terms, `"`+w+`"`)
		}
	}
	return terms
}
