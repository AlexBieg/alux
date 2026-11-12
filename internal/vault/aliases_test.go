package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The defect these tests are about, measured over 30 sections of the real vault: a question that
// reuses the note's own words finds the right section 93% of the time, and one that does not
// finds it 30% of the time. Where a query shared two or fewer word stems with the target it
// retrieved nothing at all, 0 of 15 — and a single rare matching word turned a total miss into
// rank 1. Aliases are that one word, written before the question is asked.

var aliasNotes = map[string]string{
	"Systems/Recipe Scraper.md": `---
type: system
---
# Recipe Scraper

## Outcome

Scrapped once the model kept tagging desserts as soups, and no one
would own the escalation path.

## Related

[[Sam Weiss]]
`,

	"Glossary/Taint.md": `---
type: term
---
A note is tainted once it has been read in a conversation, which constrains
where the rest of that conversation may be sent.
`,
}

// sectionHashOf is the content key of one indexed section, taken from the index so the test
// hashes exactly the text Refresh hashed rather than its own guess at where a section ends.
func sectionHashOf(t *testing.T, index *Index, rel, heading string) string {
	t.Helper()
	var h, b string
	if err := index.db.QueryRow(`select s.heading, s.body from sections s join notes n on n.id = s.note_id
		where n.path = ? and s.heading = ?`, rel, heading).Scan(&h, &b); err != nil {
		t.Fatalf("no section %s › %s: %v", rel, heading, err)
	}
	return sectionHash(h, b)
}

// storeAliases writes aliases into the persistent store the way a generation run would, so the
// read side can be tested without an LLM.
func storeAliases(t *testing.T, hash string, aliases []string) {
	t.Helper()
	store, err := openAliasStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if err := store.put(hash, aliases, "test", 0, 0); err != nil {
		t.Fatal(err)
	}
}

func storedAliasHashes(t *testing.T) int {
	t.Helper()
	store, err := openAliasStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	have, err := store.hashes()
	if err != nil {
		t.Fatal(err)
	}
	return len(have)
}

func rebuild(t *testing.T, index *Index) {
	t.Helper()
	if _, err := index.Rebuild(); err != nil {
		t.Fatal(err)
	}
}

func aliasColumn(t *testing.T, index *Index, rel, heading string) string {
	t.Helper()
	var aliases string
	if err := index.db.QueryRow(`select s.aliases from sections s join notes n on n.id = s.note_id
		where n.path = ? and s.heading = ?`, rel, heading).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	return aliases
}

// The whole point of the feature: words the author never wrote reach the section anyway.
func TestAliasesMakeASectionFindableByWordsItDoesNotContain(t *testing.T) {
	index, _ := tempIndex(t, aliasNotes)
	refresh(t, index)

	const question = "mail sorter canned"
	if hits := search(t, index, question); len(hits) != 0 {
		t.Fatalf("the vault already answers %q, so this proves nothing: %+v", question, hits)
	}

	storeAliases(t, sectionHashOf(t, index, "Systems/Recipe Scraper.md", "Outcome"),
		[]string{"mail sorter shut down", "canned inbox tool"})
	rebuild(t, index)

	hits := search(t, index, question)
	if len(hits) == 0 {
		t.Fatalf("%q still finds nothing; aliases are not searchable", question)
	}
	if hits[0].Path != "Systems/Recipe Scraper.md" || hits[0].Heading != "Outcome" {
		t.Errorf("top hit is %s › %s, want Systems/Recipe Scraper.md › Outcome", hits[0].Path, hits[0].Heading)
	}
}

// A reader has to see the note's own words. A snippet drawn from the alias column would show
// them a machine paraphrase and let them believe the note says it.
func TestTheSnippetComesFromTheBodyNeverTheAliases(t *testing.T) {
	index, _ := tempIndex(t, aliasNotes)
	refresh(t, index)
	storeAliases(t, sectionHashOf(t, index, "Systems/Recipe Scraper.md", "Outcome"),
		[]string{"mail sorter shut down", "canned inbox tool"})
	rebuild(t, index)

	hits := search(t, index, "mail sorter canned")
	if len(hits) == 0 {
		t.Fatal("no hits for the alias vocabulary")
	}
	snippet := strings.ToLower(hits[0].Snippet)
	for _, word := range []string{"sorter", "canned"} {
		if strings.Contains(snippet, word) {
			t.Errorf("the snippet shows alias text rather than the note: %q", hits[0].Snippet)
		}
	}
	if !strings.Contains(snippet, "scrapped") {
		t.Errorf("the snippet does not show the section's own words: %q", hits[0].Snippet)
	}
}

