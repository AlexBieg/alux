package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A vault holding one of each shape the migration meets: a trailer at the end of a file, a trailer
// in the middle, the em-dash glosses the real ones carry, a file with no closing newline, and the
// four shapes that must be left alone.
var trailerVault = map[string]string{
	"Systems/Scheduler.md": `---
type: system
status: active
---
# Scheduler

## Outcome

Rewritten in June, once the rutabaga backlog stopped draining overnight.

## Related

- [[Mango Exporter]] — the exporter it hands batches to
- [[Rutabaga Backlog]] — why the rewrite happened
`,

	"Systems/Mango Exporter.md": `---
type: system
---
# Mango Exporter

## See also

- [[Scheduler]] — what hands it the batches
- [[Rutabaga Backlog]]

## Outcome

The exporter retries twice and then parks the batch for a human.
`,

	"Systems/Rutabaga Backlog.md": `---
type: system
---
# Rutabaga Backlog

Drained overnight until June.
`,

	"Inbox/Fresh Thought.md": `---
type: note
---
# Fresh Thought

The zucchini order never arrived.

## Related

- [[Scheduler]]`,

	"Decisions/Prose Trailer.md": `---
type: decision
---
# Prose Trailer

## Related

- [[Scheduler]] — upstream
Read the scheduler note for how the overnight drain was measured.
`,

	"Decisions/Sub Heading.md": `---
type: decision
---
# Sub Heading

## Related

- [[Scheduler]]

### Older

- [[Mango Exporter]]
`,

	"People/People.md": `---
type: index
---
# People

- [[Sam Weiss]]

## Related

- [[Systems]]
`,

	"Inbox/Only A Trailer.md": `---
type: note
---
## Related

- [[Scheduler]]
`,

	"Glossary/Taint.md": `---
type: term
---
# Taint

A note is tainted once it has been read in a conversation.
`,
}

func vaultFiles(t *testing.T, vault string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rel := range notePaths(vault) {
		data, err := os.ReadFile(filepath.Join(vault, rel))
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = string(data)
	}
	return out
}

func plannedPaths(run TrailerRun) []string {
	var out []string
	for _, plan := range run.Plans {
		out = append(out, plan.Path)
	}
	return out
}

