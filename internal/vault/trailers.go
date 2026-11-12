package vault

// Removing the hand-maintained `## Related` trailers, now that the links table holds what they
// held.
//
// 511 notes carry one and they hold about 4,291 wikilinks between them — 36% of the vault's link
// graph, curated by hand over years, and the vault has no version control. So this file is written
// for the failure it must not have: it plans the whole run before it writes a byte, it refuses to
// start unless every link it is about to delete is already in the links table, and it skips
// anything that is not unambiguously a bare catalogue. A trailer wrongly skipped costs a human
// five minutes; a trailer wrongly deleted is gone.
//
// Nothing here infers. The only question it answers is "is this section nothing but links, and is
// every one of them already recorded", and every answer of no is a skip.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

// A note cannot sensibly hold more trailers than this, and the loop that cuts them one at a time
// needs a bound rather than a promise.
const maxTrailersPerNote = 8

// TrailerCut is one section this run would remove, as bytes and as the links it held.
type TrailerCut struct {
	Heading string
	Text    string
	Links   []string
	// Context is the catalogue line each link sat on, index-matched to Links. Compared against the
	// links table along with the target, because the gloss after the em dash is authored text that
	// the removal deletes.
	Context []string
}

// TrailerPlan is what the run would do to one note. Before and After are whole files, because
// that is what Apply verifies and writes: a plan that described the change rather than holding it
// could be applied to a file that had moved on.
type TrailerPlan struct {
	Path   string
	Before string
	After  string
	Cuts   []TrailerCut
}

// Links is every target the plan would delete from the note.
func (p TrailerPlan) Links() []string {
	var out []string
	for _, cut := range p.Cuts {
		out = append(out, cut.Links...)
	}
	return out
}

func (p TrailerPlan) bytes() int { return len(p.Before) - len(p.After) }

// TrailerSkip is a note left alone, and why. Every one of these is for a human to read: the
// reasons are the shapes this tool refuses to judge.
type TrailerSkip struct {
	Path   string
	Reason string
}

// TrailerRun is a planned run, and after Apply the run that happened.
type TrailerRun struct {
	Vault    string
	Notes    int
	Trailers int
	Plans    []TrailerPlan
	Skipped  []TrailerSkip

	// Archive is where Apply writes the removed trailers before it writes anything else, named in
	// the dry run so that whoever reads the report knows where to go if the edit was wrong.
	Archive string

	// Applied is how many files were written, and stays zero for the dry run — which is every run
	// that does not explicitly ask otherwise.
	Applied int
}

func (r TrailerRun) Links() int {
	n := 0
	for _, plan := range r.Plans {
		n += len(plan.Links())
	}
	return n
}

func (r TrailerRun) Bytes() int {
	n := 0
	for _, plan := range r.Plans {
		n += plan.bytes()
	}
	return n
}

// PlanTrailerRemoval works out what the run would change, and changes nothing.
//
// It takes an open index rather than a vault path so that the capture check and the files on disk
// cannot be about different vaults, and it refreshes first: a verification against a stale index
// would pass on link rows describing text the note no longer holds.
func PlanTrailerRemoval(x *Index) (TrailerRun, error) {
	run := TrailerRun{Vault: x.vault, Archive: trailerArchivePath()}
	// A refresh that could not read the whole vault leaves some notes indexed from older bytes,
	// and the capture check is only worth anything against current ones.
	if _, err := x.Refresh(); err != nil {
		return run, fmt.Errorf("the index could not be brought up to date, so no deletion is safe: %w", err)
	}
	if err := x.db.QueryRow(`select count(*) from notes`).Scan(&run.Notes); err != nil {
		return run, err
	}

	captured, err := capturedTrailerLinks(x)
	if err != nil {
		return run, err
	}
	candidates, err := trailerCandidates(x)
	if err != nil {
		return run, err
	}
	run.Trailers = len(candidates)

	for _, rel := range candidates {
		if reason := indexNoteReason(rel); reason != "" {
			run.Skipped = append(run.Skipped, TrailerSkip{Path: rel, Reason: reason})
			continue
		}
		data, err := os.ReadFile(filepath.Join(x.vault, rel))
		if err != nil {
			// Unreadable is not a reason to proceed on the rest and quietly leave this one: the
			// file is in the plan because the index says it holds a trailer.
			return run, fmt.Errorf("%s is indexed with a link trailer but could not be read: %w", rel, err)
		}
		before := string(data)
		after, cuts, reason := stripTrailers(before)
		if reason != "" {
			run.Skipped = append(run.Skipped, TrailerSkip{Path: rel, Reason: reason})
			continue
		}
		if len(cuts) == 0 {
			continue
		}
		// The refusal the whole tool is built around. 4,291 curated links are 36% of the graph,
		// and a link that is not in the table would be deleted from the only place it exists.
		if missing := unaccounted(cuts, captured[rel]); missing != "" {
			// The whole plan goes, not just this note: a partial plan is one a caller could apply,
			// and the reason to refuse is that the extraction cannot be trusted anywhere.
			return TrailerRun{Vault: x.vault, Archive: run.Archive},
				fmt.Errorf("%s: the trailer link %s is not in the links table, so removing the "+
					"trailer would destroy it — nothing was changed; rebuild the index "+
					"(/vault rebuild) and plan again", rel, missing)
		}
		run.Plans = append(run.Plans, TrailerPlan{Path: rel, Before: before, After: after, Cuts: cuts})
	}
	return run, nil
}

