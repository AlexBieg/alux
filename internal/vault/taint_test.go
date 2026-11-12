package vault

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/taint"
)

// Two notes that differ in one line of frontmatter and share no words, so a search can be aimed
// at either one of them.
var markedNotes = map[string]string{
	"People/Travel Notes.md": `---
type: person
sensitivity: phi
---
# Travel Notes

## Booking

The tax notice was forwarded to the Tuesday meeting.
`,

	"Systems/Scheduler.md": `---
type: system
---
# Scheduler

## Timezones

The scheduler flakes every spring when the clocks move.
`,
}

const (
	markedNote   = "People/Travel Notes.md"
	ordinaryNote = "Systems/Scheduler.md"
)

// The latch is process-wide, so a test that does not start from a known state passes or fails on
// what ran before it. That is also why nothing here runs in parallel.
func untainted(t *testing.T) {
	t.Helper()
	taint.ResetForTest()
	t.Cleanup(taint.ResetForTest)
}

// Reading a marked note is the case the whole latch exists for: afterwards nothing in this
// process may reach a service with no agreement covering it.
func TestReadingAMarkedNoteLatchesTheProcess(t *testing.T) {
	untainted(t)
	_, vault := tempIndex(t, markedNotes)

	if _, err := Read(vault, ordinaryNote, "", 0); err != nil {
		t.Fatal(err)
	}
	if taint.Tainted() {
		t.Fatal("an ordinary note latched the process, which would cost the session its egress for nothing")
	}

	if _, err := Read(vault, markedNote, "", 0); err != nil {
		t.Fatal(err)
	}
	if !taint.Tainted() {
		t.Error("reading a note marked sensitive did not latch")
	}
}

// The section read is the cheap route and the one search points at, so it cannot be the route
// that reads a marked note without saying so.
func TestASectionReadLatchesLikeAWholeRead(t *testing.T) {
	untainted(t)
	index, vault := tempIndex(t, markedNotes)
	refresh(t, index)

	if _, err := Read(vault, ordinaryNote, "Timezones", 0); err != nil {
		t.Fatal(err)
	}
	if taint.Tainted() {
		t.Fatal("a section of an ordinary note latched the process")
	}

	text, err := Read(vault, markedNote, "Booking", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Tuesday meeting") {
		t.Fatalf("the section did not come back, so this asserts nothing:\n%s", text)
	}
	if !taint.Tainted() {
		t.Error("reading a section of a note marked sensitive did not latch")
	}
}

// The section branch asks the index a second question after ReadSection, and answers an error
// the safe way — so every spelling that fails somewhere in that sequence is a chance to latch a
// vault holding nothing marked, which would cost the session its egress for no reason.
func TestNoSpellingOfAPathLatchesACleanVault(t *testing.T) {
	untainted(t)
	index, vault := tempIndex(t, map[string]string{ordinaryNote: markedNotes[ordinaryNote]})
	refresh(t, index)

	for _, c := range []struct{ name, path, section string }{
		{"whole note", ordinaryNote, ""},
		{"section", ordinaryNote, "Timezones"},
		{"section in the wrong case", ordinaryNote, "timezones"},
		{"no extension", "Systems/Scheduler", "Timezones"},
		{"rooted at the vault", "/" + ordinaryNote, "Timezones"},
		{"no such note", "Systems/Nope.md", "Timezones"},
		{"no such section", ordinaryNote, "No Such Heading"},
	} {
		_, _ = Read(vault, c.path, c.section, 0)
		if taint.Tainted() {
			t.Fatalf("a vault with nothing marked latched on %s", c.name)
		}
	}
	if _, err := Search(vault, "scheduler clocks"); err != nil {
		t.Fatal(err)
	}
	if taint.Tainted() {
		t.Error("a vault with nothing marked latched on a search")
	}
}

