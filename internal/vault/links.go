package vault

// The link graph, and the one tier of this vault worth promoting out of prose into structure.
//
// Three external systems evaluated the same question and converged: the only thing an LLM
// provably cannot do from prose is date arithmetic and event ordering — what is current, what was
// true before, what replaced what. Everything else reads fine as natural language and is left
// there. So this file types exactly two things: who links to whom, and what claims to have
// replaced what.
//
// Two measurements shape the schema. A link in an argument and a link in a `## Related` catalogue
// are not the same fact: one note's 41 inbound links were 39 catalogue entries and 23 prose
// mentions, so a single "inbound links" number means nothing until it is split. And an automated
// supersession extractor over this vault produced 60 edges by taking the first wikilink in any
// sentence containing "supersede"; constrained to the verb's object within 120 characters it gave
// 24, of which roughly three in four survived hand inspection. A quarter wrong is not acceptable
// in the layer that decides what is currently true, which is why nothing detected here is ever
// authoritative — see the source column on supersessions.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// An evidence sentence is for a human to read in a report, not a paragraph to store: the cap is
// what stops one unpunctuated note putting a kilobyte in every row.
const evidenceMax = 400

// How far after the verb its object may sit. The number is inherited from the measurement that
// made the constrained extractor defensible at all; widening it was what produced the 60-edge
// version that named whatever note happened to be mentioned next.
const supersessionWindow = 120

// Roles a link row can carry. The distinction is the point of the table: a link in a bare
// `## Related` trailer was catalogued, and a link anywhere else was being used in an argument.
const (
	roleProse   = "prose"
	roleRelated = "related"
)

// Where a supersession row came from, and the whole reason this is safe to build. `declared` is
// read off the note's own frontmatter and is the only thing notes.superseded_by is ever written
// from; `detected` is the heuristic's guess, carries its evidence sentence so a human can audit
// it, and never reaches that column.
const (
	sourceDeclared = "declared"
	sourceDetected = "detected"
)

// Targets are resolved by a join on notes.key rather than by a path written into the link row.
// Refresh only rewrites notes whose bytes changed, so a path resolved at index time goes stale
// the moment an unrelated note is created or renamed — and a link recorded as unresolved would
// stay unresolved forever, because nothing about its own note changed.
//
// Keying on the basename is Obsidian's own rule and carries Obsidian's own ambiguity: two notes
// sharing a filename both match, and `[[Archive/Scheduler]]` matches Systems/Scheduler.md. Where
// one path has to be named, the lowest in path order wins, so a rebuild reports the same thing
// twice.
const linksSchema = `
create table if not exists links (
  id      integer primary key,
  note_id integer not null references notes(id) on delete cascade,
  -- The target as the note writes it, minus any |alias and #heading: that is what the migration
  -- tool compares against the trailer it is about to delete, so it must stay verbatim.
  target     text not null,
  target_key text not null,
  -- The #heading part, kept because a link to a section is a narrower claim than a link to a note.
  anchor  text not null default '',
  section text not null default '',
  role    text not null check (role in ('prose','related')),
  -- The sentence that justifies the row, and empty for a related link — there is no argument
  -- behind a catalogue entry, which is the whole distinction the role column draws.
  evidence text not null default '',
  -- The catalogue line a related link was listed on, verbatim, and empty for a prose link. The
  -- gloss after the em dash in "- [[Note]] — why it matters" is authored text that the trailer
  -- removal deletes, so it is kept here and checked before anything is deleted.
  context text not null default ''
);

create index if not exists links_note on links(note_id);
create index if not exists links_target on links(target_key);

-- One claim that some note replaces some other note, normalised so that direction cannot be read
-- two ways: frontmatter says "I am superseded by X" and prose says "this supersedes X", and
-- storing both as (note, target) would record half the vault backwards.
create table if not exists supersessions (
  id      integer primary key,
  -- The note that states the claim, which is one of the two endpoints below.
  note_id integer not null references notes(id) on delete cascade,
  superseder_key text not null,
  superseded_key text not null,
  -- The other endpoint as written, for an unresolved target that has no path to report.
  target  text not null,
  source  text not null check (source in ('declared','detected')),
  section text not null default '',
  evidence text not null
);

create index if not exists supersessions_note on supersessions(note_id);
`

