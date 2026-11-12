package vault

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// A vault with the link shapes that matter: a prose link inside an argument, a bare `## Related`
// catalogue with the em-dash glosses the real trailers carry, an aliased link, a link to a
// heading, a link that resolves to nothing, and a link inside a fenced block that is not a link
// at all.
var linkNotes = map[string]string{
	"Decisions/Stacked PRs.md": `---
type: decision
status: active
---
# Stacked PRs

We moved to stacked pull requests after [[Draft Pull Requests]] made every review wait on the
whole branch. The measurements are in [[2026-09-14 Review Queue|the review queue writeup]], and
the vocabulary in [[Glossary/Taint#Why it spreads]].

## Running it

` + "```sh\n# the old script is described in [[Fenced Note]]\nmake stack\n```" + `

## Related

- [[Draft Pull Requests]] — the preference this replaced
- [[Triage Rota]]
`,

	"Preferences/Draft Pull Requests.md": `---
type: preference
status: archived
superseded_by: "[[Kumquat Ledger]]"
valid_from: 2024-02-01
valid_until: 2026-09-14
---
# Draft Pull Requests

Every review waited on the whole branch, so this stopped being the way to open one.
`,

	"Decisions/Import Review Column.md": `---
type: decision
status: active
---
# Import Review Column

The column shipped in March, and this supersedes [[Triage Rota]] for anyone
triaging imports.
`,

	"Decisions/Triage Rota.md": `---
type: decision
status: active
---
# Triage Rota

Written after the queue stalled overnight, it supersedes [[Import Review Column]] as the standing
instruction for a reviewer with an hour.
`,

	"People/Sam Weiss.md": `---
type: person
---
# Sam Weiss

Owns the scheduling service.
`,

	"Glossary/Taint.md": `---
type: term
---
# Taint

## Why it spreads

A note is tainted once it has been read in a conversation.
`,

	// Every sentence here holds the verb and a link, and none is a claim anyone would act on.
	"Explorations/Supersession Noise.md": `---
type: exploration
---
# Supersession Noise

The queue note was superseded. See [[Import Review Column]] for the column work.

Ownership of the superseded tracker sits behind a long paragraph of exactly the kind of filler that
pushes the next wikilink well past the hundred and twenty character window before it ever arrives
at [[Triage Rota]], which is the distance the measurement settled on.

This supersedes [[Sam Weiss]], who is a person and not a decision.

This supersedes [[Nothing Anyone Wrote]].
`,
}

type linkRow struct {
	target   string
	key      string
	anchor   string
	section  string
	role     string
	evidence string
	context  string
}