// Per hit, not per search: eighteen tokens of a marked note is that note's text in the
// conversation, while a search that returned none of it has nothing to latch on.
func TestASearchLatchesOnlyOnAMarkedHit(t *testing.T) {
	untainted(t)
	index, vault := tempIndex(t, markedNotes)
	refresh(t, index)

	out, err := Search(vault, "scheduler timezones clocks")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ordinaryNote) {
		t.Fatalf("the fixture search found nothing, so this asserts nothing:\n%s", out)
	}
	if taint.Tainted() {
		t.Fatal("a search that returned only ordinary notes latched the process")
	}

	out, err = Search(vault, "tax notice booking")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, markedNote) {
		t.Fatalf("the marked note was not among the hits, so this asserts nothing:\n%s", out)
	}
	if !taint.Tainted() {
		t.Error("a snippet of a note marked sensitive did not latch")
	}
}

// The fallback bypasses the index, so there is no column to consult and the output is lines
// quoted straight out of notes. It reads their frontmatter off the files instead, which is what
// keeps a degraded cache from also being a hole in the latch.
func TestTheRegexFallbackChecksTheFrontmatterOfWhatItQuoted(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep is not installed, and it is the whole of the fallback")
	}
	untainted(t)
	_, vault := tempIndex(t, markedNotes)
	breakTheIndex(t)

	out, err := Search(vault, "clocks")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "section index unavailable") || !strings.Contains(out, ordinaryNote) {
		t.Fatalf("this did not go through the fallback, so it asserts nothing:\n%s", out)
	}
	if taint.Tainted() {
		t.Fatal("the fallback latched on a match in an ordinary note")
	}

	out, err = Search(vault, "tax notice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, markedNote) {
		t.Fatalf("the fallback quoted nothing from the marked note:\n%s", out)
	}
	if !taint.Tainted() {
		t.Error("the fallback quoted a note marked sensitive without latching")
	}
}

// A note the fallback quoted and that cannot then be read is treated as marked: its lines are
// already in the answer, and the one thing that cannot be done is to pass them on as safe.
func TestTheRegexFallbackLatchesOnAQuotedNoteItCannotRead(t *testing.T) {
	untainted(t)
	_, vault := tempIndex(t, markedNotes)

	taintMatchedNotes(vault, "./Systems/Vanished.md:3:a line nobody can check\n")
	if !taint.Tainted() {
		t.Error("a quoted note that could not be read was passed on as unmarked")
	}
}

// The supersession notice quotes a sentence of the claiming note's prose, which is that note's
// text reaching the reader of a different note.
func TestEvidenceQuotedFromAMarkedNoteLatches(t *testing.T) {
	untainted(t)
	index, vault := tempIndex(t, map[string]string{
		"Decisions/Stacked PRs.md": `---
type: decision
status: active
sensitivity: phi
---
# Stacked PRs

We moved to stacked pull requests, and this supersedes [[Triage Rota]] for
anyone triaging imports.
`,
		"Decisions/Triage Rota.md": `---
type: decision
status: active
---
# Triage Rota

Written after the queue stalled overnight.
`,
	})
	refresh(t, index)

	out, err := Read(vault, "Decisions/Triage Rota.md", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unverified") {
		t.Fatalf("no detected notice was rendered, so this asserts nothing:\n%s", out)
	}
	if !taint.Tainted() {
		t.Error("prose quoted from a note marked sensitive did not latch")
	}
}

// One-way is the whole design: a conversation that has seen PHI cannot read its way back out of
// it, so every ordinary read and search after the first marked one leaves the latch set.
func TestTheLatchNeverGoesBack(t *testing.T) {
	untainted(t)
	index, vault := tempIndex(t, markedNotes)
	refresh(t, index)

	if _, err := Read(vault, markedNote, "", 0); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { _, err := Read(vault, ordinaryNote, "", 0); return err },
		func() error { _, err := Read(vault, ordinaryNote, "Timezones", 0); return err },
		func() error { _, err := Search(vault, "scheduler timezones clocks"); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
		if !taint.Tainted() {
			t.Fatal("an ordinary read or search cleared a latch that is supposed to be one-way")
		}
	}
}