// noteKey normalises a vault path to the name links are written with.
func noteKey(rel string) string {
	return linkKey(strings.TrimSuffix(filepath.Base(rel), ".md"))
}

// linkKey normalises a wikilink target the same way, so the two meet in a join.
func linkKey(target string) string {
	target = strings.TrimSpace(target)
	if i := strings.LastIndex(target, "/"); i >= 0 {
		target = target[i+1:]
	}
	if strings.HasSuffix(strings.ToLower(target), ".md") {
		target = target[:len(target)-3]
	}
	return strings.ToLower(strings.TrimSpace(target))
}

// Non-greedy and barred from containing brackets, so an unclosed `[[` cannot swallow the rest of
// the note looking for its partner.
var wikilinkRe = regexp.MustCompile(`\[\[([^\[\]]+?)\]\]`)

// noteLink is one extracted reference, with where it sat so the detector can ask what verb
// governs it.
type noteLink struct {
	target   string
	key      string
	anchor   string
	evidence string
	context  string
	start    int
	end      int
}

// linkedSection is one section's links together with the text they were read out of. The text is
// the fence-masked copy: links inside a fenced block are not links, for the same reason a
// `# comment` in a shell block is not a heading. Counting them took a vault-wide unresolved-link
// figure from 69 down to 58 once fixed.
type linkedSection struct {
	heading string
	masked  string
	role    string
	links   []noteLink
}

// linkedSections reads every link in a note body, grouped by the section that holds it.
func linkedSections(body string) []linkedSection {
	var out []linkedSection
	for _, s := range splitSections(body) {
		group := linkedSection{heading: s.heading, masked: maskFences(s.body), role: roleProse}
		if isLinkTrailer(s.heading) {
			group.role = roleRelated
		}
		for _, m := range wikilinkRe.FindAllStringSubmatchIndex(group.masked, -1) {
			inner := group.masked[m[2]:m[3]]
			// The display alias is what the sentence reads as, not what it points at.
			if i := strings.Index(inner, "|"); i >= 0 {
				inner = inner[:i]
			}
			target, anchor, _ := strings.Cut(inner, "#")
			target = strings.TrimSpace(target)
			// `[[#Heading]]` names a section of this same note, so there is no target note to
			// record and nothing a backlink query could answer with.
			if target == "" {
				continue
			}
			link := noteLink{
				target: target,
				key:    linkKey(target),
				anchor: strings.TrimSpace(anchor),
				start:  m[0],
				end:    m[1],
			}
			// A catalogue entry has no argument behind it, so it carries no evidence. That is not
			// a gap in the data: it is the difference the role column exists to record. What it
			// does carry is the line it was listed on, which is what the trailer removal deletes.
			if group.role == roleProse {
				from, to := sentenceRange(group.masked, m[0], m[1])
				link.evidence = tidyEvidence(group.masked[from:to])
			} else {
				link.context = lineAround(group.masked, m[0])
			}
			group.links = append(group.links, link)
		}
		if len(group.links) > 0 {
			out = append(out, group)
		}
	}
	return out
}

// sentenceRange finds the sentence holding text[start:end], as byte offsets into text.
//
// Bounded by the paragraph first and the sentence second: notes here are hard-wrapped, so a
// newline is ordinary mid-sentence whitespace, while a blank line, a heading or the start of
// another list item is a boundary no sentence crosses. The search deliberately starts outside the
// link, because a target like `[[v1.2 Rollout]]` holds a full stop of its own.
func sentenceRange(text string, start, end int) (int, int) {
	from, to := paragraphRange(text, start, end)
	for i := start - 1; i > from; i-- {
		if isSentenceEnd(text, i) {
			from = i + 1
			break
		}
	}
	for i := end; i < to; i++ {
		if isSentenceEnd(text, i) {
			to = i + 1
			break
		}
	}
	return from, to
}