func plan(t *testing.T, index *Index) TrailerRun {
	t.Helper()
	run, err := PlanTrailerRemoval(index)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// Dry run is not a flag on a tool that writes; it is what the tool does unless told otherwise.
// The vault has no version control, so a run that changed a byte before anyone read its report
// would be unrecoverable.
func TestPlanningChangesNothingOnDisk(t *testing.T) {
	index, vault := tempIndex(t, trailerVault)
	before := vaultFiles(t, vault)

	run := plan(t, index)
	if len(run.Plans) == 0 {
		t.Fatal("nothing was planned over a vault full of trailers")
	}
	if after := vaultFiles(t, vault); !mapsEqual(before, after) {
		t.Error("planning the removal changed the vault")
	}
	report := run.Report(2)
	for _, want := range []string{"Dry run", "Nothing was changed", "-apply", "links preserved"} {
		if !strings.Contains(report, want) {
			t.Errorf("the dry-run report does not say %q:\n%s", want, report)
		}
	}
}

// Err heavily towards skipping: a trailer wrongly left costs a human five minutes, and a trailer
// wrongly deleted is gone. Every shape here is one this tool refuses to judge.
func TestAnythingThatIsNotABareCatalogueIsSkipped(t *testing.T) {
	index, _ := tempIndex(t, trailerVault)
	run := plan(t, index)

	if got, want := plannedPaths(run), []string{
		"Inbox/Fresh Thought.md", "Systems/Mango Exporter.md", "Systems/Scheduler.md",
	}; !slices.Equal(got, want) {
		t.Errorf("planned %q, want %q", got, want)
	}

	reasons := map[string]string{}
	for _, skip := range run.Skipped {
		reasons[skip.Path] = skip.Reason
	}
	for _, c := range []struct{ path, contains string }{
		{"Decisions/Prose Trailer.md", "not a link"},
		{"Decisions/Sub Heading.md", "sub-heading"},
		{"People/People.md", "folder index note"},
		{"Inbox/Only A Trailer.md", "whole note"},
	} {
		if !strings.Contains(reasons[c.path], c.contains) {
			t.Errorf("%s was skipped for %q, want a reason mentioning %q", c.path, reasons[c.path], c.contains)
		}
	}
	if len(reasons) != 4 {
		t.Errorf("skipped %d notes, want 4: %+v", len(reasons), run.Skipped)
	}
}

// Nothing but the trailer may move, and the file's own trailing-newline convention is part of
// "nothing but the trailer".
func TestApplyingRemovesTheTrailerAndNothingElse(t *testing.T) {
	index, vault := tempIndex(t, trailerVault)
	run := plan(t, index)
	if err := run.Apply(); err != nil {
		t.Fatal(err)
	}
	if run.Applied != 3 {
		t.Errorf("Applied = %d, want 3", run.Applied)
	}

	want := map[string]string{
		"Systems/Scheduler.md": `---
type: system
status: active
---
# Scheduler

## Outcome

Rewritten in June, once the rutabaga backlog stopped draining overnight.
`,
		// Mid-file: the heading below the trailer keeps its blank line above it.
		"Systems/Mango Exporter.md": `---
type: system
---
# Mango Exporter

## Outcome

The exporter retries twice and then parks the batch for a human.
`,
		// A file that did not end in a newline must not grow one.
		"Inbox/Fresh Thought.md": `---
type: note
---
# Fresh Thought

The zucchini order never arrived.`,
	}
	files := vaultFiles(t, vault)
	for rel, text := range want {
		if files[rel] != text {
			t.Errorf("%s after apply:\n got %q\nwant %q", rel, files[rel], text)
		}
	}
	for rel, text := range trailerVault {
		if _, changed := want[rel]; changed {
			continue
		}
		if files[rel] != text {
			t.Errorf("%s was changed and should not have been:\n got %q\nwant %q", rel, files[rel], text)
		}
	}

	// The links table is derived from the markdown, so a catalogue entry leaves the graph with the
	// trailer that held it. The prose links are what the graph keeps, and the archive is what makes
	// the deletion recoverable — which is why Apply writes it before it writes a note.
	back, err := index.Backlinks("Systems/Rutabaga Backlog.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Related) != 0 {
		t.Errorf("a trailer that is no longer in the markdown still has %d catalogue backlinks: %+v",
			len(back.Related), back.Related)
	}

	archived := archivedTrailers(t, run.Archive)
	if len(archived) != 3 {
		t.Fatalf("archived %d trailers, want 3: %+v", len(archived), archived)
	}
	// Byte for byte: recovery is pasting this back, not reconstructing it.
	for _, plan := range run.Plans {
		var found bool
		for _, record := range archived {
			if record.Path == plan.Path && record.Text == plan.Cuts[0].Text {
				found = true
			}
		}
		if !found {
			t.Errorf("the archive does not hold %s's trailer verbatim: %+v", plan.Path, archived)
		}
	}
}

func archivedTrailers(t *testing.T, path string) []archivedTrailer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []archivedTrailer
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var record archivedTrailer
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		out = append(out, record)
	}
	return out
}

// Nothing may be written until the archive is on disk and fsynced: if the notes went first, a
// failure to archive would leave the trailers deleted and unrecoverable.
func TestNothingIsWrittenWhenTheArchiveCannotBe(t *testing.T) {
	unprivileged(t)
	index, vault := tempIndex(t, trailerVault)
	run := plan(t, index)

	// The archive's parent directory, made unwritable: the shape of a full disk or a wrong
	// permission, which is exactly when this must not proceed.
	share := filepath.Join(os.Getenv("HOME"), ".local/share")
	if err := os.MkdirAll(share, 0o700); err != nil {
		t.Fatal(err)
	}
	deny(t, share, 0o700)

	before := vaultFiles(t, vault)
	if err := run.Apply(); err == nil {
		t.Fatal("the trailers were deleted without an archive to recover them from")
	} else if !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("the refusal does not say that nothing was written: %v", err)
	}
	if after := vaultFiles(t, vault); !mapsEqual(before, after) {
		t.Error("a run that could not archive wrote to the vault anyway")
	}
}