// What the alias column's bm25 weight buys, pinned as an ordering rather than as a number: a
// paraphrase beats a passing mention in prose, and loses to a heading that names the subject.
func TestAnAliasOutranksABodyMatchAndLosesToAHeading(t *testing.T) {
	index, _ := tempIndex(t, map[string]string{
		"Systems/Kumquat Pipeline.md": `---
type: system
---
## Kumquat Pipeline

Nightly batch, parked in June when the export queue stopped draining.
`,
		"Systems/Export Queue.md": `---
type: system
---
## Outcome

The queue drains every night unless the kumquat batch is parked by hand.
`,
		"Systems/Nightly Batch.md": `---
type: system
---
## Outcome

Runs after midnight and writes a summary into the shared inbox.
`,
	})
	refresh(t, index)
	storeAliases(t, sectionHashOf(t, index, "Systems/Nightly Batch.md", "Outcome"), []string{"kumquat schedule"})
	rebuild(t, index)

	var order []string
	for _, h := range search(t, index, "kumquat") {
		order = append(order, h.Path)
	}
	want := []string{"Systems/Kumquat Pipeline.md", "Systems/Nightly Batch.md", "Systems/Export Queue.md"}
	if !slices.Equal(order, want) {
		t.Errorf("ranking is %v, want %v (heading, then alias, then body)", order, want)
	}
}

// The key is the section's text, so an edit invalidates nothing explicitly: the edited section
// simply has a key nobody has generated for, and is indexed as it was before aliases existed.
// Restoring the text restores the aliases, which is what content addressing is for.
func TestEditingASectionStopsItsOldAliasesApplying(t *testing.T) {
	const edited = `---
type: system
---
# Recipe Scraper

## Outcome

Retired in August after the escalation path was never agreed.
`
	index, vault := tempIndex(t, aliasNotes)
	refresh(t, index)
	storeAliases(t, sectionHashOf(t, index, "Systems/Recipe Scraper.md", "Outcome"),
		[]string{"mail sorter shut down", "canned inbox tool"})
	rebuild(t, index)
	if len(search(t, index, "mail sorter canned")) == 0 {
		t.Fatal("the aliases never applied in the first place")
	}

	original, err := os.ReadFile(filepath.Join(vault, "Systems/Recipe Scraper.md"))
	if err != nil {
		t.Fatal(err)
	}
	writeNoteFile(t, vault, "Systems/Recipe Scraper.md", edited)
	touch(t, vault, "Systems/Recipe Scraper.md")
	refresh(t, index)

	if hits := search(t, index, "mail sorter canned"); len(hits) != 0 {
		t.Errorf("aliases written for the old text still answer for the new: %+v", hits)
	}
	if got := aliasColumn(t, index, "Systems/Recipe Scraper.md", "Outcome"); got != "" {
		t.Errorf("the edited section carries stale aliases: %q", got)
	}

	writeNoteFile(t, vault, "Systems/Recipe Scraper.md", string(original))
	touch(t, vault, "Systems/Recipe Scraper.md")
	refresh(t, index)
	if len(search(t, index, "mail sorter canned")) == 0 {
		t.Error("restoring the text did not restore its aliases; the key is not the content")
	}
}