// isSentenceEnd reports whether text[i] closes a sentence: terminal punctuation followed by
// whitespace or by the end of the text.
func isSentenceEnd(text string, i int) bool {
	switch text[i] {
	case '.', '!', '?':
	default:
		return false
	}
	if i+1 == len(text) {
		return true
	}
	return text[i+1] == ' ' || text[i+1] == '\n' || text[i+1] == '\t'
}

// paragraphRange is the run of lines around text[start:end] that could hold one sentence. A list
// item is its own paragraph: the em-dash gloss on `- [[Note]] — why it matters` belongs to that
// item and to nothing above it.
func paragraphRange(text string, start, end int) (int, int) {
	from := 0
	for _, line := range lineSpans(text) {
		if line[0] > start {
			break
		}
		if line[0] > from && (blankLine(text[line[0]:line[1]]) || opensBlock(text[line[0]:line[1]])) {
			from = line[0]
		}
	}
	to := len(text)
	for _, line := range lineSpans(text) {
		if line[0] < end {
			continue
		}
		if blankLine(text[line[0]:line[1]]) || opensBlock(text[line[0]:line[1]]) {
			to = line[0]
			break
		}
	}
	return from, to
}

// lineSpans is every line's [start, end) offsets, the end excluding the newline.
func lineSpans(text string) [][2]int {
	var out [][2]int
	start := 0
	for i := 0; i <= len(text); i++ {
		if i == len(text) || text[i] == '\n' {
			out = append(out, [2]int{start, i})
			start = i + 1
		}
	}
	return out
}

func blankLine(line string) bool { return strings.TrimSpace(line) == "" }

// lineAround is the whole line holding offset, trimmed. The migration tool reads the same line off
// the file and compares the two, so both sides have to trim the same way.
func lineAround(text string, offset int) string {
	for _, line := range lineSpans(text) {
		if offset >= line[0] && offset <= line[1] {
			return strings.TrimSpace(text[line[0]:line[1]])
		}
	}
	return ""
}

// opensBlock reports whether a line starts something a sentence cannot have run into from above:
// a heading, a list item, a quote or a table row.
func opensBlock(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	switch {
	case strings.HasPrefix(trimmed, "#"), strings.HasPrefix(trimmed, "- "),
		strings.HasPrefix(trimmed, "* "), strings.HasPrefix(trimmed, "+ "),
		strings.HasPrefix(trimmed, ">"), strings.HasPrefix(trimmed, "|"):
		return true
	}
	return listItemRe.MatchString(trimmed)
}

var listItemRe = regexp.MustCompile(`^\d+[.)]\s`)

// tidyEvidence makes a stored sentence one readable line. The cut is silent at the cap, which is
// a report's problem rather than the index's: nothing downstream parses this text.
func tidyEvidence(s string) string {
	s = strings.Join(strings.Fields(strings.TrimLeft(s, " \t-*+>")), " ")
	if len(s) > evidenceMax {
		s = strings.TrimSpace(s[:evidenceMax])
	}
	return s
}

// ---------------------------------------------------------------- supersession

// A passive's direction is not a property of the verb: it is read off the text up to the object,
// so the voice is recorded here and readsObject decides. "replaces" is absent because in this
// vault it is about code rather than notes.
var supersessionVerbs = []struct {
	phrase  string
	passive bool
}{
	{"superseded", true},
	{"replaced", true},
	{"supersedes", false},
	{"superseding", false},
	{"supersede", false},
}

// Raising this past the clause boundary lets a `by` that names something else read as the agent.
const passiveAgentGap = 24

// A quoted or code-spanned object means the link after it is not the object.
const objectQuotes = "\"'“”‘’`"

const objectSeparators = " \t\n*_,;:·—–-()[]"

const forwardSubjectGap = 12

