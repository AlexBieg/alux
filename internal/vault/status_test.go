package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Status must not refresh. The question it exists to answer is "is the index stale", and a
// reporter that quietly brings the index up to date can only ever answer no.
func TestStatusReportsStalenessWithoutFixingIt(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	writeNoteFile(t, vault, "Systems/Brand New.md", "---\ntype: system\n---\n# Brand New\n\nunindexed\n")

	first, err := index.Status()
	if err != nil {
		t.Fatal(err)
	}
	if first.Missing != 1 {
		t.Errorf("Missing = %d, want 1 for the note added since the last refresh", first.Missing)
	}

	second, err := index.Status()
	if err != nil {
		t.Fatal(err)
	}
	if second.Missing != 1 {
		t.Errorf("second Status reports Missing = %d: the first one indexed the note instead of reporting it", second.Missing)
	}
	if second.Notes != first.Notes {
		t.Errorf("note count moved from %d to %d across two Status calls", first.Notes, second.Notes)
	}
}

// The three pending states are reported separately because they mean different things: a new
// note is a gap, a changed note is drift, and an orphan is a deletion the index has not applied.
func TestStatusSeparatesNewChangedAndDeleted(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}

	clean, err := index.Status()
	if err != nil {
		t.Fatal(err)
	}
	if clean.Missing+clean.Stale+clean.Orphaned != 0 {
		t.Fatalf("freshly refreshed index reports pending work: %+v", clean)
	}
	if !strings.Contains(clean.String(), "up to date") {
		t.Errorf("a current index should say so:\n%s", clean.String())
	}

	writeNoteFile(t, vault, "Systems/Added.md", "---\ntype: system\n---\n# Added\n\nnew\n")
	writeNoteFile(t, vault, "Glossary/Taint.md", fakeNotes["Glossary/Taint.md"]+"\nextra text\n")
	touch(t, vault, "Glossary/Taint.md")
	if err := os.Remove(filepath.Join(vault, "People/Sam Weiss.md")); err != nil {
		t.Fatal(err)
	}

	got, err := index.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got.Missing != 1 || got.Stale != 1 || got.Orphaned != 1 {
		t.Errorf("Missing=%d Stale=%d Orphaned=%d, want 1 each", got.Missing, got.Stale, got.Orphaned)
	}
	line := got.String()
	for _, want := range []string{"1 new", "1 changed", "1 deleted"} {
		if !strings.Contains(line, want) {
			t.Errorf("report missing %q:\n%s", want, line)
		}
	}
}

// A note indexed with no sections matches nothing, which looks from the outside exactly like a
// note that was never filed. The report has to name them or that failure stays invisible.
func TestStatusNamesNotesThatCouldNotBeRead(t *testing.T) {
	unprivileged(t)
	index, vault := tempIndex(t, fakeNotes)

	locked := filepath.Join(vault, "Glossary/Taint.md")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o644) })

	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	got, err := index.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got.Empty != 1 {
		t.Fatalf("Empty = %d, want 1 for the unreadable note", got.Empty)
	}
	if !strings.Contains(got.String(), "unreadable") {
		t.Errorf("report does not mention unreadable notes:\n%s", got.String())
	}
}

// Rebuild is the one repair the command offers, so it has to work against an index that is
// populated but wrong — which is the state a user reaches for it in.
func TestRebuildRestoresASabotagedIndex(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(search(t, index, "scrapped triage soups")) == 0 {
		t.Fatal("precondition failed: the healthy index does not answer the query")
	}

	// Sections gone, notes left behind with their staleness keys intact — so an ordinary
	// refresh sees nothing to do and the vault appears to have forgotten everything.
	if _, err := index.db.Exec(`delete from sections`); err != nil {
		t.Fatal(err)
	}
	if _, err := index.db.Exec(`insert into sections_fts(sections_fts) values('rebuild')`); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	if hits := search(t, index, "scrapped triage soups"); len(hits) != 0 {
		t.Fatalf("precondition failed: expected the sabotaged index to answer nothing, got %d hits", len(hits))
	}

	stats, err := index.Rebuild()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Indexed != len(fakeNotes) {
		t.Errorf("rebuild indexed %d notes, want %d", stats.Indexed, len(fakeNotes))
	}
	if len(search(t, index, "scrapped triage soups")) == 0 {
		t.Error("rebuild did not restore searchability")
	}
	if _, err := index.db.Exec(`insert into sections_fts(sections_fts) values('integrity-check')`); err != nil {
		t.Errorf("FTS index corrupt after rebuild: %v", err)
	}
}

// The schema stamp has to survive a rebuild: losing it would make the next session think the
// index came from an older build and throw away the work that just finished.
func TestRebuildKeepsTheSchemaStamp(t *testing.T) {
	index, _ := tempIndex(t, fakeNotes)
	if _, err := index.Rebuild(); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := index.db.QueryRow(`pragma user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("after rebuild user_version = %d, want %d", version, schemaVersion)
	}
}