// The index is derived and disposable — /vault rebuild drops it, a schema bump recreates it, and
// deleting the file costs one rebuild. Aliases cost money, so none of those may lose them. This
// is the round trip the whole separate store exists for.
func TestRebuildingTheIndexKeepsTheAliases(t *testing.T) {
	index, vault := tempIndex(t, aliasNotes)
	refresh(t, index)
	storeAliases(t, sectionHashOf(t, index, "Systems/Recipe Scraper.md", "Outcome"),
		[]string{"mail sorter shut down", "canned inbox tool"})
	rebuild(t, index)

	const question = "mail sorter canned"
	if len(search(t, index, question)) == 0 {
		t.Fatal("the aliases never applied in the first place")
	}

	// 1: an explicit rebuild, which drops every table in the index.
	rebuild(t, index)
	if len(search(t, index, question)) == 0 {
		t.Error("a rebuild lost the aliases")
	}

	// 2: a schema bump, which recreates the index from scratch on the next open.
	if _, err := index.db.Exec(`pragma user_version = 0`); err != nil {
		t.Fatal(err)
	}
	index.Close()
	reopened, err := OpenIndex(vault)
	if err != nil {
		t.Fatal(err)
	}
	refresh(t, reopened)
	if len(search(t, reopened, question)) == 0 {
		t.Error("a schema bump lost the aliases")
	}

	// 3: the index file deleted outright, the way a cache sweep would take it.
	reopened.Close()
	if err := os.Remove(indexPath()); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(aliasStorePath(), filepath.Dir(indexPath())) {
		t.Fatalf("the alias store sits inside the index's directory (%s), so a cache sweep takes it too", aliasStorePath())
	}
	fresh, err := OpenIndex(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	refresh(t, fresh)
	if len(search(t, fresh, question)) == 0 {
		t.Error("deleting the index lost the aliases")
	}
}

// recorder is a generator that answers from a script and remembers what it was asked.
type recorder struct {
	mu    sync.Mutex
	seen  []string
	reply func(heading, body string) (AliasReply, error)
}

func (r *recorder) Generate(_ context.Context, heading, body string) (AliasReply, error) {
	r.mu.Lock()
	r.seen = append(r.seen, heading)
	r.mu.Unlock()
	if r.reply != nil {
		return r.reply(heading, body)
	}
	return AliasReply{Aliases: []string{"paraphrase of " + heading}, InputTokens: 100, OutputTokens: 10}, nil
}

func (r *recorder) headings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// A bare "Related" trailer is withheld from the full-text index, so an alias for it could never
// match anything. Generating one is money spent on nothing.
func TestGenerationSkipsSectionsThatAreNotSearchable(t *testing.T) {
	index, vault := tempIndex(t, aliasNotes)
	refresh(t, index)

	gen := &recorder{}
	run, err := GenerateAliases(context.Background(), vault, gen, 2)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(gen.headings(), "Related") {
		t.Errorf("a link trailer was sent to the generator: %v", gen.headings())
	}
	if got := aliasColumn(t, index, "Systems/Recipe Scraper.md", "Related"); got != "" {
		t.Errorf("a withheld section carries aliases: %q", got)
	}
	if run.Generated != len(gen.headings()) {
		t.Errorf("run reports %d generated, generator saw %d", run.Generated, len(gen.headings()))
	}
}

// Generation that left the aliases unreachable until the next unrelated edit would look like it
// had done nothing: generating changes no note, and Refresh only reindexes notes that changed.
func TestGeneratedAliasesAreSearchableWithoutAFurtherEdit(t *testing.T) {
	index, vault := tempIndex(t, aliasNotes)
	refresh(t, index)

	gen := &recorder{reply: func(heading, _ string) (AliasReply, error) {
		return AliasReply{Aliases: []string{"rutabaga " + heading}, InputTokens: 50, OutputTokens: 5}, nil
	}}
	if _, err := GenerateAliases(context.Background(), vault, gen, 1); err != nil {
		t.Fatal(err)
	}
	hits := search(t, index, "rutabaga")
	if len(hits) == 0 {
		t.Fatal("nothing generated this run is searchable yet")
	}
}

// A model that answers with prose, or with the wrong shape, costs one section its aliases and
// nothing else. The run must not fail, and the answer must be recorded so a resumed run does not
// pay for it a second time.
func TestAMalformedAnswerRecordsNoAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"well formed", `{"aliases":["mail sorter","inbox triage"]}`, []string{"mail sorter", "inbox triage"}},
		{"prose", `I'm sorry, I can't help with that.`, nil},
		{"wrong type", `{"aliases":"mail sorter"}`, nil},
		{"wrong key", `{"phrases":["mail sorter"]}`, nil},
		{"blanks dropped", `{"aliases":["   ","mail sorter"]}`, []string{"mail sorter"}},
		{"a sentence is not an alias", `{"aliases":["` + strings.Repeat("word ", 40) + `"]}`, nil},
		{"capped at four", `{"aliases":["one","two","three","four","five"]}`, []string{"one", "two", "three", "four"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := aliasesFrom(json.RawMessage(tc.raw)); !slices.Equal(got, tc.want) {
				t.Errorf("aliasesFrom(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}

	index, vault := tempIndex(t, aliasNotes)
	refresh(t, index)
	gen := &recorder{reply: func(string, string) (AliasReply, error) {
		return AliasReply{Aliases: aliasesFrom(json.RawMessage("not json at all")), InputTokens: 80}, nil
	}}
	run, err := GenerateAliases(context.Background(), vault, gen, 1)
	if err != nil {
		t.Fatalf("one unreadable answer failed the whole run: %v", err)
	}
	if run.Empty != run.Generated || run.Generated == 0 {
		t.Errorf("run reports %d generated and %d empty, want every section empty", run.Generated, run.Empty)
	}
	if run.Failed != 0 {
		t.Errorf("an unreadable answer was counted as a failure: %+v", run)
	}

	second := &recorder{}
	if _, err := GenerateAliases(context.Background(), vault, second, 1); err != nil {
		t.Fatal(err)
	}
	if got := second.headings(); len(got) != 0 {
		t.Errorf("a resumed run paid again for answers already recorded as empty: %v", got)
	}
}

// Generation over the real vault is thousands of calls, so it will be interrupted. What it has
// already paid for must not be paid for twice — and identical text in two notes is one bill.
func TestGenerationResumesAndSkipsWhatItAlreadyHas(t *testing.T) {
	const duplicated = `---
type: system
---
## Outcome

The queue drains every night unless the batch is parked by hand.
`
	index, vault := tempIndex(t, map[string]string{
		"Systems/Export Queue.md": duplicated,
		"Systems/Import Queue.md": duplicated,
		"Glossary/Taint.md":       aliasNotes["Glossary/Taint.md"],
	})
	refresh(t, index)

	// First run: the glossary section fails, everything else lands.
	flaky := &recorder{reply: func(heading, _ string) (AliasReply, error) {
		if heading == "(whole note)" {
			return AliasReply{}, fmt.Errorf("the api said no")
		}
		return AliasReply{Aliases: []string{"rutabaga " + heading}, InputTokens: 100, OutputTokens: 10}, nil
	}}
	first, err := GenerateAliases(context.Background(), vault, flaky, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sections != 3 || first.Distinct != 2 {
		t.Errorf("run saw %d sections and %d distinct texts, want 3 and 2", first.Sections, first.Distinct)
	}
	if first.Failed != 1 {
		t.Errorf("run reports %d failures, want 1: %+v", first.Failed, first)
	}
	// One call's worth, not two: the two notes share their text, so the bill is shared too.
	if got, want := first.InputTokens, int64(100); got != want {
		t.Errorf("run reports %d input tokens, want %d", got, want)
	}

	// Second run: only the section the first one could not do.
	second := &recorder{}
	resumed, err := GenerateAliases(context.Background(), vault, second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := second.headings(), []string{"(whole note)"}; !slices.Equal(got, want) {
		t.Errorf("the resumed run asked for %v, want only %v", got, want)
	}
	if resumed.Skipped != 1 {
		t.Errorf("resumed run reports %d skipped, want 1 (the shared text): %+v", resumed.Skipped, resumed)
	}
}

// Refresh runs on every vault_search and every sectioned vault_read. If a missing alias could
// start a generation, an ordinary search would block on the network and spend money.
func TestRefreshNeverGenerates(t *testing.T) {
	index, _ := tempIndex(t, aliasNotes)
	refresh(t, index)
	if _, err := index.Search("scrapped soups escalation", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := index.ReadSection("Systems/Recipe Scraper.md", "Outcome"); err != nil {
		t.Fatal(err)
	}
	rebuild(t, index)
	if n := storedAliasHashes(t); n != 0 {
		t.Errorf("reading the vault wrote %d rows to the alias store; generation is on the search path", n)
	}
}