var byRe = regexp.MustCompile(`\bby\b`)

// detectedEdge is one heuristic reading of one sentence. It is a candidate, never a fact: the
// authoritative answer lives in notes.superseded_by, which only frontmatter writes.
type detectedEdge struct {
	superseder string
	superseded string
	target     string
	section    string
	evidence   string
}

// detectEdges reads supersession claims out of prose, by the one constraint that made an
// extractor over this vault defensible: the link has to be the verb's object — the first link
// after it, inside its own sentence, within supersessionWindow bytes — rather than the first link
// in any sentence that mentions the word.
//
// Catalogue sections are not read at all. A `## Related` trailer has no verbs, so any phrase found
// near a link there belongs to a different note's title.
func detectEdges(key string, groups []linkedSection) []detectedEdge {
	var out []detectedEdge
	for _, group := range groups {
		if group.role != roleProse {
			continue
		}
		lower := asciiLower(group.masked)
		for _, verb := range verbSpans(lower) {
			// A link where the subject goes means the note is reporting a supersession, not making one.
			if !verb.passive && subjectIsLink(group.masked, group.links, verb.start) {
				continue
			}
			for _, link := range group.links {
				if link.start < verb.end || link.start-verb.end > supersessionWindow {
					continue
				}
				// The verb has to govern the link, not merely precede it across a full stop.
				from, to := sentenceRange(group.masked, link.start, link.end)
				if verb.start < from || verb.start >= to {
					break
				}
				backwards, object := readsObject(lower[verb.end:link.start], verb.passive)
				if !object {
					// The first link after the verb is the only candidate object there is, so text
					// that refuses it refuses the verb: looking further down the sentence is what
					// names whatever note happened to be mentioned next.
					break
				}
				edge := detectedEdge{
					superseder: key,
					superseded: link.key,
					target:     link.target,
					section:    group.heading,
					evidence:   link.evidence,
				}
				if backwards {
					edge.superseder, edge.superseded = link.key, key
				}
				out = append(out, edge)
				// Only the first link after the verb: the object is one note, and taking the rest
				// of the sentence's links was exactly what produced the 60-edge version.
				break
			}
		}
	}
	return out
}

// readsObject says whether the first link after a verb is its object at all, and which way the
// edge then runs. Every refusal here is a shape this vault writes where the link is something
// other than what the verb replaces, and a gap is cheaper than a wrong direction.
func readsObject(gap string, passive bool) (backwards, object bool) {
	if strings.ContainsAny(gap, objectQuotes) {
		return false, false
	}
	// Emphasis is trimmed with the whitespace: "**supersedes nothing**" negates like the plain form.
	if bare := strings.TrimLeft(gap, " \t\n*_"); strings.HasPrefix(bare, "nothing") || strings.HasPrefix(bare, "no ") {
		return false, false
	}
	if !passive {
		return false, true
	}
	at := byRe.FindStringIndex(gap)
	if at == nil || at[0] > passiveAgentGap {
		return false, false
	}
	// The link has to be what `by` names: "replaced by the gate · [[Debrief]]" names the gate.
	if strings.Trim(gap[at[1]:], objectSeparators) != "" {
		return false, false
	}
	return true, true
}

// subjectIsLink reports whether a link sits close enough before the verb to be its subject.
func subjectIsLink(text string, links []noteLink, verb int) bool {
	for _, link := range links {
		if link.end > verb || verb-link.end > forwardSubjectGap {
			continue
		}
		if strings.Trim(text[link.end:verb], objectSeparators) == "" {
			return true
		}
	}
	return false
}

// asciiLower is length-preserving, so an offset into the copy indexes the original. Not decorative:
// strings.ToLower shortens "K" (U+212A) from three bytes to one and puts link offsets out of range.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

type verbSpan struct {
	start, end int
	passive    bool
}