// Running it twice must be a no-op the second time: whoever runs this against the real vault will
// run it again to check, and a second pass that ate the line above the old trailer would be the
// worst possible surprise.
func TestASecondRunHasNothingToDo(t *testing.T) {
	index, vault := tempIndex(t, trailerVault)
	first := plan(t, index)
	if err := first.Apply(); err != nil {
		t.Fatal(err)
	}
	after := vaultFiles(t, vault)

	second := plan(t, index)
	if len(second.Plans) != 0 {
		t.Errorf("the second run planned %q, want nothing", plannedPaths(second))
	}
	if err := second.Apply(); err != nil {
		t.Fatal(err)
	}
	if again := vaultFiles(t, vault); !mapsEqual(after, again) {
		t.Error("a second run changed the vault")
	}
}

// The refusal the tool exists for. 4,291 curated links are 36% of this vault's graph, so a link
// that is not already in the table must stop the whole run — named, before anything is written.
func TestAnUncapturedLinkAbortsTheRunAndNamesTheFile(t *testing.T) {
	index, vault := tempIndex(t, trailerVault)
	refresh(t, index)

	// The index losing one row is the stand-in for any reason capture could be incomplete: a
	// changed extractor, a partial refresh, a bug in the role split.
	if _, err := index.db.Exec(`delete from links where target = 'Rutabaga Backlog' and role = ?
		and note_id = (select id from notes where path = 'Systems/Scheduler.md')`, roleRelated); err != nil {
		t.Fatal(err)
	}

	before := vaultFiles(t, vault)
	run, err := PlanTrailerRemoval(index)
	if err == nil {
		t.Fatal("a trailer link that is not in the links table was planned for deletion")
	}
	for _, want := range []string{"Systems/Scheduler.md", "Rutabaga Backlog"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if len(run.Plans) != 0 {
		t.Errorf("a refused run still came back with %d plans", len(run.Plans))
	}
	if after := vaultFiles(t, vault); !mapsEqual(before, after) {
		t.Error("the refused run changed the vault")
	}
}

// A vault edited between the dry run and the apply is the one case where the plan is a lie. It
// costs the run, never the note.
func TestApplyRefusesAFileThatMovedOnSinceThePlan(t *testing.T) {
	index, vault := tempIndex(t, trailerVault)
	run := plan(t, index)

	writeNoteFile(t, vault, "Systems/Scheduler.md", trailerVault["Systems/Scheduler.md"]+
		"\n- [[Taint]] — added while the dry run was being read\n")
	before := vaultFiles(t, vault)

	if err := run.Apply(); err == nil {
		t.Fatal("a plan was applied to a file that had changed under it")
	} else if !strings.Contains(err.Error(), "Systems/Scheduler.md") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	if after := vaultFiles(t, vault); !mapsEqual(before, after) {
		t.Error("the refused apply wrote something anyway")
	}
}

// The gloss after the em dash is authored text, and the removal deletes it. It is kept on the link
// row and checked before anything is cut, so the trailer can be reconstructed from the index.
func TestTheCatalogueLineIsPreservedBeforeItIsDeleted(t *testing.T) {
	index, _ := tempIndex(t, trailerVault)
	run := plan(t, index)

	var cut *TrailerCut
	for _, p := range run.Plans {
		if p.Path == "Systems/Scheduler.md" {
			cut = &p.Cuts[0]
		}
	}
	if cut == nil {
		t.Fatal("Systems/Scheduler.md was not planned")
	}
	if want := []string{"Mango Exporter", "Rutabaga Backlog"}; !slices.Equal(cut.Links, want) {
		t.Errorf("cut links = %q, want %q", cut.Links, want)
	}
	if want := "- [[Mango Exporter]] — the exporter it hands batches to"; cut.Context[0] != want {
		t.Errorf("cut context = %q, want %q", cut.Context[0], want)
	}
	if n := countRows(t, index, `select count(*) from links where context = ?`, cut.Context[0]); n != 1 {
		t.Errorf("the catalogue line is in %d link rows, want 1", n)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