// The column is derived state like every other, so what matters is that an edit reaches it and
// that an index written before the column existed rebuilds rather than erroring on a missing one.
func TestTheMarkingSurvivesARefreshAndAStaleIndexIsRebuilt(t *testing.T) {
	untainted(t)
	index, vault := tempIndex(t, markedNotes)
	refresh(t, index)

	if got := sensitivityOf(t, index, markedNote); got != "phi" {
		t.Errorf("sensitivity of the marked note = %q, want %q", got, "phi")
	}
	if got := sensitivityOf(t, index, ordinaryNote); got != "" {
		t.Errorf("sensitivity of an unmarked note = %q, want empty", got)
	}

	// Marking a note that was already indexed goes through the update rather than the insert.
	writeNoteFile(t, vault, ordinaryNote,
		strings.Replace(markedNotes[ordinaryNote], "type: system", "type: system\nsensitivity: phi", 1))
	touch(t, vault, ordinaryNote)
	refresh(t, index)
	if got := sensitivityOf(t, index, ordinaryNote); got != "phi" {
		t.Errorf("a marking added to an indexed note did not reach the column: %q", got)
	}

	// The shape an older build left on disk: the column gone, the stamp behind.
	if _, err := index.db.Exec(`alter table notes drop column sensitivity`); err != nil {
		t.Fatal(err)
	}
	if _, err := index.db.Exec(`pragma user_version = 7`); err != nil {
		t.Fatal(err)
	}
	index.Close()

	reopened, err := OpenIndex(vault)
	if err != nil {
		t.Fatalf("an index from the previous schema was not rebuilt: %v", err)
	}
	defer reopened.Close()
	refresh(t, reopened)

	if got := sensitivityOf(t, reopened, markedNote); got != "phi" {
		t.Errorf("after the rebuild the marking reads %q, want %q", got, "phi")
	}
	hits := search(t, reopened, "tax notice booking")
	if len(hits) == 0 {
		t.Fatal("the rebuilt index found nothing, so the hits below assert nothing")
	}
	for _, h := range hits {
		if h.Sensitive != (h.Path == markedNote) {
			t.Errorf("hit %s reports Sensitive=%v", h.Path, h.Sensitive)
		}
	}
}

// The key the latch turns on, read by the same line scan as every other key. A note that does not
// carry it is not marked, which is every note in the vault today.
func TestTheSensitivityKeyIsReadFromFrontmatter(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"marked", "---\ntype: person\nsensitivity: phi\n---\nbody\n", "phi"},
		{"quoted and padded", "---\nsensitivity: \"phi\"  \n---\n", "phi"},
		{"unmarked", "---\ntype: system\n---\nbody\n", ""},
		{"no value", "---\nsensitivity:\ntype: system\n---\n", ""},
		{"no frontmatter", "# Note\n\nsensitivity: phi\n", ""},
		{"in the body, where a note about the convention writes it", "---\ntype: term\n---\nsensitivity: phi\n", ""},
		{"nested under another key", "---\ntype: system\nlinear:\n  sensitivity: phi\n---\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := parseFrontmatter(c.text).sensitivity; got != c.want {
				t.Errorf("parseFrontmatter(%q).sensitivity = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

func sensitivityOf(t *testing.T, index *Index, rel string) string {
	t.Helper()
	var sensitivity string
	if err := index.db.QueryRow(`select sensitivity from notes where path = ?`, rel).Scan(&sensitivity); err != nil {
		t.Fatal(err)
	}
	return sensitivity
}

// breakTheIndex points HOME at a file, so the cache directory the index lives in cannot be
// created and Search has nothing but the fallback.
func breakTheIndex(t *testing.T) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
}