// Apply writes the planned files, and nothing else. Every file is re-read and checked against the
// plan before any write happens, so a vault edited between planning and applying costs the run
// rather than the note.
func (r *TrailerRun) Apply() error {
	for _, plan := range r.Plans {
		data, err := os.ReadFile(filepath.Join(r.Vault, plan.Path))
		if err != nil {
			return fmt.Errorf("%s could not be re-read, so nothing was written: %w", plan.Path, err)
		}
		if string(data) != plan.Before {
			return fmt.Errorf("%s changed since the dry run, so nothing was written — plan again", plan.Path)
		}
	}
	// Every trailer goes to the archive before the first file is touched, and a failure there
	// stops the run. The links table cannot stand in for this: it is derived from the markdown, so
	// the moment a trailer leaves the file its rows leave the index on the next refresh.
	if len(r.Plans) > 0 {
		if err := archiveTrailers(r.Archive, r.Plans); err != nil {
			return fmt.Errorf("the removed trailers could not be archived to %s, so nothing was written: %w",
				r.Archive, err)
		}
	}
	for _, plan := range r.Plans {
		if err := replaceFile(filepath.Join(r.Vault, plan.Path), plan.After); err != nil {
			return fmt.Errorf("%s: %w (%d of %d notes were already written)", plan.Path, err, r.Applied, len(r.Plans))
		}
		r.Applied++
	}
	return nil
}

// archivedTrailer is one removed trailer, written so that putting it back is a paste rather than a
// reconstruction.
type archivedTrailer struct {
	Path      string   `json:"path"`
	Heading   string   `json:"heading"`
	Text      string   `json:"text"`
	Links     []string `json:"links"`
	Context   []string `json:"context"`
	RemovedAt string   `json:"removed_at"`
}

// trailerArchivePath is deliberately not under ~/.cache and not inside the vault. The cache is
// throwaway by design — the index rebuilds from the markdown in 200 ms — and a removed trailer is
// the one thing here that stops being recomputable the moment it is written: the links table is
// derived from the very text this deletes. The vault is out because the vault is what is being
// edited.
func trailerArchivePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, paths.Share, "removed-trailers.jsonl")
}

// archiveTrailers appends every planned cut, fsynced, in one write before any note is touched.
// Append-only JSON lines rather than a database: recovery is somebody reading a file at a moment
// when they have just lost something, and the format has to be readable with no tooling at all.
func archiveTrailers(path string, plans []TrailerPlan) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	when := time.Now().UTC().Format(time.RFC3339)
	var batch strings.Builder
	for _, plan := range plans {
		for _, cut := range plan.Cuts {
			line, err := json.Marshal(archivedTrailer{
				Path: plan.Path, Heading: cut.Heading, Text: cut.Text,
				Links: cut.Links, Context: cut.Context, RemovedAt: when,
			})
			if err != nil {
				return err
			}
			batch.Write(line)
			batch.WriteString("\n")
		}
	}
	if _, err := file.WriteString(batch.String()); err != nil {
		return err
	}
	return file.Sync()
}

