package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

// A small vault with the shapes that break naive indexing: frontmatter, a note whose conclusions
// sit before the first heading, a note with no headings at all, and a fenced shell block whose
// comment looks exactly like a heading.
//
// Every note here is one a search may return. The shapes that are deliberately withheld from
// search — an archived note, a bare link trailer — bring their own vault, so they cannot quietly
// change what these tests expect to rank.
var fakeNotes = map[string]string{
	"HOME.md": `---
type: moc
updated: 2026-10-01
---
Read this before the first retrieval of a session.

## Agent Brief

Nothing marked phi may reach a server without the user's consent.
`,

	"Systems/Recipe Scraper.md": `---
type: system
status: retired
---
# Recipe Scraper

A triage tool for the shared inbox, switched off in August.

## Outcome

Scrapped once the model kept tagging desserts as soups, and no one
would own the escalation path.

## Running it locally

` + "```sh\n# install the triage model\nmake install\n```" + `
`,

	"People/Sam Weiss.md": `---
type: person
---
# Sam Weiss

## Owns

The scheduling service, including the timezone handling that flakes every
spring.
`,

	"Glossary/Taint.md": `---
type: term
---
A note is tainted once it has been read in a conversation, which constrains
where the rest of that conversation may be sent.
`,
}

// tempIndex builds a throwaway vault and an index over it.
//
// HOME is redirected first: the index lands in ~/.cache/alux, which live sessions share, and
// a test run must touch neither that file nor the real vault.
func tempIndex(t *testing.T, notes map[string]string) (*Index, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	vault := tempVault(t)
	for rel, text := range notes {
		writeNoteFile(t, vault, rel, text)
	}
	index, err := OpenIndex(vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	return index, vault
}

// Several alux sessions share one index file, each with its own connection, and they all
// refresh before they read. Before the write lock was taken at BEGIN rather than at first write,
// two of them could both read "this note is not indexed" and then both insert it, failing the
// unique index on path — six processes reproduced that within 40 ms of a cold start.
//
// This is the in-process shape of the same race: separate connections to one file, racing from
// empty. It fails reliably against the deferred-locking version.
func TestConcurrentRefreshesDoNotCollide(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	vault := tempVault(t)
	for i := range 40 {
		writeNoteFile(t, vault, fmt.Sprintf("Systems/Note %d.md", i),
			fmt.Sprintf("---\ntype: system\n---\n# Note %d\n\nbody text %d\n\n## Detail\n\nmore %d\n", i, i, i))
	}

	const sessions = 8
	var wg sync.WaitGroup
	errs := make(chan error, sessions*2)
	start := make(chan struct{})
	for range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			index, err := OpenIndex(vault)
			if err != nil {
				errs <- err
				return
			}
			defer index.Close()
			<-start // released together, so they contend rather than queue politely
			if _, err := index.Refresh(); err != nil {
				errs <- err
			}
			if _, err := index.Search("body text detail", 5); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent session failed: %v", err)
	}

	index, err := OpenIndex(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	// Every note indexed exactly once: a lost race must not leave a duplicate or a gap.
	var notes, dupes int
	if err := index.db.QueryRow(`select count(*) from notes`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if err := index.db.QueryRow(`select count(*) from
		(select path from notes group by path having count(*) > 1)`).Scan(&dupes); err != nil {
		t.Fatal(err)
	}
	if notes != 40 || dupes != 0 {
		t.Errorf("after %d concurrent refreshes: %d notes (want 40), %d duplicated paths (want 0)", sessions, notes, dupes)
	}
	// FTS5 keeps its own term index; a half-applied concurrent write shows up here and nowhere else.
	if _, err := index.db.Exec(`insert into sections_fts(sections_fts) values('integrity-check')`); err != nil {
		t.Errorf("FTS index corrupt after concurrent refreshes: %v", err)
	}
}

// Every statement in indexSchema is CREATE ... IF NOT EXISTS, so an index file written by an
// older build keeps its old shape forever unless something notices. The session would then query
// a schema that no longer matches the code — a missing column at best, a silently wrong answer
// at worst. The version stamp is what makes a schema change safe to ship.
func TestAnIndexFromAnOlderSchemaIsRebuilt(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	// A column this build knows nothing about, standing in for any past shape.
	if _, err := index.db.Exec(`alter table notes add column obsolete text`); err != nil {
		t.Fatal(err)
	}
	if _, err := index.db.Exec(`pragma user_version = 0`); err != nil {
		t.Fatal(err)
	}
	index.Close()

	reopened, err := OpenIndex(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	var version int
	if err := reopened.db.QueryRow(`pragma user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("reopened index stamped version %d, want %d", version, schemaVersion)
	}
	if err := reopened.db.QueryRow(`select obsolete from notes limit 1`).Scan(new(any)); err == nil {
		t.Error("the old schema survived: a schema change would silently not apply")
	}

	// Rebuilt, not merely emptied — the session that triggers the upgrade must still be able
	// to search without the user noticing anything happened.
	if _, err := reopened.Refresh(); err != nil {
		t.Fatal(err)
	}
	hits, err := reopened.Search("scrapped triage soups", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("index did not repopulate after the schema rebuild")
	}
}

// A vault that cannot be walked — moved, unmounted, or mid-rename under an external sync — reads
// as zero notes on disk. Treated literally that is a mass deletion, and the index would empty
// itself and then answer "no matches" about everything, confidently and wrongly. It must refuse
// instead, and go on serving what it already has.
func TestAVanishedVaultDoesNotEmptyTheIndex(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := index.db.QueryRow(`select count(*) from notes`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	index.vault = filepath.Join(vault, "moved-away")
	if _, err := index.Refresh(); err == nil {
		t.Error("refresh against a vanished vault should report a problem, not succeed silently")
	}

	var after int
	if err := index.db.QueryRow(`select count(*) from notes`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("index dropped %d of %d notes because the vault was unreadable", before-after, before)
	}

	// And the session keeps working: a refresh that fails is not worth losing search over,
	// because the alternative is regex, which cannot answer a question at all.
	hits, err := index.Search("scrapped triage soups", 5)
	if err != nil {
		t.Fatalf("search should still answer from the populated index: %v", err)
	}
	if len(hits) == 0 {
		t.Error("populated index returned no hits after a failed refresh")
	}
}

// unprivileged skips a test that works by taking permissions away: root is denied nothing, so
// chmod 000 there proves the opposite of what the test claims.
func unprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 denies nothing")
	}
}

// deny makes a path unreadable for the rest of the test, and gives the permission back so the
// temporary directory can still be removed.
func deny(t *testing.T, path string, restore os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, restore) })
}

// One folder that cannot be read — a permission change, or an external sync mid-rename — must not
// read as "every note in it was deleted". A walk that quietly dropped the error and returned
// only the folders it could see would have the index delete those rows, and the session would
// answer "no matches" about a whole folder still sitting on disk, with nothing reported anywhere.
func TestAnUnreadableFolderDoesNotDeleteItsNotes(t *testing.T) {
	unprivileged(t)
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	deny(t, filepath.Join(vault, "People"), 0o755)
	// A note the walk can still see, so the partial pass is not an excuse to do nothing.
	writeNoteFile(t, vault, "Inbox/Fresh.md", "# Fresh\n\nThe rutabaga order arrived.\n")

	st, err := index.Refresh()
	if err == nil {
		t.Error("a refresh that could not read part of the vault reported success")
	}
	if st.Deleted != 0 {
		t.Errorf("refresh = %+v, want nothing deleted while part of the vault was unreadable", st)
	}
	if st.Indexed != 1 {
		t.Errorf("refresh = %+v, want the one new note in a readable folder indexed", st)
	}
	if got := headingsOf(t, index, "People/Sam Weiss.md"); len(got) == 0 {
		t.Error("the unreadable folder's note was dropped from the index")
	}
	hits, err := index.Search("scheduling timezone", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Path != "People/Sam Weiss.md" {
		t.Errorf("a note in the unreadable folder is no longer searchable: %+v", hits)
	}
}

// A note that cannot be read must still leave a row behind: no row is what stale() reads as "not
// indexed" on every pass thereafter, so every subsequent search would open the BEGIN IMMEDIATE
// transaction, find nothing to do and commit empty, which is precisely the serialisation the
// lock-free pre-check exists to avoid.
func TestAnUnreadableNoteDoesNotLeaveTheIndexPermanentlyDirty(t *testing.T) {
	unprivileged(t)
	index, vault := tempIndex(t, fakeNotes)
	deny(t, filepath.Join(vault, "Glossary/Taint.md"), 0o644)

	first, err := index.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if first.Unreadable != 1 || first.Indexed != len(fakeNotes)-1 {
		t.Fatalf("first refresh = %+v, want %d indexed and one unreadable", first, len(fakeNotes)-1)
	}

	second, err := index.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if second.Indexed != 0 || second.Reindexed != 0 {
		t.Errorf("second refresh = %+v, want zero work: an unreadable note that is still "+
			"unreadable makes every search take the write lock", second)
	}
}

// ReadSection has no fallback behind it, so what it says is read as fact. "No note at X" when
// the refresh that would have indexed X failed is a confident falsehood about the source of
// truth: a note filed since the last good pass is on disk and simply not in the index.
func TestReadSectionSaysWhenTheIndexCouldNotBeUpdated(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	index.vault = filepath.Join(vault, "moved-away")
	_, err := index.ReadSection("Systems/Filed Today.md", "Outcome")
	if err == nil {
		t.Fatal("a note missing from an index that could not be refreshed was answered as absent")
	}
	for _, want := range []string{"Systems/Filed Today.md", "moved-away"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}

	// A genuine miss against a healthy index still says plainly that there is no such note.
	index.vault = vault
	_, err = index.ReadSection("Systems/Nothing Here.md", "Outcome")
	if err == nil || !strings.Contains(err.Error(), "no note at Systems/Nothing Here.md") {
		t.Errorf("a real miss should say so plainly: %v", err)
	}
}

func writeNoteFile(t *testing.T, vault, rel, text string) {
	t.Helper()
	full := filepath.Join(vault, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// touch moves a note's mtime forward explicitly. Filesystem timestamp granularity is coarse
// enough that a rewrite within the same tick would otherwise look unchanged.
func touch(t *testing.T, vault, rel string) {
	t.Helper()
	when := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(vault, rel), when, when); err != nil {
		t.Fatal(err)
	}
}

func headingsOf(t *testing.T, index *Index, rel string) []string {
	t.Helper()
	rows, err := index.db.Query(`select s.heading from sections s join notes n on n.id = s.note_id
		where n.path = ? order by s.seq`, rel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

func sectionIDs(t *testing.T, index *Index) []int64 {
	t.Helper()
	rows, err := index.db.Query(`select id from sections order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func refresh(t *testing.T, index *Index) Stats {
	t.Helper()
	st, err := index.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func search(t *testing.T, index *Index, query string) []Hit {
	t.Helper()
	hits, err := index.Search(query, 5)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// reassembled is a note's sections concatenated in seq order, which must come back as the body
// on disk byte for byte.
func reassembled(t *testing.T, index *Index, rel string) string {
	t.Helper()
	rows, err := index.db.Query(`select s.body from sections s join notes n on n.id = s.note_id
		where n.path = ? order by s.seq`, rel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var rebuilt strings.Builder
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		rebuilt.WriteString(body)
	}
	return rebuilt.String()
}

// The property that makes a section safe to hand back in place of the file: nothing is dropped
// between the headings and nothing is invented.
func TestSectionsReassembleTheBodyByteForByte(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	for rel := range fakeNotes {
		raw, err := os.ReadFile(filepath.Join(vault, rel))
		if err != nil {
			t.Fatal(err)
		}
		want := stripFrontmatter(string(raw))
		if got := reassembled(t, index, rel); got != want {
			t.Errorf("%s does not reassemble:\n got %q\nwant %q", rel, got, want)
		}
	}
}

// mtime and size are the file as it sits on disk, and together they are what decides whether the
// note has to be read again.
func TestNoteRowsRecordTheStalenessKey(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	for rel := range fakeNotes {
		info, err := os.Stat(filepath.Join(vault, rel))
		if err != nil {
			t.Fatal(err)
		}
		var mtime, size int64
		if err := index.db.QueryRow(`select mtime, size from notes where path = ?`, rel).
			Scan(&mtime, &size); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if mtime != info.ModTime().UnixNano() || size != info.Size() {
			t.Errorf("%s = mtime %d size %d, want %d and %d", rel, mtime, size,
				info.ModTime().UnixNano(), info.Size())
		}
	}
}

// Obsidian's own config is not content. Indexing it would answer vault questions with plugin
// settings, and the existing note map excludes it for the same reason.
func TestObsidiansOwnConfigIsNotIndexed(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	writeNoteFile(t, vault, ".obsidian/plugins/notes.md", "# Plugin\n\nThe kumquat setting is on.\n")

	if st := refresh(t, index); st.Indexed != len(fakeNotes) {
		t.Errorf("refresh = %+v, want %d notes and nothing from .obsidian", st, len(fakeNotes))
	}
	if hits := search(t, index, "kumquat"); len(hits) != 0 {
		t.Errorf("a file under .obsidian was indexed: %+v", hits)
	}
}

// A "# comment" in a shell block is not a heading. Splitting on it would cut a section in the
// middle of code and invent a heading no reader of the note would recognise.
func TestAHeadingInsideAFencedBlockIsNotASection(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)
	refresh(t, index)

	got := headingsOf(t, index, "Systems/Recipe Scraper.md")
	want := []string{"Recipe Scraper", "Outcome", "Running it locally"}
	if !slices.Equal(got, want) {
		t.Errorf("headings = %q, want %q", got, want)
	}

	// Masking is for splitting only: the fenced text itself is still indexed and findable.
	hits := search(t, index, "install")
	if len(hits) == 0 || hits[0].Heading != "Running it locally" {
		t.Errorf("searching for fenced text gave %+v", hits)
	}
}

func TestTextBeforeTheFirstHeadingAndNotesWithNoHeadings(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)
	refresh(t, index)

	if got, want := headingsOf(t, index, "HOME.md"), []string{"(preamble)", "Agent Brief"}; !slices.Equal(got, want) {
		t.Errorf("HOME.md headings = %q, want %q", got, want)
	}
	if got, want := headingsOf(t, index, "Glossary/Taint.md"), []string{"(whole note)"}; !slices.Equal(got, want) {
		t.Errorf("Taint.md headings = %q, want %q", got, want)
	}
}

// The index is refreshed on every search, so an unchanged vault has to cost nothing. Rebuilding
// 4,238 sections on each tool call would be slower than the ripgrep search it replaces.
func TestRefreshOverAnUnchangedVaultDoesNothing(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)

	first := refresh(t, index)
	if first.Indexed != len(fakeNotes) || first.Reindexed != 0 || first.Deleted != 0 {
		t.Fatalf("first refresh = %+v, want %d indexed and nothing else", first, len(fakeNotes))
	}
	before := sectionIDs(t, index)

	second := refresh(t, index)
	if second.Indexed != 0 || second.Reindexed != 0 || second.Deleted != 0 {
		t.Errorf("second refresh = %+v, want all zero", second)
	}
	// Row identity, not just the counters: a section rewritten with the same text would still be
	// a write, and the counters alone would not notice.
	if after := sectionIDs(t, index); !slices.Equal(before, after) {
		t.Errorf("section rows changed across an unchanged refresh: %v -> %v", before, after)
	}
}

func TestRefreshPicksUpAModifiedNote(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	writeNoteFile(t, vault, "Glossary/Taint.md", `---
type: term
---
A note is quarantined once it has been read, which constrains where the rest of
that conversation may be sent.
`)
	touch(t, vault, "Glossary/Taint.md")

	st := refresh(t, index)
	if st.Reindexed != 1 || st.Indexed != 0 || st.Deleted != 0 {
		t.Fatalf("refresh = %+v, want exactly one reindexed", st)
	}
	if hits := search(t, index, "quarantined"); len(hits) != 1 || hits[0].Path != "Glossary/Taint.md" {
		t.Errorf("the new wording is not findable: %+v", hits)
	}
}

// Size is the other half of the staleness key, and it is there for an external sync: one can rewrite
// notes in the vault with no lock, and two writes inside one timestamp tick are a real case on
// a filesystem with coarse mtimes.
func TestRefreshPicksUpAChangeThatDidNotMoveTheClock(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	note := filepath.Join(vault, "Glossary/Taint.md")
	info, err := os.Stat(note)
	if err != nil {
		t.Fatal(err)
	}
	writeNoteFile(t, vault, "Glossary/Taint.md", `---
type: term
---
A note is tainted once it has been read in a conversation, which constrains
where the rest of that conversation may be sent, and for how long it stays so.
`)
	if err := os.Chtimes(note, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	if st := refresh(t, index); st.Reindexed != 1 {
		t.Fatalf("refresh = %+v, want the longer note reindexed", st)
	}
}

func TestRefreshDropsANoteThatIsGone(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	if err := os.Remove(filepath.Join(vault, "People/Sam Weiss.md")); err != nil {
		t.Fatal(err)
	}

	st := refresh(t, index)
	if st.Deleted != 1 || st.Indexed != 0 || st.Reindexed != 0 {
		t.Fatalf("refresh = %+v, want exactly one deleted", st)
	}
	if got := headingsOf(t, index, "People/Sam Weiss.md"); len(got) != 0 {
		t.Errorf("sections of a deleted note survived: %q", got)
	}
	if hits := search(t, index, "scheduling timezone"); len(hits) != 0 {
		t.Errorf("a deleted note still answers searches: %+v", hits)
	}
	if _, err := index.ReadSection("People/Sam Weiss.md", "Owns"); err == nil {
		t.Error("reading a section of a deleted note succeeded")
	}
}

// sections_fts is external-content, so its rows do not follow a delete from sections. If the old
// rows are not retired explicitly the index keeps answering for text the note no longer holds —
// the worst failure this layer can have, because the quote it returns no longer exists on disk.
func TestReindexingLeavesNoOrphanedIndexRows(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	writeNoteFile(t, vault, "Inbox/Fruit.md", "# Fruit\n\nThe mango shipment is late.\n")
	refresh(t, index)
	if hits := search(t, index, "mango"); len(hits) != 1 {
		t.Fatalf("mango is not findable after the first index: %+v", hits)
	}

	writeNoteFile(t, vault, "Inbox/Fruit.md", "# Fruit\n\nThe papaya shipment is late.\n")
	touch(t, vault, "Inbox/Fruit.md")
	refresh(t, index)

	if hits := search(t, index, "mango"); len(hits) != 0 {
		t.Errorf("the replaced wording is still findable: %+v", hits)
	}
	if hits := search(t, index, "papaya"); len(hits) != 1 {
		t.Errorf("the new wording is not findable: %+v", hits)
	}
	// FTS5's own check that the index and the content table still agree.
	if _, err := index.db.Exec(`insert into sections_fts(sections_fts) values('integrity-check')`); err != nil {
		t.Errorf("the full-text index no longer matches its content: %v", err)
	}
}

// The reason this layer exists: the words of a real question never appear adjacent on one line,
// so the regex search returns nothing while the terms ORed find the section that answers.
func TestSearchFindsAQuestionWhoseWordsNeverShareALine(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	const question = "why was the recipe scraper scrapped"
	words := []string{"recipe", "scraper", "scrapped"}
	for rel := range fakeNotes {
		raw, err := os.ReadFile(filepath.Join(vault, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.ToLower(string(raw)), "\n") {
			all := true
			for _, w := range words {
				all = all && strings.Contains(line, w)
			}
			if all {
				t.Fatalf("%s holds every word on one line, so this proves nothing: %q", rel, line)
			}
		}
	}

	hits := search(t, index, question)
	if len(hits) == 0 {
		t.Fatal("no hits for a question the vault answers")
	}
	if hits[0].Path != "Systems/Recipe Scraper.md" {
		t.Errorf("top hit is %s, want Systems/Recipe Scraper.md", hits[0].Path)
	}
	var answer *Hit
	for i := range hits {
		if hits[i].Path == "Systems/Recipe Scraper.md" && hits[i].Heading == "Outcome" {
			answer = &hits[i]
		}
	}
	if answer == nil {
		t.Fatalf("the section that answers was not returned: %+v", hits)
	}
	if !strings.Contains(strings.ToLower(answer.Snippet), "scrapped") {
		t.Errorf("snippet does not show the match: %q", answer.Snippet)
	}
}

// A bare "Related" section is a link trailer: wikilinked note titles, no prose. 497 of the real
// vault's 4,389 sections are one, and they took 42 of 540 result slots across 90 benchmark
// queries while answering nothing — in two measured cases the section that did answer missed the
// top six while the same note's trailer ranked first.
//
// Withheld from the full-text index only. The section itself has to stay, because a note's
// sections must still reassemble its body byte for byte and ReadSection must still hand one back.
func TestLinkTrailerSectionsAreNotSearchable(t *testing.T) {
	index, vault := tempIndex(t, map[string]string{
		"Systems/Scheduler.md": `---
type: system
---
# Scheduler

## Outcome

Rewritten in June, once the rutabaga backlog stopped draining overnight.

## Related

[[Mango Exporter]]
[[Rutabaga Backlog]]
`,
		"Systems/Mango Exporter.md": `---
type: system
---
# Mango Exporter

## Related work

We measured the kumquat stack's exporter before building this one.

## See Also

[[Scheduler]]
`,
	})
	refresh(t, index)

	for _, query := range []string{"mango exporter", "rutabaga backlog", "scheduler"} {
		for _, h := range search(t, index, query) {
			if h.Heading == "Related" || h.Heading == "See Also" {
				t.Errorf("%q ranked a link trailer: %s › %s", query, h.Path, h.Heading)
			}
		}
	}

	// Only the bare headings. A heading that merely begins with the word introduces prose, and
	// excluding it would lose the answer rather than the noise.
	var kept bool
	for _, h := range search(t, index, "kumquat stack") {
		kept = kept || h.Heading == "Related work"
	}
	if !kept {
		t.Error(`"Related work" was withheld too; only an exactly bare trailer heading may be`)
	}

	// Not searchable is not the same as not indexed: the heading is still listed, the text still
	// reads back, and the note still reassembles.
	if got, want := headingsOf(t, index, "Systems/Scheduler.md"),
		[]string{"Scheduler", "Outcome", "Related"}; !slices.Equal(got, want) {
		t.Errorf("headings = %q, want %q", got, want)
	}
	body, err := index.ReadSection("Systems/Scheduler.md", "Related")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "[[Mango Exporter]]") {
		t.Errorf("the withheld section no longer reads back: %q", body)
	}
	raw, err := os.ReadFile(filepath.Join(vault, "Systems/Scheduler.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reassembled(t, index, "Systems/Scheduler.md"), stripFrontmatter(string(raw)); got != want {
		t.Errorf("a note with a withheld section no longer reassembles:\n got %q\nwant %q", got, want)
	}
}

// A withheld section has no row in sections_fts, and retiring one that was never inserted is not
// a harmless no-op: FTS5 answers 'delete' over an absent rowid by reporting the whole database as
// malformed. So the retirement has to skip exactly the rows the insert skipped — which is what
// the searchable column records.
func TestReindexingANoteWithALinkTrailerLeavesTheIndexIntact(t *testing.T) {
	const before = `---
type: system
---
# Scheduler

## Outcome

The mango batch was parked overnight.

## Related

[[Mango Exporter]]
`
	index, vault := tempIndex(t, map[string]string{"Systems/Scheduler.md": before})
	refresh(t, index)
	if hits := search(t, index, "parked overnight"); len(hits) != 1 {
		t.Fatalf("the note is not findable after the first index: %+v", hits)
	}

	writeNoteFile(t, vault, "Systems/Scheduler.md",
		strings.ReplaceAll(before, "mango batch was parked", "papaya batch was parked"))
	touch(t, vault, "Systems/Scheduler.md")
	refresh(t, index)

	if hits := search(t, index, "mango"); len(hits) != 0 {
		t.Errorf("the replaced wording is still findable: %+v", hits)
	}
	if hits := search(t, index, "papaya"); len(hits) != 1 {
		t.Errorf("the new wording is not findable: %+v", hits)
	}
	if _, err := index.db.Exec(`insert into sections_fts(sections_fts) values('integrity-check')`); err != nil {
		t.Errorf("the full-text index no longer matches its content: %v", err)
	}
}

// 36 notes in the real vault carry status: archived, and they competed for result slots on equal
// terms — one archived duplicate was measured as the top hit for a query, outranking the note it
// was superseded by. They stay readable by path, so a link or a title still leads somewhere; they
// just stop coming back from a search.
func TestArchivedNotesAreNotSearchableButStayReadable(t *testing.T) {
	const archived = `---
type: system
status: archived
---
# Mango Exporter

## Outcome

The exporter retries twice and then parks the batch for a human.
`
	index, vault := tempIndex(t, map[string]string{
		"Systems/Mango Exporter.md": `---
type: system
status: active
---
# Mango Exporter

## Outcome

The exporter retries twice and then parks the batch for a human.
`,
		"Archive/Mango Exporter 2024.md": archived,
	})
	refresh(t, index)

	hits := search(t, index, "exporter retries parks batch")
	if len(hits) == 0 {
		t.Fatal("no hits at all: the live note has to stay findable")
	}
	for _, h := range hits {
		if h.Path == "Archive/Mango Exporter 2024.md" {
			t.Errorf("an archived note is still competing for result slots: %+v", hits)
		}
	}

	body, err := index.ReadSection("Archive/Mango Exporter 2024.md", "Outcome")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "parks the batch") {
		t.Errorf("an archived note is no longer readable by path: %q", body)
	}

	// The status came off the frontmatter, and the frontmatter itself stayed out of the text:
	// indexing it would make every note sharing a status or a type rank for the word.
	var status, kind string
	if err := index.db.QueryRow(`select status, type from notes where path = ?`,
		"Archive/Mango Exporter 2024.md").Scan(&status, &kind); err != nil {
		t.Fatal(err)
	}
	if status != "archived" || kind != "system" {
		t.Errorf("note row = status %q type %q, want \"archived\" and \"system\"", status, kind)
	}
	if hits := search(t, index, "archived system"); len(hits) != 0 {
		t.Errorf("frontmatter reached the full-text index: %+v", hits)
	}

	// Un-archiving is an ordinary edit, so the next refresh has to put the note back.
	writeNoteFile(t, vault, "Archive/Mango Exporter 2024.md",
		strings.Replace(archived, "status: archived", "status: active", 1))
	touch(t, vault, "Archive/Mango Exporter 2024.md")
	refresh(t, index)

	var found bool
	for _, h := range search(t, index, "exporter retries parks batch") {
		found = found || h.Path == "Archive/Mango Exporter 2024.md"
	}
	if !found {
		t.Error("a note whose status changed away from archived is still being withheld")
	}
}

// The vault that decides what reaches the system prompt. Two notes carry a rule, and the other
// three are the shapes that must not: a preference recording a fact rather than an instruction,
// an archived preference, and a note of another type that happens to carry the field.
var preferenceNotes = map[string]string{
	"Preferences/Metric Units.md": `---
type: preference
status: active
rule: "Give measurements in metric; convert imperial figures and keep the original in brackets."
---
# Metric Units

The user reads kilometres and grams more easily than miles and ounces.
`,

	"Preferences/Check The Changelog First.md": `---
type: preference
status: active
tags: [pref/workflow, pref/code]
rule: "Read the changelog before upgrading a dependency, rather than after the build breaks."
---
# Check The Changelog First

Written after several sessions ignored conventions they had never retrieved.
`,

	"Preferences/Plans In Fortnights.md": `---
type: preference
status: active
---
# Plans In Fortnights

A fact about how the user plans, not an instruction anyone could follow.
`,

	"Archive/Draft Pull Requests.md": `---
type: preference
status: archived
rule: "Always open a pull request as a draft first."
---
# Draft Pull Requests

Superseded by the stacking workflow.
`,

	"Systems/Scheduler.md": `---
type: system
rule: "Restart the scheduler from its runbook."
---
# Scheduler

The timezone handling flakes every spring.
`,
}

// A rule is for injection, not for matching. Frontmatter is stripped before indexing so that
// notes sharing a tag or a date do not all rank for it, and a rule let through would give every
// preference note a second body — written, worse, in exactly the imperative words a question
// about the user's preferences is phrased in.
func TestAPreferenceRuleIsNotFullTextSearchable(t *testing.T) {
	index, _ := tempIndex(t, preferenceNotes)
	refresh(t, index)

	for _, query := range []string{"imperial figures", "keep the original in brackets"} {
		if hits := search(t, index, query); len(hits) != 0 {
			t.Errorf("%q matched on rule text that only frontmatter holds: %+v", query, hits)
		}
	}
	// The note itself stays as findable as any other, so this is a property of the rule and not
	// of the note having been withheld.
	hits := search(t, index, "kilometres and grams")
	if len(hits) == 0 || hits[0].Path != "Preferences/Metric Units.md" {
		t.Errorf("the preference note's own prose is no longer findable: %+v", hits)
	}
}

// Frontmatter is read by scanning lines rather than with a YAML parser: three keys do not justify
// a dependency, and the scan is the more tolerant reader — one note in this vault defines the same
// key twice, which a strict parse rejects outright.
func TestFrontmatterIsReadByScanningLines(t *testing.T) {
	for _, c := range []struct {
		name, text, status, kind, rule string
	}{
		{"plain", "---\ntype: system\nstatus: archived\n---\nbody\n", "archived", "system", ""},
		{"quoted and padded", "---\nstatus: \"archived\"  \ntype: 'system'\n---\n", "archived", "system", ""},
		{"duplicated key", "---\nstatus: active\ntype: system\nstatus: archived\n---\n", "archived", "system", ""},
		{"nested under another key", "---\ntype: system\nlinear:\n  status: archived\n---\n", "", "system", ""},
		{"list item", "---\ntype: system\ntags:\n- status: archived\n---\n", "", "system", ""},
		{"no value", "---\nstatus:\ntype: system\n---\n", "", "system", ""},
		{"no frontmatter", "# Note\n\nstatus: archived\n", "", "", ""},
		{"unterminated", "---\nstatus: archived\n", "", "", ""},
		// A rule is written as a quoted YAML string, and the sentence inside it may hold the
		// colons and apostrophes of ordinary prose.
		{"rule", "---\ntype: preference\nrule: \"Ask first.\"\n---\n", "", "preference", "Ask first."},
		{"rule with a colon in it", "---\nrule: \"Say one thing: the deadline.\"\n---\n",
			"", "", "Say one thing: the deadline."},
		{"rule left empty", "---\ntype: preference\nrule:\n---\n", "", "preference", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := parseFrontmatter(c.text)
			if got.status != c.status || got.kind != c.kind || got.rule != c.rule {
				t.Errorf("parseFrontmatter = %+v, want status %q type %q rule %q",
					got, c.status, c.kind, c.rule)
			}
		})
	}
}

func TestSearchRefusesAQueryWithNothingToMatchOn(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)

	// Dropping stopwords and two-letter words is what keeps a question from matching every note
	// in the vault; a query made only of those has to say so rather than return a ranking of
	// noise.
	if _, err := index.Search("why was the", 5); err == nil {
		t.Error("a query of nothing but stopwords was answered")
	}
}

func TestSearchRefreshesWithoutBeingAsked(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	refresh(t, index)

	writeNoteFile(t, vault, "Inbox/Late.md", "# Late\n\nThe zucchini order never arrived.\n")

	hits := search(t, index, "zucchini")
	if len(hits) != 1 || hits[0].Path != "Inbox/Late.md" {
		t.Errorf("a note written after the last refresh was not found: %+v", hits)
	}
}

func TestReadSectionReturnsOneSection(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)

	body, err := index.ReadSection("Systems/Recipe Scraper.md", "Outcome")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "escalation path") {
		t.Errorf("section body = %q", body)
	}
	if strings.Contains(body, "make install") {
		t.Errorf("the section bled into the next one: %q", body)
	}
	// The agent copies a heading out of a search hit, where case is whatever the note used.
	lower, err := index.ReadSection("Systems/Recipe Scraper.md", "outcome")
	if err != nil || lower != body {
		t.Errorf("heading match is case sensitive: %v", err)
	}
}

func TestReadSectionNamesTheHeadingsItHasInstead(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)

	_, err := index.ReadSection("Systems/Recipe Scraper.md", "Why it died")
	if err == nil {
		t.Fatal("an unknown heading was answered")
	}
	// The caller has to be able to retry from the error alone, without reading the whole note.
	for _, want := range []string{"Outcome", "Running it locally"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not list %q: %v", want, err)
		}
	}

	if _, err := index.ReadSection("Systems/Nothing Here.md", "Outcome"); err == nil {
		t.Error("an unknown note was answered")
	} else if !strings.Contains(err.Error(), "Systems/Nothing Here.md") {
		t.Errorf("error does not name the note: %v", err)
	}
}

// With no vault configured the whole layer has to refuse rather than index the current
// directory, and the caller falls back to the search it already had.
func TestOpenIndexWithoutAVaultRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if index, err := OpenIndex(""); err == nil {
		index.Close()
		t.Error("an index was opened over no vault at all")
	}
}

// The vault is the user's Obsidian folder, kept clean by hand. The index is derived state and
// belongs in the cache, where deleting it costs one rebuild and nothing else.
func TestTheIndexIsNeverWrittenInsideTheVault(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)

	before := vaultListing(t, vault)
	refresh(t, index)
	search(t, index, "escalation path")
	if after := vaultListing(t, vault); !slices.Equal(before, after) {
		t.Errorf("indexing changed the vault:\nbefore %q\nafter  %q", before, after)
	}

	db := filepath.Join(os.Getenv("HOME"), paths.Cache, "vault-index.db")
	if _, err := os.Stat(db); err != nil {
		t.Errorf("no index at %s: %v", db, err)
	}
}

func vaultListing(t *testing.T, vault string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(vault, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(vault, path)
		if err != nil {
			return err
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	return found
}