// verbSpans finds the supersession verbs in a lowercased text, longest match winning where two
// overlap — "superseded" and "supersede" start at the same byte and are in different voices.
func verbSpans(lower string) []verbSpan {
	var spans []verbSpan
	for _, verb := range supersessionVerbs {
		for at := 0; ; {
			i := strings.Index(lower[at:], verb.phrase)
			if i < 0 {
				break
			}
			start := at + i
			spans = append(spans, verbSpan{start: start, end: start + len(verb.phrase), passive: verb.passive})
			at = start + len(verb.phrase)
		}
	}
	var kept []verbSpan
	for _, span := range spans {
		longest := true
		for _, other := range spans {
			if other.start < span.end && span.start < other.end && other.end-other.start > span.end-span.start {
				longest = false
				break
			}
		}
		if longest {
			kept = append(kept, span)
		}
	}
	return kept
}

// ---------------------------------------------------------------- writing

// writeLinks records a note's links and its supersession claims, declared and detected.
//
// Called alongside writeSections rather than from inside it: the rows describe the note, not its
// sections, and the declared half comes from frontmatter that writeSections never sees.
func writeLinks(tx *sql.Tx, noteID int64, rel string, meta noteMeta, body string) error {
	key := noteKey(rel)
	groups := linkedSections(body)
	for _, group := range groups {
		for _, link := range group.links {
			if _, err := tx.Exec(`insert into links
				(note_id, target, target_key, anchor, section, role, evidence, context) values (?,?,?,?,?,?,?,?)`,
				noteID, link.target, link.key, link.anchor, group.heading, group.role,
				link.evidence, link.context); err != nil {
				return err
			}
		}
	}

	// The authoritative edge, and the only one written from an authored statement. The evidence is
	// the frontmatter line itself, so a reader of the table sees what the note actually said.
	if meta.supersededBy != "" {
		if _, err := tx.Exec(`insert into supersessions
			(note_id, superseder_key, superseded_key, target, source, section, evidence) values (?,?,?,?,?,?,?)`,
			noteID, linkKey(meta.supersededBy), key, meta.supersededBy, sourceDeclared,
			"(frontmatter)", meta.supersededLine); err != nil {
			return err
		}
	}

	for _, edge := range detectEdges(key, groups) {
		// A note whose prose restates its own frontmatter adds nothing to audit.
		if edge.superseded == key && edge.superseder == linkKey(meta.supersededBy) {
			continue
		}
		if _, err := tx.Exec(`insert into supersessions
			(note_id, superseder_key, superseded_key, target, source, section, evidence) values (?,?,?,?,?,?,?)`,
			noteID, edge.superseder, edge.superseded, edge.target, sourceDetected,
			edge.section, edge.evidence); err != nil {
			return err
		}
	}
	return nil
}

// dropLinks retires a note's link and supersession rows.
//
// Explicit, and not left to the foreign key: a reindexed note keeps its notes row — writeNote
// updates it in place — so nothing cascades, and every edit would otherwise add a second copy of
// every link the note holds.
func dropLinks(tx *sql.Tx, noteID int64) error {
	if _, err := tx.Exec(`delete from links where note_id = ?`, noteID); err != nil {
		return err
	}
	_, err := tx.Exec(`delete from supersessions where note_id = ?`, noteID)
	return err
}

// ---------------------------------------------------------------- reading

// Backlink is one inbound link, with what justifies it.
type Backlink struct {
	Path     string
	Section  string
	Evidence string
}

// Backlinks is a note's inbound links, split by role because the two answer different questions:
// Prose is who argued from this note, Related is who filed it. One note measured 23 and 39 of
// them, and a single total would have read as engagement it did not have.
type Backlinks struct {
	Prose   []Backlink
	Related []Backlink
}

