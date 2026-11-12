package records

import (
	"slices"
	"testing"

	"github.com/alexbieg/alux/internal/taint"
)

func from(in RecordInput, ref string) RecordInput {
	in.Source = Source{Type: "file", Ref: ref}
	return in
}

func candidateIDs(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func TestASiblingNoteDuplicateIsWrittenAndShownFirstAmongSimilarRecords(t *testing.T) {
	s := open(t)
	earlier := write(t, s, from(rec("claim", "The compact_20260112 context edit is unwired and unprobed", nil), "Decisions/a.md"))
	write(t, s, from(rec("claim", "Subagent pills draw their state colour in the top bar", nil), "Decisions/a.md"))
	sibling := write(t, s, from(rec("claim", "The compact_20260112 edit was never sent on a request", nil), "Decisions/b.md"))

	res := write(t, s, from(rec("claim", "compact_20260112 remains unwired and unprobed", nil), "Decisions/b.md"))
	if res.ID == "" {
		t.Fatal("the record was not written")
	}
	cands := res.Similar
	if len(cands) == 0 || cands[0].ID != earlier.ID {
		t.Fatalf("similar = %+v, want %s first", cands, earlier.ID)
	}
	if slices.Contains(candidateIDs(cands), sibling.ID) {
		t.Errorf("a record from the writer's own note is listed: %v", candidateIDs(cands))
	}
	if !slices.Contains(cands[0].Signals, "shares compact_20260112") || !slices.Contains(cands[0].Signals, "same subject") {
		t.Errorf("signals = %q", cands[0].Signals)
	}
	for _, c := range cands {
		if c.Title == "Subagent pills draw their state colour in the top bar" {
			t.Errorf("an unrelated record is listed: %+v", c)
		}
	}
	if got, _ := s.Get(res.ID); len(got.Links) != 0 {
		t.Errorf("a hint was linked without the writer asking: %q", got.Links)
	}
	if n := count(t, s, `SELECT count(*) FROM candidate_suggestions WHERE written_id = ? AND candidate_id = ? AND rank = 1 AND score > 0`, res.ID, earlier.ID); n != 1 {
		t.Errorf("the top suggestion was logged %d times", n)
	}
}

func TestLoggedFeaturesTellAStrongTopMatchFromAWeakOneThoughBothRescaleToTheSameTitleScore(t *testing.T) {
	s := open(t)
	// bm25 weighs a word by how few records hold it, so the corpus needs records that share none of the drafts' words.
	for _, filler := range []string{"Mouse reporting is off by default", "Diffs use soft dark tints", "Vault filing is off this session",
		"Graph gates are approved by hand", "Bugbot runs only when asked", "The status line is cut to one row",
		"Fences render in catppuccin mocha", "Sessions prune to the newest 300", "Records are never edited in place"} {
		write(t, s, from(rec("claim", filler, nil), "a.md"))
	}
	write(t, s, from(rec("claim", "Window discovery reads max_input_tokens from the models endpoint at startup", nil), "a.md"))
	write(t, s, from(rec("claim", "Subagent pills draw their state colour in the top bar", nil), "a.md"))

	strong := write(t, s, from(rec("claim", "Window discovery reads max_input_tokens from the models endpoint", nil), "b.md"))
	weak := write(t, s, from(rec("claim", "Pills flash once when a run lands", nil), "c.md"))

	bm25 := func(written string) (title, raw float64) {
		t.Helper()
		err := s.db.QueryRow(`SELECT json_extract(features, '$.title'), json_extract(features, '$.bm25') FROM candidate_suggestions
			WHERE written_id = ? AND rank = 1`, written).Scan(&title, &raw)
		if err != nil {
			t.Fatal(err)
		}
		return title, raw
	}
	strongTitle, strongRaw := bm25(strong.ID)
	weakTitle, weakRaw := bm25(weak.ID)
	if strongTitle != weakTitle {
		t.Fatalf("want both top matches rescaled alike, got %v and %v", strongTitle, weakTitle)
	}
	if strongRaw <= 2*weakRaw {
		t.Errorf("raw bm25 of the strong top match %v is not well above the weak one's %v", strongRaw, weakRaw)
	}
}

func TestADuplicateOnAnotherSubjectIsShown(t *testing.T) {
	s := open(t)
	onHarness := write(t, s, from(rec("claim", "clear_thinking_20251015 must be the first context edit or the request is a 400", nil), "Decisions/a.md"))
	api := from(rec("claim", "The API rejects any edit order that does not put clear_thinking_20251015 first", nil), "Decisions/b.md")
	api.Subject = "Sam"
	cands := write(t, s, api).Similar
	if len(cands) == 0 || cands[0].ID != onHarness.ID || slices.Contains(cands[0].Signals, "same subject") {
		t.Fatalf("similar = %+v, want %s first and not marked same subject", cands, onHarness.ID)
	}
}

func TestALinkedRecordIsNoHintAndAReplacementIsNotShownWhatItsPredecessorSaw(t *testing.T) {
	s := open(t)
	a := write(t, s, from(rec("claim", "Window discovery reads max_input_tokens at startup", nil), "a.md"))
	b := write(t, s, from(rec("claim", "Window discovery caches max_input_tokens per model", nil), "b.md"))
	draft := from(rec("claim", "Window discovery reads max_input_tokens from the models endpoint", nil), "c.md")
	draft.Links = []Link{{Role: "see_also", Record: a.ID}}
	res := write(t, s, draft)
	if got := candidateIDs(res.Similar); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("similar = %v, want only %s", got, b.ID)
	}

	next := from(rec("claim", "Window discovery reads max_input_tokens from the models endpoint once", nil), "c.md")
	next.Supersedes = IDs{res.ID}
	if again := write(t, s, next); len(again.Similar) != 0 {
		t.Fatalf("a replacement was shown what its predecessor linked or saw: %v", candidateIDs(again.Similar))
	}
}