func linkRows(t *testing.T, index *Index, rel string) []linkRow {
	t.Helper()
	rows, err := index.db.Query(`select l.target, l.target_key, l.anchor, l.section, l.role,
		l.evidence, l.context from links l join notes n on n.id = l.note_id
		where n.path = ? order by l.id`, rel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []linkRow
	for rows.Next() {
		var r linkRow
		if err := rows.Scan(&r.target, &r.key, &r.anchor, &r.section, &r.role, &r.evidence, &r.context); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func countRows(t *testing.T, index *Index, query string, args ...any) int {
	t.Helper()
	var n int
	if err := index.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The measurement behind the role column: one note's 41 inbound links were 39 catalogue entries
// and 23 prose mentions, two numbers that mean different things and read as one. A link in an
// argument carries the sentence that justifies it; a catalogue entry carries no argument at all,
// and says so by carrying no evidence.
func TestALinkCarriesItsRoleAndItsEvidence(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	got := linkRows(t, index, "Decisions/Stacked PRs.md")
	want := []linkRow{
		{
			target:   "Draft Pull Requests",
			key:      "draft pull requests",
			section:  "Stacked PRs",
			role:     roleProse,
			evidence: "We moved to stacked pull requests after [[Draft Pull Requests]] made every review wait on the whole branch.",
		},
		{
			target:   "2026-09-14 Review Queue",
			key:      "2026-09-14 review queue",
			section:  "Stacked PRs",
			role:     roleProse,
			evidence: "The measurements are in [[2026-09-14 Review Queue|the review queue writeup]], and the vocabulary in [[Glossary/Taint#Why it spreads]].",
		},
		{
			target:   "Glossary/Taint",
			key:      "taint",
			anchor:   "Why it spreads",
			section:  "Stacked PRs",
			role:     roleProse,
			evidence: "The measurements are in [[2026-09-14 Review Queue|the review queue writeup]], and the vocabulary in [[Glossary/Taint#Why it spreads]].",
		},
		{
			target:  "Draft Pull Requests",
			key:     "draft pull requests",
			section: "Related",
			role:    roleRelated,
			context: "- [[Draft Pull Requests]] — the preference this replaced",
		},
		{
			target:  "Triage Rota",
			key:     "triage rota",
			section: "Related",
			role:    roleRelated,
			context: "- [[Triage Rota]]",
		},
	}
	if !slices.Equal(got, want) {
		t.Errorf("links of Decisions/Stacked PRs.md:\n got %+v\nwant %+v", got, want)
	}
}

// A `[[link]]` in a fenced block is not a link, for the same reason a `# comment` in a shell block
// is not a heading. A naive extraction counted them and took the vault's unresolved-link figure
// from 69 to 58 once fixed.
func TestALinkInsideAFencedBlockIsNotCounted(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	if n := countRows(t, index, `select count(*) from links where target_key = 'fenced note'`); n != 0 {
		t.Errorf("%d link rows came out of a fenced code block", n)
	}
}

// A target nobody wrote a note for is recorded as a link that resolves to nothing, not dropped: a
// broken link is a fact about the vault, and a table that silently omitted them could not answer
// how many there are.
func TestAnUnresolvedLinkIsRecordedRatherThanDropped(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	n, err := index.UnresolvedLinks()
	if err != nil {
		t.Fatal(err)
	}
	// 2026-09-14 Review Queue, Nothing Anyone Wrote, and the two Triage Rota / Draft Pull
	// Requests targets that do resolve are not among them.
	if n != 2 {
		t.Errorf("UnresolvedLinks = %d, want 2", n)
	}
	if got := countRows(t, index, `select count(*) from links where target_key = 'nothing anyone wrote'`); got != 1 {
		t.Errorf("the unresolved target has %d rows, want 1", got)
	}
}

// What the `## Related` trailers were doing by hand, asked of the index instead — and split,
// because "who argued from this note" and "who filed it under see also" are different questions.
func TestBacklinksComeBackSplitByRole(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	back, err := index.Backlinks("Decisions/Triage Rota.md")
	if err != nil {
		t.Fatal(err)
	}
	var prose, related []string
	for _, link := range back.Prose {
		prose = append(prose, link.Path)
	}
	for _, link := range back.Related {
		related = append(related, link.Path)
	}
	if want := []string{"Decisions/Import Review Column.md", "Explorations/Supersession Noise.md"}; !slices.Equal(prose, want) {
		t.Errorf("prose backlinks = %q, want %q", prose, want)
	}
	if want := []string{"Decisions/Stacked PRs.md"}; !slices.Equal(related, want) {
		t.Errorf("related backlinks = %q, want %q", related, want)
	}
	// The evidence is what makes a backlink worth reading: it says why the other note reached for
	// this one, which the old trailer never did.
	if len(back.Prose) == 0 || !strings.Contains(back.Prose[0].Evidence, "shipped in March") {
		t.Errorf("prose backlink carries no evidence sentence: %+v", back.Prose)
	}
	for _, link := range back.Related {
		if link.Evidence != "" {
			t.Errorf("a catalogue entry came back with evidence: %+v", link)
		}
	}
}

// Reindexing a note must replace its links rather than add a second copy. The notes row survives
// an edit — writeNote updates it in place — so nothing cascades, and the rows have to be dropped
// by hand.
func TestReindexingANoteReplacesItsLinksRatherThanDuplicatingThem(t *testing.T) {
	index, vault := tempIndex(t, linkNotes)
	refresh(t, index)
	before := countRows(t, index, `select count(*) from links`)

	writeNoteFile(t, vault, "Decisions/Stacked PRs.md",
		strings.Replace(linkNotes["Decisions/Stacked PRs.md"], "made every review wait", "made each review wait", 1))
	touch(t, vault, "Decisions/Stacked PRs.md")
	refresh(t, index)

	if after := countRows(t, index, `select count(*) from links`); after != before {
		t.Errorf("%d link rows after reindexing one note, want %d", after, before)
	}
	if n := countRows(t, index, `select count(*) from supersessions`); n == 0 {
		t.Error("the supersession rows vanished on reindex")
	}

	if err := os.Remove(vault + "/Decisions/Stacked PRs.md"); err != nil {
		t.Fatal(err)
	}
	refresh(t, index)
	if n := countRows(t, index, `select count(*) from links l join notes n on n.id = l.note_id
		where n.path = 'Decisions/Stacked PRs.md'`); n != 0 {
		t.Errorf("%d link rows survived the note being deleted", n)
	}
}

// The line the whole supersession tier rests on. An extractor over this vault produced 60 edges
// naively and 24 constrained, of which only about three in four were defensible — so the column
// that decides what is current is written from the note's own frontmatter and from nothing else,
// and the heuristic's reading is kept beside it, labelled, with the sentence it read.
func TestOnlyAFrontmatterDeclarationIsAuthoritative(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	valid, err := index.Validity("Preferences/Draft Pull Requests.md")
	if err != nil {
		t.Fatal(err)
	}
	want := Validity{
		Status:       "archived",
		SupersededBy: "Kumquat Ledger",
		Evidence:     `superseded_by: "[[Kumquat Ledger]]"`,
		From:         "2024-02-01",
		Until:        "2026-09-14",
	}
	if valid != want {
		t.Errorf("Validity = %+v, want %+v", valid, want)
	}

	// A note whose PROSE claims a supersession declares nothing, and the authoritative column
	// stays empty for it. This is the whole distinction: 25% of the detector's edges were wrong,
	// and nothing wrong may land here.
	for _, rel := range []string{"Decisions/Import Review Column.md", "Explorations/Supersession Noise.md"} {
		var declared string
		if err := index.db.QueryRow(`select superseded_by from notes where path = ?`, rel).Scan(&declared); err != nil {
			t.Fatal(err)
		}
		if declared != "" {
			t.Errorf("%s has superseded_by = %q, written from prose rather than frontmatter", rel, declared)
		}
	}

	declared, err := index.DeclaredSupersessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(declared) != 1 || declared[0].Superseded != "Preferences/Draft Pull Requests.md" ||
		declared[0].Superseder != "Kumquat Ledger" || declared[0].Source != sourceDeclared {
		t.Errorf("declared supersessions = %+v, want the one frontmatter edge", declared)
	}
}

// The constraint that took the extractor from 60 edges to 24: the link has to be the verb's
// object. A link in the next sentence, a link past the 120-character window, a person as an
// endpoint and a target nobody wrote are all refused — and each refusal was a measured false
// positive, not a hypothetical one.
func TestADetectedSupersessionNeedsTheVerbsObject(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	detected, err := index.DetectedSupersessions()
	if err != nil {
		t.Fatal(err)
	}
	type edge struct{ superseder, superseded string }
	var got []edge
	for _, s := range detected {
		got = append(got, edge{s.Superseder, s.Superseded})
		if s.Source != sourceDetected {
			t.Errorf("a detected row came back labelled %q", s.Source)
		}
		if s.Evidence == "" {
			t.Errorf("a detected edge carries no evidence, so nobody can audit it: %+v", s)
		}
	}
	want := []edge{
		{"Decisions/Import Review Column.md", "Decisions/Triage Rota.md"},
		{"Decisions/Triage Rota.md", "Decisions/Import Review Column.md"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("detected supersessions:\n got %+v\nwant %+v", got, want)
	}
}

// Two notes in the real vault each claim to replace the other. A layer that decides what is
// current has to surface that rather than pick a direction, so the pair comes back as a pair.
func TestMutualClaimsAreReportedAsACycle(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	cycles, err := index.SupersessionCycles()
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"Decisions/Import Review Column.md", "Decisions/Triage Rota.md"}}
	if !slices.Equal(cycles, want) {
		t.Errorf("SupersessionCycles = %v, want %v", cycles, want)
	}
}

// Links and supersessions are structure, not text. Letting them into the full-text index would
// give every note a second body — the frontmatter target would rank for a search naming it, and
// an evidence sentence would be indexed twice — which is the same reason the rule and the
// frontmatter are kept out.
func TestLinkAndSupersessionTextStaysOutOfTheFullTextIndex(t *testing.T) {
	index, _ := tempIndex(t, linkNotes)
	refresh(t, index)

	// Words that exist only in frontmatter the supersession columns were read from. One term each:
	// the search ORs its terms, so a phrase would match on whichever word some body happens to
	// share and prove nothing.
	for _, query := range []string{"kumquat", "2024-02-01"} {
		if hits := search(t, index, query); len(hits) != 0 {
			t.Errorf("%q matched text only the typed columns hold: %+v", query, hits)
		}
	}

	// And structurally, so that adding a link column to the FTS table has to be a deliberate act
	// rather than a default nobody noticed.
	rows, err := index.db.Query(`select name from pragma_table_info('sections_fts')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if want := []string{"heading", "body", "aliases"}; !slices.Equal(columns, want) {
		t.Errorf("sections_fts columns = %q, want %q", columns, want)
	}
}

// The invariant the whole index rests on, re-checked over the link-heavy vault: a note's sections
// in seq order are still its body, byte for byte, and the full-text index still agrees with its
// content table after the link rows are written.
func TestWritingLinksLeavesTheSectionInvariantsIntact(t *testing.T) {
	index, vault := tempIndex(t, linkNotes)
	refresh(t, index)

	for rel := range linkNotes {
		raw, err := os.ReadFile(vault + "/" + rel)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := reassembled(t, index, rel), stripFrontmatter(string(raw)); got != want {
			t.Errorf("%s does not reassemble:\n got %q\nwant %q", rel, got, want)
		}
	}
	if _, err := index.db.Exec(`insert into sections_fts(sections_fts) values('integrity-check')`); err != nil {
		t.Errorf("the full-text index no longer matches its content: %v", err)
	}
}

// Every sentence here is phrased the way a note words a supersession. Direction is the thing: an edge
// stored the wrong way round does not read as a bad guess, it reads as the opposite claim, and two
// of these produced a supersession cycle between notes where only one direction was ever stated.
//
// An empty superseder and superseded mean no edge at all, which is the right answer for a passive
// whose agent is never named, for a negated object, and for an object that is a quotation or a code
// span rather than the link that happens to follow it.
var supersessionPhrasings = []struct {
	name       string
	prose      string
	superseder string
	superseded string
}{
	{
		name:       "a date between the passive and its by",
		prose:      "Partly superseded 2026-03-04 by [[2026-03-04 Split the Pantry Inventory]].",
		superseder: "2026-03-04 split the pantry inventory",
		superseded: "claiming note",
	},
	{
		name:       "a bolded aside between the passive and its by",
		prose:      "Superseded **the same day** by [[2026-04-11 Garden Planner Uses the Allotment's Gesture Set]].",
		superseder: "2026-04-11 garden planner uses the allotment's gesture set",
		superseded: "claiming note",
	},
	{
		name:       "a passive opening an emphasised line",
		prose:      "*Superseded 2026-05-02 by [[ABC-101 Part B Collapses to One Watering Event]].",
		superseder: "abc-101 part b collapses to one watering event",
		superseded: "claiming note",
	},
	{
		name:  "a passive that never names its agent",
		prose: "Both decisions were superseded before [[ABC-101 Part B Collapses to One Watering Event]] was written.",
	},
	{
		name:  "a negated object before the link",
		prose: "**This supersedes nothing previously decided.** The reasoning is in [[Import Review Column]].",
	},
	{
		name:  "a negated object with the link inside the sentence",
		prose: "This **supersedes nothing** in [[Test a Bread Timer Under a Fake Clock]]",
	},
	{
		name:  "a quoted object, with the link naming something else",
		prose: `This supersedes every "open / awaiting review / landable" statement below.** The tracker item that gated on the label is closed: [[Build the Seed Catalogue Save Path]]`,
	},
	{
		name:  "a quoted object spanning the rest of the sentence",
		prose: "Supersedes \"#12 is what is left… the only PR still blocking the user's own #34\"*\nThe rest is in [[Import Review Column]].",
	},
	{
		name:  "a code-spanned object",
		prose: "Supersedes the 03-30 MERGEABLE / UNSTABLE reading at head `deadbeef`.* See [[Import Review Column]].",
	},
	{
		name:  "a note quoting a statistic about supersessions",
		prose: "7 of 40 decisions are named as superseded by another note while still carrying `status: active`, with a supersession cycle between [[ABC-202 Total Column in the Harvest Table]]",
	},
	{
		name:       "a passive naming its agent at once",
		prose:      "Superseded by [[Import Review Column]] once the column shipped.",
		superseder: "import review column",
		superseded: "claiming note",
	},
	{
		name:  "a passive whose by names something that is not the link",
		prose: "The tasting and its write-up are done, so the date has been replaced by the gate · calendar · [[2026-05-20 Bakery Tasting Write-Up]]",
	},
	{
		name:  "a forward verb whose subject is the preceding link",
		prose: "The planner landed on one view ([[2026-04-11 Garden Planner Uses the Allotment's Gesture Set]], superseding\n[[2026-04-10 Garden Planner Has One View With Double-Click To Expand]], which came first).",
	},
	{
		name:       "a plain forward claim",
		prose:      "The column shipped in March, and this supersedes [[Triage Rota]].",
		superseder: "claiming note",
		superseded: "triage rota",
	},
	{
		// The verb is found in a lowercased copy of the section and the link in the section itself,
		// so the two copies have to be the same length. Thirty kelvin signs are three bytes each and
		// one byte lowercased by strings.ToLower, which is enough to put the link's offset past the
		// end of the copy — and a reader then loses the note to a panic over a claim about it.
		name:       "text whose unicode lowercase is shorter than the text",
		prose:      strings.Repeat("\u212a", 30) + " Superseded by [[Import Review Column]].",
		superseder: "import review column",
		superseded: "claiming note",
	},
}

func TestWhichWayADetectedSupersessionRuns(t *testing.T) {
	for _, phrasing := range supersessionPhrasings {
		t.Run(phrasing.name, func(t *testing.T) {
			got := detectEdges("claiming note", linkedSections("# Claiming Note\n\n"+phrasing.prose+"\n"))
			if phrasing.superseder == "" {
				if len(got) != 0 {
					t.Fatalf("an edge was recorded where the link is not the verb's object: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("detected %d edges, want 1: %+v", len(got), got)
			}
			if got[0].superseder != phrasing.superseder || got[0].superseded != phrasing.superseded {
				t.Errorf("edge runs %q -> %q, want %q -> %q",
					got[0].superseder, got[0].superseded, phrasing.superseder, phrasing.superseded)
			}
		})
	}
}