// replaceFile writes through a temporary file in the same directory and renames it over the
// original, so a note is never half-written: the vault has no version control, and a crash
// mid-write would leave the user with a truncated note and no way back.
func replaceFile(path, content string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".trailer-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// trailerCandidates is every note the index says holds a bare link trailer. Asked of the index
// rather than of the disk so that the files considered are exactly the files the capture check
// covers.
func trailerCandidates(x *Index) ([]string, error) {
	rows, err := x.db.Query(`select distinct n.path from sections s join notes n on n.id = s.note_id
		where not s.searchable order by n.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

// capturedTrailerLinks is what the links table holds for every trailer in the vault: the target as
// written, and the catalogue line it was written on. Both are compared, because the gloss after
// the em dash is authored text too and the trailer removal deletes it.
func capturedTrailerLinks(x *Index) (map[string]map[string]bool, error) {
	rows, err := x.db.Query(`select n.path, l.target, l.context from links l
		join notes n on n.id = l.note_id where l.role = ?`, roleRelated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	captured := map[string]map[string]bool{}
	for rows.Next() {
		var rel, target, context string
		if err := rows.Scan(&rel, &target, &context); err != nil {
			return nil, err
		}
		if captured[rel] == nil {
			captured[rel] = map[string]bool{}
		}
		captured[rel][target+"\x00"+context] = true
	}
	return captured, rows.Err()
}

// unaccounted names the first trailer link the index does not hold, or "" when every one of them
// is recorded with the line it sat on.
func unaccounted(cuts []TrailerCut, captured map[string]bool) string {
	for _, cut := range cuts {
		for i, target := range cut.Links {
			if !captured[target+"\x00"+cut.Context[i]] {
				return fmt.Sprintf("%q (on the line %q)", target, cut.Context[i])
			}
		}
	}
	return ""
}

// indexNoteReason refuses the hand-maintained folder index notes, such as People/People.md.
// Their link lists are the note's whole content and were written on purpose,
// which is the opposite of a trailer. Matched by shape rather than by a list of names so a
// new folder is covered the day it is made.
func indexNoteReason(rel string) string {
	dir, file := filepath.Split(rel)
	if dir != "" && strings.TrimSuffix(file, ".md") == strings.TrimSuffix(dir, "/") {
		return "folder index note, hand-maintained"
	}
	return ""
}

// stripTrailers removes every bare link trailer from one file, or explains why it removed none.
//
// One trailer at a time, re-reading the whole file each pass: offsets after a cut are the reason
// multi-pass editing goes wrong, and a note with two trailers is rare enough that a second parse
// costs nothing. The same property makes the tool idempotent — a second run finds no trailer and
// cuts nothing.
func stripTrailers(raw string) (string, []TrailerCut, string) {
	var cuts []TrailerCut
	content := raw
	for range maxTrailersPerNote {
		next, cut, reason := stripFirstTrailer(content)
		if reason != "" {
			return raw, nil, reason
		}
		if cut == nil {
			return content, cuts, ""
		}
		content = next
		cuts = append(cuts, *cut)
	}
	return raw, nil, fmt.Sprintf("more than %d link trailers, which is not a shape this tool knows", maxTrailersPerNote)
}

func stripFirstTrailer(raw string) (string, *TrailerCut, string) {
	body := stripFrontmatter(raw)
	// len(raw)-len(body) is how section offsets become file offsets, and it only holds while the
	// body is a literal suffix of the file. It is — stripFrontmatter trims a prefix and nothing
	// else — but the cut is irreversible, so the assumption is checked rather than trusted.
	if !strings.HasSuffix(raw, body) {
		return raw, nil, "the frontmatter and the body do not line up, so byte offsets cannot be trusted"
	}
	prefix := len(raw) - len(body)

	sections := splitSections(body)
	at := 0
	for i, s := range sections {
		start, end := at, at+len(s.body)
		at = end
		if !isLinkTrailer(s.heading) {
			continue
		}
		links, contexts, reason := trailerContents(s.body)
		if reason != "" {
			return raw, nil, reason
		}
		if len(links) == 0 {
			// An empty trailer is cruft rather than a catalogue, and removing it would preserve
			// nothing. Left for a human, like everything else this tool is unsure about.
			return raw, nil, "the trailer holds no links"
		}
		if i+1 < len(sections) && headingLevel(sections[i+1].body) > headingLevel(s.body) {
			return raw, nil, "a sub-heading sits under the trailer, which removing it would orphan"
		}
		// A note that is nothing but its trailer would be left empty, which is a judgement about
		// whether the note should exist at all — not this tool's to make.
		if strings.TrimSpace(body[:start]) == "" && end == len(body) {
			return raw, nil, "the trailer is the whole note"
		}

		out := raw[:prefix+start] + raw[prefix+end:]
		if end == len(body) {
			// Last in the file: the cut takes the blank line that separated the trailer from the
			// text above with it, and the file keeps whatever newline convention it had.
			out = strings.TrimRight(raw[:prefix+start], "\n")
			if strings.HasSuffix(raw, "\n") {
				out += "\n"
			}
		}
		return out, &TrailerCut{Heading: s.heading, Text: s.body, Links: links, Context: contexts}, ""
	}
	return raw, nil, ""
}

// trailerContents reads a trailer's links, with the line each sat on, and refuses anything that is
// not a bare catalogue.
//
// What passes: blank lines, a list item holding at least one wikilink — the gloss after the em
// dash is part of the item and comes out with it — and a line that is nothing but wikilinks. What
// does not: a sentence, a list item with no link in it, a fence, a table, a quote. Erring towards
// refusal is the point; the measured cost of a wrong refusal is a human reading one note.
func trailerContents(section string) (links, contexts []string, reason string) {
	lines := strings.Split(section, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i == 0 {
			// The heading line itself, already known to be a bare Related or See also.
			continue
		}
		if trimmed == "" {
			continue
		}
		item, isItem := listItem(trimmed)
		if !isItem {
			item = trimmed
		}
		found := wikilinkRe.FindAllStringSubmatch(item, -1)
		if len(found) == 0 {
			return nil, nil, fmt.Sprintf("the trailer holds something that is not a link: %q", trimmed)
		}
		if !isItem && strings.TrimSpace(wikilinkRe.ReplaceAllString(item, "")) != "" {
			// A bare link line may hold links and nothing else. Prose around one is an argument,
			// and an argument is not a catalogue.
			return nil, nil, fmt.Sprintf("the trailer holds prose around a link: %q", trimmed)
		}
		for _, m := range found {
			target := m[1]
			if i := strings.Index(target, "|"); i >= 0 {
				target = target[:i]
			}
			target, _, _ = strings.Cut(target, "#")
			links = append(links, strings.TrimSpace(target))
			contexts = append(contexts, trimmed)
		}
	}
	return links, contexts, ""
}

// listItem strips a list marker, reporting whether there was one.
func listItem(trimmed string) (string, bool) {
	for _, marker := range []string{"- ", "* ", "+ "} {
		if rest, ok := strings.CutPrefix(trimmed, marker); ok {
			return rest, true
		}
	}
	if listItemRe.MatchString(trimmed) {
		return listItemRe.ReplaceAllString(trimmed, ""), true
	}
	return trimmed, false
}

func headingLevel(sectionBody string) int {
	line, _, _ := strings.Cut(sectionBody, "\n")
	return len(line) - len(strings.TrimLeft(line, "#"))
}

// Report is the dry run's whole output, and the applied run's receipt. The sample is there because
// a count is not a review: anyone about to delete 4,291 links out of a vault with no version
// control should see what the edit looks like first.
func (r TrailerRun) Report(sample int) string {
	var b strings.Builder
	if r.Applied > 0 {
		fmt.Fprintf(&b, "Applied to %s.\n", r.Vault)
	} else {
		fmt.Fprintf(&b, "Dry run over %s. Nothing was changed.\n", r.Vault)
	}
	fmt.Fprintf(&b, "  %d notes indexed, %d carry a bare link trailer.\n", r.Notes, r.Trailers)
	fmt.Fprintf(&b, "  strip    %d notes, %d links preserved, %d bytes removed\n",
		len(r.Plans), r.Links(), r.Bytes())
	fmt.Fprintf(&b, "  skip     %d notes\n", len(r.Skipped))
	for _, skip := range r.Skipped {
		fmt.Fprintf(&b, "    %s — %s\n", skip.Path, skip.Reason)
	}
	// Said in the report rather than left to be discovered: the links table is derived from the
	// markdown, so the catalogue entries leave the live graph along with the trailer. What survives
	// is the prose links — and the archive, which is the only copy of what was deleted.
	fmt.Fprintf(&b, "  archive  %s — every removed trailer is written there verbatim before any file "+
		"is touched.\n           The live graph keeps the prose links; the catalogue entries leave it "+
		"with the trailer,\n           so the archive is the only copy of the lines below.\n", r.Archive)

	shown := min(sample, len(r.Plans))
	if shown > 0 {
		fmt.Fprintf(&b, "\n  sample, %d of %d notes:\n", shown, len(r.Plans))
	}
	for _, plan := range r.Plans[:shown] {
		fmt.Fprintf(&b, "\n  %s — %d links preserved, %d bytes\n", plan.Path, len(plan.Links()), plan.bytes())
		for _, cut := range plan.Cuts {
			for _, line := range strings.Split(strings.TrimRight(cut.Text, "\n"), "\n") {
				fmt.Fprintf(&b, "    - %s\n", line)
			}
		}
	}
	if r.Applied == 0 && len(r.Plans) > 0 {
		fmt.Fprintf(&b, "\nRe-run with -apply to write the %d notes above.\n", len(r.Plans))
	}
	return b.String()
}