func TestARecordWithNothingSimilarComesBackWithNoHints(t *testing.T) {
	s := open(t)
	if res := write(t, s, from(rec("claim", "Mouse reporting is off by default", nil), "a.md")); res.ID == "" || len(res.Similar) != 0 {
		t.Fatalf("res %+v", res)
	}
}

func TestReplacedAndPHIRecordsAreNotShownToANormalWrite(t *testing.T) {
	s := open(t)
	t.Cleanup(taint.ResetForTest)
	if _, err := s.AddEntity(EntityInput{Type: "person", Name: "Person A", Sensitivity: "phi"}, agent); err != nil {
		t.Fatal(err)
	}
	old := write(t, s, from(rec("claim", "Window discovery reads max_input_tokens once", nil), "b.md"))
	newer := from(rec("claim", "Window discovery reads max_input_tokens once per session", nil), "b.md")
	newer.Supersedes = IDs{old.ID}
	current := write(t, s, newer)
	phi := from(rec("claim", "Window discovery reads max_input_tokens for Person A", nil), "c.md")
	phi.Subject, phi.Sensitivity = "Person A", "phi"
	phiRec := write(t, s, phi)

	taint.ResetForTest()
	if got := candidateIDs(write(t, s, from(rec("claim", "Window discovery reads max_input_tokens from the models endpoint", nil), "d.md")).Similar); !slices.Equal(got, []string{current.ID}) {
		t.Errorf("want only the current replacement listed, got %v", got)
	}
	if taint.Tainted() {
		t.Error("a normal write tainted the session")
	}

	phiWrite := from(rec("claim", "Window discovery reads max_input_tokens on Person A's run", nil), "e.md")
	phiWrite.Subject, phiWrite.Sensitivity = "Person A", "phi"
	if !slices.Contains(candidateIDs(write(t, s, phiWrite).Similar), phiRec.ID) {
		t.Error("a phi write does not see a phi duplicate")
	}
	if !taint.Tainted() {
		t.Error("listing a phi candidate did not taint")
	}
}

func TestANearNameEntityIsCreatedAndTheCloseNameShown(t *testing.T) {
	s := open(t)
	improvements := entity(t, s, EntityInput{Type: "project", Name: "Search Improvements", Sensitivity: "normal", Aliases: []string{"Ranking Optimization"}})
	if res := entity(t, s, EntityInput{Type: "repo", Name: "acme", Sensitivity: "normal"}); len(res.Similar) != 0 {
		t.Fatalf("an unrelated name was shown: %+v", res.Similar)
	}

	res := entity(t, s, EntityInput{Type: "project", Name: "Query Optimization", Sensitivity: "normal", Aliases: []string{"query_optimization"}})
	if res.ID == "" || res.ID == improvements.ID || len(res.Similar) == 0 || res.Similar[0].ID != improvements.ID {
		t.Fatalf("res %+v", res)
	}
	if got, _ := resolveEntity(s.db, "query_optimization"); got != res.ID {
		t.Errorf("the new alias resolves to %q, want the new entity %s", got, res.ID)
	}
	if n := count(t, s, `SELECT count(*) FROM candidate_suggestions WHERE target = 'entity' AND written_id = ? AND candidate_id = ?`, res.ID, improvements.ID); n != 1 {
		t.Errorf("the close name was logged %d times", n)
	}
}