// Backlinks returns what links to path. This is what the hand-maintained `## Related` trailers
// were for — 511 notes carry one, holding about 4,291 links between them — and it has to answer
// that need before those trailers can be removed.
func (x *Index) Backlinks(path string) (Backlinks, error) {
	var out Backlinks
	if err, proceed := x.refreshForRead(); !proceed {
		return out, err
	}
	rows, err := x.db.Query(`select src.path, l.section, l.role, l.evidence
		from links l join notes src on src.id = l.note_id
		where l.target_key = ? and src.path <> ?
		order by src.path, l.id`, noteKey(path), path)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var link Backlink
		var role string
		if err := rows.Scan(&link.Path, &link.Section, &role, &link.Evidence); err != nil {
			return out, err
		}
		if role == roleRelated {
			out.Related = append(out.Related, link)
		} else {
			out.Prose = append(out.Prose, link)
		}
	}
	return out, rows.Err()
}

// BacklinksFor opens the index, answers, and closes it again, like every other package-level call
// here: a session holds no connection between tool calls.
func BacklinksFor(vault, path string) (Backlinks, error) {
	x, err := OpenIndex(vault)
	if err != nil {
		return Backlinks{}, err
	}
	defer x.Close()
	return x.Backlinks(path)
}

// Validity is what a note says about its own currency — all of it authored, none of it inferred.
type Validity struct {
	Status string
	// SupersededBy is the target the note's own frontmatter names, as written. Empty means the
	// note declares nothing, which is not the same as the note being current: see
	// DetectedSupersessions for what the prose may be claiming.
	SupersededBy string
	Evidence     string
	From         string
	Until        string
}

func (x *Index) Validity(path string) (Validity, error) {
	var v Validity
	if err, proceed := x.refreshForRead(); !proceed {
		return v, err
	}
	err := x.db.QueryRow(`select status, superseded_by, valid_from, valid_until
		from notes where path = ?`, path).Scan(&v.Status, &v.SupersededBy, &v.From, &v.Until)
	if err != nil {
		return v, err
	}
	if v.SupersededBy != "" {
		// Read back rather than rebuilt from the column, so the report quotes the same row a human
		// auditing the table would see.
		x.db.QueryRow(`select evidence from supersessions
			where note_id = (select id from notes where path = ?) and source = ?`,
			path, sourceDeclared).Scan(&v.Evidence)
	}
	return v, nil
}

// Supersession is one recorded claim. Superseder and Superseded are vault paths where the endpoint
// resolves and the name as written where it does not, so a claim about a note nobody wrote is
// still readable.
type Supersession struct {
	Superseder string
	Superseded string
	// ClaimedBy is the note that states it, which is one of the two endpoints.
	ClaimedBy string
	Source    string
	Section   string
	Evidence  string
}

// DeclaredSupersessions is the authoritative set: every edge a note declares in its own
// frontmatter.
func (x *Index) DeclaredSupersessions() ([]Supersession, error) {
	return x.supersessions(sourceDeclared, "")
}

// DetectedSupersessions is the heuristic's set, for a human to audit and for nothing to act on.
//
// The endpoint refusal from the measurement lives here rather than at write time: a person, index
// or tracker note is not something that supersedes anything, but the target's type is not knowable
// when its own row may not be written yet. Applying it in the query also keeps it current when a
// note's type changes. An unresolved target is dropped for the same reason — a claim pointing at
// no note cannot be acted on either way.
func (x *Index) DetectedSupersessions() ([]Supersession, error) {
	return x.supersessions(sourceDetected, "")
}

// SupersededBy is every recorded claim that path has been replaced, declared before detected. It
// shares the reader above so the read-path refusals cannot drift, and asks for both tiers in one
// query because it runs on every vault_read.
func (x *Index) SupersededBy(path string) ([]Supersession, error) {
	return x.supersessions("", path)
}

var refusedEndpointTypes = []string{"person", "index", "tracker", "moc"}

