package vault

import (
	"path/filepath"
	"testing"
)

// The index path is keyed to the cache directory, not to the vault, so two vaults would
// otherwise share one file: every note of the first looks deleted and every note of the second
// looks new, on every alternating search. The stamp turns that into one clean rebuild.
func TestPointingTheIndexAtASecondVaultRebuildsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first := tempVault(t)
	writeNoteFile(t, first, "Systems/Only In First.md",
		"---\ntype: system\n---\n# Only In First\n\nthe scheduling service flakes every spring\n")

	a, err := OpenIndex(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(search(t, a, "scheduling service flakes")) == 0 {
		t.Fatal("precondition failed: the first vault's note is not searchable")
	}
	a.Close()

	// A different vault, same index file.
	second := tempVault(t)
	writeNoteFile(t, second, "Systems/Only In Second.md",
		"---\ntype: system\n---\n# Only In Second\n\nthe billing exporter retries twice\n")

	b, err := OpenIndex(second)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Opening alone must have cleared the first vault's rows: a stale note still answering
	// searches is worse than a slow rebuild, because nothing downstream can tell it is stale.
	var notes int
	if err := b.db.QueryRow(`select count(*) from notes`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 0 {
		t.Errorf("%d rows survived the switch to a different vault, want 0", notes)
	}

	if _, err := b.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(search(t, b, "billing exporter retries")) == 0 {
		t.Error("the second vault's own note is not searchable after the rebuild")
	}
	if hits, err := b.Search("scheduling service flakes", 5); err != nil {
		t.Fatal(err)
	} else if len(hits) > 0 {
		t.Errorf("the first vault's note is still being served from the second vault's index: %v", hits)
	}
}

// Reopening the same vault must NOT rebuild — the stamp is a correctness check, not a reason to
// throw away a good index on every tool call.
func TestReopeningTheSameVaultKeepsTheIndex(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := index.db.QueryRow(`select count(*) from sections`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	index.Close()

	again, err := OpenIndex(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()

	var after int
	if err := again.db.QueryRow(`select count(*) from sections`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("reopening the same vault changed the section count from %d to %d", before, after)
	}
	// And the reopen did no work at all: a refresh right after it must find nothing to do.
	stats, err := again.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Indexed != 0 || stats.Reindexed != 0 {
		t.Errorf("reopening rebuilt the index: %+v", stats)
	}
}

// A vault whose path differs only by a trailing separator is the same vault. Treating it as a
// different one would rebuild the whole index whenever a caller normalised the path differently.
func TestTheStampComparesTheResolvedPath(t *testing.T) {
	index, vault := tempIndex(t, fakeNotes)
	if _, err := index.Refresh(); err != nil {
		t.Fatal(err)
	}
	index.Close()

	// filepath.Clean is what every caller's path has already been through — Root() resolves
	// symlinks, and Resolve cleans. This asserts the stamp is stored in that same form.
	again, err := OpenIndex(filepath.Clean(vault))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()

	stats, err := again.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Indexed != 0 {
		t.Errorf("a cleaned path was treated as a different vault: %+v", stats)
	}
}