// supersessions reads the table, optionally narrowed to one tier and to claims about one note.
// An empty source means both tiers, declared first; an empty superseded means every note.
func (x *Index) supersessions(source, superseded string) ([]Supersession, error) {
	if err, proceed := x.refreshForRead(); !proceed {
		return nil, err
	}
	where, args := `where 1 = 1`, []any{}
	if source != "" {
		where += ` and s.source = ?`
		args = append(args, source)
	}
	if superseded != "" {
		where += ` and (select path from notes where key = s.superseded_key order by path limit 1) = ?`
		args = append(args, superseded)
	}
	// The endpoint paths are picked by lowest path order, which is the documented tiebreak for two
	// notes sharing a filename: without it a duplicate basename would multiply every row.
	rows, err := x.db.Query(`
		select claim.path, s.superseder_key, s.superseded_key, s.target, s.section, s.evidence, s.source,
		  (select path from notes where key = s.superseder_key order by path limit 1),
		  (select path from notes where key = s.superseded_key order by path limit 1),
		  (select type from notes where key = s.superseder_key order by path limit 1),
		  (select type from notes where key = s.superseded_key order by path limit 1)
		from supersessions s join notes claim on claim.id = s.note_id
		`+where+` order by s.source, claim.path, s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Supersession
	for rows.Next() {
		var superKey, subKey, target string
		var superPath, subPath, superType, subType sql.NullString
		var claim Supersession
		if err := rows.Scan(&claim.ClaimedBy, &superKey, &subKey, &target, &claim.Section,
			&claim.Evidence, &claim.Source, &superPath, &subPath, &superType, &subType); err != nil {
			return nil, err
		}
		claim.Superseder, claim.Superseded = superPath.String, subPath.String
		if claim.Source == sourceDetected {
			if !superPath.Valid || !subPath.Valid {
				continue
			}
			if refusedEndpoint(superType.String) || refusedEndpoint(subType.String) {
				continue
			}
		}
		// A declared edge may name a note that does not exist; the claim is still what the note
		// says, so it is reported with the name it used.
		if claim.Superseder == "" {
			claim.Superseder = target
		}
		if claim.Superseded == "" {
			claim.Superseded = target
		}
		out = append(out, claim)
	}
	return out, rows.Err()
}

func refusedEndpoint(kind string) bool {
	for _, refused := range refusedEndpointTypes {
		if kind == refused {
			return true
		}
	}
	return false
}

// SupersessionCycles names the pairs of notes that each claim to replace the other, over the
// declared and detected sets together. Two notes in this vault do exactly that, and a layer that
// decides what is current has to say so rather than silently pick a direction.
//
// Mutual pairs only, and not the general cycle: every claim recorded here is one note's statement
// about one other note, so a longer ring would be a disagreement nobody wrote on purpose, while
// the pair is a contradiction someone can go and resolve. The pairs come back in path order.
func (x *Index) SupersessionCycles() ([][2]string, error) {
	declared, err := x.DeclaredSupersessions()
	if err != nil {
		return nil, err
	}
	detected, err := x.DetectedSupersessions()
	if err != nil {
		return nil, err
	}
	claims := map[[2]string]bool{}
	for _, s := range append(declared, detected...) {
		claims[[2]string{s.Superseder, s.Superseded}] = true
	}
	seen := map[[2]string]bool{}
	var out [][2]string
	for claim := range claims {
		pair := claim
		if pair[0] > pair[1] {
			pair[0], pair[1] = pair[1], pair[0]
		}
		if claims[[2]string{claim[1], claim[0]}] && !seen[pair] {
			seen[pair] = true
			out = append(out, pair)
		}
	}
	sortPairs(out)
	return out, nil
}

func sortPairs(pairs [][2]string) {
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j][0] < pairs[j-1][0]; j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
}

// UnresolvedLinks counts the links pointing at a name no note in the vault carries, which is the
// one number that says whether the graph is intact. Recorded rather than dropped: a broken link is
// a fact about the vault, and a table that silently omitted them would make the count unanswerable.
func (x *Index) UnresolvedLinks() (int, error) {
	if err, proceed := x.refreshForRead(); !proceed {
		return 0, err
	}
	var n int
	err := x.db.QueryRow(`select count(*) from links l
		where not exists (select 1 from notes where key = l.target_key)`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting unresolved links: %w", err)
	}
	return n, nil
}
